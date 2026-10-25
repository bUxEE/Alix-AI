package identity_test

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// SEC-031: a valid token verifies, and every level of the Scope is the one the
// token asserted, except the deployment, which comes from configuration.
func TestValidTokenProducesScope(t *testing.T) {
	env := identitytest.NewEnv(t)
	token := env.Issuer.Token()

	v, err := env.Verifier.Verify(bg, token)
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if diff := cmp.Diff(identitytest.ExpectedScope(), v.Scope); diff != "" {
		t.Fatalf("scope mismatch (-want +got):\n%s", diff)
	}
	if err := v.Scope.Validate(); err != nil {
		t.Fatalf("verified scope does not validate: %v", err)
	}
	if !v.Scope.Permits("reservations.create") || v.Scope.Permits("reservations.delete") {
		t.Fatalf("capability scopes not honoured: %v", v.Scope.CapabilityScopes)
	}
	if v.Issuer != env.Issuer.Issuer || v.KeyID != env.Issuer.KeyID || v.TokenID == "" {
		t.Fatalf("verified metadata wrong: %+v", v)
	}
	if want := env.Clock.Now().Add(identitytest.DefaultLifetime).Unix(); v.ExpiresAt.Unix() != want {
		t.Fatalf("ExpiresAt = %v, want unix %d", v.ExpiresAt, want)
	}
	if got := replayRows(t, env); got != 1 {
		t.Fatalf("replay rows = %d, want 1 (the accepted nonce)", got)
	}
}

// The deployment level is never a claim: a token that carries one is refused,
// and the Scope's deployment is the configured one.
func TestDeploymentCannotComeFromToken(t *testing.T) {
	env := identitytest.NewEnv(t)
	for _, claim := range []string{"deployment_id", "deployment", "dep"} {
		token := env.Issuer.Token(func(d *identitytest.Draft) { d.Set(claim, "dep_other") })
		_, err := env.Verifier.Verify(bg, token)
		wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonBadClaims)
	}
	if replayRows(t, env) != 0 {
		t.Fatal("a refused token burned a nonce")
	}
}

// SEC-032: a token for another audience is refused.
func TestWrongAudienceRejected(t *testing.T) {
	env := identitytest.NewEnv(t)

	for name, aud := range map[string]any{
		"another deployment": "dep_other",
		"one-element array":  []string{"dep_other"},
		"empty":              "",
	} {
		t.Run(name, func(t *testing.T) {
			token := env.Issuer.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimAudience, aud) })
			_, err := env.Verifier.Verify(bg, token)
			if name == "empty" {
				wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonBadClaims)
				return
			}
			wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonWrongAudience)
		})
	}

	t.Run("several audiences are refused even if one matches", func(t *testing.T) {
		token := env.Issuer.Token(func(d *identitytest.Draft) {
			d.Set(identity.ClaimAudience, []string{identitytest.DefaultDeployment, "dep_other"})
		})
		_, err := env.Verifier.Verify(bg, token)
		wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonBadClaims)
	})

	t.Run("a one-element array naming this audience is accepted", func(t *testing.T) {
		token := env.Issuer.Token(func(d *identitytest.Draft) {
			d.Set(identity.ClaimAudience, []string{identitytest.DefaultDeployment})
		})
		if _, err := env.Verifier.Verify(bg, token); err != nil {
			t.Fatalf("rejected: %v", err)
		}
	})

	t.Run("a configured audience replaces the deployment id", func(t *testing.T) {
		e2 := identitytest.NewEnv(t, func(o *identity.Options) { o.Audience = "https://alixai.example.test" })
		_, err := e2.Verifier.Verify(bg, e2.Issuer.Token())
		wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonWrongAudience)
		ok := e2.Issuer.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimAudience, "https://alixai.example.test") })
		if _, err := e2.Verifier.Verify(bg, ok); err != nil {
			t.Fatalf("rejected: %v", err)
		}
	})

	// Only the one accepted token above (in the subtests that accept) holds a nonce.
	if got := replayRows(t, env); got != 1 {
		t.Fatalf("replay rows = %d, want 1: refused tokens must burn nothing", got)
	}
}

// SEC-032: a token for another issuer or application is refused, and one
// application's key never authorises another application.
func TestWrongIssuerRejected(t *testing.T) {
	env := identitytest.NewEnv(t)

	t.Run("iss differs from the issuer registered for the key", func(t *testing.T) {
		token := env.Issuer.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimIssuer, "https://evil.example.test") })
		_, err := env.Verifier.Verify(bg, token)
		wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonWrongIssuer)
	})

	// A second application with its own key.
	other := identitytest.NewIssuer(t, identitytest.IssuerConfig{ApplicationID: "app_other", Clock: env.Clock})
	if err := env.Keys.Register(other.Key()); err != nil {
		t.Fatal(err)
	}

	t.Run("the other application's token verifies as that application", func(t *testing.T) {
		v, err := env.Verifier.Verify(bg, other.Token())
		if err != nil {
			t.Fatal(err)
		}
		if v.Scope.ApplicationID != "app_other" {
			t.Fatalf("application = %q", v.Scope.ApplicationID)
		}
	})

	t.Run("a key cannot claim another application", func(t *testing.T) {
		token := env.Issuer.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimApplication, "app_other") })
		_, err := env.Verifier.Verify(bg, token)
		wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonWrongApplication)
	})

	t.Run("another application's key id with this application's signature", func(t *testing.T) {
		token := env.Issuer.Token(func(d *identitytest.Draft) { d.SetHeader(identity.HeaderKeyID, other.KeyID) })
		_, err := env.Verifier.Verify(bg, token)
		wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonBadSignature)
	})

	t.Run("unknown key id", func(t *testing.T) {
		token := env.Issuer.Token(func(d *identitytest.Draft) { d.SetHeader(identity.HeaderKeyID, "no-such-key") })
		_, err := env.Verifier.Verify(bg, token)
		wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonUnknownKey)
	})

	t.Run("signed by a key that is not registered", func(t *testing.T) {
		_, stranger, _ := ed25519.GenerateKey(rand.Reader)
		token := env.Issuer.Draft().SignWith(stranger).Sign()
		_, err := env.Verifier.Verify(bg, token)
		wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonBadSignature)
	})
}

// SEC-033: an expired token is refused, with the exact skew boundary.
func TestExpiredRejected(t *testing.T) {
	env := identitytest.NewEnv(t)
	skew := identity.ClockSkew

	expired := env.Issuer.Draft().Times(-6*minute, -6*minute, -minute).Sign()
	_, err := env.Verifier.Verify(bg, expired)
	wantReject(t, err, contracts.ErrExpired, identity.ReasonExpired)

	// exp + skew is the first refused instant; one second earlier still passes.
	justInside := env.Issuer.Draft().Times(-5*minute-skew, -5*minute-skew, -skew+time.Second).Sign()
	if _, err := env.Verifier.Verify(bg, justInside); err != nil {
		t.Fatalf("token inside the skew allowance rejected: %v", err)
	}
	atBoundary := env.Issuer.Draft().Times(-5*minute-skew, -5*minute-skew, -skew).Sign()
	_, err = env.Verifier.Verify(bg, atBoundary)
	wantReject(t, err, contracts.ErrExpired, identity.ReasonExpired)

	// A token that was fine becomes expired as the clock moves.
	token := env.Issuer.Token()
	env.Clock.Advance(identitytest.DefaultLifetime + skew)
	_, err = env.Verifier.Verify(bg, token)
	wantReject(t, err, contracts.ErrExpired, identity.ReasonExpired)

	if got := replayRows(t, env); got != 1 {
		t.Fatalf("replay rows = %d, want 1 (only the token inside the skew allowance)", got)
	}
}

// SEC-033: a validity window above the maximum is refused even though the
// signature is valid, including windows stretched through iat or nbf.
func TestLifetimeAboveMaximumRejected(t *testing.T) {
	env := identitytest.NewEnv(t)
	max := identity.MaxLifetime

	cases := []struct {
		name          string
		iat, nbf, exp time.Duration
		reason        identity.Reason
	}{
		{"one second over", 0, 0, max + time.Second, identity.ReasonLifetime},
		{"an hour", 0, 0, time.Hour, identity.ReasonLifetime},
		{"iat in the future stretches the window", time.Hour, 0, time.Hour + max, identity.ReasonLifetime},
		{"nbf in the past stretches the window", 0, -time.Hour, 10 * time.Minute, identity.ReasonLifetime},
		{"expires before it starts", 0, 0, -time.Second, identity.ReasonBadClaims},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			token := env.Issuer.Draft().Times(c.iat, c.nbf, c.exp).Sign()
			_, err := env.Verifier.Verify(bg, token)
			wantReject(t, err, contracts.ErrUnauthenticated, c.reason)
		})
	}

	t.Run("exactly the maximum is accepted", func(t *testing.T) {
		token := env.Issuer.Draft().Lifetime(max).Sign()
		if _, err := env.Verifier.Verify(bg, token); err != nil {
			t.Fatalf("rejected: %v", err)
		}
	})

	t.Run("a lower configured maximum applies", func(t *testing.T) {
		e2 := identitytest.NewEnv(t, func(o *identity.Options) { o.MaxLifetime = 2 * minute })
		_, err := e2.Verifier.Verify(bg, e2.Issuer.Draft().Lifetime(3*minute).Sign())
		wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonLifetime)
		if _, err := e2.Verifier.Verify(bg, e2.Issuer.Draft().Lifetime(2*minute).Sign()); err != nil {
			t.Fatalf("a token at the configured maximum was rejected: %v", err)
		}
	})

	t.Run("the verifier refuses to be configured above the ceiling", func(t *testing.T) {
		e := identitytest.NewEnv(t)
		_, err := identity.NewVerifier(identity.Options{
			DeploymentID: identitytest.DefaultDeployment, Keys: e.Keys, Replay: e.Replay,
			MaxLifetime: identity.MaxLifetime + time.Second,
		})
		if !contracts.HasCode(err, contracts.ErrValidationFailed) {
			t.Fatalf("NewVerifier accepted a lifetime above the ceiling: %v", err)
		}
	})

	t.Run("NumericDates must be plain integers", func(t *testing.T) {
		for name, v := range map[string]any{
			"float":    float64(env.Clock.Now().Unix()) + 0.5,
			"negative": -1,
			"huge":     int64(9_007_199_254_740_991),
			"string":   "1790000000",
			"null":     nil,
		} {
			token := env.Issuer.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimExpiry, v) })
			_, err := env.Verifier.Verify(bg, token)
			if err == nil {
				t.Fatalf("%s exp accepted", name)
			}
			if r := identity.ReasonOf(err); r != identity.ReasonBadClaims {
				t.Fatalf("%s exp: reason = %q, want bad_claims", name, r)
			}
		}
	})
}

// SEC-033: a token that is not valid yet is refused; the skew allowance is
// small and exact.
func TestFutureNotBeforeRejected(t *testing.T) {
	env := identitytest.NewEnv(t)
	skew := identity.ClockSkew

	_, err := env.Verifier.Verify(bg, env.Issuer.Draft().Times(0, 10*minute, 15*minute).Sign())
	wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonNotYetValid)

	_, err = env.Verifier.Verify(bg, env.Issuer.Draft().Times(0, skew+time.Second, 5*minute).Sign())
	wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonNotYetValid)

	if _, err := env.Verifier.Verify(bg, env.Issuer.Draft().Times(0, skew, 5*minute).Sign()); err != nil {
		t.Fatalf("nbf inside the skew allowance rejected: %v", err)
	}

	// iat in the future is "issued in the future": refused the same way.
	_, err = env.Verifier.Verify(bg, env.Issuer.Draft().Times(10*minute, 0, 14*minute).Sign())
	wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonNotYetValid)

	// Once the clock reaches nbf the token is good.
	later := env.Issuer.Draft().Times(0, 10*minute, 14*minute).Sign()
	env.Clock.Advance(10 * minute)
	if _, err := env.Verifier.Verify(bg, later); err != nil {
		t.Fatalf("token rejected after its nbf: %v", err)
	}
}

// SEC-034: a nonce is single use.
func TestReplayedJTIRejected(t *testing.T) {
	env := identitytest.NewEnv(t)
	token := env.Issuer.Token()

	if _, err := env.Verifier.Verify(bg, token); err != nil {
		t.Fatalf("first presentation rejected: %v", err)
	}
	_, err := env.Verifier.Verify(bg, token)
	wantReject(t, err, contracts.ErrReplayed, identity.ReasonReplayed)

	// The same nonce in a freshly signed token is the same nonce.
	jti := "fixed-nonce-0123456789"
	first := env.Issuer.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimTokenID, jti) })
	if _, err := env.Verifier.Verify(bg, first); err != nil {
		t.Fatal(err)
	}
	second := env.Issuer.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimTokenID, jti).Lifetime(10 * minute) })
	_, err = env.Verifier.Verify(bg, second)
	wantReject(t, err, contracts.ErrReplayed, identity.ReasonReplayed)

	// Presented concurrently, exactly one wins.
	racing := env.Issuer.Token()
	const workers = 16
	var wg sync.WaitGroup
	results := make([]error, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = env.Verifier.Verify(bg, racing)
		}()
	}
	wg.Wait()
	accepted := 0
	for _, e := range results {
		switch {
		case e == nil:
			accepted++
		case !contracts.HasCode(e, contracts.ErrReplayed):
			t.Fatalf("unexpected failure under contention: %v", e)
		}
	}
	if accepted != 1 {
		t.Fatalf("%d of %d concurrent presentations accepted, want exactly 1", accepted, workers)
	}
}

// SEC-035: a missing level is scope_missing, never a default.
func TestMissingWorkspaceRejected(t *testing.T) {
	env := identitytest.NewEnv(t)

	absent := env.Issuer.Token(func(d *identitytest.Draft) { d.Drop(identity.ClaimWorkspace) })
	_, err := env.Verifier.Verify(bg, absent)
	wantScopeLevels(t, err, contracts.ErrScopeMissing, "workspace")

	for name, v := range map[string]any{"empty": "", "blank": "   "} {
		token := env.Issuer.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimWorkspace, v) })
		_, err := env.Verifier.Verify(bg, token)
		if !contracts.HasCode(err, contracts.ErrScopeMissing) {
			t.Fatalf("%s workspace: err = %v, want scope_missing", name, err)
		}
	}
	if replayRows(t, env) != 0 {
		t.Fatal("a token with a missing level burned a nonce")
	}
}

// SEC-035: every other level behaves the same way.
func TestEveryMissingLevelIsScopeMissing(t *testing.T) {
	env := identitytest.NewEnv(t)
	cases := []struct{ claim, level string }{
		{identity.ClaimApplication, "application"},
		{identity.ClaimTenant, "tenant"},
		{identity.ClaimWorkspace, "workspace"},
		{identity.ClaimSubject, "principal"},
		{identity.ClaimRoles, "roles"},
		{identity.ClaimCapabilityScopes, "capability_scopes"},
		{identity.ClaimSession, "session"},
	}
	for _, c := range cases {
		t.Run(c.level, func(t *testing.T) {
			_, err := env.Verifier.Verify(bg, env.Issuer.Token(func(d *identitytest.Draft) { d.Drop(c.claim) }))
			wantScopeLevels(t, err, contracts.ErrScopeMissing, c.level)
		})
	}
	t.Run("empty role and capability lists", func(t *testing.T) {
		for _, claim := range []string{identity.ClaimRoles, identity.ClaimCapabilityScopes} {
			_, err := env.Verifier.Verify(bg, env.Issuer.Token(func(d *identitytest.Draft) { d.Set(claim, []string{}) }))
			if !contracts.HasCode(err, contracts.ErrScopeMissing) {
				t.Fatalf("empty %s: err = %v, want scope_missing", claim, err)
			}
		}
	})
	t.Run("several levels missing are all named, in order", func(t *testing.T) {
		token := env.Issuer.Token(func(d *identitytest.Draft) {
			d.Drop(identity.ClaimWorkspace).Drop(identity.ClaimTenant).Drop(identity.ClaimSession)
		})
		_, err := env.Verifier.Verify(bg, token)
		wantScopeLevels(t, err, contracts.ErrScopeMissing, "tenant", "workspace", "session")
	})
}

// SEC-035: a present but illegal value is scope_violation, through the same
// Scope.Validate the whole system uses.
func TestMalformedLevelIsScopeViolation(t *testing.T) {
	env := identitytest.NewEnv(t)
	long := strings.Repeat("a", contracts.MaxScopeValueLen+1)
	cases := []struct {
		name, claim string
		value       any
		level       string
	}{
		{"wildcard tenant", identity.ClaimTenant, "*", "tenant"},
		{"wildcard embedded", identity.ClaimWorkspace, "wsp_*", "workspace"},
		{"escaped wildcard", identity.ClaimTenant, "*", "tenant"},
		{"padded principal", identity.ClaimSubject, " usr_alice", "principal"},
		{"trailing space session", identity.ClaimSession, "ses_1 ", "session"},
		{"control character", identity.ClaimWorkspace, "wsp\u0007x", "workspace"},
		{"NUL", identity.ClaimTenant, "ten\u0000x", "tenant"},
		{"too long", identity.ClaimTenant, long, "tenant"},
		{"wildcard role", identity.ClaimRoles, []string{"member", "*"}, "roles"},
		{"wildcard capability", identity.ClaimCapabilityScopes, []string{"*"}, "capability_scopes"},
		{"padded capability", identity.ClaimCapabilityScopes, []string{" a.b"}, "capability_scopes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			token := env.Issuer.Token(func(d *identitytest.Draft) { d.Set(c.claim, c.value) })
			_, err := env.Verifier.Verify(bg, token)
			wantScopeLevels(t, err, contracts.ErrScopeViolation, c.level)
		})
	}
	if replayRows(t, env) != 0 {
		t.Fatal("a refused token burned a nonce")
	}
}

// Mistyped claims are not "missing": the token is malformed.
func TestMistypedClaimsRejected(t *testing.T) {
	env := identitytest.NewEnv(t)
	cases := map[string]struct {
		claim string
		value any
	}{
		"tenant number":     {identity.ClaimTenant, 42},
		"tenant null":       {identity.ClaimTenant, nil},
		"tenant array":      {identity.ClaimTenant, []string{"a"}},
		"tenant object":     {identity.ClaimTenant, map[string]any{"id": "a"}},
		"roles string":      {identity.ClaimRoles, "member"},
		"roles null":        {identity.ClaimRoles, nil},
		"roles with null":   {identity.ClaimRoles, []any{"member", nil}},
		"roles with number": {identity.ClaimRoles, []any{"member", 7}},
		"jti too short":     {identity.ClaimTokenID, "short"},
		"jti with spaces":   {identity.ClaimTokenID, "not allowed nonce 123"},
		"jti number":        {identity.ClaimTokenID, 1234567890123456},
		"iss null":          {identity.ClaimIssuer, nil},
		"too many roles":    {identity.ClaimRoles, manyStrings(65)},
		"unknown claim":     {"admin", true},
		"upper-case claim":  {"Tenant_ID", "ten_acme"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			token := env.Issuer.Token(func(d *identitytest.Draft) { d.Set(c.claim, c.value) })
			_, err := env.Verifier.Verify(bg, token)
			wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonBadClaims)
		})
	}
	t.Run("duplicate members in the payload", func(t *testing.T) {
		const payload = `{"tenant_id":"ten_a","tenant_id":"ten_b"}`
		token := identitytest.Raw(rawHeader(env), payload, env.Issuer.PrivateKey())
		_, err := env.Verifier.Verify(bg, token)
		wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonBadClaims)
	})
	t.Run("duplicate roles are collapsed", func(t *testing.T) {
		token := env.Issuer.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimRoles, []string{"member", "admin", "member"}) })
		v, err := env.Verifier.Verify(bg, token)
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff([]string{"member", "admin"}, v.Scope.Roles); diff != "" {
			t.Fatalf("roles (-want +got):\n%s", diff)
		}
	})
}

func manyStrings(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("r%d", i)
	}
	return out
}

func rawHeader(env *identitytest.Env) string {
	return fmt.Sprintf(`{"alg":%q,"typ":%q,"kid":%q}`, identity.Algorithm, identity.TokenType, env.Issuer.KeyID)
}

// SEC-031: unsigned, "none" and substituted algorithms are refused, and the
// verifier accepts exactly one algorithm name.
func TestUnsignedOrNoneAlgorithmRejected(t *testing.T) {
	env := identitytest.NewEnv(t)
	payload := func() string {
		d := env.Issuer.Draft()
		return mustJSON(t, d.Claims)
	}
	otherName := "Ed25519"
	if identity.Algorithm == "Ed25519" {
		otherName = "EdDSA"
	}

	header := func(alg any) string {
		return mustJSON(t, map[string]any{"alg": alg, "typ": identity.TokenType, "kid": env.Issuer.KeyID})
	}

	// Tokens whose signature segment is the right size, so the refusal can
	// only come from the header: the strongest form of "alg is not honoured".
	junkSig := base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	headerReasons := map[string]string{}
	for _, alg := range []any{
		"none", "None", "NONE", "nOnE", "", nil, 0, true, []string{identity.Algorithm},
		otherName,
		"HS256", "HS384", "HS512", "RS256", "PS256", "ES256", "ES384", "ES512",
		identity.Algorithm + " ", " " + identity.Algorithm, strings.ToLower(identity.Algorithm), strings.ToUpper(identity.Algorithm),
	} {
		name := fmt.Sprintf("alg=%v", alg)
		headerReasons[name] = header(alg)
	}
	for name, h := range headerReasons {
		t.Run(name+" with the registered key's signature", func(t *testing.T) {
			_, err := env.Verifier.Verify(bg, identitytest.Raw(h, payload(), env.Issuer.PrivateKey()))
			wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonBadHeader)
		})
		t.Run(name+" with a junk signature", func(t *testing.T) {
			h64 := base64.RawURLEncoding.EncodeToString([]byte(h))
			p64 := base64.RawURLEncoding.EncodeToString([]byte(payload()))
			_, err := env.Verifier.Verify(bg, h64+"."+p64+"."+junkSig)
			wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonBadHeader)
		})
	}

	t.Run("alg absent", func(t *testing.T) {
		h := mustJSON(t, map[string]any{"typ": identity.TokenType, "kid": env.Issuer.KeyID})
		_, err := env.Verifier.Verify(bg, identitytest.Raw(h, payload(), env.Issuer.PrivateKey()))
		wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonBadHeader)
	})

	t.Run("none with an empty signature segment", func(t *testing.T) {
		_, err := env.Verifier.Verify(bg, identitytest.Raw(header("none"), payload(), nil))
		wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonMalformed)
	})

	t.Run("none with no signature segment at all", func(t *testing.T) {
		tok := strings.TrimSuffix(identitytest.Raw(header("none"), payload(), nil), ".")
		_, err := env.Verifier.Verify(bg, tok)
		wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonMalformed)
	})

	t.Run("HS256 keyed with the public key (algorithm confusion)", func(t *testing.T) {
		h64 := base64.RawURLEncoding.EncodeToString([]byte(header("HS256")))
		p64 := base64.RawURLEncoding.EncodeToString([]byte(payload()))
		mac := hmac.New(sha256.New, env.Issuer.Public)
		mac.Write([]byte(h64 + "." + p64))
		sig := append(mac.Sum(nil), mac.Sum(nil)...) // 64 bytes so only the header can reject it
		_, err := env.Verifier.Verify(bg, h64+"."+p64+"."+base64.RawURLEncoding.EncodeToString(sig))
		wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonBadHeader)
	})

	t.Run("the accepted algorithm name is the profile constant", func(t *testing.T) {
		// Exactly one of the two spellings of Ed25519 is accepted: the one
		// in identity.Algorithm. The other was refused above (otherName).
		if identity.Algorithm != "EdDSA" && identity.Algorithm != "Ed25519" {
			t.Fatalf("Algorithm = %q, want one of the two Ed25519 spellings", identity.Algorithm)
		}
		if _, err := env.Verifier.Verify(bg, env.Issuer.Token()); err != nil {
			t.Fatalf("the profile's own algorithm was rejected: %v", err)
		}
	})

	if replayRows(t, env) != 1 {
		t.Fatalf("replay rows = %d, want 1 (only the final valid token)", replayRows(t, env))
	}
}

// SEC-031: every other header rule of the profile.
func TestHeaderProfileEnforced(t *testing.T) {
	env := identitytest.NewEnv(t)
	cases := map[string]func(*identitytest.Draft){
		"typ missing":         func(d *identitytest.Draft) { d.DropHeader(identity.HeaderType) },
		"typ JWT":             func(d *identitytest.Draft) { d.SetHeader(identity.HeaderType, "JWT") },
		"typ other case":      func(d *identitytest.Draft) { d.SetHeader(identity.HeaderType, strings.ToUpper(identity.TokenType)) },
		"kid missing":         func(d *identitytest.Draft) { d.DropHeader(identity.HeaderKeyID) },
		"kid empty":           func(d *identitytest.Draft) { d.SetHeader(identity.HeaderKeyID, "") },
		"kid number":          func(d *identitytest.Draft) { d.SetHeader(identity.HeaderKeyID, 1) },
		"kid with a slash":    func(d *identitytest.Draft) { d.SetHeader(identity.HeaderKeyID, "../keys/1") },
		"kid too long":        func(d *identitytest.Draft) { d.SetHeader(identity.HeaderKeyID, strings.Repeat("k", 65)) },
		"jwk embedded":        func(d *identitytest.Draft) { d.SetHeader("jwk", map[string]any{"kty": "OKP", "crv": "Ed25519"}) },
		"jku":                 func(d *identitytest.Draft) { d.SetHeader("jku", "https://evil.example.test/keys") },
		"x5u":                 func(d *identitytest.Draft) { d.SetHeader("x5u", "https://evil.example.test/cert") },
		"x5c":                 func(d *identitytest.Draft) { d.SetHeader("x5c", []string{"AAAA"}) },
		"crit":                func(d *identitytest.Draft) { d.SetHeader("crit", []string{"exp"}) },
		"zip":                 func(d *identitytest.Draft) { d.SetHeader("zip", "DEF") },
		"cty (nested token)":  func(d *identitytest.Draft) { d.SetHeader("cty", "JWT") },
		"unknown member":      func(d *identitytest.Draft) { d.SetHeader("x", 1) },
		"upper-case ALG name": func(d *identitytest.Draft) { d.DropHeader("alg").SetHeader("ALG", identity.Algorithm) },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := env.Verifier.Verify(bg, env.Issuer.Token(mod))
			wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonBadHeader)
		})
	}
	t.Run("duplicate alg member", func(t *testing.T) {
		h := fmt.Sprintf(`{"alg":"none","alg":%q,"typ":%q,"kid":%q}`, identity.Algorithm, identity.TokenType, env.Issuer.KeyID)
		_, err := env.Verifier.Verify(bg, identitytest.Raw(h, mustJSON(t, env.Issuer.Draft().Claims), env.Issuer.PrivateKey()))
		wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonBadHeader)
	})
	t.Run("header is not an object", func(t *testing.T) {
		for _, h := range []string{`[]`, `"EdDSA"`, `null`, `1`, `{"alg":"EdDSA"} {}`, `{`} {
			_, err := env.Verifier.Verify(bg, identitytest.Raw(h, mustJSON(t, env.Issuer.Draft().Claims), env.Issuer.PrivateKey()))
			wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonBadHeader)
		}
	})
	if replayRows(t, env) != 0 {
		t.Fatal("a refused token burned a nonce")
	}
}

// SEC-031: malformed and truncated structures are refused.
func TestMalformedStructureRejected(t *testing.T) {
	env := identitytest.NewEnv(t)
	good := env.Issuer.Token()
	h, p, s := segments(t, good)

	cases := map[string]string{
		"empty":                "",
		"one segment":          h,
		"two segments":         h + "." + p,
		"four segments":        good + ".AAAA",
		"empty header":         "." + p + "." + s,
		"empty payload":        h + ".." + s,
		"empty signature":      h + "." + p + ".",
		"only dots":            "..",
		"padded header":        h + "==." + p + "." + s,
		"standard alphabet":    strings.NewReplacer("-", "+", "_", "/").Replace(h) + "+." + p + "." + s,
		"newline in header":    h[:4] + "\n" + h[4:] + "." + p + "." + s,
		"space in payload":     h + "." + p[:4] + " " + p[4:] + "." + s,
		"non-ASCII":            h + "." + p + "é." + s,
		"truncated signature":  h + "." + p + "." + s[:len(s)-4],
		"extended signature":   h + "." + p + "." + s + "AAAA",
		"truncated payload":    h + "." + p[:len(p)/2] + "." + s,
		"bearer prefix":        "Bearer " + good,
		"leading space":        " " + good,
		"trailing newline":     good + "\n",
		"non-canonical base64": h + "." + p + "." + nonCanonical(s),
		"signature of zeros":   h + "." + p + "." + base64.RawURLEncoding.EncodeToString(make([]byte, 64)),
		"oversized":            h + "." + strings.Repeat("A", identity.MaxTokenSize) + "." + s,
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := env.Verifier.Verify(bg, token)
			if err == nil {
				t.Fatal("malformed token accepted")
			}
			if !contracts.HasCode(err, contracts.ErrUnauthenticated) {
				t.Fatalf("err = %v, want unauthenticated", err)
			}
			if r := identity.ReasonOf(err); r != identity.ReasonMalformed && r != identity.ReasonBadSignature {
				t.Fatalf("reason = %q, want malformed_token or bad_signature", r)
			}
		})
	}
	if replayRows(t, env) != 0 {
		t.Fatal("a refused token burned a nonce")
	}
	// And the original is still good: none of the above touched it.
	if _, err := env.Verifier.Verify(bg, good); err != nil {
		t.Fatalf("untouched token rejected: %v", err)
	}
}

// nonCanonical returns s with the unused low bits of its final base64url
// character set, which a lax decoder maps to the same bytes.
func nonCanonical(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, s[len(s)-1])
	// 64 bytes encode to 86 characters; the final one carries 4 data bits.
	return s[:len(s)-1] + string(alphabet[last|0x0f])
}

// A retired or revoked issuer key stops verifying at once (SEC-037).
func TestIssuerKeyRevocationAndRetirement(t *testing.T) {
	env := identitytest.NewEnv(t)

	retiring := identitytest.NewIssuer(t, identitytest.IssuerConfig{ApplicationID: "app_retiring", Clock: env.Clock})
	if err := env.Keys.Register(retiring.Key()); err != nil {
		t.Fatal(err)
	}
	if err := env.Keys.Retire(retiring.KeyID, env.Clock.Now().Add(2*minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Verifier.Verify(bg, retiring.Token()); err != nil {
		t.Fatalf("a key before its retirement time was rejected: %v", err)
	}
	env.Clock.Advance(2 * minute)
	_, err := env.Verifier.Verify(bg, retiring.Token())
	wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonKeyRetired)

	pending := env.Issuer.Token() // minted, not yet presented
	if !env.Keys.Revoke(env.Issuer.KeyID) {
		t.Fatal("Revoke reported the key missing")
	}
	_, err = env.Verifier.Verify(bg, pending)
	wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonUnknownKey)

	if n := env.Keys.RevokeApplication("app_retiring"); n != 1 {
		t.Fatalf("RevokeApplication removed %d keys, want 1", n)
	}
}

// A resolver that cannot answer makes the token fail closed.
func TestKeyResolverFailureFailsClosed(t *testing.T) {
	env := identitytest.NewEnv(t)
	v, err := identity.NewVerifier(identity.Options{
		DeploymentID: identitytest.DefaultDeployment,
		Keys:         brokenKeys{},
		Replay:       env.Replay,
		Clock:        env.Clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.Verify(bg, env.Issuer.Token())
	wantReject(t, err, contracts.ErrUnavailable, identity.ReasonKeyStoreUnavailable)

	// A resolver returning a key of the wrong size must not panic ed25519.
	v2, _ := identity.NewVerifier(identity.Options{
		DeploymentID: identitytest.DefaultDeployment,
		Keys:         shortKeys{keyID: env.Issuer.KeyID},
		Replay:       env.Replay,
		Clock:        env.Clock,
	})
	_, err = v2.Verify(bg, env.Issuer.Token())
	wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonUnknownKey)
}

var errBroken = errors.New("key store offline")

type brokenKeys struct{}

func (brokenKeys) Lookup(context.Context, string) (identity.IssuerKey, error) {
	return identity.IssuerKey{}, errBroken
}

type shortKeys struct{ keyID string }

func (s shortKeys) Lookup(_ context.Context, kid string) (identity.IssuerKey, error) {
	return identity.IssuerKey{KeyID: kid, ApplicationID: "app_demo", Issuer: "x", Public: make([]byte, 5)}, nil
}

// NewVerifier fails closed on missing or malformed configuration.
func TestNewVerifierFailsClosed(t *testing.T) {
	env := identitytest.NewEnv(t)
	ok := identity.Options{DeploymentID: "dep_x", Keys: env.Keys, Replay: env.Replay}
	if _, err := identity.NewVerifier(ok); err != nil {
		t.Fatalf("baseline options rejected: %v", err)
	}
	cases := map[string]func(*identity.Options){
		"no deployment":       func(o *identity.Options) { o.DeploymentID = "" },
		"wildcard deployment": func(o *identity.Options) { o.DeploymentID = "*" },
		"padded deployment":   func(o *identity.Options) { o.DeploymentID = " dep" },
		"no keys":             func(o *identity.Options) { o.Keys = nil },
		"no replay cache":     func(o *identity.Options) { o.Replay = nil },
		"negative lifetime":   func(o *identity.Options) { o.MaxLifetime = -time.Second },
		"audience with space": func(o *identity.Options) { o.Audience = "a b" },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			o := ok
			mod(&o)
			if _, err := identity.NewVerifier(o); !contracts.HasCode(err, contracts.ErrValidationFailed) {
				t.Fatalf("NewVerifier = %v, want validation_failed", err)
			}
		})
	}
}

func TestRetireUnknownKeyFails(t *testing.T) {
	keys := identity.NewKeyRegistry(nil)
	if err := keys.Retire("missing", time.Now()); err == nil {
		t.Fatal("Retire of an unknown key succeeded")
	}
}
