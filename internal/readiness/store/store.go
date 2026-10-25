// Package store keeps integration readiness reports in the data directory (master plan 9.5 and 19,
// ADR 0037): every report with a hash of its inputs, a bounded history per key, the latest, and a
// diff between two reports (newly met, newly missing, changed).
//
// # What is stored
//
// The typed report document (schemas/readiness/report) as canonical JSON, with a few indexed
// columns. A report holds findings with their evidence entries (path, line span, file hash) and
// the bounded, redacted summary text of each; it never holds the content of a scanned file, and
// Save re-checks that: every text is bounded and passed through the scanner's redactor again, so
// a secret that slipped into a summary does not reach the database.
//
// # The key
//
// Readiness runs on a directory before an application has a package, so reports are not keyed by
// tenant. The key is the application id the operator names (RdyAppKey) or a project id derived
// from a hash of the directory's canonical path (RdyProjectKey). Rows are key-level, in the
// module's own table with tenant and workspace empty, and every statement names all three scope
// columns, so one key never sees another key's reports. The raw path of a directory is never used
// as a key or a file name: it is hashed, and the report keeps it only as display text with control
// characters removed.
//
// # Authority
//
// The store keeps what the builder produced and refuses what it cannot stand behind: a covered
// requirement must rest on a rule or a model_verified finding, never on a model_unverified one;
// the summary must add up; ids, hashes and paths must have their schema shapes. It also sets the
// report's previous_report_id itself, from the history it holds.
//
// Every package-level identifier here starts with rdy or Rdy.
package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/readiness/catalog"
	"github.com/buxee/alix-ai/internal/readiness/scan"
	corestore "github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

//go:embed migrations/*.sql
var rdyMigrations embed.FS

func init() { corestore.RegisterMigrations("readiness", rdyMigrations) }

// Bounds.
const (
	// RdyMaxHistory is how many reports one key keeps; older ones are pruned when a new one is saved.
	RdyMaxHistory = 100
	// rdyMaxDocBytes bounds one stored document.
	rdyMaxDocBytes = 8 << 20
	rdyMaxText     = 1000
	rdyMaxFindings = 5000
	rdyMaxEvidence = 32
)

// Every statement of the module carries this predicate: a report row is key-level, so tenant and
// workspace are the empty string and the key is the application column.
const rdyWhere = "application_id = ? AND tenant_id = '' AND workspace_id = ''"

// RdyKey is the scope key reports are stored under.
type RdyKey string

var (
	rdyAppRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.\-]{0,63}$`)
	rdyProjectRE = regexp.MustCompile(`^prj_[0-9a-f]{32}$`)
	rdyTypedRE   = regexp.MustCompile(`^[a-z][a-z0-9]{1,7}_[0-9A-HJKMNP-TV-Z]{26}$`)
	rdyFindingRE = regexp.MustCompile(`^fnd_[0-9a-f]{16}$`)
	rdyHexRE     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	rdyReqRE     = regexp.MustCompile(`^req(\.[a-z][a-z0-9_]*){1,4}$`)
	rdyCheckRE   = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	rdyTimeRE    = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]{1,9})?Z$`)
)

// RdyAppKey is the key for an application id the operator named. The grammar is a plain
// identifier; the project-key shape is refused so the two namespaces cannot collide.
func RdyAppKey(app string) (RdyKey, error) {
	if !rdyAppRE.MatchString(app) || rdyProjectRE.MatchString(app) {
		return "", contracts.New(contracts.ErrValidationFailed, "readiness store: the application id is not a plain identifier")
	}
	return RdyKey(app), nil
}

// RdyProjectKey is the key for a project directory: prj_ and the first 32 hex digits of a hash of
// its canonical absolute path (symbolic links resolved when the directory exists, case folded on
// Windows). The path itself goes nowhere else.
func RdyProjectKey(root string) (RdyKey, error) {
	if strings.TrimSpace(root) == "" || strings.ContainsRune(root, 0) {
		return "", contracts.New(contracts.ErrValidationFailed, "readiness store: the project directory is not a usable path")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", contracts.New(contracts.ErrValidationFailed, "readiness store: the project directory cannot be resolved")
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	abs = filepath.ToSlash(filepath.Clean(abs))
	if runtime.GOOS == "windows" {
		abs = strings.ToLower(abs)
	}
	sum := sha256.Sum256([]byte("alixai-readiness-project\x00" + abs))
	return RdyKey("prj_" + hex.EncodeToString(sum[:])[:32]), nil
}

func (k RdyKey) valid() error {
	if !rdyAppRE.MatchString(string(k)) && !rdyProjectRE.MatchString(string(k)) {
		return contracts.New(contracts.ErrScopeMissing, "readiness store: no valid key")
	}
	return nil
}

// RdyStore is the report store.
type RdyStore struct {
	db  *corestore.DB
	clk clock.Clock
}

// RdyOpen returns the store over an open database. A nil clock means the real one.
func RdyOpen(db *corestore.DB, clk clock.Clock) *RdyStore {
	if clk == nil {
		clk = clock.Real{}
	}
	return &RdyStore{db: db, clk: clk}
}

// RdySaved is what Save reports.
type RdySaved struct {
	ReportID         common.TypedID
	PreviousReportID *common.TypedID
	// InputsChanged is false for a first report, and when the files, catalog and options hash the
	// same as the previous report's.
	InputsChanged bool
	// Pruned is how many old reports the history cap removed.
	Pruned int
}

// RdyHistoryEntry is one line of the history, newest first.
type RdyHistoryEntry struct {
	ReportID         common.TypedID
	PreviousReportID *common.TypedID
	GeneratedAt      time.Time
	StoredAt         time.Time
	InputsSHA256     string
	CatalogSHA256    string
	Mode             readiness.ReportMode
	Summary          readiness.Summary
}

func rdyWrap(err error, msg string) error {
	if err == nil {
		return nil
	}
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return err
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, msg)
}

func rdyInvalid(format string, args ...any) error {
	return contracts.New(contracts.ErrValidationFailed, "readiness store: "+fmt.Sprintf(format, args...))
}

// Save stores a report under the key. It sets previous_report_id from the history it holds, keeps
// at most RdyMaxHistory reports and prunes the oldest. A report that fails validation is refused
// and nothing is written.
func (s *RdyStore) Save(ctx context.Context, key RdyKey, doc readiness.Report) (RdySaved, error) {
	if err := key.valid(); err != nil {
		return RdySaved{}, err
	}
	doc, err := rdyScrub(doc)
	if err != nil {
		return RdySaved{}, err
	}
	if err := rdyValidate(doc); err != nil {
		return RdySaved{}, err
	}
	gen, err := time.Parse(time.RFC3339Nano, string(doc.GeneratedAt))
	if err != nil {
		return RdySaved{}, rdyInvalid("generated_at is not a timestamp")
	}
	var saved RdySaved
	err = s.db.Tx(ctx, func(tx *corestore.Tx) error {
		saved = RdySaved{}
		var prevID, prevInputs, prevCatalog sql.NullString
		row := tx.QueryRowContext(ctx,
			"SELECT report_id, inputs_sha256, catalog_sha256 FROM readiness_reports WHERE "+rdyWhere+" ORDER BY seq DESC LIMIT 1", string(key))
		switch err := row.Scan(&prevID, &prevInputs, &prevCatalog); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return rdyWrap(err, "readiness store: reading the history failed")
		}
		d := doc
		d.PreviousReportID = nil
		if prevID.Valid {
			p := common.TypedID(prevID.String)
			d.PreviousReportID = &p
			saved.PreviousReportID = &p
			saved.InputsChanged = prevInputs.String != string(d.InputsSHA256) || prevCatalog.String != string(d.CatalogSHA256)
		}
		raw, err := json.Marshal(d)
		if err != nil {
			return contracts.Wrap(contracts.ErrInternal, err, "readiness store: a report cannot be encoded")
		}
		if len(raw) > rdyMaxDocBytes {
			return rdyInvalid("the report is larger than %d bytes", rdyMaxDocBytes)
		}
		var prevCol any
		if d.PreviousReportID != nil {
			prevCol = string(*d.PreviousReportID)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO readiness_reports
			   (application_id, tenant_id, workspace_id, report_id, previous_report_id, inputs_sha256, catalog_sha256,
			    mode, found, partial, missing, unverified, contradicted, not_applicable, document, generated_at, stored_at)
			 VALUES (?, '', '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			string(key), string(d.ReportID), prevCol, string(d.InputsSHA256), string(d.CatalogSHA256), string(d.Mode),
			d.Summary.Found, d.Summary.Partial, d.Summary.Missing, d.Summary.Unverified, d.Summary.Contradicted, rdyNotApplicable(d.Summary),
			string(raw), gen.UnixMilli(), s.clk.Now().UnixMilli())
		if err != nil {
			if corestore.IsUniqueViolation(err) {
				return contracts.New(contracts.ErrConflict, "readiness store: a report with this id is already stored")
			}
			return rdyWrap(err, "readiness store: storing the report failed")
		}
		saved.ReportID = d.ReportID
		res, err := tx.ExecContext(ctx,
			`DELETE FROM readiness_reports WHERE `+rdyWhere+` AND seq NOT IN
			   (SELECT seq FROM readiness_reports WHERE `+rdyWhere+` ORDER BY seq DESC LIMIT ?)`,
			string(key), string(key), RdyMaxHistory)
		if err != nil {
			return rdyWrap(err, "readiness store: pruning the history failed")
		}
		n, _ := res.RowsAffected()
		saved.Pruned = int(n)
		return nil
	})
	if err != nil {
		return RdySaved{}, err
	}
	return saved, nil
}

type rdyRow struct {
	reportID, inputs, catalog, doc string
}

// decode reads a stored document strictly: an unknown member or value is a corrupted row, never
// silently dropped, and the row's columns must agree with the document.
func (r rdyRow) decode() (readiness.Report, error) {
	var doc readiness.Report
	dec := json.NewDecoder(bytes.NewReader([]byte(r.doc)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return readiness.Report{}, contracts.Wrap(contracts.ErrInternal, err, "readiness store: a stored report cannot be decoded")
	}
	if string(doc.ReportID) != r.reportID || string(doc.InputsSHA256) != r.inputs || string(doc.CatalogSHA256) != r.catalog {
		return readiness.Report{}, contracts.New(contracts.ErrInternal, "readiness store: a stored report does not match its row")
	}
	return doc, nil
}

func (s *RdyStore) one(ctx context.Context, key RdyKey, where string, args ...any) (readiness.Report, bool, error) {
	if err := key.valid(); err != nil {
		return readiness.Report{}, false, err
	}
	var r rdyRow
	all := append([]any{string(key)}, args...)
	err := s.db.QueryRowContext(ctx,
		"SELECT report_id, inputs_sha256, catalog_sha256, document FROM readiness_reports WHERE "+rdyWhere+where, all...).
		Scan(&r.reportID, &r.inputs, &r.catalog, &r.doc)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return readiness.Report{}, false, nil
	case err != nil:
		return readiness.Report{}, false, rdyWrap(err, "readiness store: reading a report failed")
	}
	doc, err := r.decode()
	if err != nil {
		return readiness.Report{}, false, err
	}
	return doc, true, nil
}

// Latest returns the newest report of the key.
func (s *RdyStore) Latest(ctx context.Context, key RdyKey) (readiness.Report, bool, error) {
	return s.one(ctx, key, " ORDER BY seq DESC LIMIT 1")
}

// Get returns one report of the key. A report of another key is not found.
func (s *RdyStore) Get(ctx context.Context, key RdyKey, id common.TypedID) (readiness.Report, bool, error) {
	return s.one(ctx, key, " AND report_id = ?", string(id))
}

// History lists the key's reports, newest first, at most limit (RdyMaxHistory when limit is not
// positive).
func (s *RdyStore) History(ctx context.Context, key RdyKey, limit int) ([]RdyHistoryEntry, error) {
	if err := key.valid(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > RdyMaxHistory {
		limit = RdyMaxHistory
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT report_id, previous_report_id, generated_at, stored_at, inputs_sha256, catalog_sha256, mode,
		        found, partial, missing, unverified, contradicted, not_applicable
		   FROM readiness_reports WHERE `+rdyWhere+` ORDER BY seq DESC LIMIT ?`, string(key), limit)
	if err != nil {
		return nil, rdyWrap(err, "readiness store: reading the history failed")
	}
	defer rows.Close()
	out := []RdyHistoryEntry{}
	for rows.Next() {
		var e RdyHistoryEntry
		var id, mode string
		var prev sql.NullString
		var gen, stored, na int64
		if err := rows.Scan(&id, &prev, &gen, &stored, &e.InputsSHA256, &e.CatalogSHA256, &mode,
			&e.Summary.Found, &e.Summary.Partial, &e.Summary.Missing, &e.Summary.Unverified, &e.Summary.Contradicted, &na); err != nil {
			return nil, rdyWrap(err, "readiness store: reading the history failed")
		}
		if na > 0 {
			e.Summary.NotApplicable = &na
		}
		e.ReportID, e.Mode = common.TypedID(id), readiness.ReportMode(mode)
		if prev.Valid {
			p := common.TypedID(prev.String)
			e.PreviousReportID = &p
		}
		e.GeneratedAt, e.StoredAt = time.UnixMilli(gen).UTC(), time.UnixMilli(stored).UTC()
		out = append(out, e)
	}
	return out, rdyWrap(rows.Err(), "readiness store: reading the history failed")
}

// Diff compares two stored reports of the key.
func (s *RdyStore) Diff(ctx context.Context, key RdyKey, from, to common.TypedID) (RdyDiff, error) {
	a, ok, err := s.Get(ctx, key, from)
	if err != nil {
		return RdyDiff{}, err
	}
	if !ok {
		return RdyDiff{}, rdyInvalid("no such report to compare from")
	}
	b, ok, err := s.Get(ctx, key, to)
	if err != nil {
		return RdyDiff{}, err
	}
	if !ok {
		return RdyDiff{}, rdyInvalid("no such report to compare to")
	}
	return RdyDiffReports(a, b), nil
}

// DiffLatest compares the latest report with the one before it. found is false when the key has
// fewer than two reports.
func (s *RdyStore) DiffLatest(ctx context.Context, key RdyKey) (RdyDiff, bool, error) {
	hist, err := s.History(ctx, key, 2)
	if err != nil {
		return RdyDiff{}, false, err
	}
	if len(hist) < 2 {
		return RdyDiff{}, false, nil
	}
	d, err := s.Diff(ctx, key, hist[1].ReportID, hist[0].ReportID)
	return d, err == nil, err
}

// StatusView builds the status of the latest report with what changed since the one before it.
// found is false when the key has no report.
func (s *RdyStore) StatusView(ctx context.Context, key RdyKey) (readiness.StatusView, bool, error) {
	hist, err := s.History(ctx, key, 2)
	if err != nil || len(hist) == 0 {
		return readiness.StatusView{}, false, err
	}
	latest, ok, err := s.Get(ctx, key, hist[0].ReportID)
	if err != nil || !ok {
		return readiness.StatusView{}, false, err
	}
	var prev *readiness.Report
	if len(hist) > 1 {
		p, ok, err := s.Get(ctx, key, hist[1].ReportID)
		if err != nil {
			return readiness.StatusView{}, false, err
		}
		if ok {
			prev = &p
		}
	}
	return RdyStatusView(latest, prev, s.clk.Now()), true, nil
}

// rdyScrub returns a copy of the report with the texts a hostile or careless producer could have
// let a secret into redacted again, and the directory display text cleaned.
func rdyScrub(doc readiness.Report) (readiness.Report, error) {
	var b strings.Builder
	for _, r := range doc.ScannedRoot {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			r = '?'
		}
		b.WriteRune(r)
	}
	doc.ScannedRoot = b.String()
	doc.Findings = append([]readiness.Finding(nil), doc.Findings...)
	for i := range doc.Findings {
		f := &doc.Findings[i]
		if f.Summary != nil {
			t := readiness.Text(scan.ScanRedact(string(*f.Summary)))
			f.Summary = &t
		}
		if f.UnverifiedReason != nil {
			t := readiness.Text(scan.ScanRedact(string(*f.UnverifiedReason)))
			f.UnverifiedReason = &t
		}
	}
	return doc, nil
}

func rdySafePath(p string) bool {
	if p == "" || len(p) > 512 || !utf8.ValidString(p) || p[0] == '/' {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == '\\' || r == ':' {
			return false
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// rdyValidate refuses a report the store cannot stand behind.
func rdyValidate(d readiness.Report) error {
	switch {
	case d.SchemaVersion != 1:
		return rdyInvalid("schema_version %d is not supported", d.SchemaVersion)
	case !d.Advisory:
		return rdyInvalid("a report must be advisory")
	case !rdyTypedRE.MatchString(string(d.ReportID)):
		return rdyInvalid("report_id is not a typed id")
	case d.PreviousReportID != nil && !rdyTypedRE.MatchString(string(*d.PreviousReportID)):
		return rdyInvalid("previous_report_id is not a typed id")
	case !rdyTimeRE.MatchString(string(d.GeneratedAt)):
		return rdyInvalid("generated_at is not a timestamp")
	case d.ScannedRoot == "" || len(d.ScannedRoot) > 1024:
		return rdyInvalid("scanned_root is empty or too long")
	case !rdyHexRE.MatchString(string(d.InputsSHA256)) || !rdyHexRE.MatchString(string(d.CatalogSHA256)):
		return rdyInvalid("a hash is not 64 lowercase hex digits")
	case d.CatalogVersion < 1:
		return rdyInvalid("catalog_version is not positive")
	case !d.Mode.Valid():
		return rdyInvalid("mode is not valid")
	case (d.Mode == readiness.ReportModeDeterministicAndModel) != (d.Model != nil):
		return rdyInvalid("the model must be present exactly when the mode says a model ran")
	case len(d.Checklist) < 1 || len(d.Checklist) > 512:
		return rdyInvalid("the checklist has %d entries", len(d.Checklist))
	case len(d.Findings) > rdyMaxFindings:
		return rdyInvalid("more than %d findings", rdyMaxFindings)
	}
	if d.Model != nil {
		if d.Model.ModelID == "" || len(d.Model.ModelID) > 128 || !d.Model.Runtime.Valid() || !d.Model.Qualification.Valid() {
			return rdyInvalid("the model is not described")
		}
		if l := d.Model.Label; l != nil {
			want := readiness.ReportModelLabelUnqualifiedAdvisory
			if d.Model.Qualification == readiness.ReportModelQualificationQualified {
				want = readiness.ReportModelLabelQualifiedAdvisory
			}
			if *l != want {
				return rdyInvalid("the model label does not follow its qualification")
			}
		}
	}
	if l := d.ModelLeg; l != nil {
		ran := l.Status == readiness.ReportModelLegStatusRan
		switch {
		case !l.Status.Valid():
			return rdyInvalid("the model leg has no valid status")
		case l.Calls < 0 || l.Calls > 64 || l.ProposalsReceived < 0 || l.DroppedProposals < 0 || l.AttemptedOverrides < 0 ||
			l.AttemptedOverrides > l.DroppedProposals:
			return rdyInvalid("the model leg counts are out of range")
		case l.PromptTemplateSHA256 != nil && !rdyHexRE.MatchString(string(*l.PromptTemplateSHA256)):
			return rdyInvalid("the prompt template digest is not 64 lowercase hex digits")
		case ran && (l.PromptTemplateSHA256 == nil || d.Mode != readiness.ReportModeDeterministicAndModel):
			return rdyInvalid("a model leg that ran needs its template digest and the model mode")
		case !ran && d.Mode != readiness.ReportModeDeterministicOnly:
			return rdyInvalid("a model leg that did not run leaves the report deterministic_only")
		}
	}
	byID := map[readiness.FindingID]readiness.Finding{}
	for i, f := range d.Findings {
		if !rdyFindingRE.MatchString(string(f.FindingID)) {
			return rdyInvalid("finding %d has a malformed id", i)
		}
		if _, dup := byID[f.FindingID]; dup {
			return rdyInvalid("finding id %s appears twice", f.FindingID)
		}
		byID[f.FindingID] = f
		if f.SchemaVersion != 1 || !f.Status.Valid() || !f.Source.Valid() || !f.Category.Valid() ||
			!rdyReqRE.MatchString(string(f.RequirementID)) || !rdyCheckRE.MatchString(string(f.CheckID)) {
			return rdyInvalid("finding %s is malformed", f.FindingID)
		}
		if len(f.Evidence) > rdyMaxEvidence {
			return rdyInvalid("finding %s cites more than %d evidence entries", f.FindingID, rdyMaxEvidence)
		}
		for _, e := range f.Evidence {
			if !rdySafePath(string(e.Path)) || e.LineStart < 1 || e.LineEnd < e.LineStart || !rdyHexRE.MatchString(string(e.FileSHA256)) {
				return rdyInvalid("finding %s cites malformed evidence", f.FindingID)
			}
		}
		needsEvidence := f.Status == readiness.StatusFound || f.Status == readiness.StatusPartial || f.Status == readiness.StatusContradicted
		switch {
		case needsEvidence && len(f.Evidence) == 0:
			return rdyInvalid("finding %s says %s and cites no evidence", f.FindingID, f.Status)
		case f.Source == readiness.SourceModelUnverified && f.Status != readiness.StatusUnverified:
			return rdyInvalid("finding %s is a model_unverified finding that is not unverified", f.FindingID)
		case f.Status == readiness.StatusUnverified && (f.UnverifiedReason == nil || *f.UnverifiedReason == ""):
			return rdyInvalid("finding %s is unverified and does not say why", f.FindingID)
		case f.Status == readiness.StatusNotApplicable:
			return rdyInvalid("finding %s says not_applicable, which only a checklist entry can", f.FindingID)
		}
		for _, t := range []string{rdyText(f.Summary), rdyText(f.UnverifiedReason), string(f.Remediation.En), rdyTextPtr(f.Remediation.It)} {
			if utf8.RuneCountInString(t) > rdyMaxText {
				return rdyInvalid("finding %s has a text over %d characters", f.FindingID, rdyMaxText)
			}
		}
	}
	var sum readiness.Summary
	var na int64
	seen := map[readiness.RequirementID]bool{}
	used := map[readiness.Category]bool{}
	for _, it := range d.Checklist {
		if it.Status == readiness.StatusFound && catalog.CatalogIsTransport(it.Category) {
			used[it.Category] = true
		}
	}
	for _, it := range d.Checklist {
		if !it.Status.Valid() || !it.Category.Valid() || !it.Level.Valid() || !rdyReqRE.MatchString(string(it.RequirementID)) || seen[it.RequirementID] {
			return rdyInvalid("checklist entry %s is malformed or repeated", it.RequirementID)
		}
		seen[it.RequirementID] = true
		covering := false
		contradicting := false
		for _, id := range it.FindingIDs {
			f, ok := byID[id]
			if !ok || f.RequirementID != it.RequirementID {
				return rdyInvalid("checklist entry %s points at a finding that is not its own", it.RequirementID)
			}
			if (f.Source == readiness.SourceDeterministic || f.Source == readiness.SourceModelVerified) &&
				(f.Status == readiness.StatusFound || f.Status == readiness.StatusPartial) {
				covering = true
			}
			if f.Status == readiness.StatusContradicted {
				contradicting = true
			}
		}
		// A requirement a rule did not mark covered can be raised only by a verified model
		// finding; either way a covered status needs a covering finding behind it.
		if (it.Status == readiness.StatusFound || it.Status == readiness.StatusPartial) && !covering {
			return rdyInvalid("checklist entry %s is covered but no verified finding backs it", it.RequirementID)
		}
		if it.Status == readiness.StatusContradicted && !contradicting {
			return rdyInvalid("checklist entry %s is contradicted but no finding says so", it.RequirementID)
		}
		// Not applicable is only the requirement of a transport the project does not use, while another is met.
		if it.Status == readiness.StatusNotApplicable && !catalog.CatalogOtherTransport(it.Category, used) {
			return rdyInvalid("checklist entry %s is not applicable but no other transport is met", it.RequirementID)
		}
		switch it.Status {
		case readiness.StatusFound:
			sum.Found++
		case readiness.StatusPartial:
			sum.Partial++
		case readiness.StatusMissing:
			sum.Missing++
		case readiness.StatusUnverified:
			sum.Unverified++
		case readiness.StatusContradicted:
			sum.Contradicted++
		case readiness.StatusNotApplicable:
			na++
		}
	}
	got := d.Summary
	got.NotApplicable = nil
	if sum != got || na != rdyNotApplicable(d.Summary) {
		return rdyInvalid("the summary does not match the checklist")
	}
	return nil
}

// rdyNotApplicable is the not-applicable count of a summary, which is absent when zero.
func rdyNotApplicable(s readiness.Summary) int64 {
	if s.NotApplicable == nil {
		return 0
	}
	return *s.NotApplicable
}

func rdyText(t *readiness.Text) string {
	if t == nil {
		return ""
	}
	return string(*t)
}

func rdyTextPtr(t *readiness.Text) string { return rdyText(t) }
