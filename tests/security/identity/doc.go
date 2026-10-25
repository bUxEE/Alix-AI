// Package identitysec is the identity security suite of master plan 10.1 and
// 16.4 bullet 3: what the application context token verifier, the identity
// middleware and the issuer key registry do when a token is replayed, when a
// request tries to name its own scope, and when an issuer key is revoked or
// rotated out.
//
// The suite drives the production components, never a script: the Ed25519
// verifier (internal/identity) over the SQLite replay cache and the issuer key
// stores, the real identity middleware inside the real gateway server
// (internal/gateway) in front of the real settings service and store
// (internal/settings), and, for the key lifecycle, the operator commands of the
// binary (`alixai identity add-issuer-key` and `revoke-issuer-key`, run
// in-process through cli.Run) and the audit log they write to. Tokens are minted
// by internal/identity/identitytest with keys generated per run.
//
// # Tests and the backlog (docs/security/test-backlog.md)
//
//	SEC-034  TestSecReplayCacheSurvivesRestart
//	SEC-035  TestSecScopeComesOnlyFromToken
//	SEC-037  TestSecRevokedIssuerKeyRejected
//
// # How each test proves it
//
// SEC-034 verifies a token, closes the store, opens a new verifier over the same
// database file (what a restarted process is) and presents the token again. It
// then advances the injected clock past the token's expiry and checks that the
// expired token is refused as expired, that the cache entry is pruned and that
// the same nonce is no longer held. Controls: a verifier over an empty store
// accepts the first presentation of the very same token, a fresh nonce is
// accepted after the restart, and an in-memory replay cache (the deliberately
// weak path) is flagged by the same restart detector.
//
// SEC-035 sends one request per scope level (application, tenant, workspace,
// principal, session, deployment) and per channel (query string, headers, JSON
// body, SDK-style members, a member inside the settings document) carrying a
// value that differs from the token, and observes the scope that reaches the
// settings service and the row that lands in the store. It then drops, empties
// or wildcards every level of the token itself. Controls: the same requests with
// matching values and with no extra values are accepted, and a deliberately
// leaky middleware that lets a header or the query string override the tenant is
// flagged by the same checker.
//
// SEC-037 provisions two issuer keys, revokes one with the operator command and
// shows that a token signed by it, minted before the revocation and still within
// its lifetime, is refused with unknown_key from the very next verification,
// that the removal is an audit event on the application's chain, and that the
// other key keeps working. A rotation overlap is bounded by the key's NotAfter:
// accepted one second before, refused as key_retired from that instant. Controls:
// the revoked-key token was accepted before the revocation, a revocation made
// through a store with no audit hook leaves no event and the event detector sees
// that, and the in-memory registry (the development seam) is held to the same
// rules.
//
// No test here touches the network beyond httptest, Docker or a real secret.
package identitysec
