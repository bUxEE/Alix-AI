package audit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

var t0 = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

const signerID = "alixai-audit-signer-2026"

// harness is a Log over a throwaway database with a fake clock and a
// generated platform signing key registered in the trust store. Importing
// internal/trust registers its migrations next to this package's, so the
// default registry holds exactly the "audit" and "trust" modules.
type harness struct {
	t     *testing.T
	db    *store.DB
	clk   *clock.Fake
	log   *Log
	trust *trust.Store
	pub   ed25519.PublicKey
	priv  ed25519.PrivateKey
}

func newHarness(t *testing.T, mutate ...func(*Options)) *harness {
	t.Helper()
	h := &harness{t: t, clk: clock.NewFake(t0)}
	h.db = storetest.Open(t, nil)
	h.trust = trust.NewStore(h.db, h.clk)
	var err error
	if h.pub, h.priv, err = ed25519.GenerateKey(rand.Reader); err != nil {
		t.Fatal(err)
	}
	if _, err := h.trust.RegisterKey(context.Background(), trust.PlatformApplicationID, signerID, h.pub, "audit checkpoint signer", trust.WithPurpose(trust.PurposeAuditCheckpoint)); err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Clock:    h.clk,
		Registry: NewCoreRegistry(),
		Signing:  &trust.SigningKey{KeyID: signerID, Key: h.priv},
		Verifier: trust.NewVerifier(h.trust, h.clk),
	}
	for _, m := range mutate {
		m(&opts)
	}
	if h.log, err = New(h.db, opts); err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

// scope builds a valid scope; caps are its capability scopes.
func scope(app, tenant, workspace string, caps ...string) contracts.Scope {
	s := storetest.Scope(app, tenant, workspace)
	if len(caps) > 0 {
		s.CapabilityScopes = caps
	}
	return s
}

var ctx = context.Background()

// settings is a generic tenant-or-application event with a numbered payload.
func settings(n int, extra ...string) NewEvent {
	d := map[string]any{"n": n}
	for i := 0; i+1 < len(extra); i += 2 {
		d[extra[i]] = extra[i+1]
	}
	return NewEvent{Type: EventSettingsChanged, Data: d}
}

// appendN appends n numbered events for scope and returns them.
func (h *harness) appendN(s contracts.Scope, n int) []Event {
	h.t.Helper()
	out := make([]Event, 0, n)
	for i := 1; i <= n; i++ {
		h.clk.Advance(time.Second)
		e, err := h.log.Append(ctx, s, settings(i))
		if err != nil {
			h.t.Fatalf("Append #%d: %v", i, err)
		}
		out = append(out, e)
	}
	return out
}

// dropTriggers removes the append-only triggers, which is what someone with
// write access to the database file would do first. Tests that tamper call it
// so they test chain verification, not the triggers.
func (h *harness) dropTriggers() {
	h.t.Helper()
	for _, name := range []string{"audit_events_no_update", "audit_events_no_delete", "audit_checkpoints_no_update", "audit_checkpoints_no_delete"} {
		h.exec("DROP TRIGGER " + name)
	}
}

func (h *harness) exec(query string, args ...any) {
	h.t.Helper()
	if _, err := h.db.ExecContext(ctx, query, args...); err != nil {
		h.t.Fatalf("%s: %v", query, err)
	}
}

func (h *harness) count(query string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		h.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// verify runs VerifyChain and returns the report and error.
func (h *harness) verify(key ChainKey, opts ...VerifyOptions) (VerifyReport, error) {
	h.t.Helper()
	var o VerifyOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	return h.log.VerifyChain(ctx, key, o)
}

// mustVerify fails the test unless the chain verifies.
func (h *harness) mustVerify(key ChainKey, opts ...VerifyOptions) VerifyReport {
	h.t.Helper()
	rep, err := h.verify(key, opts...)
	if err != nil {
		h.t.Fatalf("VerifyChain(%s): %v\n%+v", key, err, rep.Problems)
	}
	return rep
}

// mustBreak fails the test unless verification reports ErrChainBroken, and
// returns the report.
func (h *harness) mustBreak(key ChainKey, opts ...VerifyOptions) VerifyReport {
	h.t.Helper()
	rep, err := h.verify(key, opts...)
	if err == nil {
		h.t.Fatalf("VerifyChain(%s) found nothing wrong, want a broken chain (report %+v)", key, rep)
	}
	if !errors.Is(err, ErrChainBroken) {
		h.t.Fatalf("VerifyChain(%s) error = %v, want ErrChainBroken", key, err)
	}
	if rep.OK() {
		h.t.Fatalf("VerifyChain returned ErrChainBroken with a clean report")
	}
	return rep
}

// has reports whether the report has a problem of kind at position seq (seq
// 0 matches any position).
func has(rep VerifyReport, kind ProblemKind, seq int64) bool {
	for _, p := range rep.Problems {
		if p.Kind == kind && (seq == 0 || p.ChainSeq == seq) {
			return true
		}
	}
	return false
}

// exportLines runs Export and returns the decoded events.
func (h *harness) exportLines(s contracts.Scope, opts ExportOptions) ([]Event, string, error) {
	h.t.Helper()
	var buf bytes.Buffer
	_, err := h.log.Export(ctx, s, &buf, opts)
	raw := buf.String()
	var out []Event
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var e Event
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		if derr := dec.Decode(&e); derr != nil {
			h.t.Fatalf("export line does not decode: %v\n%s", derr, line)
		}
		out = append(out, e)
	}
	return out, raw, err
}

// rewriteChain simulates an attacker who can write the database and wants the
// chain to look intact after a change: it loads every event of key, lets mutate
// edit them in place, recomputes every prev_hash and hash from position 1 and
// writes them back.
func (h *harness) rewriteChain(key ChainKey, mutate func(events []Event)) {
	h.t.Helper()
	rows, err := h.db.QueryContext(ctx,
		`SELECT seq, id, workspace_id, chain_seq, event_type, occurred_at, principal_id, session_id, data FROM audit_events
		 WHERE application_id = ? AND tenant_id = ? ORDER BY chain_seq`, key.ApplicationID, key.TenantID)
	if err != nil {
		h.t.Fatal(err)
	}
	var (
		events []Event
		phys   []int64
	)
	for rows.Next() {
		var (
			e    Event
			p    int64
			ms   int64
			typ  string
			data string
		)
		if err := rows.Scan(&p, &e.ID, &e.WorkspaceID, &e.ChainSeq, &typ, &ms, &e.PrincipalID, &e.SessionID, &data); err != nil {
			h.t.Fatal(err)
		}
		e.ApplicationID, e.TenantID = key.ApplicationID, key.TenantID
		e.Type, e.OccurredAt, e.Data = EventType(typ), time.UnixMilli(ms).UTC(), json.RawMessage(data)
		events = append(events, e)
		phys = append(phys, p)
	}
	rows.Close()
	mutate(events)
	prev := GenesisHash(key)
	for i := range events {
		events[i].PrevHash = prev
		var err error
		if events[i].Hash, err = events[i].ComputeHash(); err != nil {
			h.t.Fatal(err)
		}
		prev = events[i].Hash
	}
	for i, e := range events {
		h.exec(`UPDATE audit_events SET data = ?, event_type = ?, occurred_at = ?, prev_hash = ?, hash = ? WHERE seq = ?`,
			string(e.Data), string(e.Type), e.OccurredAt.UnixMilli(), e.PrevHash, e.Hash, phys[i])
	}
}
