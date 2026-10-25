// Package oauth is the browser-based authorization engine of the external provider mode (additive plan
// section 11.1): OAuth 2.x authorization code flow with PKCE (S256), for every provider that offers it. It
// opens no browser and no listener. Begin returns the authorization URL, the host application opens it, the
// gateway receives the redirect and hands the parameters to Complete.
//
// # Flow
//
//  1. Prepare returns the consent data (provider, known account, organisation or project, each scope with what
//     it reaches, data access) and a digest. It has no side effect and makes no network call.
//  2. Begin takes the digest back, so a URL is issued only for a consent that was prepared, binds the redirect
//     URI, and creates a pending authorization: random state, PKCE verifier and (OIDC) nonce, in memory,
//     single use, expiring (10 minutes), bounded per tenant.
//  3. Complete consumes the callback. The state is spent by the first call whatever its outcome, so a replay
//     fails (replayed); an old one fails (state_expired); the callback must come from the same application,
//     tenant, workspace and principal and arrive on the bound redirect URI. Then the code is exchanged with the
//     verifier, an OIDC ID token is validated (issuer, audience, expiry, nonce), and the tokens go to the
//     credential service. Nothing is stored unless every check passed; tokens issued for a flow that then fails
//     are revoked at the provider.
//  4. Token returns the access value for one provider call, refreshing it first inside the refresh skew of its
//     expiry. Concurrent callers of one reference share one refresh request (single-flight). A rotated refresh
//     value is stored by Rotate; the provider's invalid_grant flags the connection reauthorization_required and
//     every later call fails closed without asking the provider again. A provider outage flags nothing.
//  5. Disconnect and DisconnectTenant (tenant removal, security reset) revoke at the provider (RFC 7009) and
//     then locally; the local revocation happens whatever the provider answered.
//
// # Redirects
//
// A remote deployment uses a registered HTTPS redirect, matched character for character (no prefix, case,
// trailing-slash, query or port slack; a private-use scheme deep link can be registered the same way). A local
// deployment uses a loopback redirect (RFC 8252 7.3): http on 127.0.0.1 or [::1], an explicit random port the
// gateway bound (LoopbackRedirectURI), exactly the configured path. "localhost" is refused. The redirect is
// bound to the flow and compared again at the callback and sent in the token request.
//
// # Scopes
//
// A provider config maps each capability to its smallest scope set. A flow requests the union of the selected
// capabilities and nothing more (plus openid for an OIDC provider and the refresh scope, both listed at
// consent). A provider that grants more than was requested gets the connection quarantined
// (excessive_permissions_reported). Only the scopes both requested and granted become the permitted scopes of
// the reference.
//
// # Account switching and re-authorization
//
// Prepare with Reauthorize renews an existing reference and shows its known account; the authorization asks
// for consent again (prompt). If a different account answers, the callback is refused (account_mismatch), the
// new grant is revoked at the provider and nothing changes. Moving a connection to another account is its own
// request (SwitchAccount): it forces account selection and consent, binds the reference to the new account
// and revokes the old account's tokens at the provider.
//
// # Secrets
//
// Tokens are credentials.Secret from the moment they are parsed and live only in internal/credentials
// (AddOAuth, Rotate; read back by Resolve for one call). The package imports no filesystem, database or store
// package, and a test enforces it. The token and revocation calls are POST forms (never a query string) sent
// through internal/egress, so their endpoints must be on the application's allowlist; errors carry a fixed
// reason and at most a protocol error code, never provider text, a URL, a code or a token. The client secret
// of a confidential client is a credential of the credential service, named by ClientSecretRef.
//
// The ID token's signature is not verified: OpenID Connect Core 3.1.3.7 lets a client rely on TLS to the token
// endpoint, which egress verifies without a skip switch, when the token is received directly from it.
//
// # Which providers offer OAuth (checked 2026-10-07)
//
// Not every provider has a browser flow for API access. Support records them; a provider that is not
// recorded, not configured, configured with OAuthSupported false, or switched off by the operator (Disabled)
// has the manual API-key fallback of internal/credentials: Prepare and Begin return an error for which
// IsManualFallback is true and make no network call.
//
//   - OpenAI: no. "The OpenAI API accepts bearer credentials from API keys or from short-lived access tokens
//     created with workload identity federation." Federation is machine to machine, not browser consent.
//     Source: https://developers.openai.com/api/reference/overview/ (read 2026-10-07).
//   - Anthropic: no. Authorization is "your API key or a short-lived access token obtained from POST
//     /v1/oauth/token through Workload Identity Federation" (machine to machine).
//     Source: https://platform.claude.com/docs/en/api/overview (read 2026-10-07).
//   - Google (Gemini API): yes. "The easiest way to authenticate to the Gemini API is to configure an API key
//     ... If you need stricter access controls, you can use OAuth instead"; the scopes
//     https://www.googleapis.com/auth/cloud-platform and
//     https://www.googleapis.com/auth/generative-language.retriever are named.
//     Source: https://ai.google.dev/gemini-api/docs/oauth (read 2026-10-07).
//   - Meta (Llama): not verified; treated as manual fallback until a source is recorded.
//
// The engine ships no registered client IDs: a deployment supplies the ProviderConfig (endpoints, client ID,
// capabilities) of the provider it uses. The mock identity provider of the tests is a plain-HTTP loopback
// httptest server; no test contacts a real provider.
//
// # Limits
//
// Pending authorizations are in memory: a restart abandons them and the user starts again. The credential
// service does not release the refresh value of a reference whose access value has already expired, so the
// engine refreshes ahead of expiry; a connection left idle past its expiry reports expired until that is
// changed in internal/credentials.
package oauth
