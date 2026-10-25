package auditsec

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

var bg = context.Background()

var clockStart = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

const checkpointKeyID = "audit-checkpoint-sec"

// world is an audit log as `alixai serve` builds it when it is told to sign checkpoints: a real
// store, the platform checkpoint key registered in the trust store under the platform
// application, a verifier over that trust store, and a log that signs with the key.
type world struct {
	t     *testing.T
	db    *store.DB
	clk   *clock.Fake
	trust *trust.Store
	log   *audit.Log
	pub   ed25519.PublicKey
	priv  ed25519.PrivateKey
}

func newWorld(t *testing.T, mutate ...func(*audit.Options)) *world {
	t.Helper()
	w := &world{t: t, clk: clock.NewFake(clockStart)}
	w.db = storetest.Open(t, nil)
	w.trust = trust.NewStore(w.db, w.clk)
	var err error
	if w.pub, w.priv, err = ed25519.GenerateKey(rand.Reader); err != nil {
		t.Fatal(err)
	}
	if _, err := w.trust.RegisterKey(bg, trust.PlatformApplicationID, checkpointKeyID, w.pub, "audit checkpoint signer", trust.WithPurpose(trust.PurposeAuditCheckpoint)); err != nil {
		t.Fatal(err)
	}
	opts := audit.Options{
		Clock:    w.clk,
		Signing:  &trust.SigningKey{KeyID: checkpointKeyID, Key: w.priv},
		Verifier: trust.NewVerifier(w.trust, w.clk),
	}
	for _, m := range mutate {
		m(&opts)
	}
	if w.log, err = audit.New(w.db, opts); err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	return w
}

// scopeFor is a valid scope of one tenant.
func scopeFor(tenant string) contracts.Scope {
	return storetest.Scope("app-sec", tenant, "ws-1")
}

func chainOf(tenant string) audit.ChainKey {
	return audit.ChainKey{ApplicationID: "app-sec", TenantID: tenant}
}

// appendN appends n numbered events of a tenant through the log's own Append.
func (w *world) appendN(tenant string, from, n int) {
	w.t.Helper()
	for i := from; i < from+n; i++ {
		w.clk.Advance(time.Second)
		if _, err := w.log.Append(bg, scopeFor(tenant), audit.NewEvent{
			Type: audit.EventSettingsChanged, Data: map[string]any{"n": i},
		}); err != nil {
			w.t.Fatalf("Append #%d: %v", i, err)
		}
	}
}

func (w *world) exec(query string, args ...any) {
	w.t.Helper()
	if _, err := w.db.ExecContext(bg, query, args...); err != nil {
		w.t.Fatalf("%s: %v", query, err)
	}
}

func (w *world) count(query string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.db.QueryRowContext(bg, query, args...).Scan(&n); err != nil {
		w.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// dropAppendOnlyTriggers is the first thing someone with write access to the database file
// does. The tests that tamper call it so that they test chain verification, not the triggers
// (the triggers are checked on their own, in TestSecAuditTailTruncationDetected).
func (w *world) dropAppendOnlyTriggers() {
	w.t.Helper()
	rows, err := w.db.QueryContext(bg, `SELECT name FROM sqlite_master WHERE type = 'trigger' AND tbl_name LIKE 'audit\_%' ESCAPE '\'`)
	if err != nil {
		w.t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			w.t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		w.t.Fatal(err)
	}
	_ = rows.Close()
	if len(names) == 0 {
		w.t.Fatal("the audit tables have no triggers: the setup of the tamper tests is stale")
	}
	for _, n := range names {
		w.exec(`DROP TRIGGER "` + n + `"`)
	}
}

// rewriteChain does what an attacker does who wants a changed history to look intact: it reads
// every event of the chain, lets mutate edit them, recomputes every prev_hash and hash from
// position 1 with the production hash function and writes them back.
func (w *world) rewriteChain(key audit.ChainKey, mutate func(events []audit.Event) []audit.Event) {
	w.t.Helper()
	rows, err := w.db.QueryContext(bg,
		`SELECT seq, id, workspace_id, chain_seq, event_type, occurred_at, principal_id, session_id, data
		   FROM audit_events WHERE application_id = ? AND tenant_id = ? ORDER BY chain_seq`, key.ApplicationID, key.TenantID)
	if err != nil {
		w.t.Fatal(err)
	}
	var (
		events []audit.Event
		phys   []int64
	)
	for rows.Next() {
		var (
			e         audit.Event
			p, ms     int64
			typ, data string
		)
		if err := rows.Scan(&p, &e.ID, &e.WorkspaceID, &e.ChainSeq, &typ, &ms, &e.PrincipalID, &e.SessionID, &data); err != nil {
			w.t.Fatal(err)
		}
		e.ApplicationID, e.TenantID = key.ApplicationID, key.TenantID
		e.Type, e.OccurredAt, e.Data = audit.EventType(typ), time.UnixMilli(ms).UTC(), json.RawMessage(data)
		events = append(events, e)
		phys = append(phys, p)
	}
	if err := rows.Err(); err != nil {
		w.t.Fatal(err)
	}
	_ = rows.Close()

	events = mutate(events)
	prev := audit.GenesisHash(key)
	for i := range events {
		events[i].ChainSeq = int64(i + 1)
		events[i].PrevHash = prev
		if events[i].Hash, err = events[i].ComputeHash(); err != nil {
			w.t.Fatal(err)
		}
		prev = events[i].Hash
	}
	// Rows that were dropped by the mutation are deleted, the others updated in place.
	keep := map[int64]bool{}
	for i := range events {
		if i < len(phys) {
			keep[phys[i]] = true
		}
	}
	for _, p := range phys {
		if !keep[p] {
			w.exec(`DELETE FROM audit_events WHERE seq = ?`, p)
		}
	}
	for i, e := range events {
		if i < len(phys) {
			w.exec(`UPDATE audit_events SET data = ?, event_type = ?, occurred_at = ?, chain_seq = ?, prev_hash = ?, hash = ? WHERE seq = ?`,
				string(e.Data), string(e.Type), e.OccurredAt.UnixMilli(), e.ChainSeq, e.PrevHash, e.Hash, phys[i])
		}
	}
}

// problemAt reports whether the report has a problem of the kind at the position (0 matches any).
func problemAt(rep audit.VerifyReport, kind audit.ProblemKind, seq int64) bool {
	for _, p := range rep.Problems {
		if p.Kind == kind && (seq == 0 || p.ChainSeq == seq) {
			return true
		}
	}
	return false
}

// mustBeBroken fails unless VerifyChain reports a broken chain, and returns its report.
func mustBeBroken(t *testing.T, rep audit.VerifyReport, err error) audit.VerifyReport {
	t.Helper()
	if err == nil {
		t.Fatalf("verification found nothing wrong (report %+v)", rep)
	}
	if !errors.Is(err, audit.ErrChainBroken) {
		t.Fatalf("verification error = %v, want audit.ErrChainBroken", err)
	}
	if rep.OK() {
		t.Fatal("ErrChainBroken with a clean report")
	}
	return rep
}
