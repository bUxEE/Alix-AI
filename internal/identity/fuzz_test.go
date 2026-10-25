package identity_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// nopReplay never refuses, so a fuzz harness can present the same valid seed
// again and again; replay behaviour has its own tests.
type nopReplay struct{}

func (nopReplay) Consume(context.Context, contracts.Scope, string, time.Time) error { return nil }

// fuzzRig builds a verifier around a deterministic, test-only key (derived from
// a fixed string, so a saved fuzz corpus entry reproduces; it protects nothing)
// and a fixed clock.
func fuzzRig(tb testing.TB) (*identity.Verifier, *identitytest.Issuer, *clock.Fake) {
	tb.Helper()
	seed := sha256.Sum256([]byte("alixai identity fuzz test key: not a secret, protects nothing"))
	clk := clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	iss := identitytest.NewIssuer(tb, identitytest.IssuerConfig{Clock: clk, Key: ed25519.NewKeyFromSeed(seed[:])})
	keys := identity.NewKeyRegistry(nil)
	if err := keys.Register(iss.Key()); err != nil {
		tb.Fatal(err)
	}
	v, err := identity.NewVerifier(identity.Options{
		DeploymentID: identitytest.DefaultDeployment,
		Keys:         keys,
		Replay:       nopReplay{},
		Clock:        clk,
	})
	if err != nil {
		tb.Fatal(err)
	}
	return v, iss, clk
}

// FuzzTokenVerify feeds arbitrary bytes to the verifier as a compact token.
// M5-08 runs it (go test -fuzz=FuzzTokenVerify ./internal/identity/).
//
// Properties: Verify never panics, every refusal is a typed contracts error
// (never a bare error that could be read as something benign), and a token is
// accepted only if it is byte-for-byte the one valid token the harness signed.
// The signature is checked before any claim is read and every segment has
// exactly one accepted encoding, so no mutation of the valid token can verify;
// if one ever does, that is a malleability bug.
//
// The seeds also run as ordinary tests under go test ./..., so they must pass
// on every run.
func FuzzTokenVerify(f *testing.F) {
	v, iss, _ := fuzzRig(f)
	valid := iss.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimTokenID, "fuzz-seed-nonce-000001") })
	h, p, s := strings.Split(valid, ".")[0], strings.Split(valid, ".")[1], strings.Split(valid, ".")[2]

	for _, seed := range []string{
		valid,
		"", ".", "..", "...", "a.b.c", "Bearer " + valid, valid + ".", valid + "\n", " " + valid,
		h + "." + p, h + "." + p + ".", h + ".." + s, "." + p + "." + s,
		h + "." + p + "." + s[:len(s)-1], h + "." + p[:len(p)-1] + "." + s,
		strings.ToUpper(valid), strings.Replace(valid, ".", "..", 1),
		identitytest.Raw(`{"alg":"none"}`, `{}`, nil),
		identitytest.Raw(`{"alg":"EdDSA","typ":"alixai-context+jwt","kid":"app_demo-key-1"}`, `{}`, iss.PrivateKey()),
		identitytest.Raw(`{"alg":"EdDSA","alg":"none","typ":"alixai-context+jwt","kid":"app_demo-key-1"}`, `{}`, iss.PrivateKey()),
		identitytest.Raw(`[]`, `[]`, iss.PrivateKey()),
		strings.Repeat("A", identity.MaxTokenSize+1),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, token string) {
		got, err := v.Verify(context.Background(), token)
		if err == nil {
			if token != valid {
				t.Fatalf("accepted a token that is not the one valid token: %q", token)
			}
			if got == nil || got.Scope.Validate() != nil {
				t.Fatalf("accepted with an invalid scope: %+v", got)
			}
			return
		}
		if got != nil {
			t.Fatalf("returned a result alongside error %v", err)
		}
		var ce *contracts.Error
		if !errors.As(err, &ce) || !ce.Code.Valid() || ce.Code == contracts.ErrInternal {
			t.Fatalf("refusal is not a typed identity error: %v", err)
		}
		if token == valid {
			t.Fatalf("the valid token was refused: %v", err)
		}
	})
}

// FuzzSignedPayload goes one level deeper: the harness signs whatever header
// and payload JSON the fuzzer produces, so the strict JSON and claim rules are
// reached instead of stopping at the signature. (It is named so that the
// -fuzz=FuzzTokenVerify pattern does not also match it.)
//
// Properties: no panic; and whenever a token is accepted, the Scope it yields
// is valid, carries the configured deployment and the signing key's own
// application, and the token's validity window is within the profile's maximum.
func FuzzSignedPayload(f *testing.F) {
	v, iss, clk := fuzzRig(f)
	validHeader := mustJSON(f, iss.Draft().Header)
	validPayload := mustJSON(f, iss.Draft().Claims)

	f.Add(validHeader, validPayload)
	f.Add(`{"alg":"EdDSA","typ":"alixai-context+jwt","kid":"app_demo-key-1"}`, `{}`)
	f.Add(validHeader, `{"iss":null}`)
	f.Add(validHeader, strings.Replace(validPayload, `"roles":["member"]`, `"roles":[null]`, 1))
	f.Add(validHeader, strings.Replace(validPayload, `"tenant_id":"ten_acme"`, `"tenant_id":"*"`, 1))
	f.Add(validHeader, strings.Replace(validPayload, `"tenant_id":"ten_acme"`, `"tenant_id":"a","tenant_id":"b"`, 1))
	f.Add(validHeader, strings.Replace(validPayload, `"exp":`, `"exp":9223372036854775807,"x":`, 1))
	f.Add(validHeader, strings.Replace(validPayload, `"workspace_id":"wsp_main"`, `"workspace_id":"\ud800"`, 1))
	f.Add(`{"alg":"none","typ":"alixai-context+jwt","kid":"app_demo-key-1"}`, validPayload)
	f.Add(validHeader, "\xff\xfe")
	f.Add("", "")

	f.Fuzz(func(t *testing.T, header, payload string) {
		token := identitytest.Raw(header, payload, iss.PrivateKey())
		got, err := v.Verify(context.Background(), token)
		if err != nil {
			var ce *contracts.Error
			if !errors.As(err, &ce) || !ce.Code.Valid() || ce.Code == contracts.ErrInternal {
				t.Fatalf("refusal is not a typed identity error: %v", err)
			}
			return
		}
		sc := got.Scope
		if err := sc.Validate(); err != nil {
			t.Fatalf("accepted with an invalid scope: %v\n%s\n%s", err, header, payload)
		}
		if sc.DeploymentID != identitytest.DefaultDeployment {
			t.Fatalf("deployment %q did not come from configuration", sc.DeploymentID)
		}
		if sc.ApplicationID != iss.ApplicationID {
			t.Fatalf("application %q is not the signing key's", sc.ApplicationID)
		}
		if life := got.ExpiresAt.Sub(got.IssuedAt); life <= 0 || life > identity.MaxLifetime {
			t.Fatalf("accepted a validity window of %s", life)
		}
		if got.ExpiresAt.Sub(clk.Now()) > identity.MaxLifetime+identity.ClockSkew {
			t.Fatalf("accepted a token expiring %s from now", got.ExpiresAt.Sub(clk.Now()))
		}
		if !clk.Now().Before(got.ExpiresAt.Add(identity.ClockSkew)) {
			t.Fatalf("accepted an expired token")
		}
	})
}
