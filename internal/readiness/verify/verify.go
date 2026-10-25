package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/readiness/catalog"
	"github.com/buxee/alix-ai/internal/readiness/scan"
	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

// Bounds. They are the schema's own (32 evidence entries) and the scanner's (file size).
const (
	rdyMaxEvidence      = 32
	rdyMaxProposals     = 500
	rdyMaxQuoteBytes    = 500
	rdyMinQuoteRunes    = 6
	rdyMaxSummaryRunes  = 300
	rdyMaxRemedyRunes   = 600
	rdyMaxPathBytes     = 512
	rdyDefaultFileBytes = 2 << 20
)

// RdyProposal is one finding a model proposed, with the text it quotes for each evidence entry
// (Quotes[i] belongs to Finding.Evidence[i]). The model is untrusted: every field is checked.
type RdyProposal struct {
	Finding readiness.Finding
	Quotes  []string
}

// RdyResult is the outcome of one verification run: every finding with its verified status, rule
// findings first in the order given, then the model proposals that became findings.
type RdyResult struct {
	Verified []RdyVerified
	Dropped  []RdyDropped
}

type rdyFile interface {
	io.Reader
	io.Closer
}

// rdyFS is the only way the verifier touches the disk. The real one is confined to the scanned
// root with os.Root; tests wrap it to record every open.
type rdyFS interface {
	Lstat(name string) (fs.FileInfo, error)
	Open(name string) (rdyFile, error)
	Close() error
}

type rdyRootFS struct{ root *os.Root }

func (r rdyRootFS) Lstat(name string) (fs.FileInfo, error) {
	return r.root.Lstat(filepath.FromSlash(name))
}

func (r rdyRootFS) Open(name string) (rdyFile, error) { return r.root.Open(filepath.FromSlash(name)) }

func (r rdyRootFS) Close() error { return r.root.Close() }

// RdyVerifier re-reads the evidence of one scan. It holds the scan's own inventory: a finding may
// cite only a file the scan recorded and read, by its exact recorded path.
type RdyVerifier struct {
	fsys     rdyFS
	cat      *catalog.Catalog
	files    map[string]scan.ScanFile
	maxBytes int64
	cache    map[string]*rdyLoaded

	// The rules below are the verifier's own; they are fields only so a test can remove one and
	// prove the tests notice (the house mutation pattern). Production code never replaces them.
	hashMatches func(want, got string) bool
	refuseKind  func(kind string) bool
	pathOK      func(p string) bool
	quoteIn     func(span []string, quote string) bool
}

type rdyLoaded struct {
	reason RdyReason
	sha    string
	lines  []string // text files only
	count  int
	text   bool
}

// RdyNew opens the scanned root for verification. It applies the scanner's own refusals (a drive
// root, a system directory) and confines every later read to the root.
func RdyNew(root string, res *scan.ScanResult, cat *catalog.Catalog) (*RdyVerifier, error) {
	if res == nil {
		return nil, errors.New("readiness verify: no scan result")
	}
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("readiness verify: no directory named")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, errors.New("readiness verify: cannot resolve the directory")
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, errors.New("readiness verify: cannot resolve the directory")
	}
	if why := scan.ScanRootRefusal(resolved); why != "" {
		return nil, fmt.Errorf("readiness verify: refused: %s", why)
	}
	r, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, errors.New("readiness verify: cannot open the directory")
	}
	v, err := rdyNewWithFS(rdyRootFS{root: r}, res, cat)
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	return v, nil
}

func rdyNewWithFS(fsys rdyFS, res *scan.ScanResult, cat *catalog.Catalog) (*RdyVerifier, error) {
	if cat == nil {
		var err error
		if cat, err = catalog.CatalogLoad(); err != nil {
			return nil, fmt.Errorf("readiness verify: %w", err)
		}
	}
	if res == nil {
		return nil, errors.New("readiness verify: no scan result")
	}
	v := &RdyVerifier{
		fsys: fsys, cat: cat, files: make(map[string]scan.ScanFile, len(res.Files)),
		maxBytes: res.Limits.MaxFileBytes, cache: map[string]*rdyLoaded{},
		hashMatches: rdyHashMatches, refuseKind: rdyRefuseKind, pathOK: rdyPathOK, quoteIn: rdyQuoteIn,
	}
	if v.maxBytes <= 0 {
		v.maxBytes = rdyDefaultFileBytes
	}
	secret := map[string]bool{}
	for _, p := range res.SecretFiles {
		secret[p] = true
	}
	for _, f := range res.Files {
		if secret[f.Path] {
			f.Kind = rdyKindSecret
		}
		v.files[f.Path] = f
	}
	return v, nil
}

// File kinds the scanner records (its constants are unexported).
const (
	rdyKindSecret = "secret"
	rdyKindText   = "text"
	rdyKindDoc    = "document"
)

// Close releases the root.
func (v *RdyVerifier) Close() error { return v.fsys.Close() }

func rdyHashMatches(want, got string) bool { return want != "" && want == got }

// rdyRefuseKind says which recorded file kinds are never read: secret-looking files.
func rdyRefuseKind(kind string) bool { return kind == rdyKindSecret }

// rdyPathOK is the schema's relative-path rule plus the refusals a hostile name needs: no leading
// slash, no backslash, no colon (drive letters and streams), no control character, no empty, "."
// or ".." segment.
func rdyPathOK(p string) bool {
	if p == "" || len(p) > rdyMaxPathBytes || !utf8.ValidString(p) || p[0] == '/' {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == '\\' || r == ':' || r == utf8.RuneError {
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

// rdyQuoteIn reports whether the quote, redacted and with whitespace collapsed, is inside the
// redacted, collapsed text of the span.
func rdyQuoteIn(span []string, quote string) bool {
	q := rdyNormalize(quote)
	if utf8.RuneCountInString(q) < rdyMinQuoteRunes {
		return false
	}
	return strings.Contains(rdyNormalize(strings.Join(span, "\n")), q)
}

func rdyNormalize(s string) string {
	return strings.Join(strings.Fields(scan.ScanRedact(s)), " ")
}

// load reads one recorded file once per run: bounded, hashed from the same buffer it is counted
// from. A path that fails a check is cached with its reason, and nothing is opened for it twice.
func (v *RdyVerifier) load(path string) *rdyLoaded {
	if l, ok := v.cache[path]; ok {
		return l
	}
	l := v.read(path)
	v.cache[path] = l
	return l
}

func (v *RdyVerifier) read(path string) *rdyLoaded {
	if !v.pathOK(path) {
		return &rdyLoaded{reason: RdyReasonOutsideRoot}
	}
	rec, ok := v.files[path]
	if !ok {
		return &rdyLoaded{reason: RdyReasonNotScanned}
	}
	if v.refuseKind(rec.Kind) {
		return &rdyLoaded{reason: RdyReasonSecretFile}
	}
	if rec.SHA256 == "" || (rec.Kind != rdyKindText && rec.Kind != rdyKindDoc) {
		return &rdyLoaded{reason: RdyReasonNotScanned}
	}
	segs := strings.Split(path, "/")
	for i := range segs {
		info, err := v.fsys.Lstat(strings.Join(segs[:i+1], "/"))
		if err != nil {
			return &rdyLoaded{reason: RdyReasonUnreadable}
		}
		if info.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			return &rdyLoaded{reason: RdyReasonLink}
		}
		if i == len(segs)-1 && !info.Mode().IsRegular() {
			return &rdyLoaded{reason: RdyReasonUnreadable}
		}
		if i < len(segs)-1 && !info.IsDir() {
			return &rdyLoaded{reason: RdyReasonUnreadable}
		}
	}
	f, err := v.fsys.Open(path)
	if err != nil {
		return &rdyLoaded{reason: RdyReasonUnreadable}
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, v.maxBytes+1))
	if err != nil {
		return &rdyLoaded{reason: RdyReasonUnreadable}
	}
	if int64(len(buf)) > v.maxBytes {
		return &rdyLoaded{reason: RdyReasonTooLarge}
	}
	sum := sha256.Sum256(buf)
	l := &rdyLoaded{sha: hex.EncodeToString(sum[:])}
	switch rec.Kind {
	case rdyKindText:
		head := buf
		if len(head) > 4096 {
			head = head[:4096]
		}
		binary := false
		for _, b := range head {
			if b == 0 {
				binary = true
				break
			}
		}
		if !binary {
			l.text = true
			l.count = scan.ScanLineCount(buf)
			l.lines = rdySplit(buf)
		}
	default:
		if len(buf) > 0 {
			l.count = 1
		}
	}
	return l
}

// rdySplit splits with the scanner's rule: \n separates, a CRLF is one separator, a final newline
// starts no line, and a line over the scanner's cap reads as empty.
func rdySplit(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	parts := strings.Split(string(content), "\n")
	if content[len(content)-1] == '\n' {
		parts = parts[:len(parts)-1]
	}
	for i, p := range parts {
		p = strings.TrimSuffix(p, "\r")
		if len(p) > 2000 {
			p = ""
		}
		parts[i] = p
	}
	return parts
}

// check confirms one evidence entry, or returns why not. The quote is checked only when non-nil.
func (v *RdyVerifier) check(e readiness.FindingEvidence, quote *string) RdyReason {
	path := string(e.Path)
	l := v.load(path)
	if l.reason != "" {
		return l.reason
	}
	rec := v.files[path]
	// The entry must carry the hash the scan recorded, and the file must still have it.
	if !v.hashMatches(string(e.FileSHA256), rec.SHA256) || !v.hashMatches(string(e.FileSHA256), l.sha) {
		return RdyReasonChanged
	}
	if e.LineStart < 1 || e.LineEnd < e.LineStart || e.LineEnd > int64(l.count) {
		return RdyReasonSpan
	}
	if quote != nil {
		if !l.text || strings.TrimSpace(*quote) == "" || len(*quote) > rdyMaxQuoteBytes {
			return RdyReasonQuoteMissing
		}
		if !v.quoteIn(l.lines[e.LineStart-1:e.LineEnd], *quote) {
			return RdyReasonQuoteMismatch
		}
	}
	return ""
}

// evidenceReasons checks every entry and returns the distinct reasons, sorted.
func (v *RdyVerifier) evidenceReasons(ev []readiness.FindingEvidence, quotes []string, needQuote bool) []RdyReason {
	if len(ev) > rdyMaxEvidence {
		return []RdyReason{RdyReasonTooMuchEvidence}
	}
	set := map[RdyReason]bool{}
	for i, e := range ev {
		var q *string
		if needQuote {
			s := ""
			if i < len(quotes) {
				s = quotes[i]
			}
			q = &s
		}
		if r := v.check(e, q); r != "" {
			set[r] = true
		}
	}
	out := make([]RdyReason, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func rdyNeedsEvidence(s readiness.Status) bool {
	return s == readiness.StatusFound || s == readiness.StatusPartial || s == readiness.StatusContradicted
}

// Verify decides every finding. rule holds the findings the deterministic scanner produced;
// proposals holds what a model suggested. A finding stands as found, partial or contradicted only
// when it cites evidence and every entry confirms; otherwise it is unverified with the reason.
// A model proposal that confirms becomes model_verified, one that does not becomes
// model_unverified, and a model can never claim missing, unverified or contradicted. A rule
// finding for a requirement the catalog does not have is an error: it is corrupt input, not a
// proposal.
func (v *RdyVerifier) Verify(rule []readiness.Finding, proposals []RdyProposal) (*RdyResult, error) {
	res := &RdyResult{Verified: make([]RdyVerified, 0, len(rule)+len(proposals))}
	seen := map[readiness.FindingID]bool{}
	for _, f := range rule {
		rv, err := v.verifyRule(f)
		if err != nil {
			return nil, err
		}
		res.Verified = append(res.Verified, rv)
		seen[rv.finding.FindingID] = true
	}
	for i, p := range proposals {
		if i >= rdyMaxProposals {
			res.Dropped = append(res.Dropped, RdyDropped{Index: i, Reason: RdyReasonTooManyProposals})
			continue
		}
		mv, dropped := v.verifyProposal(p)
		if dropped != "" {
			res.Dropped = append(res.Dropped, RdyDropped{Index: i, Reason: dropped})
			continue
		}
		if seen[mv.finding.FindingID] {
			res.Dropped = append(res.Dropped, RdyDropped{Index: i, Reason: RdyReasonDuplicate})
			continue
		}
		seen[mv.finding.FindingID] = true
		res.Verified = append(res.Verified, mv)
	}
	return res, nil
}

func (v *RdyVerifier) verifyRule(f readiness.Finding) (RdyVerified, error) {
	req, ok := v.cat.Get(f.RequirementID)
	if !ok {
		return RdyVerified{}, errors.New("readiness verify: a rule finding names a requirement the catalog does not have")
	}
	claimed := f.Status
	f.Category, f.CheckID, f.Source = req.Category, req.CheckID, readiness.SourceDeterministic
	out := RdyVerified{claimed: claimed, origin: rdyOriginRule, confirmed: true}
	switch {
	case rdyNeedsEvidence(claimed):
		reasons := v.evidenceReasons(f.Evidence, nil, false)
		if len(f.Evidence) == 0 {
			reasons = []RdyReason{RdyReasonNoEvidence}
		}
		if len(reasons) > 0 {
			f = rdyDemote(f, reasons)
			out.confirmed, out.reasons = false, reasons
		}
	case claimed == readiness.StatusMissing:
		// Nothing to confirm: a rule that found nothing cites nothing.
		f.Evidence = nil
	case claimed == readiness.StatusUnverified:
		// Stays unverified. The evidence it cites, if any, is still re-read so a report never
		// carries a citation that does not hold.
		if reasons := v.evidenceReasons(f.Evidence, nil, false); len(reasons) > 0 {
			f.Evidence = nil
			out.reasons = reasons
		}
		if f.UnverifiedReason == nil {
			t := readiness.Text("The status could not be determined.")
			f.UnverifiedReason = &t
		}
	default:
		return RdyVerified{}, fmt.Errorf("readiness verify: a rule finding has the status %q", claimed)
	}
	out.finding = f
	return out, nil
}

// rdyDemote turns a finding into an unverified one that says why.
func rdyDemote(f readiness.Finding, reasons []RdyReason) readiness.Finding {
	f.Status = readiness.StatusUnverified
	parts := make([]string, 0, len(reasons))
	for _, r := range reasons {
		parts = append(parts, r.Text())
	}
	t := readiness.Text(strings.Join(parts, " "))
	f.UnverifiedReason = &t
	return f
}

func (v *RdyVerifier) verifyProposal(p RdyProposal) (RdyVerified, RdyReason) {
	f := p.Finding
	req, ok := v.cat.Get(f.RequirementID)
	if !ok {
		return RdyVerified{}, RdyReasonUnknownReq
	}
	claimed := f.Status
	out := RdyVerified{claimed: claimed, origin: rdyOriginModel}

	var reasons []RdyReason
	switch {
	case claimed != readiness.StatusFound && claimed != readiness.StatusPartial:
		reasons = []RdyReason{RdyReasonClaimNotAllowed}
	case len(f.Evidence) == 0:
		reasons = []RdyReason{RdyReasonNoEvidence}
	default:
		reasons = v.evidenceReasons(f.Evidence, p.Quotes, true)
	}

	nf := readiness.Finding{
		SchemaVersion: 1,
		RequirementID: req.ID,
		Category:      req.Category,
		CheckID:       req.CheckID,
		Remediation:   rdyRemediation(f.Remediation, req),
	}
	if f.Summary != nil {
		if s := rdyClean(string(*f.Summary), rdyMaxSummaryRunes); s != "" {
			t := readiness.Text(s)
			nf.Summary = &t
		}
	}
	if len(reasons) == 0 {
		nf.Source, nf.Status = readiness.SourceModelVerified, claimed
		nf.Evidence = append([]readiness.FindingEvidence(nil), f.Evidence...)
		out.confirmed = true
	} else {
		// The proposal's own citations are not echoed: they did not hold, and a hostile path
		// must not reach a report.
		nf = rdyDemote(nf, reasons)
		nf.Source = readiness.SourceModelUnverified
		out.confirmed, out.reasons = false, reasons
	}
	nf.FindingID = rdyFindingID(req.ID, rdyEvidenceKey(f.Evidence))
	out.finding = nf
	return out, ""
}

func rdyRemediation(r readiness.RemediationText, req readiness.Requirement) readiness.RemediationText {
	en := rdyClean(string(r.En), rdyMaxRemedyRunes)
	if en == "" {
		it := req.Remediation.It
		return readiness.RemediationText{En: req.Remediation.En, It: &it}
	}
	out := readiness.RemediationText{En: readiness.Text(en)}
	if r.It != nil {
		if it := rdyClean(string(*r.It), rdyMaxRemedyRunes); it != "" {
			t := readiness.Text(it)
			out.It = &t
		}
	}
	return out
}

// rdyClean redacts secret-looking values, drops control characters and bounds the length of the
// free text a model supplied.
func rdyClean(s string, maxRunes int) string {
	s = scan.ScanRedact(s)
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			r = ' '
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if rs := []rune(out); len(rs) > maxRunes {
		out = string(rs[:maxRunes])
	}
	return out
}

func rdyEvidenceKey(ev []readiness.FindingEvidence) string {
	var b strings.Builder
	for _, e := range ev {
		fmt.Fprintf(&b, "%s:%d:%d:%s\n", e.Path, e.LineStart, e.LineEnd, e.FileSHA256)
	}
	return b.String()
}

// rdyFindingID has the scanner's shape (fnd_ and 16 hex digits). The model tag keeps a model
// finding's id apart from a rule finding that cites the same lines.
func rdyFindingID(req readiness.RequirementID, evidenceKey string) readiness.FindingID {
	sum := sha256.Sum256([]byte(string(req) + "\n" + evidenceKey + "model\n"))
	return readiness.FindingID("fnd_" + hex.EncodeToString(sum[:])[:16])
}

// ConfirmedExcerpts returns the scanner excerpts that may be shown to a later stage (the model
// leg). An excerpt survives only when the finding it names was confirmed and the excerpt's path
// and line span are exactly one of that finding's evidence entries; the text is redacted and
// bounded again. Scanner excerpts are sometimes a label for the signal rather than the line's
// own text, so this checks where the excerpt points, not what it says: a model proposal's quote
// is the one whose text is checked against the file.
func (v *RdyVerifier) ConfirmedExcerpts(verified []RdyVerified, excerpts []scan.ScanExcerpt) []scan.ScanExcerpt {
	type key struct {
		id, path string
		a, b     int64
	}
	ok := map[key]bool{}
	for _, vf := range verified {
		if !vf.confirmed || vf.origin != rdyOriginRule || !rdyNeedsEvidence(vf.finding.Status) {
			continue
		}
		for _, e := range vf.finding.Evidence {
			ok[key{string(vf.finding.FindingID), string(e.Path), e.LineStart, e.LineEnd}] = true
		}
	}
	out := make([]scan.ScanExcerpt, 0, len(excerpts))
	for _, x := range excerpts {
		if !ok[key{x.FindingID, x.Path, int64(x.LineStart), int64(x.LineEnd)}] {
			continue
		}
		x.Text = rdyClean(x.Text, 200)
		out = append(out, x)
	}
	return out
}
