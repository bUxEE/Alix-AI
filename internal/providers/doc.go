// Package providers is the seam between AlixAI and whatever AI runtime serves
// its models (master plan section 6: "LocalAI is always accessed through
// AIRuntimeProvider"). It holds interfaces, request budgets, the provider
// registry with per-role health, and shared helpers. It contains no HTTP and
// no runtime-specific code: adapters/localai implements the role interfaces,
// and the sub-package fake implements all of them deterministically for tests.
//
// # Roles
//
// A Role is a model job (master plan 8.1), not a Go type. Router and
// Controller are both served by a TextGenerator; Embedder and Reranker have
// their own interfaces. STT, TTS and VAD are reserved for M4: the registry
// stores providers for them but their interfaces live in
// internal/providers/voice.
//
// An AIRuntimeProvider is what a runtime adapter additionally exposes so the
// registry can name and probe it (Name, Health). One adapter value may serve
// several roles.
//
// # Authority
//
// The model proposes; deterministic code decides (master plan rule 5). The
// guarantees this package gives callers are therefore deliberately small:
//
//   - ChatResult.JSON, when set, is syntactically valid JSON. It is NOT
//     schema-validated; the caller validates it against the schema it sent
//     before trusting any field.
//   - A stream is complete only when a chunk with Done set was received.
//     Partial output is never returned as a success (see Collect).
//   - Message.Trusted is the zero value false: content is untrusted unless the
//     prompt builder says otherwise, and untrusted content cannot be placed in
//     a system message.
//   - Every request carries a validated contracts.Scope; a missing scope is an
//     error, never a default.
//
// # External providers (milestone X)
//
// MultimodalGenerator, CapabilityProvider, RichRequest and RichResult extend
// the text contract for hosted providers without changing it: content parts
// (text and AlixAI-managed image, file and audio references in; text and media
// bytes out), tool-call PROPOSALS against an allowlist of ToolSpec values (a
// proposal is data, nothing here can run it), streamed deltas with a checked
// sequence, usage and cost in integer micro-units with an Estimated flag
// (toks_in / toks_out, never a "token" key), rate-limit info with retry-after
// (rate_limited), and ModelCapabilities for rejecting an impossible request
// before any data leaves. Provider output is untrusted data. WithRichBudget
// enforces the Budget on all of it; an adapter that only knows text never
// implements MultimodalGenerator and so never receives media or tools.
//
// # Errors
//
// Every failure is a *contracts.Error: unavailable (runtime unreachable),
// timeout, resource_exhausted (OOM or model load failure), schema_invalid
// (constrained output did not parse, or the runtime answered malformed),
// validation_failed (the request itself is illegal, including budget
// violations: retrying the identical request can never succeed), and
// scope_missing / scope_violation from the scope. Context cancellation by the
// caller stays a plain error wrapping context.Canceled.
//
// # Logging
//
// Token counters are logged as tok_in / tok_out (and max_toks, prompt_len),
// never under keys containing "token": internal/log redacts those by
// substring.
package providers
