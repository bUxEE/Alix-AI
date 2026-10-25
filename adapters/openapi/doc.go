// Package openapi is the OpenAPI/HTTP transport of master plan 9.3: an
// adapters.AppAdapter that reaches an application through an approved subset of
// its HTTP API. The application package maps API operations (operationIds) to
// AlixAI capability contracts; this package builds the typed requests, sends them
// through the egress gate and turns the replies into facts. It is a transport and
// nothing else: authorisation, risk, confirmation and idempotency belong to the
// policy and action layers, and the application reauthorises every request.
//
// The package imports no application package. A composition root (internal/cli,
// transport_openapi.go) turns a verified package into a Config; the adapter is
// built from that and from nothing the application says. The OpenAPI library is
// github.com/getkin/kin-openapi v0.149.0 (MIT).
//
// # What is reachable
//
// Config maps capability operations to operationIds, as the package declares them
// (read, validate, prepare, commit, status, and the resolve operation of an entity
// type). Those operations, and the management operations of the adapter
// configuration (below), are the only operations the adapter ever calls. The
// document is read to build the requests and to check the replies, never to find
// something to call: an operation the package does not map is not callable by any
// request, whatever a capability ID, a field or a model says, and its name is
// reported to the integration audit (Description.Unmapped). Names are compared byte
// for byte.
//
// Each role has a method rule, checked when the adapter is built: read, status,
// resolve and the management operations that only read are GET or POST (a search
// with a body is a read); validate and prepare are POST, PUT or PATCH; commit and
// cancel are POST, PUT, PATCH or DELETE. A GET is never a state change, which
// caches, prefetchers and crawlers may repeat, and a read is never a PUT, PATCH or
// DELETE. A mapped operationId that the document does not define, or defines
// twice, is an error.
//
// # The document
//
// The OpenAPI 3.x document (JSON or YAML) is loaded from the package and validated.
// It is loaded without any external reference: a $ref to a file or a URL is an
// error. Its `servers` are ignored, because a document cannot say where requests
// go (the endpoint's base_url does, and it must be on the egress allowlist), and
// so are its `security` requirements (the adapter configuration says how the
// credential is sent). Every approved operation must describe at least one 2xx
// response as application/json with a schema, may take only path, query and header
// parameters in the default styles (form for query, simple for the others; no
// object parameters, no cookies, no media-type encoded parameters), and may take a
// request body only as application/json with a schema. A document that needs more
// than that is refused when the adapter is built: nothing is checked best-effort.
//
// # The wire contract
//
// A call becomes one HTTP request to base_url plus the operation's path:
//
//	where a value goes            what
//	----------------------------  ----------------------------------------------
//	path, query, header params    the call's fields whose names (after the
//	                              binding's renames) the operation declares; lists
//	                              as the parameter's style says
//	JSON request body             every other field, as a member of one object;
//	                              for a Commit the PreparedAction itself
//	X-AlixAI-Identity header      adapters.Identity as unpadded base64url of its
//	                              JSON: application, tenant, workspace, principal,
//	                              roles and session, nothing else (no deployment
//	                              ID, no capability scopes, no credential)
//	Idempotency-Key header        the idempotency key of a Commit, Status or Cancel;
//	                              also the value of a path or query parameter named
//	                              idempotency_key when the operation declares one
//	Authorization header          the credential, added on the way out (below)
//
// The names of the identity, idempotency and credential headers are settings of
// the adapter configuration. A field whose name matches no parameter and no place
// in a body is validation_failed and is not sent; a field never fills a header the
// protocol owns (the credential, identity and idempotency headers, Content-Type,
// Host and the like), and a value that cannot be carried (a path segment that is
// empty, a dot segment or holds a separator, a list item with a comma in a
// comma-separated list, a control character in a header) is refused rather than
// bent. Defaults the document declares are not filled in: what is validated is
// what is sent. The request is then validated against the operation's description
// (parameters, required members, schemas): a request the document does not allow is
// validation_failed and nothing is sent. Errors name the place of the violation and
// never the value.
//
// A Read whose fields carry the reserved argument entity_type is the core's entity
// resolution (the adapter contract has no operation of its own for it): it goes to
// the operation the capability declares as the resolver of that type
// (entity_types.<t>.resolve) and to no other, with the reserved arguments mention
// and candidate_id as fields. entity_type itself is sent only where the operation
// declares a place for it (a resolver of one type needs none, a shared one does);
// an entity type the capability declares no resolver for is validation_failed and
// nothing is sent. Several matches are several identifiers in the result, none is
// an empty result: that is the application's answer, and what candidate_id means
// is the application's decision.
//
// # Replies
//
// The host's members of a DeterministicResult travel as the JSON body, with the
// HTTP status the application picks (a REST host answers 200, 403, 404, 409, 422 or
// 503 for different results; the status carries no meaning of its own here). A
// reply is used only if it passes four checks, in this order, and none of it is
// used otherwise:
//
//  1. its status is one the document describes for the operation (the exact code,
//     then its range such as 4XX, then default), and the description has an
//     application/json schema for it;
//  2. its Content-Type is application/json;
//  3. its body satisfies the response schema (and its headers the declared
//     response headers);
//  4. adapters.Normalise accepts the body as the DeterministicResult of this call:
//     the operation and capability_id are the core's (a reply that states other
//     ones is a reply to another request), prose, a bare "ok" and extra members
//     fail, and, when the reply echoes the idempotency key in the idempotency
//     header, it must be the key that was sent.
//
// A refusal by the application is a result with status rejected, never an error.
//
// # Validate and the version Prepare stored
//
// An application must make Validate, on unchanged data, report in freshness.version
// the version that Prepare reported: the snapshot version of the records the action
// relies on (a digest of each record's own revision counter), not a per-response
// ETag and not a tenant-wide or site-wide counter. An application that breaks this
// makes every commit stale or invalidates every open preparation. The adapter passes
// the version through; the integration audit's probes check it, and the fixture of
// this package (testdata/fixture) keeps it.
//
// # Failures
//
// The adapter never retries. Every request asks for its connection to be closed, so
// a connection is never reused and net/http never repeats a request on its own
// (it does when a reused connection turns out to be dead, and it treats a request
// with an Idempotency-Key header as safe to repeat: a repeat of a Commit is the
// coordinator's decision, taken after Status). Redirects are never followed: an API
// that redirects is misconfigured, a 307 of a POST would send a Commit twice, and a
// credential must not follow one.
//
//	nothing was sent (egress refusal, connection refused, certificate refused,
//	credential unreadable, request violates the document, adapter closed)
//	                                       unavailable or validation_failed, also for Commit
//	the package maps nothing for the call  unknown_capability (operation_not_mapped)
//	the caller's deadline or cancellation,
//	the operation's timeout                the context's own error (Wrap types it)
//	a reply that is too large, not JSON,
//	not described, violates the document
//	or is not a DeterministicResult        schema_invalid, with a reason
//	500 or more, 429, a redirect without a
//	usable result                          unavailable, rate_limited, schema_invalid
//	possibly delivered (reset, EOF, timeout, or any of the three rows above for a
//	Commit)                                Commit: uncertain_outcome; others: unavailable
//
// A Commit that was sent and has no usable result is always uncertain_outcome, never
// unavailable, rate_limited or any code that says nothing happened: the application
// may have applied it, and the caller asks Status before it does anything else.
// Errors carry codes and fixed reasons, never a URL, a query, a header, a credential
// or text the application chose.
//
// # Credentials
//
// An endpoint that declares credential_ref has its credential resolved from the
// opaque reference by a SecretProvider (the environment, a file and the OS store
// come with the credential service), once per request, and sent as one header: the
// adapter configuration's auth.header (Authorization by default) with auth.scheme
// (Bearer by default; "" sends the value as it is). The credential is added by the
// HTTP client on the way out, only to requests for the endpoint's own origin; it is
// not in the request the adapter builds, not in what is validated against the
// document (a header the document declares for it is satisfied by a placeholder of
// the same shape), not in an error, not in a log record and, as a Secret, prints as
// "[redacted]" however it is formatted. The reference is not sent, not logged and
// not put in an error either. An endpoint that declares a reference when no
// provider is configured is refused when the adapter is built, and a provider's own
// error text is never passed on.
//
// # The adapter configuration of an endpoint
//
// The file a manifest's adapter_config names is read by ParseAdapterConfig and is
// strict: unknown members are an error. See AdapterConfig for its members: the
// credential header and scheme, the identity and idempotency header names, and the
// management operations.
//
// # Management operations
//
// Three operationIds named by the adapter configuration carry the operations that
// are not capability operations. They are conventions of this adapter, like the
// management tools of the MCP adapter; no capability maps them and a model never
// sees them:
//
//	describe   GET, no parameters; 200 {"contract_versions": [1], "application_version": "...",
//	           "unmapped": [...]} (its own claims about capabilities are not believed)
//	health     GET, no parameters; 200 {"status": "ok"|"degraded"|"unavailable", "reason": "...",
//	           "application_version": "..."}
//	inspect    GET with a query parameter aspects (a list); the identity header;
//	           200 {"reports": [...]}, the aspects the application chooses to expose
//
// Without describe, the contract version is the adapter's own. Without inspect,
// every aspect is unsupported, which the audit records as unverified, never as
// passed. Without health there is nothing to ask: the adapter connects to the
// endpoint's host and port (a connection, not a request: no operation is called)
// and reports degraded with a reason that says only reachability was checked, or
// unavailable when the connection is refused. It never reports ok for an
// application whose health it did not read.
//
// The operations of the audit's disposable tenants (fixtures.provisioning:
// create_tenant, seed, cleanup) are listed in management.provisioning. They are
// approved by the signed adapter configuration and are not reported as unmapped, but
// the adapter contract cannot reach them: only Manage can, which a bridge built by a
// composition root for the integration audit calls. Manage sends no identity, takes
// the members of its arguments as parameters or body members like fields, checks the
// request and the reply against the document like any other call, and returns the
// status and body of any reply the document describes (a 404 for a tenant that is
// already gone is the caller's to read).
//
// # Pinning the approved subset
//
// Endpoint.ContractSHA256 is the manifest's contract_sha256: the pin of the approved
// operation subset. ContractSHA256 defines it: the hash of the canonical JSON of the
// approved operations as the document states them, with local $refs resolved and the
// text meant for people (descriptions, summaries, examples, extensions) left out. A
// document whose subset hashes differently is refused when the adapter is built, so a
// change to what an approved operation takes or answers needs a new signed package and
// a new audit, while documenting an operation or adding an unrelated one does not.
//
// # Limits of this package
//
// Replies are cut off at Config.MaxResponseBytes (1 MiB by default), after any
// transparent decompression, and a request body is limited to 1 MiB. The size of the
// reply headers is the HTTP stack's own limit, which the egress client does not
// expose. An operation that needs more than the default parameter styles, form or
// multipart bodies, cookies or OAuth flows is not supported: the document is refused.
// With several endpoints, Describe reports the contract versions all of them speak,
// Inspect the first endpoint that has an inspect operation, and Health the worst
// state.
package openapi
