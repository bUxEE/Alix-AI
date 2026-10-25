// Package identity turns the credential a host application presents into the
// verified contracts.Scope every other package works from (master plan §10,
// §10.1; ADR 0013).
//
// # The trust model
//
// The host application authenticates the human or service principal, then
// mints a short-lived, audience-bound context token signed with the
// application's Ed25519 key. AlixAI checks the issuer, audience, application,
// tenant, workspace, principal, expiry, nonce (jti) and permitted capability
// scopes, builds a contracts.Scope from the claims and hands that Scope, and
// nothing else, to the rest of the process. The model never sees the token.
//
// # The token profile (ADR 0013)
//
// A JWS compact serialisation with exactly one accepted algorithm:
//
//	header   {"alg":"EdDSA","typ":"alixai-context+jwt","kid":"<key id>"}
//	payload  {"iss","aud","sub","exp","nbf","iat","jti",
//	          "application_id","tenant_id","workspace_id",
//	          "roles","capability_scopes","session_id"}
//
// The verifier is a small in-tree implementation on crypto/ed25519. There is
// no algorithm negotiation, no key material is read from the token, and every
// header member and claim outside the two lists above is rejected. The deployment
// level of the Scope is never a claim: it comes from deployment configuration.
// A token is single use: its jti is recorded in SQLite until exp plus the
// clock-skew allowance and a second presentation is "replayed".
//
// # Seams
//
//   - KeyResolver turns a key id into an issuer key. IssuerKeyStore is the
//     implementation a service uses: public keys in SQLite (table
//     identity_issuer_keys), per application, provisioned by an operator and
//     read on every lookup so a revocation applies to the next token.
//     KeyRegistry is the in-memory implementation.
//   - Authenticator turns a presented credential into a Verified context. The
//     token Verifier implements it, and so can the peer-authenticated local IPC
//     transport (M3-14) using NewScope, which is the single place a Scope is
//     built from asserted values.
//   - Middleware puts the Verified context into the request context; handlers
//     read it with ScopeFromContext or RequireScope and never take tenant,
//     workspace or principal from the request.
//
// # Issuer keys
//
// Nothing is trusted until an operator provisions it. IssuerKeyStore holds the
// Ed25519 PUBLIC keys that verify an application's tokens; AlixAI never stores
// or sees a private key that could mint one. Add registers a key only for the
// key id and issuer that the application package's context_exchange block
// declares (ContextExchange.CheckKey), a key id is unique across applications
// because a lookup knows only the id, and the verifier then binds the token's
// application to the key's. Revoke is permanent and keeps its record; both
// calls write their audit record in their own transaction (the Record option),
// and a published TEST-ONLY key (IsPublishedTestOnlyKey) is refused unless the
// operator gives the explicit developer flag, and is never served from a row
// that did not come through it.
//
// The identitytest subpackage mints tokens for tests. It is imported only from
// _test.go files, never from production code.
package identity
