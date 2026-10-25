package identity_test

import (
	"testing"

	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// SEC-035: a blank (empty or whitespace-only) application_id is a missing
// level, answered scope_missing naming "application", not unauthenticated.
// Mutation guard: comparing a blank value with the signing key's application
// again turns every case below into unauthenticated/wrong_application.
func TestBlankApplicationIDIsScopeMissing(t *testing.T) {
	env := identitytest.NewEnv(t)
	for _, blank := range []string{"", " ", "   ", "\t", " \n "} {
		token := env.Issuer.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimApplication, blank) })
		_, err := env.Verifier.Verify(bg, token)
		wantScopeLevels(t, err, contracts.ErrScopeMissing, "application")
		if contracts.HasCode(err, contracts.ErrUnauthenticated) {
			t.Errorf("blank %q answered unauthenticated", blank)
		}
	}
	if replayRows(t, env) != 0 {
		t.Fatal("a blank application burned a nonce")
	}
	// A non-blank wrong application is still a mismatch with the key.
	token := env.Issuer.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimApplication, "app_other") })
	_, err := env.Verifier.Verify(bg, token)
	wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonWrongApplication)
}
