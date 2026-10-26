package oauth

import (
	"net"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// ClientAuth says how the client authenticates at the token and revocation endpoints.
type ClientAuth string

// The client authentication methods.
const (
	// ClientAuthNone is a public client: PKCE alone, no client secret (the default).
	ClientAuthNone ClientAuth = "none"
	// ClientAuthPost sends client_id and client_secret in the form body.
	ClientAuthPost ClientAuth = "post"
	// ClientAuthBasic sends them in an HTTP Basic header.
	ClientAuthBasic ClientAuth = "basic"
)

// ScopeInfo describes one provider scope for the consent screen.
type ScopeInfo struct {
	// Name is the scope string as the provider knows it.
	Name string
	// Description says in plain words what the scope allows.
	Description string
	// DataAccess says what data of the account the scope reaches.
	DataAccess string
}

// AccountMapping names where a provider puts the safe account identifiers. A token response member (and, for
// an OIDC provider, an ID-token claim) with that name is read when it is a string. Only identifiers are read:
// internal/credentials drops anything that is not shaped like one, and an e-mail address never is.
type AccountMapping struct {
	AccountID      string // default for OIDC providers: "sub"
	OrganizationID string
	ProjectID      string
}

// ProviderConfig describes one provider. Everything in it is configuration, not code: a new provider is a new
// value. It holds no secret: a client secret, when the provider needs one, is a credential of the credential
// service named by ClientSecretRef.
type ProviderConfig struct {
	ID          providers.ProviderID
	DisplayName string
	// OAuthSupported is false for a provider whose API has no browser authorization flow (see the package
	// document). The engine then reports that the manual API-key fallback applies and makes no network call.
	OAuthSupported bool
	// Disabled is the operator's switch: OAuth is deliberately off for this provider, and the manual fallback
	// applies even if the provider supports OAuth.
	Disabled bool

	AuthorizationEndpoint string
	TokenEndpoint         string
	// RevocationEndpoint is the RFC 7009 endpoint; empty when the provider has none.
	RevocationEndpoint string

	ClientID string
	// ClientSecretRef is the reference (in the tenant's credential service) of the client secret, or empty for
	// a public client.
	ClientSecretRef string
	ClientAuth      ClientAuth

	// Capabilities maps a capability name to the smallest set of scopes it needs. A flow requests the union of
	// the capabilities selected, and nothing else.
	Capabilities map[string][]string
	// Scopes is the catalogue of scopes with their consent text. Every scope of a capability must be listed.
	Scopes []ScopeInfo
	// DataAccess lines are shown at consent besides the per-scope text (what the provider receives and keeps).
	DataAccess []string

	// OIDC providers return an ID token, which the engine validates (issuer, audience, expiry and nonce).
	OIDC   bool
	Issuer string // required with OIDC
	// RefreshScope is added when the provider issues a refresh value only on request through a scope
	// ("offline_access"); it is listed at consent. Providers that use a parameter put it in ExtraAuthParams.
	RefreshScope    string
	ExtraAuthParams map[string]string
	// ConsentPrompt and SwitchPrompt are the "prompt" values of a re-authorization and of an account switch
	// (defaults "consent" and "select_account consent"): both force the consent screen to appear again.
	ConsentPrompt string
	SwitchPrompt  string
	// RequireIssParam demands the RFC 9207 "iss" callback parameter.
	RequireIssParam bool

	// RedirectURIs are the registered redirects for remote deployments: exact https URLs (or a private-use
	// scheme deep link), matched character for character.
	RedirectURIs []string
	// Loopback allows RFC 8252 loopback redirects (http, 127.0.0.1 or [::1], any port) on LoopbackPath.
	Loopback     bool
	LoopbackPath string // default "/oauth/callback"
	// LoopbackOnly refuses host (https) redirects: SIWC for open-source apps is loopback-only.
	LoopbackOnly bool

	Account AccountMapping

	// GrantKind selects how Complete stores the grant. Empty or GrantOAuth stores access/refresh tokens;
	// GrantIssuedKey exchanges for a provider-issued API key (OpenRouter).
	GrantKind GrantKind
	// AuthStyle selects the authorization URL shape. Empty is standard OAuth; AuthStyleOpenRouter uses
	// callback_url + PKCE without state.
	AuthStyle AuthStyle
	// NoState: the provider has no state parameter; the flow is bound by the loopback redirect / flow id.
	NoState bool
	// Resource is the RFC 8707 resource parameter sent on authorize and token requests when set.
	Resource string
	// DynamicClient: first authorize uses InitialClientID (+ ext_agent_host_id); the token response may
	// issue a durable client_id stored with the credential for reuse.
	DynamicClient   bool
	InitialClientID string // e.g. dynamic_agent_client
	AgentNameHint   string
	// KeyExchangeEndpoint is the POST JSON endpoint for GrantIssuedKey (OpenRouter /api/v1/auth/keys).
	KeyExchangeEndpoint string
}

// GrantKind is how a completed authorization is stored.
type GrantKind string

// The grant kinds.
const (
	GrantOAuth     GrantKind = "oauth"
	GrantIssuedKey GrantKind = "issued_key"
)

// AuthStyle is the authorization URL shape.
type AuthStyle string

// The auth styles.
const (
	AuthStyleStandard   AuthStyle = ""
	AuthStyleOpenRouter AuthStyle = "openrouter"
)

// Reserved parameter names ExtraAuthParams must not set.
var reservedAuthParams = []string{"response_type", "client_id", "redirect_uri", "scope", "state", "nonce",
	"code_challenge", "code_challenge_method", "prompt", "request", "request_uri", "resource",
	"ext_agent_host_id", "agent_name_hint", "callback_url"}

const defaultLoopbackPath = "/oauth/callback"

// Normalize validates the config and returns a copy with the defaults filled in.
func (c ProviderConfig) Normalize() (ProviderConfig, error) { return c.normalized() }

// normalized validates the config and returns a copy with the defaults filled in.
func (c ProviderConfig) normalized() (ProviderConfig, error) {
	bad := func(msg string) (ProviderConfig, error) {
		return ProviderConfig{}, fail(contracts.ErrValidationFailed, ReasonBadConfig, "oauth: provider config: "+msg)
	}
	if c.ID == "" {
		return bad("an ID is required")
	}
	if !c.OAuthSupported {
		return c, nil // nothing else matters: the manual fallback applies
	}
	if c.GrantKind == "" {
		c.GrantKind = GrantOAuth
	}
	switch c.GrantKind {
	case GrantOAuth, GrantIssuedKey:
	default:
		return bad("unknown grant kind")
	}
	if c.GrantKind == GrantIssuedKey {
		if c.KeyExchangeEndpoint == "" || !validEndpoint(c.KeyExchangeEndpoint) {
			return bad("an issued-key grant needs a key exchange endpoint")
		}
	} else if c.ClientID == "" && !(c.DynamicClient && c.InitialClientID != "") {
		return bad("a client ID is required")
	}
	if c.DynamicClient && c.InitialClientID == "" {
		return bad("dynamic client registration needs an initial client id")
	}
	if c.ClientID == "" && c.DynamicClient {
		c.ClientID = c.InitialClientID
	}
	endpoints := []string{c.AuthorizationEndpoint}
	if c.GrantKind == GrantOAuth {
		endpoints = append(endpoints, c.TokenEndpoint)
	}
	for _, e := range endpoints {
		if !validEndpoint(e) {
			return bad("the authorization and token endpoints must be https (or http on loopback) URLs without credentials or fragments")
		}
	}
	if c.LoopbackOnly && !c.Loopback {
		return bad("loopback-only providers must allow loopback")
	}
	if c.RevocationEndpoint != "" && !validEndpoint(c.RevocationEndpoint) {
		return bad("the revocation endpoint is not a valid URL")
	}
	switch {
	case c.ClientAuth == "" && c.ClientSecretRef == "":
		c.ClientAuth = ClientAuthNone
	case c.ClientAuth == "":
		c.ClientAuth = ClientAuthPost
	}
	switch c.ClientAuth {
	case ClientAuthNone:
		if c.ClientSecretRef != "" {
			return bad("a public client has no client secret")
		}
	case ClientAuthPost, ClientAuthBasic:
		if c.ClientSecretRef == "" {
			return bad("client authentication needs a client secret reference")
		}
	default:
		return bad("unknown client authentication method")
	}
	known := map[string]bool{}
	for _, s := range c.Scopes {
		if s.Name == "" || known[s.Name] || strings.ContainsAny(s.Name, " \t\r\n\"\\") {
			return bad("a scope is empty, repeated or malformed")
		}
		known[s.Name] = true
	}
	if len(c.Capabilities) == 0 {
		return bad("at least one capability is required")
	}
	for name, scopes := range c.Capabilities {
		if name == "" || len(scopes) == 0 {
			return bad("a capability needs a name and scopes")
		}
		for _, s := range scopes {
			if !known[s] {
				return bad("a capability uses a scope that is not in the catalogue")
			}
		}
	}
	if c.OIDC {
		if c.Issuer == "" {
			return bad("an OIDC provider needs an issuer")
		}
		if !known["openid"] {
			return bad("an OIDC provider lists the openid scope")
		}
		if c.Account.AccountID == "" {
			c.Account.AccountID = "sub"
		}
	}
	if c.RefreshScope != "" && !known[c.RefreshScope] {
		return bad("the refresh scope is not in the catalogue")
	}
	for k := range c.ExtraAuthParams {
		if slices.Contains(reservedAuthParams, strings.ToLower(k)) {
			return bad("an extra authorization parameter is reserved")
		}
	}
	if c.ConsentPrompt == "" {
		c.ConsentPrompt = "consent"
	}
	if c.SwitchPrompt == "" {
		c.SwitchPrompt = "select_account consent"
	}
	if c.LoopbackPath == "" {
		c.LoopbackPath = defaultLoopbackPath
	}
	if !strings.HasPrefix(c.LoopbackPath, "/") || strings.ContainsAny(c.LoopbackPath, "?#") {
		return bad("the loopback path is malformed")
	}
	for _, r := range c.RedirectURIs {
		u, err := url.Parse(r)
		if err != nil || u.Fragment != "" || u.User != nil || u.RawQuery != "" || r != u.String() {
			return bad("a registered redirect is malformed")
		}
		switch {
		case u.Scheme == "https" && u.Host != "":
		case u.Scheme != "" && strings.Contains(u.Scheme, ".") && u.Host == "" && u.Path != "": // RFC 8252 7.1 private-use scheme
		default:
			return bad("a registered redirect is neither https nor a private-use scheme deep link")
		}
	}
	if !c.Loopback && len(c.RedirectURIs) == 0 {
		return bad("a provider needs a loopback or a registered redirect")
	}
	c.Capabilities = cloneCaps(c.Capabilities)
	c.Scopes = slices.Clone(c.Scopes)
	c.RedirectURIs = slices.Clone(c.RedirectURIs)
	return c, nil
}

func cloneCaps(m map[string][]string) map[string][]string {
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[k] = slices.Clone(v)
	}
	return out
}

// validEndpoint: https, or plain http to a loopback IP literal or "localhost" (the mock identity provider of
// the tests); no userinfo, query or fragment.
func validEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		h := u.Hostname()
		if h == "localhost" {
			return true
		}
		ip := net.ParseIP(h)
		return ip != nil && ip.IsLoopback()
	}
	return false
}

func (c ProviderConfig) scopeInfo(name string) (ScopeInfo, bool) {
	for _, s := range c.Scopes {
		if s.Name == name {
			return s, true
		}
	}
	return ScopeInfo{}, false
}

// scopesFor returns the deduplicated, sorted union of the scopes the capabilities need, plus the extras the
// flow itself needs (openid for an OIDC provider, the refresh scope). It never adds anything else.
func (c ProviderConfig) scopesFor(capabilities []string) ([]string, error) {
	if len(capabilities) == 0 {
		return nil, fail(contracts.ErrValidationFailed, ReasonBadRequest, "oauth: select at least one capability")
	}
	var out []string
	for _, name := range capabilities {
		scopes, ok := c.Capabilities[name]
		if !ok {
			return nil, fail(contracts.ErrUnknownCapability, ReasonUnknownCapab, "oauth: the provider has no such capability")
		}
		for _, s := range scopes {
			if !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	extras := []string{c.RefreshScope}
	if c.OIDC {
		extras = append(extras, "openid")
	}
	for _, extra := range extras {
		if extra != "" && !slices.Contains(out, extra) {
			out = append(out, extra)
		}
	}
	sort.Strings(out)
	return out, nil
}

// SupportInfo records whether a provider offers OAuth for API access, with the dated source it was read from.
type SupportInfo struct {
	ProviderID providers.ProviderID
	OAuth      bool
	// Source is the page the statement was read from; Checked is the date it was read (YYYY-MM-DD).
	Source  string
	Checked string
	Note    string
}

// Support is the recorded OAuth support of the providers the project knows (see the package document). A
// provider that is not listed is unverified and treated as having no OAuth flow: the manual fallback applies.
func Support() []SupportInfo {
	return []SupportInfo{
		{ProviderID: "openai", OAuth: true, Checked: "2026-10-26",
			Source: "https://developers.openai.com/siwc/token-sharing-open-source",
			Note:   "Sign in with ChatGPT (SIWC) for open-source / locally hosted apps: loopback-only, dynamic client registration, ChatGPT plan usage of the signed-in principal; remote deployments use the manual API key"},
		{ProviderID: "anthropic", OAuth: false, Checked: "2026-10-26",
			Source: "https://platform.claude.com/docs/en/api/overview",
			Note:   "No official third-party browser OAuth for API access; subscription OAuth outside Anthropic products is forbidden and blocked server-side; use a manual API key (or OpenRouter)"},
		{ProviderID: "gemini", OAuth: true, Checked: "2026-10-26",
			Source: "https://ai.google.dev/gemini-api/docs/oauth",
			Note:   "OAuth 2.0 offered; operator registers their own Google OAuth client (Desktop for loopback, Web for host https); scope generative-language.retriever only (narrowest named for the programmatic client on the official page)"},
		{ProviderID: "google-stt", OAuth: true, Checked: "2026-10-26",
			Source: "https://ai.google.dev/gemini-api/docs/oauth",
			Note:   "Shares the Google OAuth client config with gemini; cloud-platform (whole Cloud project; no narrower Speech-to-Text OAuth scope documented) — use a dedicated project"},
		{ProviderID: "google-tts", OAuth: true, Checked: "2026-10-26",
			Source: "https://cloud.google.com/text-to-speech/docs/reference/rest",
			Note:   "Shares the Google OAuth client config with gemini / google-stt; cloud-platform (whole Cloud project; no narrower Text-to-Speech OAuth scope documented) — use a dedicated project"},
		{ProviderID: "openrouter", OAuth: true, Checked: "2026-10-26",
			Source: "https://openrouter.ai/docs/guides/overview/auth/oauth",
			Note:   "Official PKCE login issues a user-controlled API key (stored as oauth_issued_key); no refresh; no state parameter"},
		{ProviderID: "xai", OAuth: false, Checked: "2026-10-26",
			Source: "https://docs.x.ai/",
			Note:   "No official third-party OAuth; first-party Grok logins must not be reused; use a manual API key (or OpenRouter)"},
		{ProviderID: "moonshot", OAuth: false, Checked: "2026-10-26",
			Source: "https://platform.moonshot.ai/docs",
			Note:   "No official third-party OAuth for Kimi; first-party logins must not be reused; use a manual API key (or OpenRouter)"},
		{ProviderID: "meta", OAuth: false, Checked: "2026-10-26",
			Source: "https://llama.meta.com/",
			Note:   "No verified browser OAuth for API access; manual API key"},
	}
}

// SupportFor returns the recorded support of a provider and whether it is recorded at all.
func SupportFor(id providers.ProviderID) (SupportInfo, bool) {
	for _, s := range Support() {
		if s.ProviderID == id {
			return s, true
		}
	}
	return SupportInfo{}, false
}
