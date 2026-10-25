// Package posturesec is the exposed-surface suite of master plan 16.1 and 16.4 (b7, b10):
// what routes the service has, what they accept, and what they answer to a browser.
//
// Every test builds the real gateway (internal/gateway.NewServer) the way `alixai
// serve` composes it, with every route area mounted: the real store, audit log, trust
// store, package installer, application service, capability registry, policy gateway,
// action coordinator, session store, dialogue engine, files, history, stream hub and
// AG-UI runs behind the identity middleware, and minted single-use context tokens
// (server_test.go). Only the model, the RAG index, the voice runtime, the event intake
// and the settings and ops services are stand-ins, and they exist only so that the
// gateway mounts the routes that need them; the suite fails if any route area is left
// unmounted, so the inventory can never silently skip an area.
//
// # Tests and the backlog (docs/security/test-backlog.md)
//
//	SEC-054  TestSecNoArbitraryRegistrationRoutes
//	SEC-092  TestSecNoForbiddenRoutes
//
// # The specification
//
// The public API is specified by schemas/api (a root document and one fragment per
// area). spec_test.go merges it, resolves every $ref (local pointers, other documents
// and schema $id URNs) and gives the suite the operations and the resolved request body
// schema of each. The route inventory is compared with it in both directions.
//
// # SEC-054
//
// No route registers a server, tool, agent, API or source but the two that exist and are
// documented: a package archive goes into quarantine (and serves nothing until the
// integration audit activates it), and a RAG source can only be named by the declaration
// of the active package. No request body schema of any POST, PUT or PATCH route, at any
// depth, declares a command, URL, MCP server, tool or agent member or accepts a free-form
// object, with one documented exception: POST /v1/agui, whose body is the AG-UI
// protocol's RunAgentInput. The exception is pinned to exactly its documented members, and
// the suite checks that the deployment registers no client-provided tool. Live requests
// then show that bodies carrying such members are refused before any registry is asked,
// that a registration for another application, without the capability or with a
// traversing archive is refused and records nothing, that a registered package refuses
// every traffic route with quarantined until it is activated, and that the
// paths where a registration could live answer 404 or 405 to every method, with and
// without a token.
//
// # SEC-092
//
// Every registered route is on the specification and every operation on it has a route;
// a deny-list of surfaces (browser CORS proxy, code or command execution, agent hub, MCP
// or tool registration, model apply, install or gallery, admin or web UI, profiling,
// metrics unless the specification lists it, the OpenAI-compatible LocalAI surface and
// development routes) matches no route; and a sweep of every route with an Origin header,
// as itself and as a preflight, with and without a token, plus the error paths (404, 405,
// malformed and oversized bodies, a bad token) finds no Access-Control header. The dev spike
// endpoint is absent by default (404, and no /v0 route), appears as exactly one route when
// the flag is set, and is refused on every listener beyond loopback by Listen and, for an
// already bound listener, by Serve.
//
// # Controls
//
// Each detector is run first against inline planted violations, which it must flag, and
// clean input, which it must not. The route detectors are also run against the real
// server with routes planted through Options.ExtraAreas (a registration route, an exec
// route, a CORS proxy that sets a wildcard origin, a model apply route): the inventory
// comparison, the deny-list, the registration detector, the CORS sweep and the live path
// probe must each find them. The live path probe's control is a planted route that
// answers, so that the 404s of the real server cannot come from a probe that sees
// nothing.
package posturesec
