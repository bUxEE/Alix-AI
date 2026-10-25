package ingest

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// chargeProbe books every charge as a row of its own table, written with the transaction it is handed: a charge
// made outside the data's transaction survives the data's rollback, a skipped one is not there.
type chargeProbe struct {
	mu    sync.Mutex
	limit int64 // 0 = none
	calls int
}

func (p *chargeProbe) ChargeIngestTx(ctx context.Context, tx *store.Tx, _ contracts.Scope, bytes int64) error {
	p.mu.Lock()
	p.calls++
	limit := p.limit
	p.mu.Unlock()
	var used int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes), 0) FROM chargeprobe`).Scan(&used); err != nil {
		return err
	}
	if limit > 0 && used+bytes > limit {
		return contracts.New(contracts.ErrResourceExhausted, "the tenant's ingestion quota is used up").
			WithDetails("dimension", "ingest_bytes_per_day").WithDetails("limit", limit)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO chargeprobe (bytes) VALUES (?)`, bytes)
	return err
}

func (p *chargeProbe) setLimit(n int64) {
	p.mu.Lock()
	p.limit = n
	p.mu.Unlock()
}

func (f *fixture) chargeProbe(limit int64) *chargeProbe {
	f.t.Helper()
	if _, err := f.db.ExecContext(bg, `CREATE TABLE chargeprobe (bytes INTEGER NOT NULL)`); err != nil {
		f.t.Fatal(err)
	}
	p := &chargeProbe{limit: limit}
	f.svc.charge = p
	return p
}

func (f *fixture) charged() int64 {
	f.t.Helper()
	var n int64
	if err := f.db.QueryRowContext(bg, `SELECT COALESCE(SUM(bytes), 0) FROM chargeprobe`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) sizeOf(rel string) int64 {
	f.t.Helper()
	st, err := os.Stat(filepath.Join(f.root, filepath.FromSlash(rel)))
	if err != nil {
		f.t.Fatal(err)
	}
	return st.Size()
}

// M9-01: a source sync charges the bytes of the documents that become new versions to the ingestion quota, in
// the transaction that writes those versions. An unchanged document costs nothing; a refusal writes nothing of
// the batch and ends the sync; a failure after the charge takes the charge with the rows.
func TestIngestBytesAreChargedInTheSameTransactionAndSourceSyncsAreCharged(t *testing.T) {
	f := newFixture(t)
	p := f.chargeProbe(0)
	corpus(f, 2)

	// The first sync charges what it reads: every document of the tree, once.
	mustEqual(t, "first sync", f.sync().State, jobs.StateSucceeded)
	want := f.sizeOf("doc0.md") + f.sizeOf("doc1.md")
	mustEqual(t, "bytes charged by the first sync", f.charged(), want)
	mustEqual(t, "versions", f.countRows(`SELECT count(*) FROM ragingest_version`), 2)

	// Nothing changed: nothing is charged, no charger is even asked.
	calls := p.calls
	mustEqual(t, "second sync", f.sync().State, jobs.StateSucceeded)
	mustEqual(t, "bytes charged by an unchanged sync", f.charged(), want)
	mustEqual(t, "charger calls by an unchanged sync", p.calls, calls)

	// One document changes: only its new bytes are charged.
	f.writeAt("doc1.md", doc("Title 1", para("p1a"), para("p1bNEW")), oldTime.Add(time.Hour))
	mustEqual(t, "third sync", f.sync().State, jobs.StateSucceeded)
	want += f.sizeOf("doc1.md")
	mustEqual(t, "bytes charged after a change", f.charged(), want)

	// Refused: the day's quota cannot take the new document. The sync ends (it would meet the same refusal
	// again), nothing of the refused batch is written, nothing is charged.
	f.writeAt("doc2.md", doc("Title 2", para("p2a"), para("p2b"), para("p2c")), oldTime)
	p.setLimit(want + f.sizeOf("doc2.md") - 1)
	items, versions := f.countRows(`SELECT count(*) FROM ragingest_item`), f.countRows(`SELECT count(*) FROM ragingest_version`)
	j := f.sync()
	mustEqual(t, "refused sync state", j.State, jobs.StateFailed)
	mustEqual(t, "refused sync code", j.ErrorCode, contracts.ErrResourceExhausted)
	mustEqual(t, "attempts of a refused sync (not retried)", j.Attempts, 1)
	mustEqual(t, "bytes charged by a refused sync", f.charged(), want)
	mustEqual(t, "snapshot rows after a refused sync", f.countRows(`SELECT count(*) FROM ragingest_item`), items)
	mustEqual(t, "versions after a refused sync", f.countRows(`SELECT count(*) FROM ragingest_version`), versions)

	// A failure AFTER the charge, in the same transaction, takes the charge with it. The trigger fails the
	// insert of the version once the charge is booked: a charge made in a transaction of its own would stay.
	p.setLimit(0)
	if _, err := f.db.ExecContext(bg, `CREATE TRIGGER chargeprobe_boom BEFORE INSERT ON ragingest_version BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	calls = p.calls
	mustEqual(t, "state of a sync whose write failed", f.sync().State, jobs.StateFailed)
	if p.calls == calls {
		t.Fatal("the failing sync never reached the charge: the test proves nothing")
	}
	mustEqual(t, "bytes charged by a write that failed", f.charged(), want)
	mustEqual(t, "versions after a write that failed", f.countRows(`SELECT count(*) FROM ragingest_version`), versions)

	// The trigger goes; the next sync charges the document, once.
	if _, err := f.db.ExecContext(bg, `DROP TRIGGER chargeprobe_boom`); err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "sync after the trigger is gone", f.sync().State, jobs.StateSucceeded)
	want += f.sizeOf("doc2.md")
	mustEqual(t, "bytes charged after the retry", f.charged(), want)
	mustEqual(t, "versions after the retry", f.countRows(`SELECT count(*) FROM ragingest_version WHERE state <> 'superseded'`), 3)
}

// A rebuild of the index under a new parser version reads the same documents again: that is not new ingestion
// and it is not charged a second time.
func TestIngestRebuildIsNotChargedAgain(t *testing.T) {
	f := newFixture(t)
	f.chargeProbe(0)
	corpus(f, 2)
	f.sync()
	first := f.charged()
	if first == 0 {
		t.Fatal("the first sync charged nothing")
	}
	f.parser.setVersion("2.0.0")
	f.sync()
	mustEqual(t, "bytes charged after a rebuild", f.charged(), first)
}

// With no charger bound nothing is charged, and a charger bound to the store is found by a service built before
// it was; unbinding puts it back.
func TestIngestChargerBindsToTheStore(t *testing.T) {
	f := newFixture(t)
	if f.svc.charger() != nil {
		t.Fatal("a service with no charger has one")
	}
	p := &chargeProbe{}
	unbind := BindCharger(f.db, p)
	if f.svc.charger() != Charger(p) {
		t.Fatal("the charger bound to the store is not found")
	}
	own := &chargeProbe{}
	f.svc.charge = own
	if f.svc.charger() != Charger(own) {
		t.Fatal("a service's own charger does not win over the bound one")
	}
	f.svc.charge = nil
	unbind()
	if f.svc.charger() != nil {
		t.Fatal("unbind left the charger bound")
	}
}
