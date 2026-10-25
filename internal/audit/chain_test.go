package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// independentHash derives an event's hash the way the package comment and the
// brief state it, without using the package's own hashing code: encoding/json
// writes map keys sorted and the test data has nothing to escape, which is
// what the canonical form is for such a document.
func independentHash(t *testing.T, e Event) string {
	t.Helper()
	body := map[string]any{
		"format":         "alixai.audit-event/v1",
		"id":             e.ID,
		"application_id": e.ApplicationID,
		"tenant_id":      e.TenantID,
		"workspace_id":   e.WorkspaceID,
		"chain_seq":      e.ChainSeq,
		"type":           string(e.Type),
		"occurred_at_ms": e.OccurredAt.UnixMilli(),
		"principal_id":   e.PrincipalID,
		"session_id":     e.SessionID,
		"data":           json.RawMessage(e.Data),
	}
	canon, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(append(canon, []byte(e.PrevHash)...))
	return hex.EncodeToString(sum[:])
}

// SEC-085
func TestAppendChains(t *testing.T) {
	h := newHarness(t)
	a1 := scope("app-a", "tenant-1", "ws-1")
	a1w2 := scope("app-a", "tenant-1", "ws-2")
	a2 := scope("app-a", "tenant-2", "ws-1")
	b1 := scope("app-b", "tenant-1", "ws-1")

	// Interleave appends to several chains; each chain counts on its own.
	var t1 []Event
	for i := 1; i <= 6; i++ {
		h.clk.Advance(1500 * time.Millisecond)
		s := a1
		if i%2 == 0 {
			s = a1w2 // another workspace of the same tenant: same chain
		}
		e, err := h.log.Append(ctx, s, settings(i))
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		t1 = append(t1, e)
		if _, err := h.log.Append(ctx, a2, settings(i)); err != nil {
			t.Fatal(err)
		}
	}
	bEvent, err := h.log.Append(ctx, b1, settings(1))
	if err != nil {
		t.Fatal(err)
	}

	prev := GenesisHash(ChainKey{"app-a", "tenant-1"})
	for i, e := range t1 {
		if e.ChainSeq != int64(i+1) {
			t.Errorf("event %d: chain_seq = %d, want %d", i, e.ChainSeq, i+1)
		}
		if e.PrevHash != prev {
			t.Errorf("event %d: prev_hash = %s, want the previous hash %s", i, e.PrevHash, prev)
		}
		if want := independentHash(t, e); e.Hash != want {
			t.Errorf("event %d: hash = %s, independent derivation gives %s", i, e.Hash, want)
		}
		if err := e.HashValid(); err != nil {
			t.Errorf("event %d: HashValid: %v", i, err)
		}
		if !ids.Valid(e.ID, IDPrefix) {
			t.Errorf("event %d: id %q is not aud_<ulid>", i, e.ID)
		}
		if got, want := e.OccurredAt, t0.Add(time.Duration(i+1)*1500*time.Millisecond); !got.Equal(want) {
			t.Errorf("event %d: occurred_at = %s, want %s", i, got, want)
		}
		wantWS := "ws-1"
		if (i+1)%2 == 0 {
			wantWS = "ws-2"
		}
		if e.WorkspaceID != wantWS || e.PrincipalID != "usr_test" || e.SessionID != "ses_test" {
			t.Errorf("event %d: workspace/principal/session = %q/%q/%q", i, e.WorkspaceID, e.PrincipalID, e.SessionID)
		}
		prev = e.Hash
	}

	// Chains are independent: tenant-2 and app-b start at 1 from their own genesis.
	if bEvent.ChainSeq != 1 || bEvent.PrevHash != GenesisHash(ChainKey{"app-b", "tenant-1"}) {
		t.Errorf("app-b chain: seq %d prev %s", bEvent.ChainSeq, bEvent.PrevHash)
	}
	if GenesisHash(ChainKey{"app-a", "tenant-1"}) == GenesisHash(ChainKey{"app-a", "tenant-2"}) ||
		GenesisHash(ChainKey{"app-a", "tenant-1"}) == GenesisHash(ChainKey{"app-b", "tenant-1"}) {
		t.Error("genesis hashes of different chains must differ")
	}

	// Application-level and platform events chain on their own, with an empty tenant.
	appLoc, err := ForApplication("app-a", "operator-1")
	if err != nil {
		t.Fatal(err)
	}
	pi, err := h.log.AppendAt(ctx, appLoc, NewEvent{Type: EventPackageInstalled, Data: map[string]any{"version": "1.0.0"}})
	if err != nil {
		t.Fatal(err)
	}
	pq, err := h.log.AppendAt(ctx, appLoc, NewEvent{Type: EventPackageQuarantined})
	if err != nil {
		t.Fatal(err)
	}
	if pi.TenantID != "" || pi.ChainSeq != 1 || pq.ChainSeq != 2 || pq.PrevHash != pi.Hash || pi.PrincipalID != "operator-1" {
		t.Errorf("application chain: %+v then %+v", pi, pq)
	}
	plat, err := h.log.AppendAt(ctx, Platform(), settings(1))
	if err != nil {
		t.Fatal(err)
	}
	if plat.ApplicationID != trust.PlatformApplicationID || plat.TenantID != "" || plat.ChainSeq != 1 {
		t.Errorf("platform event: %+v", plat)
	}

	keys, err := h.log.Chains(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []ChainKey{{"alixai.platform", ""}, {"app-a", ""}, {"app-a", "tenant-1"}, {"app-a", "tenant-2"}, {"app-b", "tenant-1"}}
	if fmt.Sprint(keys) != fmt.Sprint(want) {
		t.Errorf("Chains = %v, want %v", keys, want)
	}
	for _, k := range keys {
		rep := h.mustVerify(k)
		if rep.Events == 0 || rep.HeadHash == "" {
			t.Errorf("%s: empty report %+v", k, rep)
		}
	}
	if rep := h.mustVerify(ChainKey{"app-a", "tenant-1"}); rep.Events != 6 || rep.HeadSeq != 6 || rep.HeadHash != t1[5].Hash {
		t.Errorf("tenant-1 report = %+v", rep)
	}
	// An empty chain verifies trivially.
	if rep := h.mustVerify(ChainKey{"app-a", "tenant-9"}); rep.Events != 0 {
		t.Errorf("empty chain report = %+v", rep)
	}
}

func TestAppendRefusals(t *testing.T) {
	h := newHarness(t)
	good := scope("app-a", "tenant-1", "ws-1")
	missing := good
	missing.TenantID = ""
	wild := good
	wild.WorkspaceID = "*"
	platform := scope(trust.PlatformApplicationID, "tenant-1", "ws-1")

	tests := []struct {
		name string
		do   func() error
		code contracts.ErrCode
	}{
		{"missing tenant is refused, never defaulted", func() error { _, err := h.log.Append(ctx, missing, settings(1)); return err }, contracts.ErrScopeMissing},
		{"wildcard workspace", func() error { _, err := h.log.Append(ctx, wild, settings(1)); return err }, contracts.ErrScopeViolation},
		{"a request scope cannot name the platform application", func() error { _, err := h.log.Append(ctx, platform, settings(1)); return err }, contracts.ErrScopeViolation},
		{"zero Locator", func() error { _, err := h.log.AppendAt(ctx, Locator{}, settings(1)); return err }, contracts.ErrScopeMissing},
		{"unregistered type", func() error {
			_, err := h.log.Append(ctx, good, NewEvent{Type: "made.up", Data: map[string]any{}})
			return err
		}, contracts.ErrValidationFailed},
		{"application-level type on a tenant chain", func() error {
			_, err := h.log.Append(ctx, good, NewEvent{Type: EventPackageInstalled})
			return err
		}, contracts.ErrValidationFailed},
		{"tenant-level type on an application chain", func() error {
			loc, _ := ForApplication("app-a", "")
			_, err := h.log.AppendAt(ctx, loc, NewEvent{Type: EventDeletionTenant})
			return err
		}, contracts.ErrValidationFailed},
		{"required key missing", func() error {
			_, err := h.log.Append(ctx, good, NewEvent{Type: EventActionCommitting, Data: map[string]any{"other": 1}})
			return err
		}, contracts.ErrValidationFailed},
		{"a float is refused, never rounded", func() error {
			_, err := h.log.Append(ctx, good, NewEvent{Type: EventSettingsChanged, Data: map[string]any{"ratio": 1.5}})
			return err
		}, contracts.ErrSchemaInvalid},
		{"an integer beyond 2^53", func() error {
			_, err := h.log.Append(ctx, good, NewEvent{Type: EventSettingsChanged, Data: map[string]any{"big": int64(1) << 60}})
			return err
		}, contracts.ErrSchemaInvalid},
		{"duplicate keys in raw JSON", func() error {
			_, err := h.log.Append(ctx, good, NewEvent{Type: EventSettingsChanged, Data: json.RawMessage(`{"a":1,"a":2}`)})
			return err
		}, contracts.ErrSchemaInvalid},
		{"data that is not an object", func() error {
			_, err := h.log.Append(ctx, good, NewEvent{Type: EventSettingsChanged, Data: []string{"a"}})
			return err
		}, contracts.ErrSchemaInvalid},
		{"oversized data", func() error {
			_, err := h.log.Append(ctx, good, NewEvent{Type: EventSettingsChanged, Data: map[string]any{"blob": strings.Repeat("x", DefaultMaxDataBytes)}})
			return err
		}, contracts.ErrValidationFailed},
		{"a canceled context writes nothing", func() error {
			c, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := h.log.Append(c, good, settings(1))
			return err
		}, contracts.ErrUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.do()
			if err == nil {
				t.Fatal("the append succeeded")
			}
			if got := contracts.CodeOf(err); got != tc.code {
				t.Errorf("code = %s, want %s (%v)", got, tc.code, err)
			}
		})
	}
	if n := h.count(`SELECT COUNT(*) FROM audit_events`); n != 0 {
		t.Fatalf("%d events were stored by refused appends", n)
	}

	// A registered Validate hook has the last word.
	reg := NewCoreRegistry()
	if err := reg.Register(TypeSpec{Name: "x.checked", Validate: func(d map[string]any) error {
		if d["ok"] != true {
			return fmt.Errorf("not ok")
		}
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	h2 := newHarness(t, func(o *Options) { o.Registry = reg })
	if _, err := h2.log.Append(ctx, good, NewEvent{Type: "x.checked", Data: map[string]any{"ok": false}}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("Validate hook: %v", err)
	}
	if _, err := h2.log.Append(ctx, good, NewEvent{Type: "x.checked", Data: map[string]any{"ok": true}}); err != nil {
		t.Errorf("Validate hook accepting: %v", err)
	}
}

// SEC-085: a changed event, a reordered pair and a spliced event are detected
// by chain verification. Every case removes the append-only triggers first, so
// it is the chain that detects the change, not the trigger. Each case lists
// the exact findings ("kind@position"): nothing less is detected and nothing
// more is reported.
func TestTamperDetected(t *testing.T) {
	key := ChainKey{"app-a", "tenant-1"}
	const otherID = "aud_01J9ZQ3K8V4W5X6Y7Z8A9B0C1D"
	tests := []struct {
		name   string
		tamper func(h *harness)
		want   []string
	}{
		{"data modified", func(h *harness) {
			h.exec(`UPDATE audit_events SET data = '{"n":99}' WHERE tenant_id = 'tenant-1' AND chain_seq = 3`)
		}, []string{"hash_mismatch@3"}},
		{"event type modified", func(h *harness) {
			h.exec(`UPDATE audit_events SET event_type = 'deletion.tenant' WHERE tenant_id = 'tenant-1' AND chain_seq = 3`)
		}, []string{"hash_mismatch@3"}},
		{"principal modified", func(h *harness) {
			h.exec(`UPDATE audit_events SET principal_id = 'someone-else' WHERE tenant_id = 'tenant-1' AND chain_seq = 3`)
		}, []string{"hash_mismatch@3"}},
		{"session modified", func(h *harness) {
			h.exec(`UPDATE audit_events SET session_id = 'ses_other' WHERE tenant_id = 'tenant-1' AND chain_seq = 3`)
		}, []string{"hash_mismatch@3"}},
		{"timestamp modified", func(h *harness) {
			h.exec(`UPDATE audit_events SET occurred_at = occurred_at + 1000 WHERE tenant_id = 'tenant-1' AND chain_seq = 3`)
		}, []string{"hash_mismatch@3"}},
		{"moved to another workspace (would defeat export scoping)", func(h *harness) {
			h.exec(`UPDATE audit_events SET workspace_id = 'ws-other' WHERE tenant_id = 'tenant-1' AND chain_seq = 3`)
		}, []string{"hash_mismatch@3"}},
		{"id modified", func(h *harness) {
			h.exec(`UPDATE audit_events SET id = ? WHERE tenant_id = 'tenant-1' AND chain_seq = 3`, otherID)
		}, []string{"hash_mismatch@3"}},
		{"stored hash replaced", func(h *harness) {
			h.exec(`UPDATE audit_events SET hash = ? WHERE tenant_id = 'tenant-1' AND chain_seq = 3`, strings.Repeat("0", 64))
		}, []string{"hash_mismatch@3", "link_broken@4"}},
		{"event modified and its own hash recomputed", func(h *harness) {
			var e Event
			var ms int64
			var typ string
			row := h.db.QueryRowContext(ctx, `SELECT id, workspace_id, chain_seq, event_type, occurred_at, principal_id, session_id, prev_hash FROM audit_events WHERE tenant_id = 'tenant-1' AND chain_seq = 3`)
			if err := row.Scan(&e.ID, &e.WorkspaceID, &e.ChainSeq, &typ, &ms, &e.PrincipalID, &e.SessionID, &e.PrevHash); err != nil {
				h.t.Fatal(err)
			}
			e.ApplicationID, e.TenantID, e.Type, e.Data = "app-a", "tenant-1", EventType(typ), json.RawMessage(`{"n":99}`)
			e.OccurredAt = time.UnixMilli(ms).UTC()
			newHash, err := e.ComputeHash()
			if err != nil {
				h.t.Fatal(err)
			}
			h.exec(`UPDATE audit_events SET data = ?, hash = ? WHERE tenant_id = 'tenant-1' AND chain_seq = 3`, string(e.Data), newHash)
		}, []string{"link_broken@4"}},
		{"two events swapped (reordered)", func(h *harness) {
			h.exec(`UPDATE audit_events SET chain_seq = 102 WHERE tenant_id = 'tenant-1' AND chain_seq = 2`)
			h.exec(`UPDATE audit_events SET chain_seq = 2 WHERE tenant_id = 'tenant-1' AND chain_seq = 3`)
			h.exec(`UPDATE audit_events SET chain_seq = 3 WHERE tenant_id = 'tenant-1' AND chain_seq = 102`)
		}, []string{"hash_mismatch@2", "hash_mismatch@3", "link_broken@2", "link_broken@3", "link_broken@4", "order@3"}},
		{"an event with made-up hashes appended at the tail", func(h *harness) {
			h.exec(`INSERT INTO audit_events (id, application_id, tenant_id, workspace_id, chain_seq, event_type, occurred_at, principal_id, session_id, data, prev_hash, hash)
			        VALUES (?, 'app-a', 'tenant-1', 'ws-1', 6, 'settings.changed', 1, 'x', 'y', '{}', ?, ?)`,
				otherID, strings.Repeat("1", 64), strings.Repeat("2", 64))
		}, []string{"hash_mismatch@6", "link_broken@6"}},
		{"a row re-inserted after later rows (physical order)", func(h *harness) {
			// Take event 2 out and put it back: same content, same hash, but now the
			// newest row of the table.
			h.exec(`CREATE TABLE scratch AS SELECT * FROM audit_events WHERE tenant_id = 'tenant-1' AND chain_seq = 2`)
			h.exec(`DELETE FROM audit_events WHERE tenant_id = 'tenant-1' AND chain_seq = 2`)
			h.exec(`INSERT INTO audit_events (id, application_id, tenant_id, workspace_id, chain_seq, event_type, occurred_at, principal_id, session_id, data, prev_hash, hash)
			        SELECT id, application_id, tenant_id, workspace_id, chain_seq, event_type, occurred_at, principal_id, session_id, data, prev_hash, hash FROM scratch`)
			h.exec(`DROP TABLE scratch`)
		}, []string{"order@3"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.appendN(scope("app-a", "tenant-1", "ws-1"), 5)
			if rep := h.mustVerify(key); rep.Events != 5 {
				t.Fatalf("baseline: %+v", rep)
			}
			h.dropTriggers()
			tc.tamper(h)
			rep := h.mustBreak(key)
			var got []string
			for _, p := range rep.Problems {
				got = append(got, fmt.Sprintf("%s@%d", p.Kind, p.ChainSeq))
			}
			sort.Strings(got)
			sort.Strings(tc.want)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("findings = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("events spliced into another tenant's chain", func(t *testing.T) {
		h := newHarness(t)
		h.appendN(scope("app-a", "tenant-1", "ws-1"), 3)
		h.appendN(scope("app-a", "tenant-2", "ws-1"), 3)
		h.dropTriggers()
		// Move tenant-1's first event over to tenant-2's chain, in place of
		// tenant-2's first event.
		h.exec(`DELETE FROM audit_events WHERE tenant_id = 'tenant-2' AND chain_seq = 1`)
		h.exec(`UPDATE audit_events SET tenant_id = 'tenant-2' WHERE tenant_id = 'tenant-1' AND chain_seq = 1`)
		rep := h.mustBreak(ChainKey{"app-a", "tenant-2"})
		if !has(rep, ProblemHashMismatch, 1) || !has(rep, ProblemLinkBroken, 1) || !has(rep, ProblemLinkBroken, 2) {
			t.Errorf("tenant-2 findings: %+v", rep.Problems)
		}
		if rep := h.mustBreak(key); !has(rep, ProblemGap, 2) {
			t.Errorf("tenant-1 lost its first event: %+v", rep.Problems)
		}
	})

	t.Run("the triggers refuse an update or a delete before anyone drops them", func(t *testing.T) {
		h := newHarness(t)
		h.appendN(scope("app-a", "tenant-1", "ws-1"), 3)
		for _, q := range []string{
			`UPDATE audit_events SET data = '{}' WHERE chain_seq = 2`,
			`DELETE FROM audit_events WHERE chain_seq = 2`,
		} {
			if _, err := h.db.ExecContext(ctx, q); err == nil {
				t.Errorf("%s succeeded on an append-only table", q)
			}
		}
		h.mustVerify(key)
	})
}

// SEC-085
func TestDeletionOfMiddleEventDetected(t *testing.T) {
	h := newHarness(t)
	key := ChainKey{"app-a", "tenant-1"}
	h.appendN(scope("app-a", "tenant-1", "ws-1"), 6)
	h.mustVerify(key)

	h.dropTriggers()
	h.exec(`DELETE FROM audit_events WHERE tenant_id = 'tenant-1' AND chain_seq = 3`)
	rep := h.mustBreak(key)
	if !has(rep, ProblemGap, 4) {
		t.Errorf("a missing position 3 must show as a gap at position 4: %+v", rep.Problems)
	}
	if !has(rep, ProblemLinkBroken, 4) {
		t.Errorf("position 4 must not link to position 2: %+v", rep.Problems)
	}
	if rep.Events != 5 || rep.HeadSeq != 6 {
		t.Errorf("report = %+v", rep)
	}

	// Removing the first event is just as visible: the new first event does not
	// link to the genesis hash.
	h2 := newHarness(t)
	h2.appendN(scope("app-a", "tenant-1", "ws-1"), 4)
	h2.dropTriggers()
	h2.exec(`DELETE FROM audit_events WHERE chain_seq = 1`)
	if rep := h2.mustBreak(key); !has(rep, ProblemGap, 2) || !has(rep, ProblemLinkBroken, 2) {
		t.Errorf("deleted first event: %+v", rep.Problems)
	}

}

// SEC-085
func TestConcurrentAppendsKeepChain(t *testing.T) {
	h := newHarness(t)
	key := ChainKey{"app-a", "tenant-1"}

	// A second store handle on the same file with its own Log is a second
	// process as far as SQLite is concerned: its writes contend for the
	// database write lock instead of the first handle's writer connection.
	db2, err := store.Open(ctx, store.Options{Path: h.db.Path()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	log2, err := New(db2, Options{Clock: h.clk, Registry: NewCoreRegistry()})
	if err != nil {
		t.Fatal(err)
	}

	const workers, each = 12, 20
	var wg sync.WaitGroup
	errs := make(chan error, workers*each)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l := h.log
			if w%3 == 0 {
				l = log2
			}
			ws := fmt.Sprintf("ws-%d", w%4)
			for i := 0; i < each; i++ {
				// Half the workers also write a second tenant's chain.
				for _, tenant := range []string{"tenant-1", "tenant-2"} {
					if tenant == "tenant-2" && w%2 == 1 {
						continue
					}
					if _, err := l.Append(ctx, scope("app-a", tenant, ws), settings(w*1000+i)); err != nil {
						errs <- err
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent append failed: %v", err)
	}

	// Chain tenant-1 holds every append, contiguous from 1, and verifies.
	rep := h.mustVerify(key)
	if rep.Events != workers*each || rep.HeadSeq != workers*each {
		t.Fatalf("tenant-1: events %d head %d, want %d", rep.Events, rep.HeadSeq, workers*each)
	}
	rows, err := h.db.QueryContext(ctx, `SELECT chain_seq FROM audit_events WHERE tenant_id = 'tenant-1' ORDER BY chain_seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := int64(1)
	for rows.Next() {
		var s int64
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		if s != want {
			t.Fatalf("chain_seq %d where %d was expected: positions are not contiguous", s, want)
		}
		want++
	}
	if rep2 := h.mustVerify(ChainKey{"app-a", "tenant-2"}); rep2.Events != (workers/2)*each {
		t.Errorf("tenant-2 holds %d events, want %d", rep2.Events, (workers/2)*each)
	}
}

func TestEventHashValidOnExportedShape(t *testing.T) {
	h := newHarness(t)
	e := h.appendN(scope("app-a", "tenant-1", "ws-1"), 1)[0]
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var back Event
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if err := back.HashValid(); err != nil {
		t.Fatalf("an event must survive its JSON form: %v", err)
	}
	back.Data = json.RawMessage(`{"n":2}`)
	if err := back.HashValid(); err == nil {
		t.Fatal("HashValid accepted changed data")
	}
}
