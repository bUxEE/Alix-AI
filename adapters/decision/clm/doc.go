// Package clm is the CLM service adapter of the optional decision layer (master plan
// 8.5, docs/architecture/decision-layer.md section 6, the "supervised CLM sidecar"
// deployment). It implements the provider-neutral DecisionModelProvider contract of
// internal/decision over the protocol of the CLM service, structurally: it imports
// neither internal/decision nor any other adapter, so the scorer path pulls in no
// database and no LocalAI code. The lifecycle of the sidecar (start, restart,
// demand-load, unload) is internal/decision/lifecycle, not this package; this
// package only talks to a service that is already there, over loopback, a unix
// socket or https with a verified certificate.
//
// # What was read, and when
//
// Documentation, read 2026-10-07 and recorded as documentation, not as observed
// behaviour of any server; the fixtures in testdata are constructed from these
// examples, not captured from a running service:
//
//   - https://github.com/Contrastive-LM/CLM : service on :8700 (clm-serve), the
//     Qwen3-8B pooling encoder behind vLLM on :8090 (/v1/embeddings);
//     POST /v1/systemone {state, model, questions{id: {instructions, criteria}},
//     temperature} answering {answers{id: {noul, choice, probabilities, score,
//     confidence}}, usage, latency_ms}; POST /v1/rank {context, question,
//     answers[]} answering {model, ranked[{rank, candidate, prob}]};
//     GET /v1/models (names, descriptions, release dates); GET /health (vector
//     cache occupancy and hit rate); "States longer than 2048 tokens are
//     truncated" (--max-model-len for vLLM, --max-tokens for clm-serve).
//   - https://huggingface.co/Contrastive-LM/CLM-v0.1-8B : two small projection
//     heads on a frozen Qwen3-8B encoder, Apache-2.0 weights and encoder, scores
//     candidates and generates no text, "zero-shot performance requires
//     fine-tuning for optimal agentic benchmarks" (vendor statement, not ours).
//
// The shape of /v1/models entries, and whether the service reports a model
// revision at all, are not documented in what was read. The adapter therefore
// reads the entry tolerantly (id or name, and revision or version) and, when the
// operator pinned a revision, fails closed unless the service reports exactly that
// revision. Nothing is pinned that could not be verified.
//
// # Requests
//
// A request carries the decision type, the locale and the bounded state as one
// state string, and the authorised candidates as label and optional text; never
// tenant, application or session identifiers, deadlines, data versions or the
// allowed-next-actions list. Mode "rank" (the default) uses /v1/rank and matches
// each ranked candidate back to its identifier by its exact text; mode "systemone"
// uses one choice question whose options are the identifiers.
//
// # Limits and unsupported shapes
//
// The service truncates a state over 2048 tokens, and a truncated state is a
// different question. The adapter has no tokenizer, so it uses a conservative
// estimate (one token per three ASCII bytes, one per non-ASCII rune, rounded up)
// and treats a state whose estimate exceeds Config.MaxStateTokens (2048) as an
// unsupported shape (ood_unsupported_shape) before any connection is opened. The
// same fallback covers fewer than two or more than 32 candidates, candidates that
// cannot be told apart and a body over Config.MaxBodyBytes (64 KiB).
//
// # Model and revision
//
// Before the first request, and again after Config.ModelCheckTTL, the adapter reads
// GET /v1/models: the configured model must be listed, and a pinned revision must
// match. A response that names another model than the configured one is also a
// mismatch. Mismatch is model_mismatch, and no state is sent to a service that
// failed the check.
//
// # Failures
//
// Every failure maps to a typed fallback reason (see wire.ReasonOf). A request or
// response body never appears in an error or a log record.
package clm
