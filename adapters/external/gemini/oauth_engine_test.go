package gemini

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/oauth"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// engineTokens is the wrapper the composition root (internal/cli, task X-26)
// puts around the shared OAuth engine. It lives in a test because only tests may
// import internal/oauth here (module map); the test proves the engine fits the
// TokenSource contract and that the Google facts this package exports build a
// valid engine provider config.
type engineTokens struct{ e *oauth.Engine }

func (w engineTokens) Bearer(ctx context.Context, scope contracts.Scope, ref string, scopes []string, force bool) (credentials.Secret, error) {
	return w.e.Token(ctx, scope, oauth.TokenRequest{Ref: pv.CredentialRefID(ref), Scopes: scopes, ForceRefresh: force})
}

var _ TokenSource = engineTokens{}

// noCreds satisfies the engine's credential interface; this test never reaches it.
type noCreds struct{ oauth.Credentials }

func TestExportedOAuthFactsBuildAValidEngineConfig(t *testing.T) {
	cfg := oauth.ProviderConfig{
		ID: ProviderName, DisplayName: "Google Gemini API", OAuthSupported: true,
		AuthorizationEndpoint: OAuthAuthorizationEndpoint, TokenEndpoint: OAuthTokenEndpoint, RevocationEndpoint: OAuthRevocationEndpoint,
		ClientID:     "client-id-of-the-operators-project",
		Capabilities: map[string][]string{OAuthCapability: InferenceScopes},
		Scopes: []oauth.ScopeInfo{{
			Name:        ScopeCloudPlatform,
			Description: "Call the Gemini API as the signed-in Google account",
			DataAccess:  "The scope covers Google Cloud for the account; AlixAI uses it to call the Gemini API only. Prompts sent are processed under the Cloud project's terms.",
		}},
		DataAccess:      OAuthConsentNotes(),
		ExtraAuthParams: OAuthAuthParams(),
		Loopback:        true,
	}
	e, err := oauth.New(oauth.Config{Credentials: noCreds{}, Providers: []oauth.ProviderConfig{cfg}})
	if err != nil {
		t.Fatal(err)
	}
	if e.Mode(ProviderName) != oauth.ModeOAuth {
		t.Fatalf("mode %s", e.Mode(ProviderName))
	}
	// the engine's own record agrees: Google offers OAuth for API access
	if s, ok := oauth.SupportFor(ProviderName); !ok || !s.OAuth {
		t.Fatalf("support %+v %v", s, ok)
	}
	_ = engineTokens{e}
}
