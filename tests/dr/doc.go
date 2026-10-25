// Package dr is the disaster-recovery drill of milestone M6 (master plan 18 "Reliability and
// graceful degradation", 19 "Data lifecycle", 21 milestone 6 "reference deployment and
// disaster-recovery evidence"). It breaks a deployment of the clinic-booking reference
// application on purpose, at points that matter, and asserts what the plan says must be true
// afterwards: nothing is committed twice, an uncertain commit is resolved by asking the
// application and never by retrying, damaged derived data is rebuilt, a restore brings back every
// receipt and an audit chain that still verifies, and an interrupted backup is never mistaken
// for a finished one.
//
// # What runs for real
//
//   - The program. The tests build `alixai` from this checkout (CGO_ENABLED=0, as shipped) and
//     run it as a subprocess against a throw-away data directory in a temporary directory: every
//     operator step (trust, keys, package sign and install, audit, backup, restore, upgrade) is
//     the real command line, and the service is a real `alixai serve` process whose PID the test
//     holds. A service is stopped with a hard kill (TerminateProcess / SIGKILL), which is what a
//     crash or a power cut looks like to SQLite and to the application.
//   - The application. The clinic-booking reference application runs as a node subprocess
//     (test support mode, a loopback port the process picks) and AlixAI reaches it over MCP,
//     through the signed package's own endpoint. Every claim about what the application was asked
//     to do is made on the application's own counters (GET /test/stats), never on a double.
//   - The injection point. A small loopback MCP proxy (proxy_test.go) sits between AlixAI and the
//     application: the package is signed with the proxy's address as its endpoint. It passes
//     everything through, except that it can hold one named tool call so the test can kill the
//     service while that call is in flight: before the application has seen it, or after the
//     application applied it and before the reply came back.
//
// # What is seeded in-process, and why
//
// `alixai serve` starts its inference path only from a model file whose SHA-256 matches the
// registry (SEC-063) and has no seam for another provider, so no turn through the binary can
// produce a preparation on a machine without the model. The drill therefore creates the
// preparation the way the dialogue engine would after a turn: the test opens the data directory
// before the service starts and calls the real action coordinator (actions.Coordinator.Prepare)
// with the real MCP adapter built from the installed package (cli.BuildPackageAdapter), for a
// session of a real principal. The model only ever proposes (master plan 3 rule 5), so nothing
// authoritative is skipped: confirmation, commit, receipt, write-ahead audit and recovery are
// all the service's, over HTTP, with context tokens the application minted.
//
// # Evidence
//
// The tests log what they measure. With ALIXAI_DR_EVIDENCE_DIR set they also write
// report.md and results.json into that directory (docs/evidence/m6-dr); a default `go test` never
// writes into the repository. Timings come from the machine that ran the drill and are labelled
// as such: an emulated dev-box run is not a certified measurement.
//
// # Running it
//
// The tests need Go, node (22.18 or newer) and the clinic application's dependencies:
//
//	pnpm install --ignore-workspace --offline --frozen-lockfile   # in examples/reference-apps/clinic-booking
//
// Without them the tests fail, with that instruction; a drill that skips proves nothing. Nothing
// here uses the network, a model, Docker or a real key: signing uses the published TEST-ONLY keys
// and the throw-away keys the test generates.
package dr
