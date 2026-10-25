package auditsec

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/trust"
)

const (
	tenant   = "tenant-1"
	chainLen = 12
)

// heldHead is how a signed chain head travels outside the database: as the JSON of an
// audit.Checkpoint, in a report or a backup manifest. It is read back strictly, as a reader of
// such a document would.
func heldHead(t *testing.T, w *world, key audit.ChainKey) (audit.Checkpoint, []byte) {
	t.Helper()
	cp, err := w.log.ExportCheckpoint(bg, key)
	if err != nil {
		t.Fatalf("ExportCheckpoint: %v", err)
	}
	raw, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	return decodeHead(t, raw), raw
}

func decodeHead(t *testing.T, raw []byte) audit.Checkpoint {
	t.Helper()
	var cp audit.Checkpoint
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cp); err != nil {
		t.Fatalf("the exported head does not decode: %v\n%s", err, raw)
	}
	return cp
}

// headedWorld is a chain of chainLen events whose newest event is covered by a signed head that
// the test then holds outside the database, with the database as it stood.
func headedWorld(t *testing.T) (*world, audit.ChainKey, audit.Checkpoint) {
	t.Helper()
	w := newWorld(t)
	key := chainOf(tenant)
	w.appendN(tenant, 1, chainLen)
	w.clk.Advance(time.Second)
	if _, err := w.log.Checkpoint(bg, key); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	held, _ := heldHead(t, w, key)
	if held.Checkpoint.HeadSeq != chainLen {
		t.Fatalf("the held head covers position %d, want %d", held.Checkpoint.HeadSeq, chainLen)
	}
	rep, err := w.log.VerifyChain(bg, key, audit.VerifyOptions{Expected: &held, RequireAnchor: true})
	if err != nil || !rep.Anchored || rep.AnchorSeq != chainLen || rep.Events != chainLen {
		t.Fatalf("the untouched chain does not verify against its own head: %+v, %v", rep, err)
	}
	return w, key, held
}

// SEC-086: someone with write access to the database removes the newest events, or rewrites
// the whole chain with fresh hashes. A hash chain alone cannot see either; a chain head signed
// with the platform key and held outside the database can. Each attack is run against the real
// audit log, and the verdict is read from audit.VerifyChain given the held head.
//
// Backlog gap G6 (docs/security/test-backlog.md section 7).
func TestSecAuditTailTruncationDetected(t *testing.T) {
	t.Run("control: the intact chain verifies against its head, before and after it grows", func(t *testing.T) {
		w, key, held := headedWorld(t)
		w.appendN(tenant, chainLen+1, 3)
		rep, err := w.log.VerifyChain(bg, key, audit.VerifyOptions{Expected: &held, RequireAnchor: true})
		if err != nil {
			t.Fatalf("a chain that extends its held head is refused: %v (%+v)", err, rep.Problems)
		}
		if !rep.Anchored || rep.AnchorSeq != chainLen || rep.EventsAfterAnchor != 3 || rep.Events != chainLen+3 {
			t.Errorf("report = %+v", rep)
		}
	})

	t.Run("control: the database itself refuses a deletion, until the attacker removes its triggers", func(t *testing.T) {
		w, _, _ := headedWorld(t)
		if _, err := w.db.ExecContext(bg, `DELETE FROM audit_events WHERE chain_seq = ?`, chainLen); err == nil {
			t.Fatal("an audit event was deleted with the append-only triggers in place")
		}
		w.dropAppendOnlyTriggers()
		w.exec(`DELETE FROM audit_events WHERE chain_seq = ?`, chainLen)
		if n := w.count(`SELECT COUNT(*) FROM audit_events`); n != chainLen-1 {
			t.Fatalf("%d events after the deletion, want %d: the attacker's step did not work, so the tests below would prove nothing", n, chainLen-1)
		}
	})

	for _, removed := range []int{1, 2, 5, chainLen} {
		t.Run("the newest events are removed: "+strconv.Itoa(removed), func(t *testing.T) {
			w, key, held := headedWorld(t)
			w.dropAppendOnlyTriggers()
			// The attacker also removes the checkpoints the database holds: only the head held
			// outside the database is left to see the removal.
			w.exec(`DELETE FROM audit_checkpoints`)
			w.exec(`DELETE FROM audit_events WHERE application_id = ? AND tenant_id = ? AND chain_seq > ?`, key.ApplicationID, key.TenantID, chainLen-removed)

			// Control: the truncated chain is internally consistent, so without the held head
			// nothing is wrong with it. This is the attack G6 is about.
			if rep, err := w.log.VerifyChain(bg, key, audit.VerifyOptions{}); err != nil {
				t.Fatalf("the truncated chain should look intact on its own: %v (%+v)", err, rep.Problems)
			}

			rep, err := w.log.VerifyChain(bg, key, audit.VerifyOptions{Expected: &held})
			rep = mustBeBroken(t, rep, err)
			if !problemAt(rep, audit.ProblemTruncated, chainLen) {
				t.Errorf("the removal of the newest %d event(s) is not reported as a truncation at the held head: %+v", removed, rep.Problems)
			}
			if rep.Anchored {
				t.Errorf("a truncated chain must not count as anchored: %+v", rep)
			}
			if rep.Events != int64(chainLen-removed) {
				t.Errorf("the report counts %d events, want %d", rep.Events, chainLen-removed)
			}
		})
	}

	t.Run("the whole chain is recomputed after a change of history", func(t *testing.T) {
		w, key, held := headedWorld(t)
		w.dropAppendOnlyTriggers()
		w.exec(`DELETE FROM audit_checkpoints`)
		w.rewriteChain(key, func(ev []audit.Event) []audit.Event {
			ev[2].Data = json.RawMessage(`{"n":"rewritten"}`) // position 3 now says something else
			return ev
		})

		if rep, err := w.log.VerifyChain(bg, key, audit.VerifyOptions{}); err != nil {
			t.Fatalf("the rewritten chain should look intact on its own (control): %v (%+v)", err, rep.Problems)
		}
		rep, err := w.log.VerifyChain(bg, key, audit.VerifyOptions{Expected: &held})
		rep = mustBeBroken(t, rep, err)
		if !problemAt(rep, audit.ProblemRewritten, chainLen) {
			t.Errorf("the rewrite is not reported at the held head: %+v", rep.Problems)
		}
		// Every link and hash is consistent: only the head can have seen it.
		if problemAt(rep, audit.ProblemHashMismatch, 0) || problemAt(rep, audit.ProblemLinkBroken, 0) {
			t.Errorf("the recomputed chain was reported as inconsistent, so the attack was not a full rewrite: %+v", rep.Problems)
		}
	})

	t.Run("the chain is rewritten, shortened and the attacker signs a head of their own", func(t *testing.T) {
		w, key, held := headedWorld(t)
		w.dropAppendOnlyTriggers()
		w.exec(`DELETE FROM audit_checkpoints`)
		w.rewriteChain(key, func(ev []audit.Event) []audit.Event {
			ev[5].Data = json.RawMessage(`{"n":"rewritten"}`)
			return ev[:chainLen-4]
		})
		// The attacker registers their own key as a platform checkpoint key (they can write
		// the trust store too) and signs a head that matches the forged chain.
		evilPub, evilPriv, _ := ed25519.GenerateKey(rand.Reader)
		if _, err := w.trust.RegisterKey(bg, trust.PlatformApplicationID, "attacker-signer", evilPub, "not the platform key", trust.WithPurpose(trust.PurposeAuditCheckpoint)); err != nil {
			t.Fatal(err)
		}
		forged, err := audit.New(w.db, audit.Options{
			Clock: w.clk, Signing: &trust.SigningKey{KeyID: "attacker-signer", Key: evilPriv},
			Verifier: trust.NewVerifier(w.trust, w.clk),
		})
		if err != nil {
			t.Fatal(err)
		}
		w.clk.Advance(time.Second)
		if _, err := forged.Checkpoint(bg, key); err != nil {
			t.Fatalf("the attacker cannot plant their head, so this test checks nothing: %v", err)
		}

		// Control: the chain with the attacker's own head verifies, anchored: the database alone
		// is defeated by someone who can write it.
		rep, err := w.log.VerifyChain(bg, key, audit.VerifyOptions{RequireAnchor: true})
		if err != nil || !rep.Anchored {
			t.Fatalf("the forgery should satisfy the stored head (control): %v %+v", err, rep)
		}
		// The head held elsewhere, signed with the real platform key, still exposes it.
		rep, err = w.log.VerifyChain(bg, key, audit.VerifyOptions{Expected: &held, RequireAnchor: true})
		rep = mustBeBroken(t, rep, err)
		if !problemAt(rep, audit.ProblemTruncated, chainLen) {
			t.Errorf("the forgery is not reported against the held head: %+v", rep.Problems)
		}
	})

	t.Run("the chain is rewritten and brought back to its old length", func(t *testing.T) {
		w, key, held := headedWorld(t)
		w.dropAppendOnlyTriggers()
		w.exec(`DELETE FROM audit_checkpoints`)
		w.rewriteChain(key, func(ev []audit.Event) []audit.Event {
			ev[chainLen-1].Data = json.RawMessage(`{"n":"the last event, replaced"}`)
			return ev
		})
		rep, err := w.log.VerifyChain(bg, key, audit.VerifyOptions{Expected: &held})
		rep = mustBeBroken(t, rep, err)
		if !problemAt(rep, audit.ProblemRewritten, chainLen) {
			t.Errorf("a same-length rewrite is not reported: %+v", rep.Problems)
		}
	})

	t.Run("a head that is not the platform's is not believed", func(t *testing.T) {
		w, key, _ := headedWorld(t)
		w.dropAppendOnlyTriggers()
		w.exec(`DELETE FROM audit_events WHERE application_id = ? AND tenant_id = ? AND chain_seq > ?`, key.ApplicationID, key.TenantID, chainLen-3)
		w.exec(`DELETE FROM audit_checkpoints`)

		// A head signed with a key the trust store has never heard of, matching the truncated
		// chain, handed in as the expected one.
		_, evilPriv, _ := ed25519.GenerateKey(rand.Reader)
		evil, err := audit.New(w.db, audit.Options{
			Clock: w.clk, Signing: &trust.SigningKey{KeyID: "unregistered-signer", Key: evilPriv},
			Verifier: trust.NewVerifier(w.trust, w.clk),
		})
		if err != nil {
			t.Fatal(err)
		}
		cp, err := evil.Checkpoint(bg, key)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := w.log.VerifyChain(bg, key, audit.VerifyOptions{Expected: &cp, RequireAnchor: true})
		rep = mustBeBroken(t, rep, err)
		if !problemAt(rep, audit.ProblemCheckpointInvalid, 0) || rep.Anchored {
			t.Errorf("a head signed by an unknown key was trusted: %+v", rep)
		}
	})

	t.Run("a held head that was altered in transit is not believed", func(t *testing.T) {
		w, key, _ := headedWorld(t)
		_, raw := heldHead(t, w, key)
		altered := decodeHead(t, []byte(strings.Replace(string(raw), `"head_seq":12`, `"head_seq":4`, 1)))
		if altered.Checkpoint.HeadSeq != 4 {
			t.Fatalf("the test did not alter the head: %+v", altered.Checkpoint)
		}
		rep, err := w.log.VerifyChain(bg, key, audit.VerifyOptions{Expected: &altered})
		rep = mustBeBroken(t, rep, err)
		if !problemAt(rep, audit.ProblemCheckpointInvalid, 0) {
			t.Errorf("a head with a changed position was trusted: %+v", rep.Problems)
		}
		if err := w.log.VerifyCheckpoint(bg, altered, key); err == nil {
			t.Error("VerifyCheckpoint accepted a head whose body was changed after signing")
		}
	})

	t.Run("a head of another tenant's chain is not replayed onto this one", func(t *testing.T) {
		w, key, _ := headedWorld(t)
		other := chainOf("tenant-2")
		w.appendN("tenant-2", 1, chainLen)
		if _, err := w.log.Checkpoint(bg, other); err != nil {
			t.Fatal(err)
		}
		foreign, _ := heldHead(t, w, other)
		rep, err := w.log.VerifyChain(bg, key, audit.VerifyOptions{Expected: &foreign, RequireAnchor: true})
		rep = mustBeBroken(t, rep, err)
		if !problemAt(rep, audit.ProblemCheckpointInvalid, 0) {
			t.Errorf("another chain's head was accepted: %+v", rep.Problems)
		}
	})

	t.Run("a chain with events and no verified head fails when an anchor is required", func(t *testing.T) {
		w := newWorld(t)
		key := chainOf(tenant)
		w.appendN(tenant, 1, 5)
		rep, err := w.log.VerifyChain(bg, key, audit.VerifyOptions{RequireAnchor: true})
		rep = mustBeBroken(t, rep, err)
		if !problemAt(rep, audit.ProblemUnanchored, 0) {
			t.Errorf("an unanchored chain passed RequireAnchor: %+v", rep.Problems)
		}
	})

	t.Run("the platform signs heads by itself: every N events and by the sweep", func(t *testing.T) {
		w := newWorld(t, func(o *audit.Options) { o.CheckpointEvery = 5 })
		key := chainOf(tenant)
		w.appendN(tenant, 1, 12)
		cp, err := w.log.ExportCheckpoint(bg, key)
		if err != nil || cp.Checkpoint.HeadSeq != 10 {
			t.Fatalf("after 12 appends with CheckpointEvery=5 the newest head covers %d (%v), want 10", cp.Checkpoint.HeadSeq, err)
		}
		n, err := w.log.CheckpointAll(bg)
		if err != nil || n != 1 {
			t.Fatalf("the sweep signed %d heads (%v), want 1", n, err)
		}
		cp, _ = w.log.ExportCheckpoint(bg, key)
		rep, err := w.log.VerifyChain(bg, key, audit.VerifyOptions{Expected: &cp, RequireAnchor: true})
		if err != nil || rep.EventsAfterAnchor != 0 {
			t.Errorf("after the sweep nothing should lie beyond the newest head: %v %+v", err, rep)
		}
	})
}
