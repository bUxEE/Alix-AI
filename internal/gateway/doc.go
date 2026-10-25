// Package gateway is AlixAI's HTTP edge (master plan sections 5, 14, 16.1): a
// loopback-first listener, request IDs, body limits and timeouts, one typed
// error envelope, liveness and readiness, and the self-registering route
// areas the later milestones add to. It uses only the standard library's
// net/http.
//
// # Posture
//
//   - The listener is loopback-only. A non-loopback address (an empty host, a
//     wildcard, a LAN address or any other name) is refused unless a TLS
//     certificate and key are configured (SEC-093). The decision is made
//     before anything is bound and is repeated on the bound address.
//   - Authentication is not the gateway's: everything under /v1 runs behind
//     the identity middleware the composition root supplies (Options.V1Auth;
//     context tokens, internal/identity) and refuses every request until one
//     is. The only unauthenticated routes are /healthz, /readyz and the
//     dev-only spike endpoint, and the spike endpoint exists only when
//     dev.spike_endpoint is true and the listener is loopback or a unix socket
//     (SEC-092).
//   - No CORS headers are ever sent.
//
// # Error envelope
//
// Every failure is the bare contracts.Error JSON that schemas/common/error
// describes ({"code","message","retryable","details"}), the same shape
// identity.Middleware writes, so a /v1 client sees one error format. The
// request ID is in the X-Request-ID response header and in details.request_id;
// the gateway always mints it and never trusts one from the caller. Nothing
// from the wrapped cause of an error, and no text of an untyped error, is sent
// to the client (SEC-083). The HTTP status comes from the error code unless the
// caller knows better (model output that fails its schema is 502, not 4xx).
//
// # Routes and services
//
// Each route area is one routes_<area>.go file that calls RegisterArea from
// init(); NewServer applies the areas in name order. An area registers
// "METHOD /path" patterns on a Router, and /v1 routes through Router.V1, which
// wraps them in Options.V1Auth (identity.Middleware) and, while that is nil,
// refuses every request. Unknown paths and wrong methods answer with the
// envelope too. The /v1 areas of the public API (master plan 14.2) are
// v1_sessions, v1_turnevents, v1_actions and v1_apps; the areas of M2 and later (rag, files,
// settings, history, voice, providers, oauth, decision) are further files of
// the same shape and nothing in this package lists them.
//
// An area gets what it needs from a typed service registry (services.go), not
// from a struct that every area widens and not from Options.ExtraAreas:
//
//	// routes_things.go
//	func init() { RegisterArea("v1_things", registerThings) }
//
//	func registerThings(rt *Router, d *Deps) {
//		n := d.Needs()
//		things := Need[ThingService](n) // required: by static type
//		audit, _ := Want[audit.Appender](n) // optional
//		if !n.Ready() {
//			return
//		}
//		rt.V1().HandleFunc("GET /things", ...)
//	}
//
//	// the composition root (internal/cli), and a test with fakes
//	svc := gateway.NewServices()
//	gateway.Provide[gateway.ThingService](svc, impl)
//	gateway.NewServer(gateway.Options{Services: svc, ...})
//
// A service is registered under the type it is provided as, so a handler holds
// the narrow interface the area names (ActionService has none of the
// coordinator's cross-tenant sweeps) and never the whole object. The rule for
// an area whose services are missing is fail closed in both directions: none
// provided, and the area mounts nothing (Server.Unmounted names it, and a
// server with no services at all serves only its probes); only some provided,
// and NewServer fails, so a handler never holds a service it does not have.
// The composition root refuses to serve while Unmounted is not empty, and the
// route inventory and OpenAPI tests build their server with every area's
// fakes (a test file registers them with registerTestServices) and fail for an
// area that mounted nothing, so a new area cannot slip past either check.
//
// # Dev-only spike
//
// POST /v0/dev/propose (routes_dev.go) is the smallest surface that lets M0-13
// prove one text request runs through AlixAI and LocalAI. It takes
//
//	{"text": "...", "today": "2026-10-12",
//	 "capabilities": [{"id": "demo.create", "description": "Create a demo",
//	   "fields": [{"name": "count", "type": "integer", "required": true},
//	              {"name": "kind", "type": "enum", "enum": ["a", "b"]}]}]}
//
// builds a short prompt (the seed of the M1 prompt builder: a trusted system
// message with the catalogue, the user text fenced as data), asks the
// controller for a JSON object constrained by a compact schema narrowed to the
// catalogue, and then validates the answer before returning it: it must satisfy
// the model-authority subset of schemas/conversation/task-proposal, it must
// satisfy the compact schema it was constrained with (so a capability outside
// the catalogue is refused) and, once the core has attached the trace_id, the
// whole TaskProposal schema. Anything else is a 502 envelope with code
// schema_invalid and no part of the model's output. The route is deleted or
// replaced by the real /v1 turn API in M1-16.
//
// # Config
//
// The package registers two sections: "gateway" (timeouts, limits, TLS) and
// "dev" (spike_endpoint, default false). The listen address is the core
// server.listen section.
package gateway
