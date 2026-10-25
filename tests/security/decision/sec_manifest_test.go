package decisionsec

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/decision"
	"github.com/buxee/alix-ai/internal/decision/fakescorer"
	"github.com/buxee/alix-ai/internal/decision/lifecycle"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	actgen "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// SEC-098
func TestSecDecisionManifestSignatureAndRevisionHashVerified(t *testing.T) {
	good := revFiles("rev2")

	t.Run("control: a manifest and the files it was made from verify", func(t *testing.T) {
		if err := lifecycle.VerifyArtifacts(bg, manifestOf("rev2", good), &memSource{files: good}); err != nil {
			t.Fatalf("a genuine revision does not verify: %v", err)
		}
	})

	t.Run("a substituted, altered, truncated or padded file is a hash mismatch and fails closed", func(t *testing.T) {
		flip := func(b []byte) []byte { c := append([]byte(nil), b...); c[0] ^= 1; return c }
		for name, mut := range map[string]func(artifacts){
			"same size, one bit flipped": func(a artifacts) { a["head.bin"] = flip(a["head.bin"]) },
			"truncated":                  func(a artifacts) { a["head.bin"] = a["head.bin"][:len(a["head.bin"])-1] },
			"padded":                     func(a artifacts) { a["head.bin"] = append(a["head.bin"], 0) },
			"empty":                      func(a artifacts) { a["encoder.bin"] = nil },
			"swapped for another file":   func(a artifacts) { a["head.bin"], a["tok.json"] = a["tok.json"], a["head.bin"] },
		} {
			served := revFiles("rev2")
			mut(served)
			err := lifecycle.VerifyArtifacts(bg, manifestOf("rev2", good), &memSource{files: served})
			if !errors.Is(err, lifecycle.ErrHashMismatch) {
				t.Errorf("%s: err = %v, want a hash mismatch", name, err)
			}
		}
	})

	t.Run("a file that never ends is read only as far as the manifest says", func(t *testing.T) {
		src := &infiniteSource{}
		done := make(chan error, 1)
		go func() { done <- lifecycle.VerifyArtifacts(bg, manifestOf("rev2", good), src) }()
		select {
		case err := <-done:
			if !errors.Is(err, lifecycle.ErrHashMismatch) {
				t.Fatalf("err = %v, want a hash mismatch", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("verification of an endless file did not stop")
		}
	})

	t.Run("an unsafe name, a duplicate, a malformed hash, an empty list or no source is refused", func(t *testing.T) {
		base := func() gen.ScorerManifest { return manifestOf("rev2", good) }
		cases := map[string]func(*gen.ScorerManifest){
			"parent directory": func(m *gen.ScorerManifest) { m.Files[0].Name = "../outside.bin" },
			"nested path":      func(m *gen.ScorerManifest) { m.Files[0].Name = "sub/head.bin" },
			"backslash":        func(m *gen.ScorerManifest) { m.Files[0].Name = `sub\head.bin` },
			"drive":            func(m *gen.ScorerManifest) { m.Files[0].Name = "C:head.bin" },
			"dots":             func(m *gen.ScorerManifest) { m.Files[0].Name = ".." },
			"empty name":       func(m *gen.ScorerManifest) { m.Files[0].Name = "" },
			"duplicate name":   func(m *gen.ScorerManifest) { m.Files[1].Name = m.Files[0].Name },
			"upper-case hash": func(m *gen.ScorerManifest) {
				m.Files[0].SHA256 = gen.SHA256(strings.ToUpper(string(m.Files[0].SHA256)))
			},
			"short hash":         func(m *gen.ScorerManifest) { m.Files[0].SHA256 = "abcd" },
			"negative size":      func(m *gen.ScorerManifest) { m.Files[0].SizeBytes = -1 },
			"no files":           func(m *gen.ScorerManifest) { m.Files = nil },
			"encoder hash of no": func(m *gen.ScorerManifest) { h := gen.SHA256(sha([]byte("not a listed file"))); m.Encoder.SHA256 = &h },
		}
		for name, mut := range cases {
			m := base()
			mut(&m)
			if err := lifecycle.VerifyArtifacts(bg, m, &memSource{files: good}); err == nil {
				t.Errorf("%s: verification passed", name)
			}
		}
		if err := lifecycle.VerifyArtifacts(bg, base(), nil); err == nil {
			t.Error("verification with no source passed")
		}
		if err := lifecycle.VerifyArtifacts(bg, base(), &memSource{files: good, openErr: io.ErrUnexpectedEOF}); err == nil {
			t.Error("verification of an unreadable source passed")
		}
	})

	t.Run("an update to files that do not match the manifest loads nothing and the serving revision stays", func(t *testing.T) {
		r := newLCRig(t, nil)
		if got := r.serve(); got != "rev1" {
			t.Fatalf("serving %q", got)
		}
		next, src := r.next("rev2")
		src.files["head.bin"] = append([]byte(nil), src.files["head.bin"]...)
		src.files["head.bin"][0] ^= 1

		rep, err := r.sup.Update(bg, next, src, nil)

		if err == nil || rep.Status != lifecycle.UpdateRejected || rep.Reason != "hash_mismatch" {
			t.Fatalf("report %+v, err %v", rep, err)
		}
		if r.ld.loads("rev2") != 0 {
			t.Fatal("a revision that failed verification was loaded")
		}
		if got := r.serve(); got != "rev1" || r.sup.Manifest().ModelRevision != "rev1" {
			t.Fatalf("serving %q after a rejected update", got)
		}
		if r.gate.resident() != 8*gib {
			t.Fatalf("resident bytes = %d: a rejected update reserved memory", r.gate.resident())
		}
	})

	t.Run("a result that names another revision than the manifest, or than the operator pinned, is model_mismatch", func(t *testing.T) {
		r := newRig(t)
		r.sc.Script(fakescorer.Step{Mutate: func(res *gen.DecisionScoreResult) { v := actgen.OpaqueVersion("rev9"); res.ModelRevision = &v }})
		wantFallback(t, r.score(scopeOf("ten1"), r.request("ten1")), gen.FallbackReasonModelMismatch)

		r = newRig(t, func(c *decision.Config) { c.PinnedRevision = "rev-approved" })
		wantFallback(t, r.score(scopeOf("ten1"), r.request("ten1")), gen.FallbackReasonModelMismatch)
		if r.sc.received() != 1 {
			t.Fatalf("scorer requests = %d (the pin is checked on the answer)", r.sc.received())
		}
	})

	t.Run("the signature recipe of the schema is tamper evident: any change to the manifest breaks it", func(t *testing.T) {
		clk := clock.NewFake(t0)
		ts := trust.NewStore(storetest.Open(t, nil), clk)
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		const keyID = "release-key-1"
		if _, err := ts.RegisterKey(bg, trust.PlatformApplicationID, keyID, pub, "scorer manifest signer"); err != nil {
			t.Fatal(err)
		}
		ver := trust.NewVerifier(ts, clk)
		opts := trust.PayloadOptions{ApplicationID: trust.PlatformApplicationID}

		m := manifestOf("rev2", good)
		sig, err := trust.SignPayload(manifestPayload(t, m), keyID, priv, clk.Now(), clk.Now().Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		verify := func(m gen.ScorerManifest, sigs ...trust.Signature) error {
			_, err := ver.VerifyPayload(bg, manifestPayload(t, m), sigs, opts)
			return err
		}
		if err := verify(m, sig); err != nil {
			t.Fatalf("control: a genuine signature does not verify: %v", err)
		}

		tampered := map[string]func(*gen.ScorerManifest){
			"revision":  func(m *gen.ScorerManifest) { m.ModelRevision = "rev-evil" },
			"model":     func(m *gen.ScorerManifest) { m.ModelID = "other-model" },
			"provider":  func(m *gen.ScorerManifest) { m.ProviderID = "other-provider" },
			"file hash": func(m *gen.ScorerManifest) { m.Files[0].SHA256 = gen.SHA256(sha([]byte("evil"))) },
			"file size": func(m *gen.ScorerManifest) { m.Files[0].SizeBytes++ },
			"extra file": func(m *gen.ScorerManifest) {
				m.Files = append(m.Files, gen.ScorerManifestFileHash{Name: "x.bin", SHA256: gen.SHA256(sha([]byte("x"))), SizeBytes: 1})
			},
			"encoder": func(m *gen.ScorerManifest) { m.Encoder.Revision = "enc-evil" },
			"licence": func(m *gen.ScorerManifest) {
				m.Licence.Redistribution = gen.ScorerManifestLicenceRedistributionNotAllowed
			},
			"licence url": func(m *gen.ScorerManifest) { u := "https://evil.example/licence"; m.Licence.URL = &u },
		}
		for name, mut := range tampered {
			c := manifestOf("rev2", good)
			mut(&c)
			if err := verify(c, sig); err == nil {
				t.Errorf("a manifest with an altered %s still verifies", name)
			}
		}

		// The signature member itself is not covered (it is the signature), so a copy that only differs there verifies.
		c := m
		c.Signature = gen.ScorerManifestSignature{Algorithm: "ed25519", KeyID: keyID, Value: sig.Signature}
		if err := verify(c, sig); err != nil {
			t.Errorf("the embedded signature changed what is signed: %v", err)
		}

		// Another key, an unregistered key, a revoked key and an expired signature are all refused.
		_, other, _ := ed25519.GenerateKey(rand.Reader)
		forged, err := trust.SignPayload(manifestPayload(t, m), keyID, other, clk.Now(), clk.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if err := verify(m, forged); err == nil {
			t.Error("a signature made with a different private key verifies")
		}
		unregistered, err := trust.SignPayload(manifestPayload(t, m), "unregistered-key", priv, clk.Now(), clk.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if err := verify(m, unregistered); err == nil {
			t.Error("a signature by an unregistered key verifies")
		}
		if err := verify(m); err == nil {
			t.Error("a manifest with no signature verifies")
		}
		appOpts := trust.PayloadOptions{ApplicationID: "some-application"}
		if _, err := ver.VerifyPayload(bg, manifestPayload(t, m), []trust.Signature{sig}, appOpts); err == nil {
			t.Error("a platform key is trusted for another application's payload")
		}
		pub2, priv2, _ := ed25519.GenerateKey(rand.Reader)
		if _, err := ts.RegisterKey(bg, trust.PlatformApplicationID, "release-key-2", pub2, "second signer"); err != nil {
			t.Fatal(err)
		}
		sig2, err := trust.SignPayload(manifestPayload(t, m), "release-key-2", priv2, clk.Now(), clk.Now().Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if err := verify(m, sig2); err != nil {
			t.Fatalf("control: the second key does not verify: %v", err)
		}
		if err := ts.RevokeKey(bg, trust.PlatformApplicationID, "release-key-2", "key leaked"); err != nil {
			t.Fatal(err)
		}
		if err := verify(m, sig2); err == nil {
			t.Error("a signature by a revoked key verifies")
		}
		clk.Advance(48 * time.Hour)
		if err := verify(m, sig); !contracts.HasCode(err, contracts.ErrExpired) {
			t.Errorf("an expired signature: err = %v, want expired", err)
		}
	})

	t.Run("the published manifest schema requires a signature", func(t *testing.T) {
		schemas := compileDecisionSchemas(t)
		m := manifestOf("rev2", good)
		m.Signature = gen.ScorerManifestSignature{Algorithm: "ed25519", KeyID: "release-key-1", Value: strings.Repeat("A", 86) + "=="}
		if err := schemas.validate("scorer-manifest", m); err != nil {
			t.Fatalf("control: a signed manifest fails its schema: %v", err)
		}
		doc := toMap(t, m)
		delete(doc, "signature")
		if err := schemas.validate("scorer-manifest", doc); err == nil {
			t.Error("a manifest without a signature validates")
		}
		doc = toMap(t, m)
		doc["signature"].(map[string]any)["algorithm"] = "md5"
		if err := schemas.validate("scorer-manifest", doc); err == nil {
			t.Error("a manifest signed with an undefined algorithm validates")
		}
	})
}

// SEC-099
//
// The update path verifies the manifest signature, through trust.Verifier and its validity window, before it
// believes any hash the manifest lists. Each case below is a signed revision with one thing wrong; every one of
// them must be rejected with the reason "signature" before a file is read, a byte reserved or an instance loaded.
func TestSecScorerManifestSignatureIsVerified(t *testing.T) {
	const day = 24 * time.Hour
	type tc struct {
		name   string
		edit   func(r *lcRig, rev lifecycle.Revision) lifecycle.Revision // builds the offered revision
		reason string
	}
	win := func(r *lcRig, from, to time.Duration) lifecycle.ManifestSignatureWindow {
		return lifecycle.ManifestSignatureWindow{SignedAt: r.clk.Now().Add(from), ExpiresAt: r.clk.Now().Add(to)}
	}
	cases := []tc{
		{"unsigned: no algorithm, key or value", func(r *lcRig, rev lifecycle.Revision) lifecycle.Revision {
			rev.Manifest.Signature = gen.ScorerManifestSignature{}
			return rev
		}, "signature"},
		{"signature value cut to nothing", func(r *lcRig, rev lifecycle.Revision) lifecycle.Revision {
			rev.Manifest.Signature.Value = ""
			return rev
		}, "signature"},
		{"an algorithm the schema does not define", func(r *lcRig, rev lifecycle.Revision) lifecycle.Revision {
			rev.Manifest.Signature.Algorithm = "md5"
			return rev
		}, "signature"},
		{"no validity window", func(r *lcRig, rev lifecycle.Revision) lifecycle.Revision {
			rev.SignatureWindow = lifecycle.ManifestSignatureWindow{}
			return rev
		}, "signature"},
		{"expired", func(r *lcRig, rev lifecycle.Revision) lifecycle.Revision {
			return r.keys.signWith(t, rev, r.keys.priv, scorerSecKeyID, win(r, -2*day, -time.Hour))
		}, "signature"},
		{"expired, window then extended without re-signing", func(r *lcRig, rev lifecycle.Revision) lifecycle.Revision {
			rev = r.keys.signWith(t, rev, r.keys.priv, scorerSecKeyID, win(r, -2*day, -time.Hour))
			rev.SignatureWindow.ExpiresAt = r.clk.Now().Add(day)
			return rev
		}, "signature"},
		{"dated in the future", func(r *lcRig, rev lifecycle.Revision) lifecycle.Revision {
			return r.keys.signWith(t, rev, r.keys.priv, scorerSecKeyID, win(r, time.Hour, day))
		}, "signature"},
		{"signed with a key the trust store does not hold", func(r *lcRig, rev lifecycle.Revision) lifecycle.Revision {
			return r.keys.signWith(t, rev, r.keys.other, scorerSecKeyID, scorerSecWindow(r.clk.Now()))
		}, "signature"},
		{"signed under a key id nobody registered", func(r *lcRig, rev lifecycle.Revision) lifecycle.Revision {
			return r.keys.signWith(t, rev, r.keys.priv, "attacker-key", scorerSecWindow(r.clk.Now()))
		}, "signature"},
		{"signed by a revoked key", func(r *lcRig, rev lifecycle.Revision) lifecycle.Revision {
			r.keys.status = trust.KeyRevoked
			return rev
		}, "signature"},
		{"a file hash swapped after signing", func(r *lcRig, rev lifecycle.Revision) lifecycle.Revision {
			rev.Manifest.Files = append([]gen.ScorerManifestFileHash(nil), rev.Manifest.Files...)
			rev.Manifest.Files[0].SHA256 = gen.SHA256(sha([]byte("attacker weights")))
			return rev
		}, "signature"},
		{"the revision renamed after signing", func(r *lcRig, rev lifecycle.Revision) lifecycle.Revision {
			rev.Manifest.ModelRevision = "rev3"
			return rev
		}, "signature"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newLCRig(t, nil)
			if got := r.serve(); got != "rev1" {
				t.Fatalf("serving %q", got)
			}
			next, src := r.next("rev2")
			next = c.edit(r, next)

			rep, err := r.sup.Update(bg, next, src, nil)

			if err == nil || rep.Status != lifecycle.UpdateRejected || rep.Reason != c.reason {
				t.Fatalf("an unacceptable signature was trusted: report %+v, err %v", rep, err)
			}
			if len(src.opened) != 0 {
				t.Fatalf("files were read before the manifest was trusted: %v", src.opened)
			}
			if r.ld.loads("rev2") != 0 || r.ld.loads("rev3") != 0 || r.gate.resident() != 8*gib {
				t.Fatal("an unverified revision was loaded or reserved memory")
			}
			if got := r.serve(); got != "rev1" || r.sup.Manifest().ModelRevision != "rev1" {
				t.Fatalf("serving %q after a rejected update", got)
			}
		})
	}

	t.Run("control: the same revision, signed by the trusted key inside its window, is promoted", func(t *testing.T) {
		r := newLCRig(t, nil)
		r.serve()
		next, src := r.next("rev2")
		rep, err := r.sup.Update(bg, next, src, nil)
		if err != nil || rep.Status != lifecycle.UpdateProbation {
			t.Fatalf("a correctly signed revision was refused: %+v, %v", rep, err)
		}
	})

	t.Run("a supervisor with no verifier trusts no revision", func(t *testing.T) {
		r := newLCRig(t, nil)
		sup, err := lifecycle.New(lifecycle.Deps{Clock: r.clk, Loader: r.ld, Gate: r.gate, Revision: r.cur, Config: lifecycle.DefaultConfig()})
		if err != nil {
			t.Fatal(err)
		}
		next, src := r.next("rev2")
		rep, err := sup.Update(bg, next, src, nil)
		if err == nil || rep.Status != lifecycle.UpdateRejected || r.ld.loads("rev2") != 0 {
			t.Fatalf("no verifier, yet a revision was accepted: %+v, %v", rep, err)
		}
	})

	t.Run("a retired key with no retirement time vouches for nothing", func(t *testing.T) {
		r := newLCRig(t, nil)
		r.serve()
		next, src := r.next("rev2")
		r.keys.status = trust.KeyRetired // RetiredAt is zero: trust refuses a retired key with no retirement time
		if _, err := r.sup.Update(bg, next, src, nil); err == nil {
			t.Fatal("a retired key with no retirement time was trusted")
		}
	})
}

// infiniteSource serves a file that never ends, whatever its name.
type infiniteSource struct{}

func (infiniteSource) Open(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(endless{}), nil
}

// manifestPayload is what the schema says is signed: the canonical manifest without its signature member.
func manifestPayload(t *testing.T, m gen.ScorerManifest) map[string]any {
	t.Helper()
	m.Signature = gen.ScorerManifestSignature{Algorithm: "ed25519", KeyID: "placeholder", Value: "AAAA"} // the generated type refuses to encode an unsigned manifest; the member is dropped below
	doc := toMap(t, m)
	delete(doc, "signature")
	return doc
}
