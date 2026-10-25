package files

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// chargeProbe is a Charger that books every charge as a row of its own table, written with the transaction it
// is handed. A charge that was made outside the data's transaction therefore survives the data's rollback, and
// a charge that was skipped is not there: both are visible to the tests below.
type chargeProbe struct {
	mu     sync.Mutex
	db     *store.DB
	limit  map[ChargeKind]int64 // 0 = no limit
	calls  int
	refund []ChargeKind
}

func (p *chargeProbe) ChargeTx(ctx context.Context, tx *store.Tx, _ contracts.Scope, kind ChargeKind, bytes int64) error {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	var used int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes), 0) FROM chargeprobe WHERE kind = ?`, string(kind)).Scan(&used); err != nil {
		return err
	}
	if lim := p.limit[kind]; lim > 0 && used+bytes > lim {
		return contracts.New(contracts.ErrResourceExhausted, "the tenant's quota is used up").
			WithDetails("dimension", string(kind)).WithDetails("limit", lim)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO chargeprobe (kind, bytes) VALUES (?, ?)`, string(kind), bytes)
	return err
}

func (p *chargeProbe) Refund(ctx context.Context, _ contracts.Scope, kind ChargeKind, bytes int64) error {
	p.mu.Lock()
	p.refund = append(p.refund, kind)
	p.mu.Unlock()
	_, err := p.db.ExecContext(ctx, `INSERT INTO chargeprobe (kind, bytes) VALUES (?, ?)`, string(kind), -bytes)
	return err
}

func (p *chargeProbe) charged(t *testing.T, kind ChargeKind) int64 {
	t.Helper()
	var n int64
	if err := p.db.QueryRowContext(context.Background(), `SELECT COALESCE(SUM(bytes), 0) FROM chargeprobe WHERE kind = ?`, string(kind)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func withCharger(c Charger) envOpt { return func(b *build) { b.opts.Charge = c } }

// chargeEnv builds the env with the probe. The probe needs the env's database, so the service is rebuilt around it.
func chargeEnv(t *testing.T, limits map[ChargeKind]int64, opts ...envOpt) (*env, *chargeProbe) {
	t.Helper()
	p := &chargeProbe{limit: limits}
	e := newEnv(t, append([]envOpt{withCharger(p)}, opts...)...)
	p.db = e.db
	if _, err := e.db.ExecContext(context.Background(), `CREATE TABLE chargeprobe (kind TEXT NOT NULL, bytes INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	return e, p
}

// M9-01: storage and ingestion bytes are charged in the very transaction that stores the file. A refusal stores
// nothing and charges nothing; a failure after the charge takes the charge with the row; a file the ingestion
// refuses afterwards gives its charge back. The probe's rows make a charge outside the transaction, or none at
// all, visible.
func TestStorageIsChargedInTheSameTransactionAndRolledBackOnRefusal(t *testing.T) {
	e, p := chargeEnv(t, map[ChargeKind]int64{ChargeStorage: 30, ChargeIngest: 12}, withRAG(), withConfig(ragOn))
	if !e.svc.ChargesInTransaction() {
		t.Fatal("a service built with a charger does not say it charges")
	}

	// Charged: the bytes of the file, to storage only for an ordinary attachment.
	e.mustPut(scopeA, "a.txt", "text/plain", textBytes("0123456789"))
	if got := p.charged(t, ChargeStorage); got != 10 {
		t.Fatalf("storage charged after a 10-byte upload = %d", got)
	}
	if got := p.charged(t, ChargeIngest); got != 0 {
		t.Fatalf("an ordinary attachment was charged to ingestion: %d", got)
	}

	// Refused by the storage quota: typed, nothing stored, nothing charged, no blob.
	rows, blobs := e.rowCount(), len(diskBlobs(t, e.dir))
	_, err := e.put(scopeA, "big.txt", "text/plain", textBytes("01234567890123456789012345"))
	ce := wantCode(t, err, contracts.ErrResourceExhausted)
	if ce.Details["dimension"] != string(ChargeStorage) {
		t.Errorf("refusal details = %v", ce.Details)
	}
	if e.rowCount() != rows || len(diskBlobs(t, e.dir)) != blobs || p.charged(t, ChargeStorage) != 10 {
		t.Errorf("a refused upload left rows %d->%d, blobs %d->%d, storage charged %d", rows, e.rowCount(), blobs, len(diskBlobs(t, e.dir)), p.charged(t, ChargeStorage))
	}

	// A failure AFTER the charge, in the same transaction, takes the charge with it. The trigger makes the
	// insert of the row fail once the charge is booked: a charge made in a transaction of its own would stay.
	ctx := context.Background()
	if _, err := e.db.ExecContext(ctx, `CREATE TRIGGER chargeprobe_boom BEFORE INSERT ON files BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.put(scopeA, "c.txt", "text/plain", textBytes("abcde")); err == nil {
		t.Fatal("an upload whose row could not be written was accepted")
	}
	if got := p.charged(t, ChargeStorage); got != 10 {
		t.Fatalf("the charge of a write that failed stayed: storage charged = %d, want 10", got)
	}
	if p.calls < 3 {
		t.Fatalf("the failing upload never reached the charge (%d calls): the test proves nothing", p.calls)
	}
	if _, err := e.db.ExecContext(ctx, `DROP TRIGGER chargeprobe_boom`); err != nil {
		t.Fatal(err)
	}

	// A rag_source file is charged to both volumes.
	e.mustPut(scopeA, "d.txt", "text/plain", textBytes("rules"), asTreatment(TreatmentRAGSource))
	if s, i := p.charged(t, ChargeStorage), p.charged(t, ChargeIngest); s != 15 || i != 5 {
		t.Fatalf("a 5-byte source charged storage %d (want 15), ingestion %d (want 5)", s, i)
	}

	// Refused by the ingestion quota (5 + 8 > 12): the storage charge made first in the same transaction goes too.
	_, err = e.put(scopeA, "e.txt", "text/plain", textBytes("12345678"), asTreatment(TreatmentRAGSource))
	ce = wantCode(t, err, contracts.ErrResourceExhausted)
	if ce.Details["dimension"] != string(ChargeIngest) {
		t.Errorf("refusal details = %v", ce.Details)
	}
	if s, i := p.charged(t, ChargeStorage), p.charged(t, ChargeIngest); s != 15 || i != 5 {
		t.Errorf("a refusal by the ingestion quota left storage %d (want 15), ingestion %d (want 5)", s, i)
	}

	// Registered, then refused by the ingestion: the file is withdrawn and both charges are given back.
	e.rag.failRegister = errors.New("parser queue full")
	_, err = e.put(scopeA, "f.txt", "text/plain", textBytes("xyz"), asTreatment(TreatmentRAGSource))
	wantCode(t, err, contracts.ErrUnavailable)
	if s, i := p.charged(t, ChargeStorage), p.charged(t, ChargeIngest); s != 15 || i != 5 {
		t.Errorf("a source the ingestion refused kept its charge: storage %d (want 15), ingestion %d (want 5)", s, i)
	}
	if len(p.refund) != 2 {
		t.Errorf("refunds = %v, want one per volume", p.refund)
	}
	if u, _ := e.svc.Usage(ctx, scopeA); u.Files != 2 || u.Bytes != 15 {
		t.Errorf("usage after the withdrawn source = %+v", u)
	}
}

// A service with no charger charges nothing and says so.
func TestStorageIsNotChargedWithoutACharger(t *testing.T) {
	e := newEnv(t)
	if e.svc.ChargesInTransaction() {
		t.Fatal("a service with no charger says it charges")
	}
	e.mustPut(scopeA, "a.txt", "text/plain", textBytes("hello"))
}
