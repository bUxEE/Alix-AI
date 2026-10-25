// Package adapterssec is the adapter and tool-confusion security suite of master plan
// 16.4 (b6, b7): what the core does with tool results that lie, tool names that
// collide or look alike, and capabilities of one application invoked in another.
//
// Every test drives the production components end to end and asserts on what they did,
// never on what a script said: the MCP adapter (adapters/mcp) and the OpenAPI adapter
// (adapters/openapi) over real loopback HTTP, the adapter contract wrapper
// (internal/adapters), the action coordinator (internal/actions) over a real store,
// audit log and policy gateway, the package loader (internal/apppkg), the capability
// registry (internal/capabilities) and the integration audit (internal/integrationaudit).
// The only scripted part is the host application: internal/adapters/fake.App behind the
// transports' own test fixtures, with a hostile reverse proxy between adapter and host
// that rewrites the real host's replies (rig_test.go).
//
// # Tests and the backlog (docs/security/test-backlog.md)
//
//	SEC-050  TestSecForgedToolResultRejected
//	SEC-052  TestSecToolNameCollisionAndLookalike
//	SEC-053  TestSecCapabilityIdsNamespacedPerApplication
//
// # SEC-050
//
// Sixteen kinds of forged result (an array, a bare string or number, prose, a status and
// nothing else, extra members, ok with errors, no freshness, values of the wrong type, a
// result for another capability, operation or idempotency key, a preparation without
// data versions, a cut body, an undocumented HTTP status, a non-JSON content type) are
// delivered at every point a result becomes a decision: a read, a prepare, the
// re-check of the data version at commit, the commit reply, and the Status call that
// resolves it. Each forgery is the honest reply planted with a random marker plus one
// defect, so the marker shows up in the receipt, the preparation or the store if (and
// only if) the reply was used. The assertions are the typed error (schema_invalid), no
// preparation row, no commit sent, no receipt, no state change, and the marker absent
// from every table of the store. A forged Commit reply is not a missing effect: the
// host did apply it, so the coordinator asks Status; an honest Status gives a committed
// receipt made of the host's own records, a forged Status as well gives uncertain with
// no records. Records of another type, another record than an update targets, and a
// reply replayed from another tenant are bound to the prepared action and refused.
//
// Controls: a well-formed reply carrying a marker, from the same server, is accepted at
// each point and the marker is delivered, stored and found by the store dump; the honest
// commit after a forged re-check goes through; every armed forgery is checked to have been
// delivered (a rule that never fired fails the case).
//
// # SEC-052
//
// Tool names are compared byte for byte. Names with whitespace, a zero-width space, a
// Cyrillic homoglyph, a path or a line separator are refused by the package loader; two
// names that differ only by case or by '_', '-' and '.' on one endpoint are a critical
// finding; a mapped name that the adapter configuration does not approve stops the MCP
// adapter being built. Against a hostile server that lists look-alikes of every mapped
// tool, renames or duplicates a mapped tool after approval, the exact mapped name
// executes and nothing else is ever called (the server counts tool calls); the renamed
// and duplicated cases are quarantined as drift. Capability ids with case, whitespace,
// homoglyph, zero-width, trailing-dot and path variants resolve nowhere: not in the
// registry, not in the policy gateway, not in the token, not in the adapter.
//
// # SEC-053
//
// Two real packages register the same raw capability names with different policies;
// decisions are made against the session's application only, a package or binding of
// another application decides nothing, and a crossed adapter or package source is
// stopped by the wrapper (scope_violation) and the gateway before any host is called.
// An OpenAPI operation the document defines and no capability maps is never reached
// by any spelling of its name; an operationId the document does not define stops the
// adapter being built. The integration audit run over the real MCP adapter reports every
// case, '_', '-' and '.' look-alike of a mapped tool as a critical ambiguous operation,
// and an honest application as none.
package adapterssec
