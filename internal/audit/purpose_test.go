package audit

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/trust"
)

// H1: a checkpoint or tombstone is believed only under a key registered for
// the audit-checkpoint purpose. A release key, or a platform key with no
// purpose (an integration-audit signer, a key registered before purposes
// existed), is a valid registered platform key and still signs nothing here.
func TestCheckpointsAndTombstonesNeedTheCheckpointPurpose(t *testing.T) {
	others := []struct {
		name    string
		purpose trust.KeyPurpose
	}{
		{"a release key", trust.PurposeRelease},
		{"a platform key with no purpose", trust.PurposeNone},
	}

	t.Run("checkpoint", func(t *testing.T) {
		for _, o := range others {
			t.Run(o.name, func(t *testing.T) {
				h := newHarness(t)
				h.appendN(s1, 3)
				head := h.mustVerify(key1)
				pub, priv, _ := ed25519.GenerateKey(rand.Reader)
				if _, err := h.trust.RegisterKey(ctx, trust.PlatformApplicationID, "other-platform-key", pub, "x", trust.WithPurpose(o.purpose)); err != nil {
					t.Fatal(err)
				}
				forged := forgeCheckpoint(t, key1, head.HeadSeq, head.HeadHash, "other-platform-key", priv, h.clk.Now())
				err := h.log.VerifyCheckpoint(ctx, forged, key1)
				if !errors.Is(err, trust.ErrWrongPurpose) {
					t.Fatalf("VerifyCheckpoint = %v, want a wrong-purpose refusal", err)
				}
				// Control: the same body signed with the checkpoint key verifies.
				genuine := forgeCheckpoint(t, key1, head.HeadSeq, head.HeadHash, signerID, h.priv, h.clk.Now())
				if err := h.log.VerifyCheckpoint(ctx, genuine, key1); err != nil {
					t.Fatalf("the control does not verify, so the refusal proves nothing: %v", err)
				}
				// Planted in the database, the forged one does not anchor the chain.
				h.insertCheckpoint(forged)
				rep := h.mustBreak(key1, VerifyOptions{RequireAnchor: true})
				if !has(rep, ProblemCheckpointInvalid, 0) || rep.Anchored {
					t.Errorf("a checkpoint signed by %s anchored the chain: %+v", o.name, rep)
				}
			})
		}
	})

	t.Run("tombstone", func(t *testing.T) {
		for _, o := range others {
			t.Run(o.name, func(t *testing.T) {
				c := setupErased(t)
				h := c.h
				_, raw := h.tombstoneBody(4)
				var tb Tombstone
				if err := decodeStrict([]byte(raw), &tb.Tombstone); err != nil {
					t.Fatal(err)
				}
				signedAt, err := time.Parse(time.RFC3339Nano, tb.Tombstone.ErasedAt)
				if err != nil {
					t.Fatal(err)
				}
				expiresAt, err := time.Parse(time.RFC3339Nano, tb.Tombstone.ExpiresAt)
				if err != nil {
					t.Fatal(err)
				}
				pub, priv, _ := ed25519.GenerateKey(rand.Reader)
				if _, err := h.trust.RegisterKey(ctx, trust.PlatformApplicationID, "other-platform-key", pub, "x", trust.WithPurpose(o.purpose)); err != nil {
					t.Fatal(err)
				}
				if tb.Signature, err = trust.SignPayload(tb.Tombstone, "other-platform-key", priv, signedAt, expiresAt); err != nil {
					t.Fatal(err)
				}
				if err := h.log.VerifyTombstone(ctx, tb, key1); !errors.Is(err, trust.ErrWrongPurpose) {
					t.Fatalf("VerifyTombstone = %v, want a wrong-purpose refusal", err)
				}
				// Control: the same body signed with the checkpoint key verifies.
				if tb.Signature, err = trust.SignPayload(tb.Tombstone, signerID, h.priv, signedAt, expiresAt); err != nil {
					t.Fatal(err)
				}
				if err := h.log.VerifyTombstone(ctx, tb, key1); err != nil {
					t.Fatalf("the control does not verify, so the refusal proves nothing: %v", err)
				}
			})
		}
	})
}
