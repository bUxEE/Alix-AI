// Package mcp is the MCP transport of master plan 9.3: an adapters.AppAdapter that
// reaches an application through its MCP server. MCP is the preferred way to
// discover and call an application's operations, and AlixAI "filters and wraps
// MCP tools; it never gives a model unrestricted access to an entire MCP server".
// This package is the filter and the wrapper. It is a transport and nothing else:
// authorisation, risk, confirmation and idempotency belong to the policy and
// action layers, and the application reauthorises every request.
//
// The package imports no application package. A composition root (internal/cli,
// transport_mcp.go) turns a verified package into a Config; the adapter is built
// from that and from nothing the server says.
//
// # What is reachable
//
// Config maps capability operations to tool names, as the package declares them
// (read, validate, prepare, commit, status, and the resolve operation of an entity
// type). Those tools, and the three management tools below, are the only tools
// the adapter ever calls. It lists the server's tools to compare them with the
// mapping and to check the pins, never to find something to call: a tool the
// package does not map is not callable by any request, whatever a capability ID,
// a field or a model says, and its name is reported to the integration audit
// (Description.Unmapped) in a form that is safe to print. Names are compared byte
// for byte: a tool that differs from a mapped one by case, by "-" for "_", by a
// homoglyph or by a control character is another tool, and is reported as unmapped
// (a name that is not a valid tool name is reported by its length and a hash
// prefix only). A tool the adapter configuration (tools.allow) approves but no
// capability maps is not callable by a capability and is not reported as unmapped; a host
// reaches it only through CallApprovedTool.
//
// Tool descriptions, titles and annotations, error text and result text a server
// sends are display text for people. They never reach a model, a Description, an
// error or a log through this package (master plan 16.4, SEC-008).
//
// # The wire contract
//
// Every capability tool takes the minimum identity AlixAI sends (the six members of
// adapters.Identity and nothing else: no deployment ID, no capability scopes, no
// credential) and the call's own members:
//
//	read, validate, prepare   {"identity": {...}, "fields": {...}}
//	commit                    {"identity": {...}, "idempotency_key": "...", "prepared": {PreparedAction}}
//	status, cancel            {"identity": {...}, "idempotency_key": "..."}
//	describe, health          {}
//	inspect                   {"identity": {...}, "aspects": ["versions", ...]}   (aspects omitted: all)
//
// and answers with structuredContent: the host members of a DeterministicResult
// (status, identifiers, values, errors, freshness, allowed_next_actions, and for
// prepare the preparation). operation and capability_id are the core's and the
// server does not state them; one that does must state the right ones. The same
// JSON is usually mirrored as text, which is ignored. A refusal is a result with
// status rejected, never a tool error. A server may echo the idempotency key of
// the call in the result's _meta under "alixai/idempotency_key"; one that does must
// echo the key it was sent.
//
// A binding's Arguments rename capability fields to tool argument names for the
// fields that differ; the rest keep their names.
//
// # Entity resolution
//
// The adapter contract has no resolve operation. The core resolves a mention with
// a Read of the capability that declares the field, whose fields carry the
// reserved arguments entity_type, mention and (when the model proposed one)
// candidate_id. A Read with entity_type is routed to the resolve tool the
// capability declares for that type (entity_types.<t>.resolve) and to no other,
// with its fields unchanged; an entity type the capability does not declare is
// validation_failed and nothing is sent. A Read without entity_type goes to the
// capability's read tool, and a capability that maps none has no read
// (unknown_capability). That a capability resolves a type does not make read an
// operation of it: Description lists only the operations the package maps.
// Several matches are several identifiers in the result, none is an empty result:
// that is the application's answer, not the adapter's. The application also decides
// what candidate_id means (alone it re-resolves exactly that record, access
// checked; with a mention it changes nothing).
//
// # Validate and the version Prepare stored
//
// A server must make Validate, on unchanged data, report in freshness.version the
// version that Prepare reported: the snapshot version of the records the action
// relies on (a digest of each record's own revision counter), not a per-response
// ETag and not a tenant-wide or site-wide counter. A server that breaks this makes
// every commit stale or invalidates every open preparation. The adapter passes the
// version through; the integration audit's probes check it.
//
// # Management tools
//
// Three tools carry the operations that are not capability operations. They are
// conventions of this adapter and no capability maps them:
//
//	describe   the host's contract_versions and application_version (its own claims
//	           about capabilities and unmapped operations are not believed)
//	health     {"status": "ok"|"degraded"|"unavailable", "reason", "application_version"}
//	inspect    {"reports": [...]}, the aspects the application chooses to expose
//
// A server without them is still usable: its contract version is the adapter's own,
// its health is the answer to an MCP ping, and every aspect is unsupported (which
// the audit records as unverified, never as passed). Their names can be changed in
// the Config.
//
// # The adapter configuration of a package
//
// The file a manifest's adapter_config names is read by ParseAdapterConfig:
//
//	server: clinic-mcp        # optional; when present it must be the endpoint's ID
//	tools:
//	  allow: [availability_list, ...]
//
// tools.allow must list every tool the capabilities map on that endpoint; a mapped
// tool it does not list is an error when the adapter is built. Unknown members are
// an error. Version 1 of the format has nothing else.
//
// # Pins and drift (SEC-049)
//
// Each tool the adapter calls is pinned by the SHA-256 of the canonical JSON of its
// name, input schema and output schema (descriptions and annotations are not part
// of it). A pin is the one the Config was given (Config.Pins, which an audit takes
// from Adapter.Pins), otherwise the hash at the first listing. Endpoint.ContractSHA256
// is the manifest's signed pin of the approved list as a whole: ContractSHA256 of
// the names and input schemas of the mapped tools of the endpoint.
//
// The tools are listed again before the first call, after every new session, when
// the server says its list changed and at least every Config.SurfaceRecheck (5
// seconds by default; a call can therefore still reach a tool that changed during
// that interval, and an application can change what a tool does without changing its
// schema, which no pin can see). A listing that shows a mapped tool with another
// hash, missing, listed twice with different definitions, or a list that no longer
// hashes as the manifest approved is Drift. Drift is sticky until the adapter is
// rebuilt after a re-audit: calls to the tool (to every mapped tool of the endpoint
// for a changed contract hash) are refused with quarantined (reason
// tool_schema_drift) before anything is sent, which is why a refused Commit is not an
// uncertain outcome; Describe leaves the operation out, which the audit reports as
// unreachable; Adapter.ReauditRequired is true; Config.OnDrift is called once per
// drift. A new MCP session over the same adapter keeps its pins.
//
// # Results
//
// A tool result is used only if it carries structuredContent that is a JSON object
// no larger than Config.MaxResponseBytes and that adapters.Normalise accepts as the
// DeterministicResult of this call. Text is never read, not even text that is a
// perfect result: a result stated only as text (schema_invalid, reason
// text_only_result), a tool error (isError), a server that asks the client for input
// (the adapter offers no sampling, elicitation or roots) and a structured value that
// is not an object are all refused, and none is partly used. A Commit whose reply
// is unusable is uncertain_outcome, because the server may have applied it.
//
// # Failures
//
// The adapter never retries. One exception: a server that answers that it does not
// know the session (the request was refused, not processed) gets a new session and
// the call is sent once more.
//
//	nothing was sent (egress refusal, connection refused, certificate refused,
//	session not opened, adapter closed)     unavailable, also for Commit
//	the tool is not offered by the server    unknown_capability (operation_not_offered)
//	the package maps nothing for the call    unknown_capability (operation_not_mapped)
//	a tool has drifted                       quarantined (tool_schema_drift)
//	the caller's deadline or cancellation    the context's own error
//	a reply that is too large, unusable      schema_invalid (response_too_large, ...)
//	possibly delivered (reset, EOF, tool or  Commit: uncertain_outcome; others: unavailable
//	JSON-RPC error)
//
// adapters.Wrap types the context errors (timeout, and uncertain_outcome for Commit).
// Errors carry codes and fixed reasons, never a URL, a query, a credential or text the
// server chose.
//
// # Connections
//
// HTTP endpoints use the official Go SDK's streamable HTTP client, over an
// egress.Dialer: every connection and every redirect is held to the destination
// allowlist (master plan 16.1), certificates are verified, there is no proxy, and a
// reply is cut off at twice MaxResponseBytes plus 64 KiB. The server-to-client stream
// is not opened. The endpoint's Authorization value is sent only to requests for the
// endpoint's own origin. Sessions are opened lazily, one per endpoint, and opened
// again after one breaks; a call that fails because the connection broke is not
// repeated.
//
// A stdio endpoint (Endpoint.Command) runs a local server as a child process, started
// directly with only the environment it is given and no stderr, whose output is
// bounded the same way, and which is stopped (closed, then killed after two seconds)
// when its session ends. An application package cannot declare one, because a package
// cannot make AlixAI start a process: stdio is for hosts that wire the adapter in code.
//
// The protocol revision asked for is 2025-11-25. The SDK serves tool lists from a cache
// under the newer revision, which would hide drift.
//
// # Limits of this package
//
// With several endpoints, Describe reports the contract versions all of them speak and
// Inspect the first endpoint that has an inspect tool. A package credential_ref is
// resolved by the composition root into Endpoint.Authorization (header and scheme from
// the adapter configuration, defaults Authorization / Bearer, mirroring OpenAPI) and
// sent only on streamable HTTP / SSE to the endpoint's own origin; stdio transports
// ignore it. The SDK is github.com/modelcontextprotocol/go-sdk v1.8.0 (Apache-2.0 for
// new contributions, MIT for older ones).
package mcp
