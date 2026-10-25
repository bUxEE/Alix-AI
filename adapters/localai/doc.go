// Package localai is the only code in AlixAI that knows LocalAI exists (master
// plan section 6: "LocalAI is always accessed through AIRuntimeProvider"). It
// implements internal/providers.TextGenerator, Embedder and AIRuntimeProvider
// over LocalAI's OpenAI-compatible HTTP API. Nothing outside adapters/localai
// and the composition roots (cmd/, internal/cli/, tools/) may import it.
//
// The adapter imports only the base packages (pkg/contracts, internal/clock,
// internal/providers) and the standard library. It starts no process, reads no
// model file and never calls an installation or administration route.
//
// # Pinned runtime
//
// Everything below was read from the LocalAI v4.10.0 sources and documentation
// (the version pinned by deploy/container/localai):
//
//   - chat handler (response_format, grammar, streaming):
//     https://github.com/mudler/LocalAI/blob/v4.10.0/core/http/endpoints/openai/chat.go
//   - request and response types:
//     https://github.com/mudler/LocalAI/blob/v4.10.0/core/schema/openai.go
//   - JSON schema item type used for the grammar conversion:
//     https://github.com/mudler/LocalAI/blob/v4.10.0/pkg/functions/function_structure.go
//   - embeddings handler:
//     https://github.com/mudler/LocalAI/blob/v4.10.0/core/http/endpoints/openai/embeddings.go
//   - liveness and readiness routes:
//     https://github.com/mudler/LocalAI/blob/v4.10.0/core/http/routes/health.go
//   - /version route:
//     https://github.com/mudler/LocalAI/blob/v4.10.0/core/http/routes/localai.go
//   - routes that need no key:
//     https://github.com/mudler/LocalAI/blob/v4.10.0/core/http/auth/public_routes.go
//   - grammar parameter (documentation, llama.cpp backend only):
//     https://localai.io/docs/features/constrained_grammars/
//   - authentication (a legacy key is a full administrator credential):
//     https://localai.io/docs/features/authentication/
//
// # Structured output
//
// A providers.OutputConstraint is sent in one of two ways, never both:
//
//   - JSON Schema: the OpenAI field
//     "response_format": {"type": "json_schema", "json_schema": {"name":
//     "alixai_output", "strict": true, "schema": <the schema>}}.
//     LocalAI converts the schema to a GBNF grammar on the server (chat.go,
//     "If we are using a response format, we need to generate a grammar").
//   - GBNF grammar: the LocalAI/llama.cpp field "grammar": "<GBNF text>"
//     (llama.cpp backend only, per the constrained-grammars page).
//
// Two properties of v4.10.0 shape every caller:
//
//  1. The json_schema constraint is best effort, and it can fail open. The
//     schema is decoded into functions.Item, which keeps only the top-level
//     "type" and "properties"; top-level "required", "additionalProperties",
//     "items", "enum" and "$defs" are dropped. If the conversion fails LocalAI
//     only logs "Failed generating grammar" and then runs the request
//     unconstrained. The adapter therefore never assumes the constraint held:
//     it parses the output with providers.ParseJSONOutput (schema_invalid when
//     it is not exactly one JSON document), and ChatResult.JSON is only
//     syntactically valid. Callers must validate it against the schema they
//     sent. A caller that needs the decoder itself constrained supplies a
//     GBNF grammar, which is passed through untouched.
//  2. Temperature is a pointer on the LocalAI side, so an omitted value falls
//     back to the model configuration's default. The adapter always sends the
//     temperature, including 0, so "greedy" is what the caller asked for and
//     not what the model file says.
//
// The adapter sends no "tools", "functions" or "tool_choice" and executes
// nothing LocalAI returns (hardening checklist HC-39). A response that carries
// tool calls, or a finish reason other than "stop" or "length", is a
// schema_invalid failure.
//
// # Streaming
//
// Streaming requests set "stream": true and "stream_options":
// {"include_usage": true}. LocalAI answers with server-sent events:
// "data: {chunk}" lines, a final chunk whose finish_reason is set, an optional
// trailer with "choices": [] and the usage totals, and always "data: [DONE]".
// A worker error mid-stream arrives inside the HTTP 200 as
// "data: {"error": {...}}" followed by "data: [DONE]". The stream therefore
// reports a Done chunk only when a finish_reason was seen, no error event
// arrived and [DONE] was received; the Done chunk is followed by io.EOF. A body
// that ends without [DONE] is unavailable, an error event is its mapped error,
// and neither is ever a success.
//
// # Health, readiness and the observed version
//
// Health calls, in order:
//
//   - GET /readyz (no key needed): 200 is ready, 503 with {"status":
//     "starting"} means the startup preload is still running. Readiness turns
//     200 even when a model failed to download, so
//   - GET /v1/models (key) must list the configured models: a missing
//     controller model is unavailable, a missing router or embedding model is
//     degraded.
//   - GET /version (key) answers {"version": "v4.10.0 (<commit>)"}. The version
//     token is reported in HealthReport.Version (SEC-072), the commit is kept
//     for diagnostics, and neither is trusted until it matches the shape
//     checked in ParseVersion. When Config.QualifiedVersions is set, any other
//     version makes the runtime unavailable; otherwise callers compare
//     HealthReport.Version with the qualified set themselves.
//
// Only the runtime version is observable through the allowlisted paths: the
// backends' versions come from routes that are administration (/backends,
// /backend/monitor, /system) and are deliberately not callable. The backend
// version is pinned by digest in deploy/container/localai/config instead.
//
// # Endpoint allowlist
//
// The client may call exactly the paths in AllowedEndpoints: /readyz,
// /version, /v1/models, /v1/chat/completions and /v1/embeddings. The check
// lives in the transport that every request passes through (guardTransport),
// not in the callers: method and path must match exactly, with no query,
// userinfo or escaped path, the scheme and host must be the configured ones,
// redirects are never followed, and the Authorization header is attached
// there and only there. Model install, apply, import, edit, delete, backend,
// gallery, settings, agent, MCP, CORS-proxy and metrics paths cannot be
// reached, and a test asserts it (SEC-067, SEC-069). A legacy LocalAI key is
// full administrator access, so this list and the network boundary are the
// real controls (docs/security/localai-hardening-checklist.md, HC-37).
//
// # Transport
//
// Endpoints are http://127.0.0.1:8080 style loopback URLs, https:// URLs, or
// unix:///path sockets. A host that is not a literal loopback address (or
// localhost) is refused unless the endpoint is https, there is no
// skip-verification switch, and the proxy environment variables are ignored.
// For plaintext http the dialer re-checks the resolved address, so a name that
// resolves off-box is refused at connect time (SEC-065). The API key is held as
// an opaque Secret resolved from a reference ("env:NAME" or "file:PATH"), is
// sent only to the configured host and never appears in a log line, error or
// formatted value (SEC-082, SEC-083).
//
// # Errors
//
// Every failure is a *contracts.Error. The classification uses the HTTP
// status first and, for 500 and 503, short phrases from the body; the body is
// never copied into the error (it can hold prompt text):
//
//	connection refused, reset, EOF, 502, 503, 404      unavailable
//	deadline, 504, 408, network timeout                timeout
//	429, 507, out of memory, model load failure,
//	  load cooldown, backend capacity reached          resource_exhausted
//	prompt larger than the context window, 413         validation_failed
//	constrained output that is not JSON, malformed
//	  runtime answer, tool calls, unknown finish       schema_invalid
//	401, 403, other 4xx, other 5xx                     internal
//
// Caller cancellation stays a plain error wrapping context.Canceled; a
// deadline is a timeout that still matches context.DeadlineExceeded. The
// adapter never retries.
//
// With LocalAI's opaque-errors setting every error body is blank and every
// failure is a bare status. The mapping then still separates 503 (unavailable),
// 504 (timeout) and 429/507 (resource_exhausted), but a model load failure or
// out-of-memory arrives as a plain 500 and is reported as internal. That is the
// answer to hardening checklist HC-20: the production container keeps opaque
// errors off (deploy/container/localai/hardening.md), and M0-13 confirms the
// choice against a live runtime.
//
// # Logging
//
// An optional *slog.Logger receives debug records with the method, path,
// status, model, duration and counters under the keys tok_in and tok_out (never
// a key containing "token"). Prompts, completions, bodies and the key are never
// logged.
package localai
