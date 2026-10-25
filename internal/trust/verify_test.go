package trust

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// auditReport stands in for the payloads other tasks sign with the same signer
// (integration-audit reports, release manifests).
type auditReport struct {
	Application string   `json:"application"`
	Passed      []string `json:"passed"`
	Failed      []string `json:"failed"`
	Findings    int      `json:"findings"`
	Notes       string   `json:"notes"`
}

// TestSignVerifyRoundTrip: what the signer produces the verifier accepts, for a
// package and for an arbitrary payload, and any change to the signed bytes
// breaks it.
//
// SEC-057: a signed package verifies; SEC-088: a signed report verifies and a
// modified report does not.
func TestSignVerifyRoundTrip(t *testing.T) {
	ctx := context.Background()

	t.Run("package", func(t *testing.T) {
		f := newFixture(t)
		pv := f.mustVerify()
		if pv.Accepted.KeyID != f.key.id || pv.Accepted.Index != 0 {
			t.Errorf("accepted = %+v, want key %s at index 0", pv.Accepted, f.key.id)
		}
		if got, want := len(pv.Manifest.Signed.Files), len(samplePackage()); got != want {
			t.Errorf("%d files signed, want %d", got, want)
		}
		if !pv.Accepted.ExpiresAt.Equal(t0.Add(30 * 24 * time.Hour)) {
			t.Errorf("expires_at = %v", pv.Accepted.ExpiresAt)
		}

		t.Run("the file on disk parses and re-verifies from an in-memory copy", func(t *testing.T) {
			if _, err := f.v.VerifyPackage(ctx, mapPackageWithSignature(t, f.dir), f.opts); err != nil {
				t.Errorf("in-memory copy: %v", err)
			}
		})
	})

	t.Run("package signed with two keys verifies with either, and after the first is revoked", func(t *testing.T) {
		f := newFixture(t)
		second := newKey(t, "demo-library-publisher-2027")
		if _, err := f.store.RegisterKey(ctx, testApp, second.id, second.pub, "next"); err != nil {
			t.Fatal(err)
		}
		if _, err := SignPackageDir(f.dir, SignOptions{
			PackageIdentity: testID, Keys: []SigningKey{f.key.signing(), second.signing()},
			Now: t0, ExpiresAt: t0.Add(24 * time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		f.opts.PublisherKeyIDs = []string{f.key.id, second.id}
		if pv := f.mustVerify(); pv.Accepted.Index != 0 {
			t.Errorf("both valid: the first signature should be used, got index %d", pv.Accepted.Index)
		}

		if err := f.store.RevokeKey(ctx, testApp, f.key.id, "compromised"); err != nil {
			t.Fatal(err)
		}
		pv := f.mustVerify()
		if pv.Accepted.KeyID != second.id || pv.Accepted.Index != 1 {
			t.Errorf("with the first key revoked the second must carry it, got %+v", pv.Accepted)
		}

		if err := f.store.RevokeKey(ctx, testApp, second.id, "compromised"); err != nil {
			t.Fatal(err)
		}
		_, err := f.verify()
		wantFail(t, err, contracts.ErrUnauthenticated, ErrRevokedKey)
		var ve *VerifyError
		if !errors.As(err, &ve) || len(ve.Failures) != 2 {
			t.Errorf("both failures should be reported: %v", err)
		}
	})

	t.Run("append adds a signature to an unchanged package only", func(t *testing.T) {
		f := newFixture(t)
		second := newKey(t, "second")
		if _, err := f.store.RegisterKey(ctx, testApp, second.id, second.pub, ""); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(filepath.Join(f.dir, filepath.FromSlash(SignatureManifestPath)))
		if err != nil {
			t.Fatal(err)
		}
		sm, err := SignPackageDir(f.dir, SignOptions{
			PackageIdentity: testID, Keys: []SigningKey{second.signing()},
			Now: t0.Add(time.Hour), ExpiresAt: t0.Add(48 * time.Hour), Append: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(sm.Signatures) != 2 || sm.Signed.CreatedAt != f.sm.Signed.CreatedAt {
			t.Fatalf("append: %d signatures, created_at %s (want 2 and %s)", len(sm.Signatures), sm.Signed.CreatedAt, f.sm.Signed.CreatedAt)
		}
		f.opts.PublisherKeyIDs = []string{f.key.id, second.id}
		f.clk.Advance(2 * time.Hour)
		f.mustVerify()

		// Appending the same key again, or to a changed package, is refused and
		// leaves the file as it was.
		now := mustReadFile(t, filepath.Join(f.dir, filepath.FromSlash(SignatureManifestPath)))
		if _, err := SignPackageDir(f.dir, SignOptions{PackageIdentity: testID, Keys: []SigningKey{second.signing()}, Now: t0, ExpiresAt: t0.Add(time.Hour), Append: true}); err == nil {
			t.Error("the same key signed twice")
		}
		writeFiles(t, f.dir, map[string]string{"extra.txt": "x"})
		_, err = SignPackageDir(f.dir, SignOptions{PackageIdentity: testID, Keys: []SigningKey{newKey(t, "third").signing()}, Now: t0, ExpiresAt: t0.Add(time.Hour), Append: true})
		wantFail(t, err, contracts.ErrConflict, ErrTampered)
		if string(mustReadFile(t, filepath.Join(f.dir, filepath.FromSlash(SignatureManifestPath)))) != string(now) {
			t.Error("a refused append changed the signature manifest")
		}
		_ = before
	})

	t.Run("arbitrary payload under the platform application", func(t *testing.T) {
		clk := clock.NewFake(t0)
		st, _ := newTrustStore(t, clk)
		key := newKey(t, "alixai-audit-2026")
		if _, err := st.RegisterKey(ctx, PlatformApplicationID, key.id, key.pub, "audit signer"); err != nil {
			t.Fatal(err)
		}
		v := NewVerifier(st, clk)
		report := auditReport{Application: testApp, Passed: []string{"check-1", "check-3"}, Failed: []string{"check-2"}, Findings: 3, Notes: "<critical> & \"quoted\"\n"}
		sig, err := SignPayload(report, key.id, key.priv, t0, t0.Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		opts := PayloadOptions{ApplicationID: PlatformApplicationID}
		if _, err := v.VerifyPayload(ctx, report, []Signature{sig}, opts); err != nil {
			t.Fatalf("round trip: %v", err)
		}

		// The verifier works on canonical bytes, so the same report as a map, in
		// any key order, verifies too.
		asMap := map[string]any{"notes": report.Notes, "findings": 3, "failed": []string{"check-2"}, "passed": []string{"check-1", "check-3"}, "application": testApp}
		if _, err := v.VerifyPayload(ctx, asMap, []Signature{sig}, opts); err != nil {
			t.Errorf("same report as a map: %v", err)
		}

		for name, mutate := range map[string]func(r *auditReport){
			"a finding count":   func(r *auditReport) { r.Findings = 0 },
			"a passed check":    func(r *auditReport) { r.Passed = append(r.Passed, "check-2") },
			"the failed list":   func(r *auditReport) { r.Failed = nil },
			"one character":     func(r *auditReport) { r.Notes += "." },
			"the application":   func(r *auditReport) { r.Application = "other-app" },
			"list order":        func(r *auditReport) { r.Passed = []string{"check-3", "check-1"} },
			"nil versus empty":  func(r *auditReport) { r.Failed = []string{} },
			"unicode lookalike": func(r *auditReport) { r.Notes = "<critical> & \"quoted\" " },
		} {
			tampered := report
			tampered.Passed = append([]string(nil), report.Passed...)
			tampered.Failed = append([]string(nil), report.Failed...)
			mutate(&tampered)
			_, err := v.VerifyPayload(ctx, tampered, []Signature{sig}, opts)
			if err == nil {
				t.Errorf("a modified report (%s) verified", name)
				continue
			}
			wantFail(t, err, contracts.ErrUnauthenticated, ErrBadSignature)
		}

		// A platform signature does not authenticate anything for another application.
		_, err = v.VerifyPayload(ctx, report, []Signature{sig}, PayloadOptions{ApplicationID: testApp})
		wantFail(t, err, contracts.ErrUnauthenticated, ErrUnknownKey)
	})

	t.Run("signer refuses what it cannot sign safely", func(t *testing.T) {
		key := newKey(t, "k")
		for name, tc := range map[string]struct {
			payload   any
			keyID     string
			key       ed25519.PrivateKey
			signed    time.Time
			expires   time.Time
			wantErrOK bool
		}{
			"no expiry":          {map[string]any{"a": 1}, "k", key.priv, t0, time.Time{}, false},
			"expiry not later":   {map[string]any{"a": 1}, "k", key.priv, t0, t0, false},
			"expiry before":      {map[string]any{"a": 1}, "k", key.priv, t0, t0.Add(-time.Hour), false},
			"sub-second expiry":  {map[string]any{"a": 1}, "k", key.priv, t0, t0.Add(500 * time.Millisecond), false},
			"short private key":  {map[string]any{"a": 1}, "k", key.priv[:32], t0, t0.Add(time.Hour), false},
			"nil private key":    {map[string]any{"a": 1}, "k", nil, t0, t0.Add(time.Hour), false},
			"float in payload":   {map[string]any{"a": 1.5}, "k", key.priv, t0, t0.Add(time.Hour), false},
			"bad key id":         {map[string]any{"a": 1}, "has space", key.priv, t0, t0.Add(time.Hour), false},
			"wildcard key id":    {map[string]any{"a": 1}, "*", key.priv, t0, t0.Add(time.Hour), false},
			"empty key id":       {map[string]any{"a": 1}, "", key.priv, t0, t0.Add(time.Hour), false},
			"valid for contrast": {map[string]any{"a": 1}, "k", key.priv, t0, t0.Add(time.Hour), true},
		} {
			_, err := SignPayload(tc.payload, tc.keyID, tc.key, tc.signed, tc.expires)
			if tc.wantErrOK != (err == nil) {
				t.Errorf("%s: SignPayload error = %v, want success=%v", name, err, tc.wantErrOK)
			}
		}
	})
}

func mustReadFile(t testing.TB, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestUnknownKeyFails: a signature by a key the trust store does not hold for
// the application never verifies, however valid the signature is.
//
// SEC-058: unknown publisher key.
func TestUnknownKeyFails(t *testing.T) {
	ctx := context.Background()

	t.Run("empty trust store trusts nothing", func(t *testing.T) {
		f := newFixture(t)
		empty, _ := newTrustStore(t, f.clk)
		v := NewVerifier(empty, f.clk)
		_, err := v.VerifyPackageDir(ctx, f.dir, f.opts)
		wantFail(t, err, contracts.ErrUnauthenticated, ErrUnknownKey)
		var ve *VerifyError
		if !errors.As(err, &ve) || len(ve.Failures) != 1 || ve.Failures[0].Reason != ReasonUnknownKey {
			t.Errorf("failures = %+v", ve)
		}
	})

	t.Run("signed by a key that was never registered", func(t *testing.T) {
		f := newFixture(t)
		stranger := newKey(t, "stranger")
		signStranger(t, f, stranger)
		f.opts.PublisherKeyIDs = []string{stranger.id}
		_, err := f.verify()
		wantFail(t, err, contracts.ErrUnauthenticated, ErrUnknownKey)
	})

	t.Run("key registered for another application", func(t *testing.T) {
		f := newFixture(t)
		other := newKey(t, "other-publisher")
		if _, err := f.store.RegisterKey(ctx, "another-app", other.id, other.pub, "other app's key"); err != nil {
			t.Fatal(err)
		}
		signStranger(t, f, other)
		f.opts.PublisherKeyIDs = []string{other.id}
		_, err := f.verify()
		wantFail(t, err, contracts.ErrUnauthenticated, ErrUnknownKey)
	})

	t.Run("registered key, but the package does not name it as a publisher", func(t *testing.T) {
		f := newFixture(t)
		f.mustVerify()
		f.opts.PublisherKeyIDs = []string{"some-other-key"}
		_, err := f.verify()
		wantFail(t, err, contracts.ErrUnauthenticated, ErrKeyNotListed)

		f.opts.PublisherKeyIDs = nil
		_, err = f.verify()
		wantFail(t, err, contracts.ErrValidationFailed, ErrKeyNotListed) // a package naming no publisher cannot verify
	})

	t.Run("right key id, wrong private key", func(t *testing.T) {
		f := newFixture(t)
		impostor := newKey(t, f.key.id) // same id, different key pair
		signStranger(t, f, impostor)
		_, err := f.verify()
		wantFail(t, err, contracts.ErrUnauthenticated, ErrBadSignature)
	})

	t.Run("signed for another application or version", func(t *testing.T) {
		f := newFixture(t)
		f.opts.ApplicationID = "another-app"
		_, err := f.verify()
		wantFail(t, err, contracts.ErrUnauthenticated, ErrBindingMismatch)

		f = newFixture(t)
		f.opts.PackageVersion = "2.0.0"
		_, err = f.verify()
		wantFail(t, err, contracts.ErrUnauthenticated, ErrBindingMismatch)
	})

	t.Run("a different signature algorithm is not accepted", func(t *testing.T) {
		f := newFixture(t)
		rewriteManifest(t, f.dir, func(sm *SignatureManifest) { sm.Signatures[0].Algorithm = "rsa" })
		_, err := f.verify()
		wantFail(t, err, contracts.ErrSchemaInvalid, ErrMalformedManifest)

		// The payload API has no schema in front of it: the verifier itself refuses.
		_, err = f.v.VerifyPayload(ctx, map[string]int{"a": 1}, []Signature{{KeyID: f.key.id, Algorithm: "rsa", Signature: f.sm.Signatures[0].Signature, SignedAt: f.sm.Signatures[0].SignedAt, ExpiresAt: f.sm.Signatures[0].ExpiresAt}}, PayloadOptions{ApplicationID: testApp})
		wantFail(t, err, contracts.ErrUnauthenticated, ErrUnsupportedAlgorithm)
	})

	t.Run("no signatures, too many signatures, invalid application", func(t *testing.T) {
		f := newFixture(t)
		_, err := f.v.VerifyPayload(ctx, map[string]int{"a": 1}, nil, PayloadOptions{ApplicationID: testApp})
		wantFail(t, err, contracts.ErrUnauthenticated, ErrUnsigned)
		five := make([]Signature, MaxSignatures+1)
		_, err = f.v.VerifyPayload(ctx, map[string]int{"a": 1}, five, PayloadOptions{ApplicationID: testApp})
		wantFail(t, err, contracts.ErrSchemaInvalid, ErrMalformedSignature)
		_, err = f.v.VerifyPayload(ctx, map[string]int{"a": 1}, f.sm.Signatures, PayloadOptions{ApplicationID: "*"})
		wantFail(t, err, contracts.ErrValidationFailed)
	})
}

// signStranger replaces the package's signature manifest by one made with key.
func signStranger(t *testing.T, f *fixture, key keyPair) {
	t.Helper()
	if _, err := SignPackageDir(f.dir, SignOptions{
		PackageIdentity: testID, Keys: []SigningKey{key.signing()}, Now: t0, ExpiresAt: t0.Add(24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRevokedKeyFails: revoking a key makes every signature it ever made fail,
// including ones made before the revocation and ones that claim an earlier
// signing time; revocation cannot be undone. A retired key (normal rotation)
// keeps vouching for what it signed while it was active.
//
// SEC-058: revoked publisher key.
func TestRevokedKeyFails(t *testing.T) {
	ctx := context.Background()

	t.Run("revoked key", func(t *testing.T) {
		f := newFixture(t)
		f.mustVerify() // verifies right up to the revocation
		f.clk.Advance(time.Hour)
		if err := f.store.RevokeKey(ctx, testApp, f.key.id, "private key leaked"); err != nil {
			t.Fatal(err)
		}
		_, err := f.verify()
		wantFail(t, err, contracts.ErrUnauthenticated, ErrRevokedKey)
		var ve *VerifyError
		if !errors.As(err, &ve) || ve.Failures[0].Reason != ReasonRevokedKey {
			t.Errorf("failures = %+v", ve)
		}

		k, err := f.store.LookupKey(ctx, testApp, f.key.id)
		if err != nil || k.Status != KeyRevoked || k.RevokedReason != "private key leaked" || !k.RevokedAt.Equal(t0.Add(time.Hour)) {
			t.Errorf("stored key = %+v, %v", k, err)
		}
	})

	t.Run("revocation is permanent and idempotent", func(t *testing.T) {
		f := newFixture(t)
		if err := f.store.RevokeKey(ctx, testApp, f.key.id, "first"); err != nil {
			t.Fatal(err)
		}
		f.clk.Advance(time.Hour)
		if err := f.store.RevokeKey(ctx, testApp, f.key.id, "second"); err != nil {
			t.Errorf("revoking twice: %v", err)
		}
		k, _ := f.store.LookupKey(ctx, testApp, f.key.id)
		if k.RevokedReason != "first" || !k.RevokedAt.Equal(t0) {
			t.Errorf("a second revocation rewrote the first: %+v", k)
		}
		if err := f.store.RetireKey(ctx, testApp, f.key.id); !errors.Is(err, ErrRevokedKey) {
			t.Errorf("retiring a revoked key: %v", err)
		}
		// The database itself refuses to undo a revocation or delete a key,
		// whatever the Go code does.
		for _, stmt := range []string{
			`UPDATE trust_publisher_keys SET status = 'active', revoked_at = NULL`,
			`UPDATE trust_publisher_keys SET status = 'retired', retired_at = 1, revoked_at = NULL`,
			`UPDATE trust_publisher_keys SET revoked_at = 5`,
			`UPDATE trust_publisher_keys SET public_key = 'AAAA'`,
			`UPDATE trust_publisher_keys SET key_id = 'renamed'`,
			`DELETE FROM trust_publisher_keys`,
		} {
			if _, err := f.db.ExecContext(ctx, stmt); err == nil {
				t.Errorf("database accepted: %s", stmt)
			}
		}
		if _, err := f.verify(); err == nil {
			t.Error("package verifies after the revoked key was tampered with in the database")
		}
	})

	t.Run("revoking an unknown key is an error, not a silent no-op", func(t *testing.T) {
		f := newFixture(t)
		wantFail(t, f.store.RevokeKey(ctx, testApp, "nope", "x"), contracts.ErrValidationFailed, ErrUnknownKey)
		wantFail(t, f.store.RevokeKey(ctx, "another-app", f.key.id, "x"), contracts.ErrValidationFailed, ErrUnknownKey)
		f.mustVerify() // the registered key is untouched
	})

	t.Run("revoked key material cannot return under a new name or for another application", func(t *testing.T) {
		f := newFixture(t)
		if err := f.store.RevokeKey(ctx, testApp, f.key.id, "leaked"); err != nil {
			t.Fatal(err)
		}
		_, err := f.store.RegisterKey(ctx, testApp, "renamed-key", f.key.pub, "same key, new name")
		wantFail(t, err, contracts.ErrConflict, ErrKeyConflict)
		_, err = f.store.RegisterKey(ctx, "another-app", "renamed-key", f.key.pub, "same key, other app")
		wantFail(t, err, contracts.ErrConflict, ErrKeyConflict)
		_, err = f.store.RegisterKey(ctx, testApp, f.key.id, f.key.pub, "same id again")
		wantFail(t, err, contracts.ErrConflict, ErrKeyConflict)
		if _, err := f.verify(); err == nil {
			t.Error("package verifies although its only key was revoked")
		}
	})

	t.Run("key material revoked for one application verifies nothing for another", func(t *testing.T) {
		f := newFixture(t)
		// The same key material was registered for a second application before the compromise.
		if _, err := f.store.RegisterKey(ctx, "another-app", "shared-key", f.key.pub, "same material, other app"); err != nil {
			t.Fatal(err)
		}
		other := newKey(t, "other-key")
		if _, err := f.store.RegisterKey(ctx, "another-app", other.id, other.pub, "another key"); err != nil {
			t.Fatal(err)
		}
		payload := map[string]int{"a": 1}
		shared, err := SignPayload(payload, "shared-key", f.key.priv, t0, t0.Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		good, err := SignPayload(payload, other.id, other.priv, t0, t0.Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		forOther := PayloadOptions{ApplicationID: "another-app"}
		if _, err := f.v.VerifyPayload(ctx, payload, []Signature{shared}, forOther); err != nil {
			t.Fatalf("before the revocation: %v", err)
		}
		f.clk.Advance(time.Hour)
		if err := f.store.RevokeKey(ctx, testApp, f.key.id, "private key leaked"); err != nil {
			t.Fatal(err)
		}
		_, err = f.v.VerifyPayload(ctx, payload, []Signature{shared}, forOther)
		wantFail(t, err, contracts.ErrUnauthenticated, ErrRevokedKey)
		k, err := f.store.LookupKey(ctx, "another-app", "shared-key")
		if err != nil || k.Status != KeyRevoked || !k.RevokedAt.Equal(t0.Add(time.Hour)) {
			t.Errorf("the other application's row of the revoked material = %+v, %v", k, err)
		}
		if apps, err := f.store.ApplicationsSharingKey(ctx, testApp, f.key.id); err != nil || len(apps) != 1 || apps[0] != "another-app" {
			t.Errorf("ApplicationsSharingKey = %v, %v, want [another-app]", apps, err)
		}
		// The list an operator reads says the same as the lookup.
		listed, err := f.store.ListKeys(ctx, "another-app")
		if err != nil {
			t.Fatal(err)
		}
		for _, lk := range listed {
			if want := map[string]KeyStatus{"shared-key": KeyRevoked, other.id: KeyActive}[lk.KeyID]; lk.Status != want {
				t.Errorf("ListKeys shows %s as %s, want %s", lk.KeyID, lk.Status, want)
			}
		}
		// The other application's other keys are untouched: a payload it also signed still verifies.
		acc, err := f.v.VerifyPayload(ctx, payload, []Signature{shared, good}, forOther)
		if err != nil || acc.KeyID != other.id {
			t.Errorf("a payload signed by the revoked material and a good key = %+v, %v", acc, err)
		}
	})

	t.Run("a revoked key fails even when the signature claims an earlier time", func(t *testing.T) {
		f := newFixture(t)
		if err := f.store.RevokeKey(ctx, testApp, f.key.id, "leaked"); err != nil {
			t.Fatal(err)
		}
		// signed_at is whatever the signer says; revocation ignores it.
		old, err := SignPayload(map[string]int{"a": 1}, f.key.id, f.key.priv, t0.Add(-365*24*time.Hour), t0.Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.v.VerifyPayload(ctx, map[string]int{"a": 1}, []Signature{old}, PayloadOptions{ApplicationID: testApp})
		wantFail(t, err, contracts.ErrUnauthenticated, ErrRevokedKey)
	})

	t.Run("a retired key vouches only for signatures made before it was retired", func(t *testing.T) {
		f := newFixture(t)
		f.clk.Advance(time.Hour)
		next := newKey(t, "next")
		if _, err := f.store.RotateKey(ctx, testApp, f.key.id, next.id, next.pub, "rotation"); err != nil {
			t.Fatal(err)
		}
		retired, err := f.store.LookupKey(ctx, testApp, f.key.id)
		if err != nil || retired.Status != KeyRetired || !retired.RetiredAt.Equal(t0.Add(time.Hour)) {
			t.Fatalf("old key = %+v, %v", retired, err)
		}
		f.mustVerify() // signed at t0, before the rotation, still valid

		// A signature the retired key makes after retirement is refused.
		late, err := SignPayload(map[string]int{"a": 1}, f.key.id, f.key.priv, t0.Add(2*time.Hour), t0.Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		f.clk.Advance(3 * time.Hour)
		_, err = f.v.VerifyPayload(ctx, map[string]int{"a": 1}, []Signature{late}, PayloadOptions{ApplicationID: testApp})
		wantFail(t, err, contracts.ErrUnauthenticated, ErrRetiredKey)

		// The new key works.
		ok, err := SignPayload(map[string]int{"a": 1}, next.id, next.priv, t0.Add(2*time.Hour), t0.Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.v.VerifyPayload(ctx, map[string]int{"a": 1}, []Signature{ok}, PayloadOptions{ApplicationID: testApp}); err != nil {
			t.Errorf("new key: %v", err)
		}

		// Retirement is not reversible, and revoking a retired key is.
		if _, err := f.db.ExecContext(ctx, `UPDATE trust_publisher_keys SET status = 'active', retired_at = NULL WHERE key_id = ?`, f.key.id); err == nil {
			t.Error("database accepted reactivating a retired key")
		}
		if err := f.store.RevokeKey(ctx, testApp, f.key.id, "found out later"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.verify(); err == nil {
			t.Error("a package signed by a key revoked after retirement still verifies")
		}
	})
}

// TestExpiredSignatureFails: a signature is valid from signing until, and not
// including, its expires_at; after that, and for one dated in the future, it
// fails closed. Expiry is mandatory.
func TestExpiredSignatureFails(t *testing.T) {
	ctx := context.Background()
	expires := t0.Add(30 * 24 * time.Hour)

	f := newFixture(t)
	f.mustVerify()

	f.clk.Set(expires.Add(-time.Second))
	f.mustVerify() // one second before expiry

	f.clk.Set(expires)
	_, err := f.verify()
	wantFail(t, err, contracts.ErrExpired, ErrSignatureExpired) // at expires_at it is already invalid

	f.clk.Set(expires.Add(365 * 24 * time.Hour))
	_, err = f.verify()
	wantFail(t, err, contracts.ErrExpired, ErrSignatureExpired)
	var ve *VerifyError
	if !errors.As(err, &ve) || ve.Failures[0].Reason != ReasonExpired {
		t.Errorf("failures = %+v", ve)
	}

	t.Run("re-signing renews the package", func(t *testing.T) {
		if _, err := SignPackageDir(f.dir, SignOptions{PackageIdentity: testID, Keys: []SigningKey{f.key.signing()}, Now: f.clk.Now(), ExpiresAt: f.clk.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		f.mustVerify()
	})

	t.Run("expiry does not mask a failure that is not expiry", func(t *testing.T) {
		g := newFixture(t)
		other := newKey(t, g.key.id)
		signStranger(t, g, other) // wrong private key for the registered id
		g.clk.Set(t0.Add(365 * 24 * time.Hour))
		_, err := g.verify()
		wantFail(t, err, contracts.ErrUnauthenticated, ErrBadSignature) // an invalid signature is not "just expired"
		if errors.Is(err, ErrSignatureExpired) {
			t.Error("an invalid signature was reported as expired")
		}
	})

	t.Run("expired and revoked together is not reported as plain expiry", func(t *testing.T) {
		g := newFixture(t)
		second := newKey(t, "second")
		if _, err := g.store.RegisterKey(ctx, testApp, second.id, second.pub, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := SignPackageDir(g.dir, SignOptions{PackageIdentity: testID, Keys: []SigningKey{g.key.signing(), second.signing()}, Now: t0, ExpiresAt: t0.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		g.opts.PublisherKeyIDs = []string{g.key.id, second.id}
		if err := g.store.RevokeKey(ctx, testApp, g.key.id, "x"); err != nil {
			t.Fatal(err)
		}
		g.clk.Advance(2 * time.Hour)
		_, err := g.verify()
		wantFail(t, err, contracts.ErrUnauthenticated, ErrRevokedKey, ErrSignatureExpired)
	})

	t.Run("a signature from the future", func(t *testing.T) {
		g := newFixture(t)
		g.clk.Set(t0.Add(-ClockSkew - time.Minute)) // verifier's clock is behind the signer's
		_, err := g.verify()
		wantFail(t, err, contracts.ErrUnauthenticated, ErrSignatureNotYetValid)

		g.clk.Set(t0.Add(-ClockSkew)) // within the tolerance
		g.mustVerify()
	})

	t.Run("a signature without an expiry is not a signature", func(t *testing.T) {
		g := newFixture(t)
		rewriteManifest(t, g.dir, func(sm *SignatureManifest) { sm.Signatures[0].ExpiresAt = "" })
		_, err := g.verify()
		wantFail(t, err, contracts.ErrSchemaInvalid, ErrMalformedManifest)

		raw := mustReadFile(t, filepath.Join(g.dir, filepath.FromSlash(SignatureManifestPath)))
		if _, err := ParseSignatureManifest(removeKey(t, raw, "expires_at")); err == nil {
			t.Error("a signature with no expires_at key parsed")
		}

		// expires_at not later than signed_at is refused as well.
		g = newFixture(t)
		rewriteManifest(t, g.dir, func(sm *SignatureManifest) { sm.Signatures[0].ExpiresAt = sm.Signatures[0].SignedAt })
		_, err = g.verify()
		wantFail(t, err, contracts.ErrSchemaInvalid, ErrMalformedManifest)
	})
}

// mapPackageWithSignature reads the package directory into memory.
func mapPackageWithSignature(t testing.TB, dir string) fstest.MapFS {
	t.Helper()
	m := fstest.MapFS{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		m[filepath.ToSlash(rel)] = &fstest.MapFile{Data: data}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func removeKey(t testing.TB, raw []byte, key string) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, s := range doc["signatures"].([]any) {
		delete(s.(map[string]any), key)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
