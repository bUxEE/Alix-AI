// Package injectionsec is the injection suite of master plan 12.6 and 16.4 (bullet 1) for the
// three sources of text that reach a model without being the core's own: the user's turn, the
// application's own data, and what an MCP server says about itself and its results.
//
// Text from any of them is data. It cannot make the model reveal a secret, cannot become an
// instruction, and cannot widen what the engine will do with the model's output. Each test here
// drives the real dialogue engine end to end (the real policy gateway, action coordinator, session
// store, capability registry over the clinic reference application's signed package, and the real
// adapters.Wrap guard) and asserts on what the engine and the application did, never on what a
// script said. The only scripted part is the model: the fake provider records every prompt it
// receives, with the scope it was called under, and the tests read that record.
//
// # Tests and the backlog (docs/security/test-backlog.md, section 4.1)
//
//	SEC-002  TestSecUserTurnCannotExfiltrateSecrets
//	SEC-007  TestSecAPIDataFencedAsData
//	SEC-008  TestSecToolResponseCannotInstructModel
//
// # The world
//
// The application is the clinic stand-in of tests/conversation (ClinicHost), planted through its
// exported host events and put behind a router (rig_test.go) that records every request the
// application receives and serves one host per tenant, so a second tenant's data is real data in
// a real host reached through the same engine, store and session database as the first. A router
// is the only addition: the rest is the wiring tests/conversation uses.
//
// # Controls
//
// Every "X never appears" assertion has a control that X would have been seen: the canary of the
// same tenant's authorised record does appear in the prompt that legitimately needs it, a canary
// planted into a prompt by hand is flagged by the same scanner, an unfenced message is flagged by
// the same fence check, the benign flow completes and does reach Prepare, and a well-formed reply
// from a hostile server is accepted. A test that passes because nothing was scanned is a failure.
//
// # Secrets and canaries
//
// Every canary is generated at run time (random), never a literal. The context token is a real
// one, minted by the identity test issuer, verified by the real verifier, and its verified scope
// is what the engine runs under; the raw token, its parts, its nonce and the planted credential
// are scanned for in every prompt. The engine only ever receives the verified scope: that no
// credential has a path into it is the point, and the scan proves it for the values a deployment
// does hold (the token, the API key a runtime would be given by a secret reference, the host's
// foreign-tenant data).
package injectionsec
