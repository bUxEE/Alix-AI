package audit

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

var s1 = scope("app-a", "tenant-1", "ws-1")
var key1 = ChainKey{"app-a", "tenant-1"}

// setupAnchored makes a chain of 10 events with a checkpoint after the 6th
// and another after the 10th, and returns the harness and both checkpoints.
func setupAnchored(t *testing.T) (*harness, Checkpoint, Checkpoint) {
	t.Helper()
	h := newHarness(t)
	h.appendN(s1, 6)
	cp6, err := h.log.Checkpoint(ctx, key1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 7; i <= 10; i++ {
		h.clk.Advance(time.Second)
		if _, err := h.log.Append(ctx, s1, settings(i)); err != nil {
			t.Fatal(err)
		}
	}
	h.clk.Advance(time.Second)
	cp10, err := h.log.Checkpoint(ctx, key1)
	if err != nil {
		t.Fatal(err)
	}
	if cp6.Checkpoint.HeadSeq != 6 || cp10.Checkpoint.HeadSeq != 10 {
		t.Fatalf("checkpoint heads %d and %d", cp6.Checkpoint.HeadSeq, cp10.Checkpoint.HeadSeq)
	}
	rep := h.mustVerify(key1, VerifyOptions{RequireAnchor: true})
	if !rep.Anchored || rep.AnchorSeq != 10 || rep.EventsAfterAnchor != 0 {
		t.Fatalf("baseline report: %+v", rep)
	}
	return h, cp6, cp10
}

// SEC-086: removing the newest events, or rewriting the whole chain with
// recomputed hashes, passes a plain hash chain and is caught by the signed
// checkpoint.
func TestSecAuditTailTruncationDetected(t *testing.T) {
	t.Run("a plain hash chain cannot see it (the reason checkpoints exist)", func(t *testing.T) {
		h := newHarness(t)
		h.appendN(s1, 10)
		h.dropTriggers()
		h.exec(`DELETE FROM audit_events WHERE tenant_id = 'tenant-1' AND chain_seq > 6`)
		rep, err := h.verify(key1)
		if err != nil {
			t.Fatalf("a truncated, unanchored chain is expected to pass: %v", err)
		}
		if rep.Anchored || rep.Events != 6 {
			t.Errorf("report = %+v", rep)
		}
		// With RequireAnchor the missing anchor is itself a failure.
		if rep := h.mustBreak(key1, VerifyOptions{RequireAnchor: true}); !has(rep, ProblemUnanchored, 0) {
			t.Errorf("RequireAnchor: %+v", rep.Problems)
		}
	})

	t.Run("removing the last N events below the checkpoint count", func(t *testing.T) {
		h, _, _ := setupAnchored(t)
		h.dropTriggers()
		h.exec(`DELETE FROM audit_events WHERE tenant_id = 'tenant-1' AND chain_seq > 4`) // below 6 and below 10
		rep := h.mustBreak(key1)
		if !has(rep, ProblemTruncated, 10) {
			t.Errorf("truncation below the newest checkpoint not reported: %+v", rep.Problems)
		}
		if rep.Anchored {
			t.Errorf("a truncated chain must not count as anchored: %+v", rep)
		}
	})

	t.Run("removing the last event only", func(t *testing.T) {
		h, _, _ := setupAnchored(t)
		h.dropTriggers()
		h.exec(`DELETE FROM audit_events WHERE tenant_id = 'tenant-1' AND chain_seq = 10`)
		if rep := h.mustBreak(key1); !has(rep, ProblemTruncated, 10) {
			t.Errorf("%+v", rep.Problems)
		}
	})

	t.Run("a full rewrite with recomputed hashes", func(t *testing.T) {
		h, _, _ := setupAnchored(t)
		h.dropTriggers()
		h.rewriteChain(key1, func(ev []Event) {
			ev[2].Data = json.RawMessage(`{"n":"rewritten"}`) // history changed at position 3
		})
		// The rewritten chain is internally consistent: without the checkpoint
		// nothing is wrong with it.
		h.exec(`DELETE FROM audit_checkpoints`)
		if _, err := h.verify(key1); err != nil {
			t.Fatalf("the rewritten chain should look intact on its own: %v", err)
		}
	})

	t.Run("a full rewrite is caught by the stored checkpoint", func(t *testing.T) {
		h, _, _ := setupAnchored(t)
		h.dropTriggers()
		h.rewriteChain(key1, func(ev []Event) {
			ev[2].Data = json.RawMessage(`{"n":"rewritten"}`)
		})
		rep := h.mustBreak(key1)
		if !has(rep, ProblemRewritten, 10) {
			t.Errorf("rewrite not reported at the checkpoint position: %+v", rep.Problems)
		}
		if has(rep, ProblemHashMismatch, 0) || has(rep, ProblemLinkBroken, 0) {
			t.Errorf("the rewrite was complete, only the checkpoint can see it: %+v", rep.Problems)
		}
	})

	t.Run("removing the newest checkpoint too falls back to the older one, an exported copy still sees it", func(t *testing.T) {
		h, cp6, cp10 := setupAnchored(t)
		// Held outside the database, as in an integration-audit report.
		external := cp10
		h.dropTriggers()
		h.exec(`DELETE FROM audit_checkpoints WHERE head_seq = 10`)
		h.exec(`DELETE FROM audit_events WHERE tenant_id = 'tenant-1' AND chain_seq > 8`)

		// Stored checkpoints alone: the older one (6) is still satisfied. This is
		// the stated limit of checkpoints that live in the same database.
		rep, err := h.verify(key1)
		if err != nil {
			t.Fatalf("with the newest checkpoint gone the truncation to 8 is invisible to the stored ones: %v", err)
		}
		if !rep.Anchored || rep.AnchorSeq != cp6.Checkpoint.HeadSeq || rep.EventsAfterAnchor != 2 {
			t.Errorf("report = %+v", rep)
		}
		// The copy held elsewhere does not depend on the database.
		rep = h.mustBreak(key1, VerifyOptions{Expected: &external})
		if !has(rep, ProblemTruncated, 10) {
			t.Errorf("the expected checkpoint did not catch the truncation: %+v", rep.Problems)
		}
	})

	t.Run("an expected checkpoint the chain extends is satisfied", func(t *testing.T) {
		h, _, cp10 := setupAnchored(t)
		for i := 0; i < 3; i++ {
			h.clk.Advance(time.Second)
			if _, err := h.log.Append(ctx, s1, settings(100+i)); err != nil {
				t.Fatal(err)
			}
		}
		rep := h.mustVerify(key1, VerifyOptions{Expected: &cp10, RequireAnchor: true})
		if !rep.Anchored || rep.AnchorSeq != 10 || rep.EventsAfterAnchor != 3 {
			t.Errorf("report = %+v", rep)
		}
	})

	t.Run("events after the newest checkpoint are not protected (stated limit)", func(t *testing.T) {
		h, _, _ := setupAnchored(t)
		for i := 0; i < 3; i++ {
			h.clk.Advance(time.Second)
			if _, err := h.log.Append(ctx, s1, settings(100+i)); err != nil {
				t.Fatal(err)
			}
		}
		h.dropTriggers()
		h.exec(`DELETE FROM audit_events WHERE tenant_id = 'tenant-1' AND chain_seq > 10`)
		rep := h.mustVerify(key1)
		if rep.EventsAfterAnchor != 0 || rep.HeadSeq != 10 {
			t.Errorf("report = %+v", rep)
		}
	})

	t.Run("every event removed", func(t *testing.T) {
		h, _, _ := setupAnchored(t)
		h.dropTriggers()
		h.exec(`DELETE FROM audit_events WHERE tenant_id = 'tenant-1'`)
		if rep := h.mustBreak(key1); !has(rep, ProblemTruncated, 10) {
			t.Errorf("%+v", rep.Problems)
		}
	})
}

// SEC-086: a checkpoint is only believed if a platform key from the trust
// store signed it, for this chain, and it has not expired.
func TestSecCheckpointForgeryRejected(t *testing.T) {
	t.Run("signed by a key that is not registered", func(t *testing.T) {
		h := newHarness(t)
		h.appendN(s1, 6)
		h.dropTriggers()
		// The attacker truncates, then plants a checkpoint of their own that
		// matches the truncated chain.
		h.exec(`DELETE FROM audit_events WHERE tenant_id = 'tenant-1' AND chain_seq > 3`)
		rep0 := h.mustVerify(key1)
		_, evilPriv, _ := ed25519.GenerateKey(rand.Reader)
		forged := forgeCheckpoint(t, key1, rep0.HeadSeq, rep0.HeadHash, "attacker-key", evilPriv, h.clk.Now())
		h.insertCheckpoint(forged)
		rep := h.mustBreak(key1, VerifyOptions{RequireAnchor: true})
		if !has(rep, ProblemCheckpointInvalid, 0) || rep.Anchored {
			t.Errorf("a checkpoint from an unknown key was trusted: %+v", rep)
		}
	})

	t.Run("signed by the platform key id but with another private key", func(t *testing.T) {
		h := newHarness(t)
		h.appendN(s1, 3)
		head := h.mustVerify(key1)
		_, evilPriv, _ := ed25519.GenerateKey(rand.Reader)
		forged := forgeCheckpoint(t, key1, head.HeadSeq, head.HeadHash, signerID, evilPriv, h.clk.Now())
		if err := h.log.VerifyCheckpoint(ctx, forged, key1); contracts.CodeOf(err) != contracts.ErrUnauthenticated {
			t.Errorf("VerifyCheckpoint = %v, want unauthenticated", err)
		}
	})

	t.Run("a checkpoint of another tenant is not replayed onto this one", func(t *testing.T) {
		h := newHarness(t)
		h.appendN(s1, 4)
		h.appendN(scope("app-a", "tenant-2", "ws-1"), 4)
		cp1, err := h.log.Checkpoint(ctx, key1)
		if err != nil {
			t.Fatal(err)
		}
		key2 := ChainKey{"app-a", "tenant-2"}
		// Plant tenant-1's genuine, validly signed checkpoint in tenant-2's slot.
		h.insertCheckpointRow(key2, cp1)
		rep := h.mustBreak(key2, VerifyOptions{RequireAnchor: true})
		if !has(rep, ProblemCheckpointInvalid, 0) {
			t.Errorf("replayed checkpoint: %+v", rep.Problems)
		}
		if err := h.log.VerifyCheckpoint(ctx, cp1, key2); contracts.CodeOf(err) != contracts.ErrScopeViolation {
			t.Errorf("VerifyCheckpoint(other chain) = %v, want scope_violation", err)
		}
		// Same chain id, different application.
		if err := h.log.VerifyCheckpoint(ctx, cp1, ChainKey{"app-b", "tenant-1"}); err == nil {
			t.Error("a checkpoint verified for another application")
		}
	})

	t.Run("a changed body invalidates the signature", func(t *testing.T) {
		h := newHarness(t)
		h.appendN(s1, 4)
		cp, err := h.log.Checkpoint(ctx, key1)
		if err != nil {
			t.Fatal(err)
		}
		cp.Checkpoint.HeadSeq = 3
		if err := h.log.VerifyCheckpoint(ctx, cp, key1); contracts.CodeOf(err) != contracts.ErrUnauthenticated {
			t.Errorf("changed head: %v", err)
		}
	})

	t.Run("signature timestamps must agree with the signed body", func(t *testing.T) {
		h := newHarness(t)
		h.appendN(s1, 4)
		cp, err := h.log.Checkpoint(ctx, key1)
		if err != nil {
			t.Fatal(err)
		}
		cp.Signature.ExpiresAt = "2099-01-01T00:00:00Z" // not covered by the signature itself
		if err := h.log.VerifyCheckpoint(ctx, cp, key1); err == nil {
			t.Error("a checkpoint with a rewritten expiry verified")
		}
	})

	t.Run("a revoked signing key voids its checkpoints", func(t *testing.T) {
		h, _, _ := setupAnchored(t)
		if err := h.trust.RevokeKey(ctx, trust.PlatformApplicationID, signerID, "test"); err != nil {
			t.Fatal(err)
		}
		if rep := h.mustBreak(key1); !has(rep, ProblemCheckpointInvalid, 0) {
			t.Errorf("%+v", rep.Problems)
		}
	})

	t.Run("a key registered for a host application is not a platform key", func(t *testing.T) {
		h := newHarness(t)
		h.appendN(s1, 3)
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		if _, err := h.trust.RegisterKey(ctx, "app-a", "app-a-publisher", pub, "host key"); err != nil {
			t.Fatal(err)
		}
		head := h.mustVerify(key1)
		forged := forgeCheckpoint(t, key1, head.HeadSeq, head.HeadHash, "app-a-publisher", priv, h.clk.Now())
		if err := h.log.VerifyCheckpoint(ctx, forged, key1); err == nil {
			t.Error("a host application's publisher key signed a checkpoint")
		}
	})

	t.Run("an expired checkpoint is reported, and refreshed by signing again", func(t *testing.T) {
		h := newHarness(t, func(o *Options) { o.CheckpointValidity = 24 * time.Hour })
		h.appendN(s1, 4)
		first, err := h.log.Checkpoint(ctx, key1)
		if err != nil {
			t.Fatal(err)
		}
		again, err := h.log.Checkpoint(ctx, key1)
		if err != nil || again.Checkpoint.ID != first.Checkpoint.ID {
			t.Fatalf("an unchanged head must return the same checkpoint: %v / %v", again.Checkpoint.ID, err)
		}
		h.clk.Advance(48 * time.Hour)
		rep := h.mustBreak(key1)
		if !has(rep, ProblemCheckpointExpired, 0) {
			t.Errorf("expiry not reported: %+v", rep.Problems)
		}
		fresh, err := h.log.Checkpoint(ctx, key1)
		if err != nil || fresh.Checkpoint.ID == first.Checkpoint.ID {
			t.Fatalf("an expired checkpoint over an unchanged head must be re-signed: %v", err)
		}
		if rep := h.mustVerify(key1); !rep.Anchored {
			t.Errorf("after refresh: %+v", rep)
		}
	})

	t.Run("without a verifier a stored checkpoint is not believed", func(t *testing.T) {
		h := newHarness(t, func(o *Options) { o.Verifier = nil })
		h.appendN(s1, 3)
		if _, err := h.log.Checkpoint(ctx, key1); err != nil {
			t.Fatal(err)
		}
		if rep := h.mustBreak(key1); !has(rep, ProblemCheckpointUnverifiable, 0) {
			t.Errorf("%+v", rep.Problems)
		}
	})

	t.Run("a stored checkpoint that does not parse is a finding", func(t *testing.T) {
		h := newHarness(t)
		h.appendN(s1, 3)
		if _, err := h.log.Checkpoint(ctx, key1); err != nil {
			t.Fatal(err)
		}
		h.dropTriggers()
		h.exec(`UPDATE audit_checkpoints SET body = 'not json'`)
		if rep := h.mustBreak(key1); !has(rep, ProblemCheckpointInvalid, 0) {
			t.Errorf("%+v", rep.Problems)
		}
	})

	t.Run("the checkpoint row must agree with its signed body", func(t *testing.T) {
		h := newHarness(t)
		h.appendN(s1, 5)
		if _, err := h.log.Checkpoint(ctx, key1); err != nil {
			t.Fatal(err)
		}
		h.dropTriggers()
		h.exec(`UPDATE audit_checkpoints SET head_seq = 2`)
		if rep := h.mustBreak(key1); !has(rep, ProblemCheckpointInvalid, 2) {
			t.Errorf("%+v", rep.Problems)
		}
	})
}

func TestCheckpointRefusals(t *testing.T) {
	h := newHarness(t)
	if _, err := h.log.Checkpoint(ctx, key1); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("an empty chain: %v", err)
	}
	if _, err := h.log.Checkpoint(ctx, ChainKey{"app-a", "*"}); contracts.CodeOf(err) != contracts.ErrScopeViolation {
		t.Errorf("a malformed chain key: %v", err)
	}
	if _, err := h.log.ExportCheckpoint(ctx, key1); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("no checkpoint to export: %v", err)
	}
	unsigned := newHarness(t, func(o *Options) { o.Signing = nil })
	unsigned.appendN(s1, 1)
	if _, err := unsigned.log.Checkpoint(ctx, key1); contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Errorf("no signing key: %v", err)
	}
	if _, err := New(h.db, Options{CheckpointEvery: 5}); err == nil {
		t.Error("CheckpointEvery without a signing key was accepted")
	}
	if _, err := New(h.db, Options{Signing: &trust.SigningKey{KeyID: "k", Key: ed25519.PrivateKey("short")}}); err == nil {
		t.Error("a malformed signing key was accepted")
	}
}

func TestPeriodicCheckpoints(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.CheckpointEvery = 3 })
	h.appendN(s1, 7)
	if n := h.count(`SELECT COUNT(*) FROM audit_checkpoints`); n != 2 {
		t.Fatalf("%d checkpoints after 7 events with CheckpointEvery=3, want 2 (at 3 and 6)", n)
	}
	rep := h.mustVerify(key1, VerifyOptions{RequireAnchor: true})
	if rep.AnchorSeq != 6 || rep.EventsAfterAnchor != 1 {
		t.Errorf("report = %+v", rep)
	}

	// CheckpointAll covers the chains whose head moved, and is idempotent.
	h.appendN(scope("app-a", "tenant-2", "ws-1"), 2)
	n, err := h.log.CheckpointAll(ctx)
	if err != nil || n != 2 {
		t.Fatalf("CheckpointAll = %d, %v; want 2 (tenant-1 moved on, tenant-2 never checkpointed)", n, err)
	}
	if n, err := h.log.CheckpointAll(ctx); err != nil || n != 0 {
		t.Fatalf("CheckpointAll again = %d, %v; want 0", n, err)
	}
	if rep := h.mustVerify(key1, VerifyOptions{RequireAnchor: true}); rep.AnchorSeq != 7 || rep.EventsAfterAnchor != 0 {
		t.Errorf("after CheckpointAll: %+v", rep)
	}

	// A checkpoint that cannot be signed never fails the append it follows: the
	// event is already durable. (64 random bytes are a well-sized key whose
	// halves do not belong together, so signing refuses.)
	broken := make([]byte, ed25519.PrivateKeySize)
	if _, err := rand.Read(broken); err != nil {
		t.Fatal(err)
	}
	h2 := newHarness(t, func(o *Options) {
		o.Signing = &trust.SigningKey{KeyID: signerID, Key: broken}
		o.CheckpointEvery = 1
	})
	if _, err := h2.log.Append(ctx, s1, settings(1)); err != nil {
		t.Fatalf("a failing checkpoint failed the append: %v", err)
	}
	if n := h2.count(`SELECT COUNT(*) FROM audit_events`); n != 1 {
		t.Errorf("%d events stored, want 1", n)
	}
	if n := h2.count(`SELECT COUNT(*) FROM audit_checkpoints`); n != 0 {
		t.Errorf("%d checkpoints stored by a failing signer", n)
	}
}

func TestExportedCheckpointRoundTrips(t *testing.T) {
	h, _, cp10 := setupAnchored(t)
	got, err := h.log.ExportCheckpoint(ctx, key1)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var back Checkpoint
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&back); err != nil {
		t.Fatal(err)
	}
	if back != cp10 {
		t.Errorf("round trip changed the checkpoint:\n%+v\n%+v", back, cp10)
	}
	if err := h.log.VerifyCheckpoint(ctx, back, key1); err != nil {
		t.Errorf("a checkpoint must verify after its JSON round trip: %v", err)
	}
}

// forgeCheckpoint builds a checkpoint for (key, headSeq, headHash) signed by
// an arbitrary key, as an attacker would.
func forgeCheckpoint(t *testing.T, key ChainKey, headSeq int64, headHash, keyID string, priv ed25519.PrivateKey, now time.Time) Checkpoint {
	t.Helper()
	at := now.UTC().Truncate(time.Second)
	exp := at.Add(24 * time.Hour)
	body := CheckpointBody{
		Format: CheckpointFormat, ID: "achk_01J9ZQ3K8V4W5X6Y7Z8A9B0C1D",
		ApplicationID: key.ApplicationID, TenantID: key.TenantID,
		HeadSeq: headSeq, HeadHash: headHash,
		CreatedAt: checkpointTime(at), ExpiresAt: checkpointTime(exp),
	}
	sig, err := trust.SignPayload(body, keyID, priv, at, exp)
	if err != nil {
		t.Fatal(err)
	}
	return Checkpoint{Checkpoint: body, Signature: sig}
}

func (h *harness) insertCheckpoint(cp Checkpoint) {
	h.t.Helper()
	h.insertCheckpointRow(cp.Checkpoint.Chain(), cp)
}

// insertCheckpointRow stores cp in the slot of chain, whatever cp says.
func (h *harness) insertCheckpointRow(chain ChainKey, cp Checkpoint) {
	h.t.Helper()
	body, err := trust.CanonicalJSON(cp.Checkpoint)
	if err != nil {
		h.t.Fatal(err)
	}
	sig, err := json.Marshal(cp.Signature)
	if err != nil {
		h.t.Fatal(err)
	}
	h.exec(`INSERT INTO audit_checkpoints (id, application_id, tenant_id, workspace_id, head_seq, body, signature, created_at)
	        VALUES (?, ?, ?, '', ?, ?, ?, 1)`,
		"achk_planted_"+chain.TenantID+cp.Checkpoint.ID, chain.ApplicationID, chain.TenantID, cp.Checkpoint.HeadSeq, string(body), string(sig))
}

// A chain whose every event was deleted keeps its signed checkpoints: a sweep
// must still list it so the truncation is reported, never skip it.
func TestVerifiableChainsListsAChainWithNoEventsLeft(t *testing.T) {
	h, _, _ := setupAnchored(t)
	h.dropTriggers()
	h.exec(`DELETE FROM audit_events WHERE tenant_id = 'tenant-1'`)

	plain, err := h.log.Chains(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range plain {
		if k == key1 {
			t.Fatalf("Chains lists %v, which has no event left", key1)
		}
	}
	keys, err := h.log.VerifiableChains(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, k := range keys {
		found = found || k == key1
	}
	if !found {
		t.Fatalf("VerifiableChains = %v, want it to include %v", keys, key1)
	}
	if rep := h.mustBreak(key1); !has(rep, ProblemTruncated, 10) {
		t.Errorf("an emptied chain must be reported truncated: %+v", rep.Problems)
	}
}
