// Package localai is the LocalAI Decisions adapter of the optional decision layer
// (master plan 8.5, docs/architecture/decision-layer.md section 6). It implements
// the provider-neutral DecisionModelProvider contract of internal/decision over
// LocalAI's typed Decisions/SystemOne API, structurally: it imports neither
// internal/decision nor adapters/localai, so the scorer path pulls in no database
// and the five-route allowlist of adapters/localai (and the voice one) stay as
// they are. This package has its own allowlist: POST /v1/systemone and GET /readyz.
//
// # What was read, and when
//
// Documentation, read 2026-10-07 and recorded as documentation, not as observed
// behaviour of any server; nothing here was run against a LocalAI that hosts it:
//
//   - https://localai.io/docs/features/decisions/ : POST /v1/systemone (also
//     /v1/systemone/permute and /v1/systemone/separate, which this package never
//     calls); request {model, state, questions{id: {type: choice|score|noul,
//     instructions, criteria{key: description}}}, images}; answers per question
//     (choice, probabilities, confidence; score, legend; noul, entities); limits:
//     text body 64 KiB, 1 to 64 questions, a choice has 2 to (26 typical, fewer
//     for some models) options, a score at least 2 levels; a model declares
//     `known_usecases: [decisions]` on its vllm-cpp or llama-cpp backend; errors
//     400 (invalid request, unsupported model), 413, 501 (backend does not
//     support decisions or lacks metadata), 503 (admission ceiling, 8 concurrent).
//
// Not assumed: that the pinned LocalAI v4.10.0 ships that route or backend, or
// that any model hosted by LocalAI is CLM. An absent route (404, 405), a 501 or a
// 400 naming an unsupported model is reported as unsupported_runtime, never
// guessed around. The deployment choice (LocalAI backend or supervised sidecar) is
// ADR-0030, after testing.
//
// # What a request carries
//
// The configured model, one state string (the decision type, the locale and the
// bounded state as JSON) and one typed choice question whose options are the
// authorised candidate identifiers with their label and optional text. Tenant,
// application and session identifiers, deadlines and the allowed-next-actions
// list are never sent. Limits are enforced before any connection: more options
// than LocalAI documents, fewer than two, or a body over 64 KiB is an unsupported
// shape (ood_unsupported_shape), not a truncated request.
//
// # Failures
//
// Every failure maps to a typed fallback reason (see wire.ReasonOf): transport
// and server errors to unavailable, timeout or resource_gate, an answer that is
// not the documented shape or names a candidate that was not offered to
// malformed_output, a model that does not match the configuration to
// model_mismatch. A request or response body never appears in an error or a log
// record; logging is method, path, status and duration at debug level.
//
// # Revision
//
// LocalAI's answer carries no model revision. The revision is the operator's pin
// (Config.ModelRevision); unset, it is "unverified", which has no calibration
// entry, so the engine fails closed with calibration_failed.
package localai
