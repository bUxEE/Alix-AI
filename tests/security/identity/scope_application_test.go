package identitysec

import (
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// SEC-035
//
// A whitespace-only application level is a missing level, like every other
// level: contracts.Scope.Validate and ADR 0013 say it is scope_missing naming
// the level. The verifier compares a non-empty application claim with the
// signing key's application before the scope is built, so a blank one is
// refused as unauthenticated (wrong_application) instead. The request is still
// refused (401, the service is not called); the typed code is the defect.
func TestSecScopeBlankApplicationIsScopeMissing(t *testing.T) {
	env, svc := newEnv(t)
	good := newRig(t, env, svc, identity.Middleware(env.Verifier))
	good.rec.take()
	rec := good.do(t, good.token(good.sub(), func(d *identitytest.Draft) { d.Set(identity.ClaimApplication, "   ") }),
		request{method: http.MethodGet, target: "/v1/settings/user"})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401: %s", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec); got != contracts.ErrScopeMissing {
		t.Errorf("code = %q, want %q: %s", got, contracts.ErrScopeMissing, rec.Body.String())
	}
	if got := errorLevels(t, rec); !cmp.Equal(got, []string{"application"}) {
		t.Errorf("levels = %v, want exactly [application]", got)
	}
	if calls := good.rec.take(); len(calls) != 0 {
		t.Errorf("the service was called %d times with a blank application", len(calls))
	}
}
