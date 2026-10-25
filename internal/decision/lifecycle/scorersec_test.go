package lifecycle

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// The signing keys exist only for the test run: generated here, registered in memory, never written.
const scorerSecKeyID = "scorer-release-1"

var (
	scorerSecOnce  sync.Once
	scorerSecPub   ed25519.PublicKey
	scorerSecPriv  ed25519.PrivateKey
	scorerSecOther ed25519.PrivateKey
)

func scorerSecKeys() {
	scorerSecOnce.Do(func() {
		var err error
		if scorerSecPub, scorerSecPriv, err = ed25519.GenerateKey(rand.Reader); err != nil {
			panic(err)
		}
		if _, scorerSecOther, err = ed25519.GenerateKey(rand.Reader); err != nil {
			panic(err)
		}
	})
}

// scorerSecStore is an in-memory trust.KeyLookup.
type scorerSecStore struct {
	keys map[string]trust.Key // by key id, registered under the platform application
	err  error
}

func (s scorerSecStore) LookupKey(_ context.Context, app, id string) (trust.Key, error) {
	if s.err != nil {
		return trust.Key{}, s.err
	}
	k, ok := s.keys[id]
	if !ok || app != trust.PlatformApplicationID {
		return trust.Key{}, trust.ErrUnknownKey
	}
	return k, nil
}

func scorerSecTrust() scorerSecStore {
	scorerSecKeys()
	return scorerSecStore{keys: map[string]trust.Key{
		scorerSecKeyID: {ApplicationID: trust.PlatformApplicationID, KeyID: scorerSecKeyID, PublicKey: scorerSecPub, Status: trust.KeyActive},
	}}
}

func scorerSecVerifier(t testing.TB, clk clock.Clock) *ManifestVerifier {
	t.Helper()
	v, err := NewManifestVerifier(scorerSecTrust(), clk, nil)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// scorerSecSignWith signs rev as a release would: the window is bound into the signed payload.
func scorerSecSignWith(t testing.TB, rev Revision, key ed25519.PrivateKey, keyID string, w ManifestSignatureWindow) Revision {
	t.Helper()
	payload, err := ManifestSigningPayload(rev.Manifest, w)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := trust.SignPayload(payload, keyID, key, w.SignedAt, w.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	rev.Manifest.Signature = gen.ScorerManifestSignature{
		Algorithm: gen.ScorerManifestSignatureAlgorithmEd25519, KeyID: common.Identifier(keyID), Value: sig.Signature,
	}
	rev.SignatureWindow = w
	return rev
}

func scorerSecWindow() ManifestSignatureWindow {
	return ManifestSignatureWindow{SignedAt: t0.Add(-time.Hour), ExpiresAt: t0.Add(24 * time.Hour)}
}

// scorerSecSign signs rev with the trusted release key and a window around the fake clock.
func scorerSecSign(t testing.TB, rev Revision) Revision {
	t.Helper()
	scorerSecKeys()
	return scorerSecSignWith(t, rev, scorerSecPriv, scorerSecKeyID, scorerSecWindow())
}

// scorerSecSource counts how many files Update asked for.
type scorerSecSource struct {
	memSource
	opens int
}

func (s *scorerSecSource) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	s.opens++
	return s.memSource.Open(ctx, name)
}

func TestUpdateRefusesAnUnsignedOrExpiredManifest(t *testing.T) {
	files := map[string][]byte{"w.bin": []byte("weights of rev2")}
	good := func() Revision { return scorerSecSign(t, revision("model-a", "rev2", 100, files)) }
	w := scorerSecWindow()
	window := func(from, to time.Time) ManifestSignatureWindow {
		return ManifestSignatureWindow{SignedAt: from, ExpiresAt: to}
	}

	cases := []struct {
		name     string
		mutate   func(r *Revision)            // applied after signing: the tampering
		resign   func(r Revision) Revision    // replaces the signature
		verifier func(*rig) *ManifestVerifier // nil keeps the rig's
		code     contracts.ErrCode
	}{
		{name: "unsigned: no algorithm, key or value", code: contracts.ErrUnauthenticated,
			mutate: func(r *Revision) { r.Manifest.Signature = gen.ScorerManifestSignature{} }},
		{name: "unsigned: empty value", code: contracts.ErrUnauthenticated,
			mutate: func(r *Revision) { r.Manifest.Signature.Value = "" }},
		{name: "unsigned: empty key id", code: contracts.ErrUnauthenticated,
			mutate: func(r *Revision) { r.Manifest.Signature.KeyID = "" }},
		{name: "algorithm other than ed25519", code: contracts.ErrUnauthenticated,
			mutate: func(r *Revision) { r.Manifest.Signature.Algorithm = "md5" }},
		{name: "no validity window", code: contracts.ErrUnauthenticated,
			mutate: func(r *Revision) { r.SignatureWindow = ManifestSignatureWindow{} }},
		{name: "window ends before it starts", code: contracts.ErrUnauthenticated,
			mutate: func(r *Revision) { r.SignatureWindow = window(w.ExpiresAt, w.SignedAt) }},
		{name: "expired", code: contracts.ErrExpired,
			resign: func(r Revision) Revision {
				return scorerSecSignWith(t, r, scorerSecPriv, scorerSecKeyID, window(t0.Add(-48*time.Hour), t0.Add(-time.Hour)))
			}},
		{name: "expires exactly now", code: contracts.ErrExpired,
			resign: func(r Revision) Revision {
				return scorerSecSignWith(t, r, scorerSecPriv, scorerSecKeyID, window(t0.Add(-time.Hour), t0))
			}},
		{name: "expired, then the window is extended without re-signing", code: contracts.ErrUnauthenticated,
			resign: func(r Revision) Revision {
				r = scorerSecSignWith(t, r, scorerSecPriv, scorerSecKeyID, window(t0.Add(-48*time.Hour), t0.Add(-time.Hour)))
				r.SignatureWindow.ExpiresAt = t0.Add(24 * time.Hour)
				return r
			}},
		{name: "signed in the future", code: contracts.ErrUnauthenticated,
			resign: func(r Revision) Revision {
				return scorerSecSignWith(t, r, scorerSecPriv, scorerSecKeyID, window(t0.Add(time.Hour), t0.Add(2*time.Hour)))
			}},
		{name: "signed by a key the store does not trust", code: contracts.ErrUnauthenticated,
			resign: func(r Revision) Revision { return scorerSecSignWith(t, r, scorerSecOther, scorerSecKeyID, w) }},
		{name: "signed under an unregistered key id", code: contracts.ErrUnauthenticated,
			resign: func(r Revision) Revision { return scorerSecSignWith(t, r, scorerSecPriv, "unknown-key", w) }},
		{name: "key narrowed away by the allowlist", code: contracts.ErrUnauthenticated,
			verifier: func(r *rig) *ManifestVerifier {
				v, _ := NewManifestVerifier(scorerSecTrust(), r.clk, []string{})
				return v
			}},
		{name: "the trust store cannot answer", code: contracts.ErrUnavailable,
			verifier: func(r *rig) *ManifestVerifier {
				s := scorerSecTrust()
				s.err = contracts.New(contracts.ErrUnavailable, "store down")
				v, _ := NewManifestVerifier(s, r.clk, nil)
				return v
			}},
		{name: "no verifier configured", code: contracts.ErrUnauthenticated,
			verifier: func(*rig) *ManifestVerifier { return nil }},
		// A valid signature binds every field of the manifest, the file hashes above all.
		{name: "model revision changed after signing", code: contracts.ErrUnauthenticated,
			mutate: func(r *Revision) { r.Manifest.ModelRevision = "rev3" }},
		{name: "model id changed after signing", code: contracts.ErrUnauthenticated,
			mutate: func(r *Revision) { r.Manifest.ModelID = "model-b" }},
		{name: "file hash swapped after signing", code: contracts.ErrUnauthenticated,
			mutate: func(r *Revision) {
				r.Manifest.Files = append([]gen.ScorerManifestFileHash(nil), r.Manifest.Files...)
				r.Manifest.Files[0].SHA256 = sum([]byte("evil"))
			}},
		{name: "file listed after signing", code: contracts.ErrUnauthenticated,
			mutate: func(r *Revision) {
				r.Manifest.Files = append(append([]gen.ScorerManifestFileHash(nil), r.Manifest.Files...),
					gen.ScorerManifestFileHash{Name: "x.bin", SHA256: sum([]byte("x")), SizeBytes: 1})
			}},
		{name: "encoder hash changed after signing", code: contracts.ErrUnauthenticated,
			mutate: func(r *Revision) { h := sum([]byte("evil encoder")); r.Manifest.Encoder.SHA256 = &h }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, 1000, rev1())
			r.mustScore()
			if tc.verifier != nil {
				r.sup.verify = tc.verifier(r)
			}
			next := good()
			if tc.resign != nil {
				next = tc.resign(next)
			}
			if tc.mutate != nil {
				tc.mutate(&next)
			}
			src := &scorerSecSource{memSource: memSource(files)}

			rep, err := r.sup.Update(context.Background(), next, src, nil)

			if err == nil || !contracts.HasCode(err, tc.code) {
				t.Fatalf("err = %v (code %q), want code %q", err, contracts.CodeOf(err), tc.code)
			}
			if rep.Status != UpdateRejected || rep.Reason != "signature" {
				t.Fatalf("report = %+v, want rejected/signature", rep)
			}
			// Nothing in the unverified manifest was believed: no hash check, no gate, no load, no switch.
			if src.opens != 0 || r.gate.admits != 1 || r.ld.loads("rev2") != 0 || r.ld.loads("rev3") != 0 {
				t.Fatalf("the unverified manifest was used: opens=%d admits=%d loads=%d", src.opens, r.gate.admits, r.ld.loads("rev2"))
			}
			if got := r.sup.Manifest().ModelRevision; got != "rev1" || !r.sup.State().Ready {
				t.Fatalf("serving %q ready=%v after a rejected update", got, r.sup.State().Ready)
			}
			labels := Labels{Provider: string(next.Manifest.ProviderID), Model: string(next.Manifest.ModelID), Revision: string(next.Manifest.ModelRevision), Profile: "lite", Reason: "signature"}
			if got := r.reg.Counter(MetricRollbacks, labels); got != 1 {
				t.Fatalf("the rejection was not counted: %d", got)
			}
		})
	}

	t.Run("control: the signed revision is accepted, and the hash check still applies after it", func(t *testing.T) {
		r := newRig(t, 1000, rev1())
		r.mustScore()
		rep, err := r.sup.Update(context.Background(), good(), memSource(files), nil)
		if err != nil || rep.Status != UpdatePromoted || r.sup.Manifest().ModelRevision != "rev2" {
			t.Fatalf("update = %+v, %v", rep, err)
		}
		r = newRig(t, 1000, rev1())
		r.mustScore()
		rep, err = r.sup.Update(context.Background(), good(), memSource(map[string][]byte{"w.bin": []byte("tampered")}), nil)
		if err == nil || rep.Reason != "hash_mismatch" {
			t.Fatalf("a signed manifest over tampered files: %+v, %v", rep, err)
		}
	})

	t.Run("a retired key signs only what it signed before it was retired, a revoked key nothing", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			status trust.KeyStatus
			at     time.Time
			ok     bool
		}{
			{"retired after signing", trust.KeyRetired, t0.Add(time.Hour), true},
			{"retired before signing", trust.KeyRetired, t0.Add(-2 * time.Hour), false},
			{"revoked", trust.KeyRevoked, t0.Add(time.Hour), false},
		} {
			r := newRig(t, 1000, rev1())
			r.mustScore()
			s := scorerSecTrust()
			k := s.keys[scorerSecKeyID]
			k.Status, k.RetiredAt = tc.status, tc.at
			s.keys[scorerSecKeyID] = k
			r.sup.verify, _ = NewManifestVerifier(s, r.clk, nil)
			_, err := r.sup.Update(context.Background(), good(), memSource(files), nil)
			if (err == nil) != tc.ok {
				t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
			}
		}
	})
}

type scorerSecPanicInst struct{ *fakeInst }

func (scorerSecPanicInst) ScoreDecision(context.Context, contracts.Scope, gen.DecisionRequest) (gen.DecisionScoreResult, error) {
	panic("scorer crashed")
}

type scorerSecPanicLoader struct{ *fakeLoader }

func (l scorerSecPanicLoader) Load(ctx context.Context, rev Revision) (Instance, error) {
	in, err := l.fakeLoader.Load(ctx, rev)
	if err != nil {
		return nil, err
	}
	return scorerSecPanicInst{in.(*fakeInst)}, nil
}

// A panicking instance must not leak its in-flight slot: the supervisor marks the runtime dead
// (so it restarts after the backoff) and releases the reservation once the panic has passed.
func TestScorePanicReleasesTheInflightSlot(t *testing.T) {
	r := newRig(t, 1000, rev1())
	sup, err := New(Deps{Clock: r.clk, Loader: scorerSecPanicLoader{r.ld}, Gate: r.gate, Registry: r.reg, Revision: rev1(), Config: testConfig()})
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic was swallowed by the supervisor: the Engine would not record it")
			}
		}()
		_, _ = sup.ScoreDecision(context.Background(), scope(), request(r.clk.Now()))
	}()
	if st := sup.State(); st.Ready {
		t.Fatalf("a crashed runtime is still ready: %+v", st)
	}
	if r.gate.heldBytes() != 0 {
		t.Fatalf("held %d bytes after the crashed instance was retired", r.gate.heldBytes())
	}
}
