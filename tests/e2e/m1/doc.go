// Package m1 is the end-to-end exit gate of milestone M1, "Application contract and safe
// controller" (master plan 21): natural multi-turn text completes read and write workflows,
// every write goes through deterministic prepare / confirm / commit, unauthorised, unknown and
// malformed actions fail closed, replay and stale confirmations are refused, and an application
// cannot activate until its integration audit is clean.
//
// # What runs for real
//
//   - The application. The clinic-booking reference application is started as a subprocess
//     (node, test support mode, a loopback port the process picks) and is reached over MCP by
//     the real MCP adapter, built from the signed package's own endpoint, adapter
//     configuration, operation bindings and egress allowlist. Every assertion about what the
//     application was asked to do is made on the application's own counters and records
//     (GET /test/stats, the MCP tools), never on a test double.
//   - The operator. Trust, install, audit, accept-warning, identity and quarantine are the real
//     `alixai` commands, run in-process through cli.Run against one data directory. The
//     integration audit's runtime probes run against the application subprocess through the
//     MCP provisioning bridge. The package under test is the clinic's own signed package,
//     retargeted at the port the subprocess got and signed again with the TEST-ONLY publisher
//     key (the manifest pins a port, and a port cannot be pinned for a test run).
//   - The host. Context tokens are minted by the application itself (POST /host/context-token)
//     and verified by AlixAI against the issuer key the operator registered; every request
//     carries a fresh single-use token.
//   - The service. The public API of milestone M1 (sessions, turns, actions, applications) is
//     served by a real gateway.Server behind identity.Middleware on a loopback port, and every
//     call in the tests is an HTTP request to it.
//
// # What is scripted
//
// The model. The controller is the scripted fake provider (internal/providers/fake): the test
// decides what the model proposes and what text it offers, and every assertion is on what the
// core did with it. A model call nobody scripted fails the turn, so nothing here passes on a
// silent fallback. The quality of a live model is measured by tools/bench, not here.
//
// # Why the service is composed here and is not `alixai serve`
//
// `alixai serve` starts its inference path only from a model file whose SHA-256 matches the
// registry (SEC-063) and has no seam for another provider, so a scripted provider cannot be
// given to it from outside the package. The rig (rig_test.go) therefore builds, in the same
// order and from the same exported constructors, what cmd_serve.go's wireV1 builds: the audit
// log, the trust verifier, the policy gateway with its audit sink, the installer and the
// package source, the capability registry, the action coordinator, the session store, the
// dialogue engine, the stream hub, the identity verifier over the operator-provisioned issuer
// keys, and the admin interface the operator commands tell about an activation or a
// quarantine. The one swap is the provider selector.
//
// # Running it
//
// The tests need node (22.18 or newer) and the clinic application's dependencies:
//
//	pnpm install --ignore-workspace --offline --frozen-lockfile   # in examples/reference-apps/clinic-booking
//
// Without them the tests fail, with that instruction; a gate that skips proves nothing.
package m1
