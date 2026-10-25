// Package conformancepkg holds the multi-application conformance suite of ADR 0016
// ("Several applications in one process, or one isolated deployment per security
// domain"): what it costs, in resident memory, to register one more application in a
// process, and whether the applications of one process can reach each other.
//
// Both are measured on the platform itself, not on a model of it. Several applications are
// registered in ONE set of services (one store, one audit log, one policy gateway, one
// capability registry, one action coordinator, one session store, one dialogue engine), as
// `alixai serve` composes them; each application is the clinic-booking reference package
// under its own application id, signed again with the TEST-ONLY publisher key, over its own
// host (the in-process stand-in of tests/conversation). The model is the scripted provider.
//
// # Isolation (isolation_test.go)
//
// TestTwoApplicationsShareNothing is a black-box scenario over the public surface of the
// services: application A prepares a booking; every identifier, secret and name that exists
// for A (its session id, its preparation id, its confirmation token, its source id) is then
// presented under application B's scope, which is identical to A's in every other level. B
// must not see A's session, preparation, receipt, audit events or sources, must not be able
// to confirm, reject or commit A's preparation, and must never cause a request to A's host;
// and A's own flow must be undisturbed (a positive control commits once).
//
// The identity layer's own cross-application tests (a key registered for one application
// cannot verify a token of another: internal/identity TestIssuerKeyScopedToApplication,
// TestWrongAudienceRejected) are not repeated here; the scenario starts after a token has
// been verified into a Scope, which is the only thing the services below it ever see.
//
// # Memory (memory_test.go)
//
// TestMarginalMemoryPerApplication registers 1, 2, 4, 8 and 16 applications and reads the
// process's resident set and Go heap after a full collection each time (idle), then serves
// conversations in all of them (loaded). The numbers are printed (go test -v) and are the
// ones ADR 0016 quotes: dev-host measurements, labelled as such, never certification.
//
// # Examples route (examples_route_test.go)
//
// TestEveryReferencePackageExampleRoutes puts every example of every user capability of the three
// reference packages (clinic-booking, facility-maintenance, ledger) through the rules-only router,
// once per role that may use the capability, and fails when the capability is not on the shortlist:
// the router matches titles and terminology only, so the fix for an example that does not route is
// in the package, never in the router. It needs no application and no model. The conformance
// conversation stage replays the examples of an MCP application only; this test is what shows the
// examples of the others (OpenAPI, native) reach their capability. Every example of the three packages
// routes, and there is no list of tolerated gaps.
package conformancepkg
