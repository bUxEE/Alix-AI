package upgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Derived data (full-text indexes, embeddings) can always be rebuilt from its
// sources; receipts and the audit log cannot, so recovery never touches them
// (master plan 18 "SQLite/index corruption", 19; SEC-074). A damaged derived
// index is handled in two steps that a crash can fall between:
//
//  1. Isolate: the index is recorded in the quarantine directory and its
//     contents are dropped, so nothing can be served from damaged bytes: a search
//     finds nothing (a clear, local degradation) instead of wrong rows.
//  2. Rebuild: the index is rebuilt from its source, checked, and released.
//
// A quarantine record that is still there at the next start means step 2 did not
// finish; Recover does it. If the damage is structural (the rebuild itself hits
// it) the error says so and the quarantine stays: the other branch of the plan
// is to restore the last backup, which the staged upgrade does by itself.

// DerivedIndex is a rebuildable index inside the database.
type DerivedIndex interface {
	// Name identifies the index in the quarantine and in reports.
	Name() string
	// Check returns nil when the index is consistent with its source, an error
	// wrapping ErrDamaged when it is not, and any other error when the check
	// could not be made (busy, cancelled): which is not evidence of damage.
	Check(ctx context.Context) error
	// Isolate drops the index contents without reading them.
	Isolate(ctx context.Context) error
	// Rebuild recreates the index from its source.
	Rebuild(ctx context.Context) error
}

// ErrDamaged marks a Check failure that says the index is damaged.
var ErrDamaged = errors.New("derived index is damaged")

type ftsIndex struct {
	db   *store.DB
	name string
}

func (f ftsIndex) quoted() string { return `"` + strings.ReplaceAll(f.name, `"`, `""`) + `"` }
func (f ftsIndex) Name() string   { return f.name }

func (f ftsIndex) command(ctx context.Context, cmd string) error {
	q := f.quoted()
	_, err := f.db.ExecContext(ctx, `INSERT INTO `+q+`(`+q+`, rank) VALUES('`+cmd+`', 1)`)
	return err
}

// Check runs FTS5's integrity-check, which for an external-content table also
// compares the index with the content table. (A long-lived connection that has
// seen an FTS5 optimize can report false findings; the check runs on the
// writer, and a finding is only ever answered with a rebuild, which is safe.)
func (f ftsIndex) Check(ctx context.Context) error {
	err := f.command(ctx, "integrity-check")
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil || store.IsBusy(err):
		return err
	}
	return fmt.Errorf("%w: %s: %v", ErrDamaged, f.name, err)
}

func (f ftsIndex) Isolate(ctx context.Context) error {
	q := f.quoted()
	_, err := f.db.ExecContext(ctx, `INSERT INTO `+q+`(`+q+`) VALUES('delete-all')`)
	return err
}

func (f ftsIndex) Rebuild(ctx context.Context) error {
	q := f.quoted()
	_, err := f.db.ExecContext(ctx, `INSERT INTO `+q+`(`+q+`) VALUES('rebuild')`)
	return err
}

var contentlessRE = regexp.MustCompile(`(?i)content\s*=\s*''`)

// FTSIndexes discovers the FTS5 tables of the database that have a source to
// rebuild from: external-content tables and ordinary ones. Contentless tables
// have nothing to rebuild from and are not listed.
func FTSIndexes(ctx context.Context, db *store.DB) ([]DerivedIndex, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT name, sql FROM sqlite_master WHERE type = 'table' AND sql LIKE 'CREATE VIRTUAL TABLE%' AND lower(sql) LIKE '%using fts5%' ORDER BY name`)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "the full-text indexes cannot be listed")
	}
	defer rows.Close()
	var out []DerivedIndex
	for rows.Next() {
		var name, sql string
		if err := rows.Scan(&name, &sql); err != nil {
			return nil, contracts.Wrap(contracts.ErrUnavailable, err, "the full-text indexes cannot be listed")
		}
		if contentlessRE.MatchString(sql) {
			continue
		}
		out = append(out, ftsIndex{db: db, name: name})
	}
	if err := rows.Err(); err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "the full-text indexes cannot be listed")
	}
	return out, nil
}

// Quarantine is the set of derived indexes that were isolated and not yet
// rebuilt, kept as one small file each so it survives a crash and a restart.
// Code that serves from an index consults Has before it does.
type Quarantine struct {
	dir string
	clk clock.Clock
}

// NewQuarantine returns the quarantine kept in <dataDir>/upgrade/quarantine.
func NewQuarantine(dataDir string, clk clock.Clock) *Quarantine {
	if clk == nil {
		clk = clock.Real{}
	}
	return &Quarantine{dir: filepath.Join(dataDir, dirName, quarantineDir), clk: clk}
}

// QuarantineEntry is one isolated index.
type QuarantineEntry struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
	Since  string `json:"since"`
}

var quarantineNameRE = regexp.MustCompile(`^[A-Za-z0-9_]{1,128}$`)

func (q *Quarantine) path(name string) (string, error) {
	if !quarantineNameRE.MatchString(name) {
		return "", contracts.Newf(contracts.ErrValidationFailed, "index name %q cannot be quarantined", name)
	}
	return filepath.Join(q.dir, name+".json"), nil
}

// Add records name as isolated.
func (q *Quarantine) Add(name, reason string) error {
	if len(reason) > 512 {
		reason = reason[:512]
	}
	return q.write(QuarantineEntry{Name: name, Reason: reason, Since: q.clk.Now().UTC().Format(timeLayout)})
}

// write records en, atomically.
func (q *Quarantine) write(en QuarantineEntry) error {
	p, err := q.path(en.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(q.dir, 0o700); err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "quarantine directory cannot be created")
	}
	raw, _ := json.MarshalIndent(en, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "quarantine record cannot be written")
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return contracts.Wrap(contracts.ErrUnavailable, err, "quarantine record cannot be written")
	}
	return nil
}

// Has reports whether name is isolated. It fails closed: a record that cannot be
// looked up (the directory is unreadable, the disk errs) is reported as present,
// because an index that cannot be shown to be healthy must not be served from
// without saying so. Only a record that is absent says the index is not isolated.
// A name that cannot be recorded at all is never isolated.
func (q *Quarantine) Has(name string) bool {
	if q == nil {
		return false // no quarantine configured: nothing is isolated
	}
	p, err := q.path(name)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}

// Clear releases name.
func (q *Quarantine) Clear(name string) error {
	p, err := q.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return contracts.Wrap(contracts.ErrUnavailable, err, "quarantine record cannot be removed")
	}
	return nil
}

// reset makes the quarantine hold exactly entries, each as it was recorded: what
// is not among them is released, the others are recorded again.
func (q *Quarantine) reset(entries []QuarantineEntry) error {
	cur, err := q.List()
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, en := range entries {
		want[en.Name] = true
	}
	for _, en := range cur {
		if !want[en.Name] {
			if err := q.Clear(en.Name); err != nil {
				return err
			}
		}
	}
	for _, en := range entries {
		if err := q.write(en); err != nil {
			return err
		}
	}
	return nil
}

// List returns the isolated indexes, by name.
func (q *Quarantine) List() ([]QuarantineEntry, error) {
	entries, err := os.ReadDir(q.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "quarantine cannot be listed")
	}
	var out []QuarantineEntry
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(q.dir, e.Name()))
		if err != nil {
			return nil, contracts.Wrap(contracts.ErrUnavailable, err, "quarantine record cannot be read")
		}
		var en QuarantineEntry
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&en); err != nil || !quarantineNameRE.MatchString(en.Name) {
			return nil, contracts.Newf(contracts.ErrInternal, "quarantine record %s is malformed", e.Name())
		}
		out = append(out, en)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DerivedReport says what a repair found and did.
type DerivedReport struct {
	// Damaged are the indexes that were found damaged and isolated.
	Damaged []string
	// Rebuilt are the indexes that were rebuilt from their sources and released.
	Rebuilt []string
	// Pending are the isolated indexes that stay in the quarantine because they
	// can only be rebuilt by something else (a vector index is rebuilt by
	// embedding its chunks again, ErrReembedRequired). Retrieval keeps labelling
	// its results reduced recall until they are released.
	Pending []string
}

func (r *DerivedReport) merge(o DerivedReport) {
	r.Damaged = append(r.Damaged, o.Damaged...)
	r.Rebuilt = append(r.Rebuilt, o.Rebuilt...)
	r.Pending = append(r.Pending, o.Pending...)
}

// IsolateDamaged checks every index and isolates the damaged ones: recorded in
// the quarantine and emptied. An index whose check could not be made is an
// error, not damage.
func IsolateDamaged(ctx context.Context, q *Quarantine, idx []DerivedIndex) ([]string, error) {
	var damaged []string
	for _, ix := range idx {
		err := ix.Check(ctx)
		switch {
		case err == nil:
			continue
		case !errors.Is(err, ErrDamaged):
			return damaged, err
		}
		if err := q.Add(ix.Name(), err.Error()); err != nil {
			return damaged, err
		}
		if err := ix.Isolate(ctx); err != nil {
			return damaged, structuralError(ix.Name(), err)
		}
		damaged = append(damaged, ix.Name())
	}
	return damaged, nil
}

// RebuildQuarantined rebuilds every isolated index from its source, checks it
// and releases it. A record for an index that no longer exists (a migration
// dropped it) is cleared. If an index cannot be rebuilt or does not pass its
// check the error says the damage is structural and the record stays. An index
// that reports ErrReembedRequired is not rebuilt here and is not an error: its
// record stays (RepairDerived lists it in DerivedReport.Pending).
func RebuildQuarantined(ctx context.Context, q *Quarantine, idx []DerivedIndex) ([]string, error) {
	rebuilt, _, err := repairQuarantined(ctx, q, idx)
	return rebuilt, err
}

func repairQuarantined(ctx context.Context, q *Quarantine, idx []DerivedIndex) (rebuilt, pending []string, err error) {
	entries, err := q.List()
	if err != nil {
		return nil, nil, err
	}
	byName := map[string]DerivedIndex{}
	for _, ix := range idx {
		byName[ix.Name()] = ix
	}
	for _, en := range entries {
		ix, ok := byName[en.Name]
		if !ok {
			if err := q.Clear(en.Name); err != nil {
				return rebuilt, pending, err
			}
			continue
		}
		if err := ix.Rebuild(ctx); err != nil {
			if errors.Is(err, ErrReembedRequired) {
				pending = append(pending, en.Name)
				continue
			}
			return rebuilt, pending, structuralError(en.Name, err)
		}
		if err := ix.Check(ctx); err != nil {
			return rebuilt, pending, structuralError(en.Name, err)
		}
		if err := q.Clear(en.Name); err != nil {
			return rebuilt, pending, err
		}
		rebuilt = append(rebuilt, en.Name)
	}
	return rebuilt, pending, nil
}

// RepairDerived isolates what is damaged and rebuilds everything isolated.
func RepairDerived(ctx context.Context, q *Quarantine, idx []DerivedIndex) (DerivedReport, error) {
	var rep DerivedReport
	var err error
	if rep.Damaged, err = IsolateDamaged(ctx, q, idx); err != nil {
		return rep, err
	}
	rep.Rebuilt, rep.Pending, err = repairQuarantined(ctx, q, idx)
	return rep, err
}

func structuralError(name string, cause error) error {
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return contracts.Wrap(contracts.ErrTimeout, cause, "derived index "+name+": repair interrupted")
	}
	return contracts.Wrap(contracts.ErrInternal, cause,
		"derived index "+name+" cannot be rebuilt in place; the damage is structural, restore the last backup").
		WithDetails("index", name)
}
