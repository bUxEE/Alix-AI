package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// allowAll is a guard that keeps nothing: every event may be erased.
var allowAll Guard = func(context.Context, store.Reader, ChainKey) (func(Event) string, error) {
	return func(Event) string { return "" }, nil
}

// blockSeqs is a guard that keeps the events at the given chain positions, as a
// pending action would.
func blockSeqs(seqs ...int64) Guard {
	return func(context.Context, store.Reader, ChainKey) (func(Event) string, error) {
		return func(e Event) string {
			for _, s := range seqs {
				if e.ChainSeq == s {
					return "a pending action depends on it"
				}
			}
			return ""
		}, nil
	}
}

const (
	erasedReason   = "tenant erasure request"
	erasedOperator = "ops-alice"
)

// secretNote is the payload text of event n: what erasure must remove.
func secretNote(n int) string { return fmt.Sprintf("PAYLOAD-SECRET-%d", n) }

// request is an erasure of the events at the given positions of events.
func request(events []Event, positions ...int) EraseRequest {
	ids := make([]string, 0, len(positions))
	for _, p := range positions {
		ids = append(ids, events[p-1].ID)
	}
	return EraseRequest{Chain: key1, EventIDs: ids, Kind: "tenant", Reason: erasedReason, Operator: erasedOperator, Guard: allowAll}
}

// appendSecrets appends n events whose payload carries a secret note.
func (h *harness) appendSecrets(s contracts.Scope, n int) []Event {
	h.t.Helper()
	out := make([]Event, 0, n)
	for i := 1; i <= n; i++ {
		h.clk.Advance(time.Second)
		e, err := h.log.Append(ctx, s, settings(i, "note", secretNote(i)))
		if err != nil {
			h.t.Fatalf("Append #%d: %v", i, err)
		}
		out = append(out, e)
	}
	return out
}

type linkRow struct {
	Seq        int64
	Hash, Prev string
}

// links reads the hash and prev_hash of every event of the chain, in order.
func (h *harness) links(key ChainKey) []linkRow {
	h.t.Helper()
	rows, err := h.db.QueryContext(ctx, `SELECT chain_seq, hash, prev_hash FROM audit_events WHERE application_id = ? AND tenant_id = ? ORDER BY chain_seq`,
		key.ApplicationID, key.TenantID)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []linkRow
	for rows.Next() {
		var r linkRow
		if err := rows.Scan(&r.Seq, &r.Hash, &r.Prev); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// dropTombstoneTriggers is what a database writer does to forge or remove a
// tombstone, after dropTriggers has opened the events up.
func (h *harness) dropTombstoneTriggers() {
	h.t.Helper()
	h.dropTriggers()
	h.exec("DROP TRIGGER audit_tombstones_no_update")
	h.exec("DROP TRIGGER audit_tombstones_no_delete")
}

func (h *harness) tombstoneBody(chainSeq int64) (id, body string) {
	h.t.Helper()
	if err := h.db.QueryRowContext(ctx, `SELECT id, body FROM audit_tombstones WHERE chain_seq = ?`, chainSeq).Scan(&id, &body); err != nil {
		h.t.Fatalf("tombstone at %d: %v", chainSeq, err)
	}
	return id, body
}

// erasedChain is a chain of 8 events with secret payloads, anchored by a
// checkpoint taken BEFORE the erasure, with events 3, 4 and 6 then erased.
type erasedChain struct {
	h      *harness
	events []Event
	before []linkRow
	cp     Checkpoint
	res    EraseResult
}

func setupErased(t *testing.T) erasedChain {
	t.Helper()
	h := newHarness(t)
	c := erasedChain{h: h, events: h.appendSecrets(s1, 8)}
	var err error
	if c.cp, err = h.log.Checkpoint(ctx, key1); err != nil {
		t.Fatal(err)
	}
	c.before = h.links(key1)
	h.clk.Advance(time.Hour)
	if c.res, err = h.log.Erase(ctx, request(c.events, 3, 4, 6)); err != nil {
		t.Fatalf("Erase: %v", err)
	}
	return c
}

// An erased event keeps its hash, its position and its links, so the chain
// around it verifies as before and a checkpoint taken before the erasure still
// matches. Erasure that rewrote hashes would fail the anchored verification;
// erasure that dropped a tombstone's backing, or let a tombstone be forged,
// edited, moved or removed, is seen by verify-chain.
func TestErasedEventKeepsTheChainVerifiable(t *testing.T) {
	t.Run("hashes and links do not move and the chain verifies anchored", func(t *testing.T) {
		c := setupErased(t)
		if c.res.Erased != 3 || c.res.Blocked != 0 || c.res.RecordID == "" {
			t.Fatalf("result = %+v", c.res)
		}
		after := c.h.links(key1)
		if len(after) != len(c.before)+1 {
			t.Fatalf("%d events after, %d before: only the erasure record may be new", len(after), len(c.before))
		}
		for i, b := range c.before {
			if after[i] != b {
				t.Errorf("position %d: link %+v became %+v", b.Seq, b, after[i])
			}
		}
		rep := c.h.mustVerify(key1, VerifyOptions{RequireAnchor: true, Expected: &c.cp})
		if !rep.Anchored || rep.AnchorSeq != 8 || rep.Erased != 3 || rep.Events != 9 {
			t.Errorf("report = %+v", rep)
		}
		// The payload is gone from the rows.
		if n := c.h.count(`SELECT COUNT(*) FROM audit_events WHERE data LIKE '%PAYLOAD-SECRET-3%' OR data LIKE '%PAYLOAD-SECRET-4%' OR data LIKE '%PAYLOAD-SECRET-6%'`); n != 0 {
			t.Errorf("%d rows still hold an erased payload", n)
		}
		if n := c.h.count(`SELECT COUNT(*) FROM audit_events WHERE data = ?`, ErasedMarker); n != 3 {
			t.Errorf("%d rows hold the erased marker, want 3", n)
		}
		// The others keep theirs.
		if n := c.h.count(`SELECT COUNT(*) FROM audit_events WHERE data LIKE '%PAYLOAD-SECRET-%'`); n != 5 {
			t.Errorf("%d rows still hold a payload, want 5", n)
		}
	})

	t.Run("the chain goes on and a later checkpoint covers it", func(t *testing.T) {
		c := setupErased(t)
		for i := 0; i < 3; i++ {
			c.h.clk.Advance(time.Second)
			if _, err := c.h.log.Append(ctx, s1, settings(100+i)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := c.h.log.Checkpoint(ctx, key1); err != nil {
			t.Fatal(err)
		}
		rep := c.h.mustVerify(key1, VerifyOptions{RequireAnchor: true, Expected: &c.cp})
		if rep.Events != 12 || rep.Erased != 3 || rep.EventsAfterAnchor != 0 {
			t.Errorf("report = %+v", rep)
		}
	})

	t.Run("erasing twice changes nothing and records nothing", func(t *testing.T) {
		c := setupErased(t)
		again, err := c.h.log.Erase(ctx, request(c.events, 3, 4, 6))
		if err != nil {
			t.Fatal(err)
		}
		if again.Erased != 0 || again.AlreadyErased != 3 || again.RecordID != "" {
			t.Errorf("second erasure = %+v", again)
		}
		if n := c.h.count(`SELECT COUNT(*) FROM audit_events WHERE event_type = 'audit.erased'`); n != 1 {
			t.Errorf("%d erasure records, want 1", n)
		}
		c.h.mustVerify(key1, VerifyOptions{RequireAnchor: true, Expected: &c.cp})
	})

	// What someone who can write the database might try after an erasure. Each
	// case drops the triggers first, so it tests verification, not the triggers.
	rogueSigned := func(h *harness, body TombstoneBody) (string, string) {
		_, rogue, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			h.t.Fatal(err)
		}
		at := h.clk.Now().UTC().Truncate(time.Second)
		body.ErasedAt, body.ExpiresAt = checkpointTime(at), checkpointTime(at.Add(DefaultTombstoneValidity))
		sig, err := trust.SignPayload(body, "rogue-key", rogue, at, at.Add(DefaultTombstoneValidity))
		if err != nil {
			h.t.Fatal(err)
		}
		canon, _ := trust.CanonicalJSON(body)
		sj, _ := json.Marshal(sig)
		return string(canon), string(sj)
	}
	for _, tc := range []struct {
		name string
		do   func(c erasedChain)
		kind ProblemKind
		seq  int64
	}{
		{"the reason of a tombstone is edited", func(c erasedChain) {
			_, body := c.h.tombstoneBody(3)
			edited := strings.Replace(body, erasedReason, "nothing happened here", 1)
			if edited == body {
				t.Fatal("the reason is not in the body")
			}
			c.h.exec(`UPDATE audit_tombstones SET body = ? WHERE chain_seq = 3`, edited)
		}, ProblemTombstoneInvalid, 3},
		{"the operator of a tombstone is edited", func(c erasedChain) {
			_, body := c.h.tombstoneBody(4)
			c.h.exec(`UPDATE audit_tombstones SET body = ? WHERE chain_seq = 4`, strings.Replace(body, erasedOperator, "ops-mallory", 1))
		}, ProblemTombstoneInvalid, 4},
		{"the time of an erased event is changed", func(c erasedChain) {
			c.h.exec(`UPDATE audit_events SET occurred_at = occurred_at + 1000 WHERE tenant_id = 'tenant-1' AND chain_seq = 3`)
		}, ProblemTombstoneMismatch, 3},
		{"the principal of an erased event is changed", func(c erasedChain) {
			c.h.exec(`UPDATE audit_events SET principal_id = 'someone-else' WHERE tenant_id = 'tenant-1' AND chain_seq = 4`)
		}, ProblemTombstoneMismatch, 4},
		{"the type of an erased event is changed", func(c erasedChain) {
			c.h.exec(`UPDATE audit_events SET event_type = 'deletion.tenant' WHERE tenant_id = 'tenant-1' AND chain_seq = 6`)
		}, ProblemTombstoneMismatch, 6},
		{"the hash of an erased event is changed", func(c erasedChain) {
			c.h.exec(`UPDATE audit_events SET hash = ? WHERE tenant_id = 'tenant-1' AND chain_seq = 3`, strings.Repeat("0", 64))
		}, ProblemTombstoneMismatch, 3},
		{"a tombstone is moved to another event", func(c erasedChain) {
			id3, id4 := c.events[2].ID, c.events[3].ID
			c.h.exec(`UPDATE audit_tombstones SET event_id = 'tmp' WHERE chain_seq = 3`)
			c.h.exec(`UPDATE audit_tombstones SET event_id = ? WHERE chain_seq = 4`, id3)
			c.h.exec(`UPDATE audit_tombstones SET event_id = ? WHERE event_id = 'tmp'`, id4)
		}, ProblemTombstoneMismatch, 3},
		{"a tombstone row is deleted", func(c erasedChain) {
			c.h.exec(`DELETE FROM audit_tombstones WHERE chain_seq = 3`)
		}, ProblemHashMismatch, 3},
		{"the payload is put back and the tombstone left", func(c erasedChain) {
			c.h.exec(`UPDATE audit_events SET data = ? WHERE tenant_id = 'tenant-1' AND chain_seq = 3`, string(c.events[2].Data))
		}, ProblemTombstoneMismatch, 3},
		{"the erased marker is written over an event with no tombstone", func(c erasedChain) {
			c.h.exec(`UPDATE audit_events SET data = ? WHERE tenant_id = 'tenant-1' AND chain_seq = 2`, ErasedMarker)
		}, ProblemHashMismatch, 2},
		{"a tombstone signed by a key the trust store does not know", func(c erasedChain) {
			_, body := c.h.tombstoneBody(3)
			var b TombstoneBody
			if err := json.Unmarshal([]byte(body), &b); err != nil {
				t.Fatal(err)
			}
			b.Reason = "forged"
			canon, sig := rogueSigned(c.h, b)
			c.h.exec(`UPDATE audit_tombstones SET body = ?, signature = ? WHERE chain_seq = 3`, canon, sig)
		}, ProblemTombstoneInvalid, 3},
		{"a tombstone signed by the right key and then revoked", func(c erasedChain) {
			if err := c.h.trust.RevokeKey(ctx, trust.PlatformApplicationID, signerID, "compromise"); err != nil {
				t.Fatal(err)
			}
		}, ProblemTombstoneInvalid, 3},
	} {
		t.Run("detected: "+tc.name, func(t *testing.T) {
			c := setupErased(t)
			c.h.mustVerify(key1, VerifyOptions{RequireAnchor: true, Expected: &c.cp}) // the baseline is sound
			c.h.dropTombstoneTriggers()
			tc.do(c)
			rep := c.h.mustBreak(key1)
			if !has(rep, tc.kind, tc.seq) {
				t.Errorf("want %s at %d, got %+v", tc.kind, tc.seq, rep.Problems)
			}
		})
	}

	t.Run("detected: a log with no verifier cannot believe a tombstone", func(t *testing.T) {
		c := setupErased(t)
		blind, err := New(c.h.db, Options{Clock: c.h.clk, Registry: NewCoreRegistry()})
		if err != nil {
			t.Fatal(err)
		}
		rep, err := blind.VerifyChain(ctx, key1, VerifyOptions{})
		if !errors.Is(err, ErrChainBroken) || !has(rep, ProblemTombstoneUnverifiable, 3) {
			t.Errorf("err = %v, report %+v", err, rep.Problems)
		}
	})

	t.Run("detected: the record of the erasure is gone", func(t *testing.T) {
		c := setupErased(t)
		c.h.dropTombstoneTriggers()
		// An attacker who removes the record (the newest event) and re-anchors
		// nothing is caught by the chain; one who rewrites its type is caught too.
		c.h.exec(`UPDATE audit_events SET event_type = 'settings.changed' WHERE event_type = 'audit.erased'`)
		rep := c.h.mustBreak(key1)
		if !has(rep, ProblemHashMismatch, 0) || !has(rep, ProblemTombstoneUnrecorded, 0) {
			t.Errorf("report = %+v", rep.Problems)
		}
	})

	t.Run("a database from before tombstones verifies as it did", func(t *testing.T) {
		h := newHarness(t)
		h.appendSecrets(s1, 4)
		h.dropTriggers()
		h.exec(`DROP TABLE audit_tombstones`)
		rep := h.mustVerify(key1)
		if rep.Events != 4 || rep.Erased != 0 {
			t.Errorf("report = %+v", rep)
		}
	})

	t.Run("a database from before tombstones exports as it did", func(t *testing.T) {
		h := newHarness(t)
		h.appendSecrets(s1, 4)
		h.dropTriggers()
		h.exec(`DROP TABLE audit_tombstones`)
		h.exec(`UPDATE audit_events SET data = ? WHERE tenant_id = 'tenant-1' AND chain_seq IN (2, 3)`, ErasedMarker)
		events, _, err := h.exportLines(scope("app-a", "tenant-1", "ws-1", CapabilityExport), ExportOptions{})
		if err != nil || len(events) != 5 {
			t.Fatalf("Export = %d events, %v", len(events), err)
		}
		for _, ev := range events {
			if ev.Tombstone != nil {
				t.Errorf("event %d carries a tombstone the database cannot hold", ev.ChainSeq)
			}
		}
	})
}

// An event that no longer hashes to what is stored is never tombstoned:
// erasing it would sign the tampered hash and launder the tampering.
func TestErasureRefusesATamperedEvent(t *testing.T) {
	h := newHarness(t)
	events := h.appendSecrets(s1, 5)
	h.dropTriggers()
	h.exec(`UPDATE audit_events SET data = '{"n":99}' WHERE tenant_id = 'tenant-1' AND chain_seq = 3`)
	_, err := h.log.Erase(ctx, request(events, 2, 3))
	if !errors.Is(err, ErrChainBroken) {
		t.Fatalf("err = %v, want ErrChainBroken", err)
	}
	if n := h.count(`SELECT COUNT(*) FROM audit_tombstones`); n != 0 {
		t.Errorf("%d tombstones were written", n)
	}
	if n := h.count(`SELECT COUNT(*) FROM audit_events WHERE data = ?`, ErasedMarker); n != 0 {
		t.Errorf("%d payloads were erased", n)
	}
	if n := h.count(`SELECT COUNT(*) FROM audit_events WHERE event_type = 'audit.erased'`); n != 0 {
		t.Errorf("%d erasure records were written", n)
	}
	// The healthy event alone is still erasable: the refusal is about the tampered one.
	if _, err := h.log.Erase(ctx, request(events, 2)); err != nil {
		t.Errorf("erasing an untouched event: %v", err)
	}
}

// What the tombstone records: who erased what and when, and where the chain
// records it. Nothing of the erased payload is left anywhere in the database.
func TestTombstoneRecordsWhoErasedWhatAndWhen(t *testing.T) {
	c := setupErased(t)
	h := c.h
	erasedAt := t0.Add(8*time.Second + time.Hour).Truncate(time.Second)

	id, raw := h.tombstoneBody(4)
	var tb Tombstone
	var sigJSON string
	if err := h.db.QueryRowContext(ctx, `SELECT signature FROM audit_tombstones WHERE chain_seq = 4`).Scan(&sigJSON); err != nil {
		t.Fatal(err)
	}
	if err := decodeStrict([]byte(raw), &tb.Tombstone); err != nil {
		t.Fatal(err)
	}
	if err := decodeStrict([]byte(sigJSON), &tb.Signature); err != nil {
		t.Fatal(err)
	}
	b, e := tb.Tombstone, c.events[3]
	switch {
	case b.Format != TombstoneFormat || b.ID != id:
		t.Errorf("format/id = %q %q", b.Format, b.ID)
	case b.Operator != erasedOperator:
		t.Errorf("who: operator = %q", b.Operator)
	case b.Reason != erasedReason:
		t.Errorf("why: reason = %q", b.Reason)
	case b.EventID != e.ID || b.ChainSeq != 4 || b.EventType != string(e.Type):
		t.Errorf("what: %q at %d type %q, want %q at 4 type %q", b.EventID, b.ChainSeq, b.EventType, e.ID, e.Type)
	case b.EventHash != e.Hash || b.PrevHash != e.PrevHash:
		t.Errorf("the erased event's hash %q (prev %q), want %q (prev %q)", b.EventHash, b.PrevHash, e.Hash, e.PrevHash)
	case b.OccurredAtMs != e.OccurredAt.UnixMilli() || b.PrincipalID != e.PrincipalID || b.WorkspaceID != e.WorkspaceID || b.SessionID != e.SessionID:
		t.Errorf("the envelope is not the event's: %+v", b)
	case b.ErasedAt != checkpointTime(erasedAt) || tb.Signature.SignedAt != b.ErasedAt:
		t.Errorf("when: erased_at %q, signed_at %q, want %q", b.ErasedAt, tb.Signature.SignedAt, checkpointTime(erasedAt))
	case tb.Signature.KeyID != signerID:
		t.Errorf("signed by %q", tb.Signature.KeyID)
	}
	if err := h.log.VerifyTombstone(ctx, tb, key1); err != nil {
		t.Errorf("VerifyTombstone: %v", err)
	}
	if err := h.log.VerifyTombstone(ctx, tb, ChainKey{ApplicationID: "app-a", TenantID: "tenant-2"}); contracts.CodeOf(err) != contracts.ErrScopeViolation {
		t.Errorf("a tombstone of another chain: %v", err)
	}

	// The chain records the erasure, as an event of its own, and names no payload.
	var rec Event
	var data string
	if err := h.db.QueryRowContext(ctx, `SELECT id, chain_seq, principal_id, workspace_id, data FROM audit_events WHERE event_type = 'audit.erased'`).
		Scan(&rec.ID, &rec.ChainSeq, &rec.PrincipalID, &rec.WorkspaceID, &data); err != nil {
		t.Fatal(err)
	}
	if rec.ID != b.RecordedBy || rec.ID != c.res.RecordID {
		t.Errorf("the tombstone is recorded by %q, the chain holds %q, EraseTx said %q", b.RecordedBy, rec.ID, c.res.RecordID)
	}
	if rec.ChainSeq != 9 || rec.PrincipalID != erasedOperator || rec.WorkspaceID != OperatorWorkspace {
		t.Errorf("the record: position %d principal %q workspace %q", rec.ChainSeq, rec.PrincipalID, rec.WorkspaceID)
	}
	var rd EraseRecord
	if err := json.Unmarshal([]byte(data), &rd); err != nil {
		t.Fatal(err)
	}
	if rd != (EraseRecord{Kind: "tenant", Reason: erasedReason, Operator: erasedOperator, Erased: 3, FirstSeq: 3, LastSeq: 6}) {
		t.Errorf("the record's data = %+v", rd)
	}

	// Nothing of an erased payload survives in any text column of the audit tables.
	for _, q := range []string{
		`SELECT COUNT(*) FROM audit_events WHERE chain_seq IN (3, 4, 6) AND (data LIKE '%SECRET%' OR data LIKE '%"n"%')`,
		`SELECT COUNT(*) FROM audit_tombstones WHERE body LIKE '%SECRET%' OR signature LIKE '%SECRET%'`,
		`SELECT COUNT(*) FROM audit_events WHERE event_type = 'audit.erased' AND data LIKE '%SECRET%'`,
	} {
		if n := h.count(q); n != 0 {
			t.Errorf("%s: %d", q, n)
		}
	}

	// An export carries the tombstone, and says plainly that the hash is not recomputable.
	exp := scope("app-a", "tenant-1", "ws-1", CapabilityExport)
	events, _, err := h.exportLines(exp, ExportOptions{})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	var erased int
	for _, ev := range events {
		if !ev.Erased() {
			if herr := ev.HashValid(); herr != nil {
				t.Errorf("event %d: %v", ev.ChainSeq, herr)
			}
			continue
		}
		erased++
		if string(ev.Data) != ErasedMarker || ev.Tombstone == nil {
			t.Errorf("exported erased event %d: data %s tombstone %v", ev.ChainSeq, ev.Data, ev.Tombstone)
			continue
		}
		if herr := ev.HashValid(); contracts.CodeOf(herr) != contracts.ErrConflict {
			t.Errorf("an erased event claims its hash is checkable: %v", herr)
		}
		if verr := h.log.VerifyTombstone(ctx, *ev.Tombstone, key1); verr != nil || ev.Tombstone.Tombstone.EventHash != ev.Hash {
			t.Errorf("exported tombstone of %d: %v", ev.ChainSeq, verr)
		}
	}
	if erased != 3 {
		t.Errorf("%d exported events were erased, want 3", erased)
	}
}

// The guard decides what stays. A request that names an event a guard keeps is
// refused whole and writes nothing; retention (SkipBlocked) erases the rest.
func TestEraseGuard(t *testing.T) {
	t.Run("refused whole", func(t *testing.T) {
		h := newHarness(t)
		events := h.appendSecrets(s1, 5)
		req := request(events, 1, 2, 3)
		req.Guard = blockSeqs(3)
		_, err := h.log.Erase(ctx, req)
		if !errors.Is(err, ErrErasureBlocked) || contracts.CodeOf(err) != contracts.ErrConflict {
			t.Fatalf("err = %v", err)
		}
		if n := h.count(`SELECT COUNT(*) FROM audit_tombstones`) + h.count(`SELECT COUNT(*) FROM audit_events WHERE data = ?`, ErasedMarker) +
			h.count(`SELECT COUNT(*) FROM audit_events WHERE event_type = 'audit.erased'`); n != 0 {
			t.Errorf("a refused erasure wrote %d rows", n)
		}
		req.Guard = allowAll
		if res, err := h.log.Erase(ctx, req); err != nil || res.Erased != 3 {
			t.Errorf("the same request once nothing depends on the events: %+v %v", res, err)
		}
	})
	t.Run("skip blocked", func(t *testing.T) {
		h := newHarness(t)
		events := h.appendSecrets(s1, 5)
		req := request(events, 1, 2, 3)
		req.Guard, req.SkipBlocked = blockSeqs(3), true
		res, err := h.log.Erase(ctx, req)
		if err != nil || res.Erased != 2 || res.Blocked != 1 {
			t.Fatalf("result = %+v, err %v", res, err)
		}
		if n := h.count(`SELECT COUNT(*) FROM audit_events WHERE chain_seq = 3 AND data LIKE '%SECRET-3%'`); n != 1 {
			t.Error("the blocked event lost its payload")
		}
		h.mustVerify(key1)
	})
	t.Run("a guard that fails refuses", func(t *testing.T) {
		h := newHarness(t)
		events := h.appendSecrets(s1, 3)
		req := request(events, 1)
		req.Guard = func(context.Context, store.Reader, ChainKey) (func(Event) string, error) {
			return nil, contracts.New(contracts.ErrUnavailable, "cannot tell")
		}
		if _, err := h.log.Erase(ctx, req); contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Errorf("err = %v", err)
		}
		if n := h.count(`SELECT COUNT(*) FROM audit_tombstones`); n != 0 {
			t.Error("a tombstone was written")
		}
	})
}

func TestEraseRefusals(t *testing.T) {
	h := newHarness(t)
	events := h.appendSecrets(s1, 4)
	other := h.appendSecrets(scope("app-a", "tenant-2", "ws-1"), 2)
	good := request(events, 1)

	t.Run("no signing key means no erasure", func(t *testing.T) {
		unsigned, err := New(h.db, Options{Clock: h.clk, Registry: NewCoreRegistry(), Verifier: trust.NewVerifier(h.trust, h.clk)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := unsigned.Erase(ctx, good); contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Errorf("err = %v", err)
		}
		if n := h.count(`SELECT COUNT(*) FROM audit_events WHERE data = ?`, ErasedMarker); n != 0 {
			t.Error("a payload was erased with no tombstone")
		}
	})

	bad := map[string]func(*EraseRequest){
		"no guard":                  func(r *EraseRequest) { r.Guard = nil },
		"no events":                 func(r *EraseRequest) { r.EventIDs = nil },
		"too many events":           func(r *EraseRequest) { r.EventIDs = make([]string, MaxEraseBatch+1) },
		"an event of another chain": func(r *EraseRequest) { r.EventIDs = []string{other[0].ID} },
		"an unknown event":          func(r *EraseRequest) { r.EventIDs = []string{"aud_nothing"} },
		"the platform chain":        func(r *EraseRequest) { r.Chain = ChainKey{ApplicationID: trust.PlatformApplicationID} },
		"an unknown kind":           func(r *EraseRequest) { r.Kind = "because" },
		"no reason":                 func(r *EraseRequest) { r.Reason = "  " },
		"a reason with a newline":   func(r *EraseRequest) { r.Reason = "line\nbreak" },
		"a long reason":             func(r *EraseRequest) { r.Reason = strings.Repeat("x", MaxReasonBytes+1) },
		"no operator":               func(r *EraseRequest) { r.Operator = "" },
		"an operator with a space":  func(r *EraseRequest) { r.Operator = "ops alice" },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			req := good
			req.EventIDs = append([]string(nil), good.EventIDs...)
			mutate(&req)
			_, err := h.log.Erase(ctx, req)
			if contracts.CodeOf(err) != contracts.ErrValidationFailed && contracts.CodeOf(err) != contracts.ErrScopeMissing && contracts.CodeOf(err) != contracts.ErrScopeViolation {
				t.Errorf("err = %v", err)
			}
			if n := h.count(`SELECT COUNT(*) FROM audit_tombstones`); n != 0 {
				t.Errorf("a refused request wrote %d tombstones", n)
			}
		})
	}

	t.Run("the record of an erasure is never erased", func(t *testing.T) {
		res, err := h.log.Erase(ctx, good)
		if err != nil || res.RecordID == "" {
			t.Fatalf("%+v %v", res, err)
		}
		req := good
		req.EventIDs = []string{res.RecordID}
		if _, err := h.log.Erase(ctx, req); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("an event cannot carry the erased marker from the start", func(t *testing.T) {
		_, err := h.log.Append(ctx, s1, NewEvent{Type: EventSettingsChanged, Data: json.RawMessage(ErasedMarker)})
		if contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("err = %v", err)
		}
	})
}

// EraseTx decides about the named events in request order, however they are read: a duplicate
// counts once, an event already erased is counted as such, a request larger than one read is
// handled whole, and the first event that cannot be erased is the one the refusal names.
func TestEraseDecidesInRequestOrder(t *testing.T) {
	t.Run("a large request with duplicates, erased and blocked events", func(t *testing.T) {
		h := newHarness(t)
		events := h.appendN(s1, 530)
		if _, err := h.log.Erase(ctx, request(events, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)); err != nil {
			t.Fatal(err)
		}
		req := request(events)
		for _, e := range events {
			req.EventIDs = append(req.EventIDs, e.ID)
		}
		req.EventIDs = append(req.EventIDs, events[5].ID, events[300].ID)
		req.Guard, req.SkipBlocked = blockSeqs(20, 512), true
		res, err := h.log.Erase(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if res.Erased != 518 || res.AlreadyErased != 10 || res.Blocked != 2 || res.FirstSeq != 11 || res.LastSeq != 530 || res.RecordID == "" {
			t.Errorf("result = %+v", res)
		}
		if n := h.count(`SELECT COUNT(*) FROM audit_tombstones`); n != 528 {
			t.Errorf("%d tombstones, want 528", n)
		}
		h.mustVerify(key1)
	})
	t.Run("the first event that cannot be erased decides", func(t *testing.T) {
		h := newHarness(t)
		events := h.appendN(s1, 4)
		h.dropTriggers()
		h.exec(`UPDATE audit_events SET data = '{"n":99}' WHERE tenant_id = 'tenant-1' AND chain_seq = 3`)
		tampered, missing := request(events, 2, 3), request(events, 2)
		tampered.EventIDs = append(tampered.EventIDs, "aud_nothing")
		missing.EventIDs = append(missing.EventIDs, "aud_nothing", events[2].ID)
		if _, err := h.log.Erase(ctx, tampered); !errors.Is(err, ErrChainBroken) {
			t.Errorf("a tampered event before a missing one: %v, want ErrChainBroken", err)
		}
		if _, err := h.log.Erase(ctx, missing); contracts.CodeOf(err) != contracts.ErrValidationFailed || errors.Is(err, ErrChainBroken) {
			t.Errorf("a missing event before a tampered one: %v, want validation_failed", err)
		}
		if n := h.count(`SELECT COUNT(*) FROM audit_tombstones`); n != 0 {
			t.Errorf("a refused erasure wrote %d tombstones", n)
		}
	})
}

// The database itself refuses to turn an event into an erased one unless its
// signed tombstone exists and nothing but the payload changes, and refuses to
// touch a tombstone at all.
func TestTombstoneTriggers(t *testing.T) {
	h := newHarness(t)
	events := h.appendSecrets(s1, 3)
	refuses := func(query string, args ...any) {
		t.Helper()
		if _, err := h.db.ExecContext(ctx, query, args...); err == nil {
			t.Errorf("%s was allowed", query)
		}
	}
	refuses(`UPDATE audit_events SET data = ? WHERE chain_seq = 2`, ErasedMarker) // no tombstone
	refuses(`UPDATE audit_events SET data = '{"n":1}' WHERE chain_seq = 2`)       // not the marker
	res, err := h.log.Erase(ctx, request(events, 2))
	if err != nil || res.Erased != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	refuses(`UPDATE audit_events SET data = '{"n":1}' WHERE chain_seq = 2`)                                      // an erased payload cannot be put back
	refuses(`UPDATE audit_events SET data = ? WHERE chain_seq = 1`, ErasedMarker)                                // a tombstone for another event is no licence
	refuses(`UPDATE audit_events SET data = ?, occurred_at = occurred_at + 1 WHERE chain_seq = 3`, ErasedMarker) // more than the payload
	refuses(`UPDATE audit_tombstones SET body = '{}'`)
	refuses(`DELETE FROM audit_tombstones`)
	refuses(`DELETE FROM audit_events`)
	h.mustVerify(key1)
}

func TestSelectEvents(t *testing.T) {
	h := newHarness(t)
	alice, _ := ForIDs("app-a", "tenant-1", "ws-1", "usr_alice", "")
	bob, _ := ForIDs("app-a", "tenant-1", "ws-1", "usr_bob", "")
	var ids []string
	add := func(loc Locator, data map[string]any) {
		h.clk.Advance(time.Hour)
		e, err := h.log.AppendAt(ctx, loc, NewEvent{Type: EventSettingsChanged, Data: data})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, e.ID)
	}
	add(alice, map[string]any{"n": 1})                       // 1: by alice
	add(bob, map[string]any{"n": 2})                         // 2: by bob
	add(bob, map[string]any{"n": 3, "subject": "usr_alice"}) // 3: names alice in its payload
	add(bob, map[string]any{"n": 4, "note": "usr_alicia"})   // 4: names someone else
	add(bob, map[string]any{"n": 5})                         // 5

	sel := func(s Selector) []int64 {
		t.Helper()
		if s.Limit == 0 {
			s.Limit = 100
		}
		refs, err := SelectEvents(ctx, h.db, key1, s)
		if err != nil {
			t.Fatalf("SelectEvents: %v", err)
		}
		var out []int64
		for _, r := range refs {
			out = append(out, r.ChainSeq)
		}
		return out
	}
	eq := func(got []int64, want ...int64) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	}
	eq(sel(Selector{Principal: "usr_alice"}), 1, 3)
	eq(sel(Selector{All: true}), 1, 2, 3, 4, 5)
	eq(sel(Selector{All: true, Limit: 2}), 1, 2)
	eq(sel(Selector{All: true, AfterSeq: 2, Limit: 2}), 3, 4)
	cutoff := t0.Add(3*time.Hour + time.Second) // after the third event
	eq(sel(Selector{Before: cutoff}), 1, 2, 3)

	// Erased events and records of erasure are not selected again.
	if _, err := h.log.Erase(ctx, EraseRequest{Chain: key1, EventIDs: ids[:3], Kind: "retention", Reason: "retention", Operator: "alixai.retention", Guard: allowAll}); err != nil {
		t.Fatal(err)
	}
	eq(sel(Selector{All: true}), 4, 5)
	eq(sel(Selector{Principal: "usr_alice"}))

	for name, s := range map[string]Selector{
		"nothing":  {Limit: 1},
		"two":      {All: true, Principal: "usr_alice", Limit: 1},
		"no limit": {All: true},
		"too many": {All: true, Limit: MaxEraseBatch + 1},
	} {
		if _, err := SelectEvents(ctx, h.db, key1, s); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// An erasure on the application chain (no tenant) records itself there.
func TestEraseOnAnApplicationChain(t *testing.T) {
	h := newHarness(t)
	loc, _ := ForApplication("app-a", "ops-bob")
	var ids []string
	for i := 0; i < 3; i++ {
		h.clk.Advance(time.Second)
		e, err := h.log.AppendAt(ctx, loc, NewEvent{Type: EventPackageInstalled, Data: map[string]any{"version": fmt.Sprint(i), "note": secretNote(i)}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, e.ID)
	}
	appKey := ChainKey{ApplicationID: "app-a"}
	res, err := h.log.Erase(ctx, EraseRequest{Chain: appKey, EventIDs: ids, Kind: "retention", Reason: "retention", Operator: "alixai.retention", Guard: allowAll})
	if err != nil || res.Erased != 3 {
		t.Fatalf("%+v %v", res, err)
	}
	rep := h.mustVerify(appKey)
	if rep.Erased != 3 || rep.Events != 4 {
		t.Errorf("report = %+v", rep)
	}
}
