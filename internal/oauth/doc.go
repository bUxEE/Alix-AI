// Package oauth is the browser-based authorization engine of the external provider mode:
// OAuth 2.x authorization code with PKCE (S256), for every provider that officially offers a
// third-party browser flow. It opens no browser itself. Begin returns the authorization URL; the
// host opens it (popup or redirect). With callback host, the gateway receives the redirect and
// hands it to Complete. With callback loopback, an ephemeral 127.0.0.1 listener completes the
// exchange and serves a minimal callback page (postMessage to the opener, or a close message for
// the CLI).
//
// # Flow
//
//  1. Prepare returns the consent data (provider, known account, organisation or project, each scope
//     with what it reaches, data access) and a digest. It has no side effect and makes no network call.
//  2. Begin takes the digest back, so a URL is issued only for a consent that was prepared, binds the
//     redirect URI, and creates a pending authorization: random state (unless the provider has none),
//     PKCE verifier and (OIDC) nonce, in memory, single use, expiring (10 minutes), bounded per tenant.
//     presentation popup mints a flow_id for polling; callback loopback binds an ephemeral listener
//     URI (the caller sends the documented placeholder http://127.0.0.1/oauth/loopback, which Begin
//     accepts only after BindLoopback replaced it).
//  3. Complete consumes the callback. The state (or loopback flow key) is spent by the first call
//     whatever its outcome. The code is exchanged (or, for OpenRouter, exchanged for an issued API
//     key), an OIDC ID token is validated when present, and the grant goes to the credential service.
//  4. Token returns the access value for one provider call, refreshing it first inside the refresh
//     skew. Concurrent callers of one reference share one refresh. Rotating refresh values are stored
//     by Rotate. A provider's invalid_grant flags reauthorization_required.
//  5. Disconnect and DisconnectTenant revoke at the provider (RFC 7009) then locally.
//  6. FlowGet / FlowCancel expose popup-flow status to the gateway (principal-scoped).
//
// # Redirects
//
// A remote deployment uses a registered HTTPS redirect, matched character for character. A local
// deployment uses a loopback redirect (RFC 8252 §7.3): http on 127.0.0.1 only (never "localhost"),
// an explicit random port, exactly the configured path. SIWC is loopback-only.
//
// # Secrets
//
// Tokens and issued keys are credentials.Secret from parse time and live only in internal/credentials.
// The package imports no filesystem, database or store package. Errors carry a fixed reason and at
// most a protocol error code, never provider text, a URL, a code or a token. The callback page and
// postMessage carry only flow_id and status.
//
// # Which providers offer OAuth (checked 2026-10-26)
//
// Not every provider has a browser flow for API access. Support records them; a provider that is not
// recorded, not configured, configured with OAuthSupported false, or switched off (Disabled) has the
// manual API-key fallback of internal/credentials: Prepare and Begin return an error for which
// IsManualFallback is true and make no network call. Never reuse another product's OAuth client id.
//
//   - OpenAI — Sign in with ChatGPT (SIWC) for open-source / locally hosted apps (launched 2026-09-29).
//     Dynamic self-registration: first authorize uses client_id=dynamic_agent_client with
//     ext_agent_host_id (opaque, stable per host, e.g. urn:uuid:..., never user-identifying) and
//     agent_name_hint; the callback issues a durable client_id (oaiapp_...) to store and reuse. No
//     client secret. Authorization https://auth.openai.com/api/accounts/authorize, token
//     https://auth.openai.com/api/accounts/oauth/token (prefer discovery
//     https://auth.openai.com/.well-known/openid-configuration). Scopes openid profile email +
//     offline_access resource.invoke chatgpt.tokens.use.direct; resource=https://api.openai.com/v1 on
//     both authorize and token. PKCE S256, state, nonce. Redirect: loopback only,
//     http://127.0.0.1:<port>/auth/callback. Access ~1 h; refresh tokens rotate. Inference is the
//     signed-in person's ChatGPT plan: bind the connection to that principal; other principals do not
//     route via it. AlixAI offers SIWC only for same-machine (loopback) deployments; remote hosts use
//     the manual API key. The OpenAI adapter always sends store:false and requires
//     chatgpt.tokens.use.direct before SIWC inference.
//     Sources: https://developers.openai.com/siwc/token-sharing-open-source ,
//     https://developers.openai.com/siwc/token-sharing-open-source/sign-in ,
//     https://developers.openai.com/cookbook/articles/sign-in-with-chatgpt (read 2026-10-26).
//   - Google (Gemini API and Google Cloud Speech-to-Text / google-stt): OAuth officially supported.
//     AlixAI ships no client id (sensitive scopes): the operator registers their own Google OAuth
//     client (Desktop for loopback, Web for host https) and puts client_id (+ client_secret_ref for
//     Web) in config. Gemini "inference" requests only
//     https://www.googleapis.com/auth/generative-language.retriever — the scope the official page's
//     programmatic InstalledAppFlow example uses alone with the Gemini client (the gcloud ADC sample
//     also lists cloud-platform, which is broader; AlixAI does not request it for gemini).
//     google-stt requests cloud-platform (Speech-to-Text v2 is a Cloud API; no narrower speech OAuth
//     scope is named on that page); consent text says it grants the whole Cloud project — use a
//     dedicated project. The same Google client config is shareable by gemini and google-stt.
//     Source: https://ai.google.dev/gemini-api/docs/oauth (read 2026-10-26).
//   - OpenRouter: official OAuth PKCE. Authorize https://openrouter.ai/auth?callback_url=…&code_challenge=…
//     &code_challenge_method=S256; exchange POST https://openrouter.ai/api/v1/auth/keys → {key}. The
//     key is stored as credential kind oauth_issued_key (no refresh). No state parameter: bind by
//     per-flow loopback port / callback path. Prefer 127.0.0.1; AlixAI refuses the name localhost.
//     Source: https://openrouter.ai/docs/guides/overview/auth/oauth (read 2026-10-26).
//   - Anthropic: no. Official API access is API key or workload-identity federation; subscription
//     OAuth outside Anthropic products is forbidden and blocked server-side. Use a manual key (or
//     reach models through OpenRouter's OAuth). Source: https://platform.claude.com/docs/en/api/overview
//     (read 2026-10-26).
//   - xAI (Grok): no official third-party OAuth. First-party Grok logins must not be reused (risk of
//     suspension). Manual API key (or OpenRouter). Source: https://docs.x.ai/ (read 2026-10-26).
//   - Moonshot (Kimi): no official third-party OAuth. First-party logins must not be reused. Manual
//     API key (or OpenRouter). Source: https://platform.moonshot.ai/docs (read 2026-10-26).
//   - Meta (Llama): no verified browser OAuth for API access; manual API key.
//
// The engine ships no registered client IDs except SIWC's dynamic_agent_client bootstrap. A deployment
// supplies ProviderConfig (endpoints, client ID, capabilities) via external_providers.oauth.providers.
// Tests use a plain-HTTP loopback httptest IdP; no test contacts a real provider.
//
// # Limits
//
// Pending authorizations and popup flows are in memory: a restart abandons them. The credential
// service does not release the refresh value of a reference whose access value has already expired
// except to the engine's refresh path.
package oauth
