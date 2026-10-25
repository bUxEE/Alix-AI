// Package adapters is the seam between the AlixAI core and the host
// applications it serves (master plan 9.3). It holds the AppAdapter interface
// that every transport (MCP, OpenAPI, native) implements, the rules that make a
// reply from a host a fact the core may use, and the wrapper that applies the
// same safety rules to every transport. It contains no transport and no network
// code: the transports live in adapters/{mcp,openapi,native} and reach the
// network through internal/egress, and internal/adapters/fake is an in-memory
// application and adapter for tests.
//
// # What the core relies on
//
//   - One interface. Describe, Read, Validate, Prepare, Commit, Status, Cancel,
//     Health and Inspect. Nothing above this package knows which transport is
//     behind it. A transport is a transport: it carries requests and replies and
//     does not decide permissions, risk, confirmation or idempotency (those are
//     internal/policy and internal/actions), and the host application
//     reauthorises every read, prepare and commit.
//   - Minimal identity. Every call carries a contracts.Scope; only
//     IdentityFrom(scope) (application, tenant, workspace, principal, roles,
//     session) goes to the host. No scope, an invalid one or one bound to
//     another application is an error and nothing is sent.
//   - Typed facts only. A reply becomes an actions.DeterministicResult through
//     Normalise, or it is a schema_invalid error and is never partially used. The
//     check runs against the published JSON Schema (embedded here, with a drift
//     test), not just against the generated Go type, because the type cannot
//     express the schema's cross-field rules. operation and capability_id are the
//     core's: a reply that names a different operation, capability or
//     idempotency key is a reply to another request. Prose, a bare "ok", extra
//     members and wrong shapes all fail.
//   - Bounded everything. ReadLimited stops reading a reply at its size limit,
//     Wrap bounds time (default 30 s, never above 120 s) and concurrency, and
//     requests are bounded before they leave the process.
//   - Honest outcomes. A host that refuses answers with a result (status
//     rejected, with typed errors). Failures the core finds itself are typed
//     errors. A Commit that fails after it may have reached the host is
//     uncertain_outcome, never timeout, unavailable or schema_invalid on their
//     own, because those are retryable or look harmless and the effect may have
//     been applied: the coordinator asks Status first.
//
// # Writing a transport
//
// Implement AppAdapter over your wire protocol, read replies with ReadLimited,
// turn them into results with Normalise (passing the echoed idempotency key if
// the protocol has one), parse Describe, Health and Inspect replies with
// ParseDescription, ParseHealth and NormaliseInspection, never retry, honour
// the context, and classify failures as described on AppAdapter. Production
// wiring returns Wrap(transport, Options{ApplicationID: ...}). Then run the
// shared conformance suite (tests/conformance/adapters, RunAdapterSuite) with a
// fixture that puts internal/adapters/fake.App behind your wire.
//
// # Status, cancel and idempotency
//
// Commit is keyed by the idempotency key the core minted at prepare. The host
// applies the effect once per key and answers a repeat with the original
// result (or replayed, if its capability says so). Status(key) reports what the
// host recorded: ok, queued or partial with the same identifiers and values as
// the commit. Only the no-record marker (rejected with one validation_failed
// error on field idempotency_key, tested by StatusShowsNoRecord) proves nothing
// was applied and makes a retry of Commit safe. Any other result, including a
// rejection of the Status call itself (a denial, a scope error), leaves the
// outcome unknown, and nothing may be retried. Cancel withdraws pending work for
// a key and never reverses an applied effect.
//
// # Describe and versions
//
// Describe returns the contract versions the adapter and host speak and the
// operations mapped per capability, plus host operations that no capability
// maps (Unmapped), which the integration audit reports. Negotiate picks the
// highest version shared with SupportedContractVersions; a host that speaks only
// a higher major version is refused with validation_failed, never guessed at.
// Description JSON is frozen for version 1: members may be added as optional,
// and a reader ignores members it does not know.
//
// # Inspect
//
// Inspect returns, per requested aspect (versions, modules, dependencies,
// health, scopes, credentials, limits, storage, network), either facts the
// application chose to expose or the state unsupported. An aspect that was not
// answered is unsupported: the audit records it as unverified and AlixAI never
// scans for it.
package adapters
