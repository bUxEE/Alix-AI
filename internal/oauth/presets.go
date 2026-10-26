package oauth

import (
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Preset returns a built-in ProviderConfig skeleton for a known provider id. The operator still
// supplies ClientID / secrets / redirects via config; presets fill endpoints, scopes and grant style.
// Unknown ids return ok=false.
func Preset(id providers.ProviderID) (ProviderConfig, bool) {
	switch id {
	case "openai":
		return ProviderConfig{
			ID: "openai", DisplayName: "OpenAI (Sign in with ChatGPT)", OAuthSupported: true,
			AuthorizationEndpoint: "https://auth.openai.com/api/accounts/authorize",
			TokenEndpoint:         "https://auth.openai.com/api/accounts/oauth/token",
			OIDC:                  true, Issuer: "https://auth.openai.com",
			InitialClientID: "dynamic_agent_client", DynamicClient: true,
			AgentNameHint: "AlixAI",
			Resource:      "https://api.openai.com/v1",
			RefreshScope:  "offline_access",
			Loopback:      true, LoopbackOnly: true, LoopbackPath: "/auth/callback",
			Capabilities: map[string][]string{
				"inference": {
					"openid", "profile", "email", "offline_access",
					"resource.invoke", "chatgpt.tokens.use.direct",
				},
			},
			Scopes: []ScopeInfo{
				{Name: "openid", Description: "OpenID identity", DataAccess: "account identity"},
				{Name: "profile", Description: "Profile", DataAccess: "profile claims"},
				{Name: "email", Description: "Email", DataAccess: "email address"},
				{Name: "offline_access", Description: "Refresh token", DataAccess: "renewed access without re-consent"},
				{Name: "resource.invoke", Description: "Invoke the API resource", DataAccess: "API calls under the ChatGPT plan"},
				{Name: "chatgpt.tokens.use.direct", Description: "Use ChatGPT plan tokens directly", DataAccess: "inference billed to the signed-in ChatGPT plan"},
			},
			DataAccess: []string{
				"Access tokens are short-lived (~1h); refresh tokens rotate.",
				"Inference uses the signed-in person's ChatGPT plan; bind the connection to that principal.",
				"SIWC is offered only for same-machine (loopback) deployments; remote hosts use a manual API key.",
			},
			Account: AccountMapping{AccountID: "sub"},
		}, true
	case "gemini":
		// Official page programmatic example (read 2026-10-26) uses generative-language.retriever alone
		// with the Gemini client; the gcloud ADC sample also lists cloud-platform (broader). Prefer the
		// narrower scope for generateContent / list models.
		scopes := []string{"https://www.googleapis.com/auth/generative-language.retriever"}
		return ProviderConfig{
			ID: id, DisplayName: "Gemini", OAuthSupported: true,
			AuthorizationEndpoint: "https://accounts.google.com/o/oauth2/v2/auth",
			TokenEndpoint:         "https://oauth2.googleapis.com/token",
			RevocationEndpoint:    "https://oauth2.googleapis.com/revoke",
			OIDC:                  true, Issuer: "https://accounts.google.com",
			ClientAuth: ClientAuthPost, RefreshScope: "",
			ExtraAuthParams: map[string]string{"access_type": "offline"},
			Loopback:        true, LoopbackPath: "/oauth/callback",
			Capabilities: map[string][]string{"inference": scopes},
			Scopes: []ScopeInfo{
				{Name: "https://www.googleapis.com/auth/generative-language.retriever", Description: "Gemini Generative Language API", DataAccess: "generateContent and related Generative Language calls on the project"},
				{Name: "openid", Description: "OpenID identity", DataAccess: "account identity"},
			},
			DataAccess: []string{
				"Quota and billing follow the Google Cloud project, not a consumer Gemini plan.",
				"Scope generative-language.retriever is the narrowest named on the official OAuth page for the programmatic client (read 2026-10-26).",
			},
			Account: AccountMapping{AccountID: "sub"},
		}, true
	case "google-stt":
		// Cloud Speech-to-Text v2 is a Cloud API; the vendor's Gemini OAuth page does not name a
		// speech-specific scope, so cloud-platform remains required (broad: whole project).
		scopes := []string{"https://www.googleapis.com/auth/cloud-platform"}
		return ProviderConfig{
			ID: id, DisplayName: "Google Cloud Speech-to-Text", OAuthSupported: true,
			AuthorizationEndpoint: "https://accounts.google.com/o/oauth2/v2/auth",
			TokenEndpoint:         "https://oauth2.googleapis.com/token",
			RevocationEndpoint:    "https://oauth2.googleapis.com/revoke",
			OIDC:                  true, Issuer: "https://accounts.google.com",
			ClientAuth: ClientAuthPost, RefreshScope: "",
			ExtraAuthParams: map[string]string{"access_type": "offline"},
			Loopback:        true, LoopbackPath: "/oauth/callback",
			Capabilities: map[string][]string{"inference": scopes},
			Scopes: []ScopeInfo{
				{Name: "https://www.googleapis.com/auth/cloud-platform", Description: "Google Cloud platform access", DataAccess: "Full access to the Google Cloud project (no narrower Speech-to-Text OAuth scope is documented); use a dedicated project"},
				{Name: "openid", Description: "OpenID identity", DataAccess: "account identity"},
			},
			DataAccess: []string{
				"cloud-platform grants access to the whole Google Cloud project, not only Speech-to-Text; register a dedicated project for AlixAI.",
				"Quota and billing follow that Cloud project.",
			},
			Account: AccountMapping{AccountID: "sub"},
		}, true
	case "openrouter":
		return ProviderConfig{
			ID: "openrouter", DisplayName: "OpenRouter", OAuthSupported: true,
			AuthorizationEndpoint: "https://openrouter.ai/auth",
			GrantKind:             GrantIssuedKey, AuthStyle: AuthStyleOpenRouter, NoState: true,
			KeyExchangeEndpoint: "https://openrouter.ai/api/v1/auth/keys",
			Loopback:            true, LoopbackPath: "/oauth/callback",
			Capabilities: map[string][]string{"inference": {"api"}},
			Scopes:       []ScopeInfo{{Name: "api", Description: "API access via issued key", DataAccess: "models reachable on the OpenRouter account"}},
			DataAccess:   []string{"Browser login issues a user-controlled API key (no refresh). Prefer 127.0.0.1 loopback; localhost is refused by AlixAI."},
		}, true
	}
	return ProviderConfig{}, false
}
