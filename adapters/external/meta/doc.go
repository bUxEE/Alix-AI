// Package meta is the Meta provider adapter (task X-21, ADR-0021): it implements
// the extended provider contract of internal/providers (MultimodalGenerator,
// CapabilityProvider, AIRuntimeProvider) for two services that speak the Chat
// Completions wire format:
//
//   - DialectModelAPI (the default): Meta's own Model API at https://api.meta.ai/v1
//     with the Muse Spark models. This is the official, first-party developer
//     access that additive plan 4.1 and 4 name "Meta Model API".
//   - DialectOpenAICompat: a third-party host of Llama models behind an
//     OpenAI-compatible endpoint that the operator names (additive plan 4,
//     "approved hosted Llama endpoint"). There is no default host.
//
// It is DISABLED by default and absent from a binary built with -tags noexternal
// (ADR-0022); only internal/cli/providers_external.go builds it (task X-26), and
// only when external mode is enabled for the deployment. The provider id is
// "meta" (egress allowlist, credential binding).
//
// # What it is
//
// An interchangeable inference backend. It never executes a tool, never owns a
// permission, fact or commit, and treats everything the provider returns as
// untrusted data: text, tool-call proposals and structured output go back to the
// caller, which validates them and goes through the normal prepare / confirm /
// commit path (master plan 3, additive plan 7 and 9). The router
// (internal/modes) decides whether a request may be sent at all; this package
// receives an already admitted, filtered request.
//
// # Research record (re-checked 2026-10-08, documentation only, no provider call)
//
// Everything below was read through a summarising fetch tool or a search summary,
// which sometimes mislabels pages. None of it is verified by AlixAI; live
// behaviour is owed to X-H02 and the terms to X-H01. No Meta page was reachable
// without sign-in other than the developer documentation named below.
//
// What exists:
//
//   - Meta Model API, https://dev.meta.ai/docs/ (the old host
//     https://llama.developer.meta.com/docs/ answered 302 to it): hosted
//     inference with an API key created in its own dashboard, sent as
//     "Authorization: Bearer"; no OAuth flow is documented, so the key is the
//     manual credential held by internal/credentials. It is "drop-in compatible
//     with the OpenAI SDK" and offers the Responses, Chat Completions and
//     Messages protocols. This adapter uses Chat Completions only
//     (POST /v1/chat/completions), because a single wire format serves both
//     dialects. Models: muse-spark-1.1 / 1.2 / 1.3 (1,048,576-token context, text
//     out; image, video, audio and PDF in), muse-image-1.0, muse-voice-transcribe-1.0,
//     sam-3.1. NO Llama model is listed on the Model API. Muse Spark "always
//     reasons" (reasoning tokens bill as output and count against the cap;
//     reasoning_content is redacted to empty; reasoning_effort "none" is
//     refused). Pricing as summarised: standard tier $1.25 in / $4.25 out per
//     million tokens, cached input $0.15; a "contributor" tier is listed at
//     $0.10 / $0.20. Rate limits per team: 3,000 requests and 4,000,000 tokens
//     per minute (standard), 100 and 3,000,000 (contributor). Errors:
//     {"error": {message, type, param, code}} with types invalid_request_error,
//     authentication_error, rate_limit_error, server_error; 402 for billing;
//     429 with Retry-After; 503 (server_shutting_down, service_overloaded,
//     backend_unavailable) retryable; 504 for a non-streaming request that took
//     too long. An unauthenticated GET /v1/status reports availability.
//     Chat Completions parameters: max_completion_tokens (max_tokens deprecated),
//     tools, tool_choice, response_format, stream_options.include_usage, among
//     others; stop is refused (400) on reasoning models; tool_choice accepts
//     "auto" ONLY ("none", "required" and a named function answer 400);
//     response_format json_schema has strict defaulting to false, and with
//     strict:true "decoding itself is constrained" over a documented subset
//     (object root, additionalProperties:false, every property required, depth
//     10, 5,000 properties, no allOf/oneOf, no recursion). Two tools run
//     server-side (web_search, tool_search); nothing here requests them.
//   - Hosted Llama: Meta's first-party Llama API preview (api.llama.com) is
//     reported, by third-party summaries only (spheron.network, ecorpit.com,
//     aibase.com; none cites a primary Meta notice that was readable), to have
//     stopped answering on 2026-07-06, with a sunset response. The Llama weights
//     remain downloadable under Meta's licence. The same summaries name Groq,
//     Together AI, DeepInfra, OpenRouter and AWS Bedrock as hosts of Llama 4;
//     they report that Groq deprecated Llama 4 Maverick (2026-03-09) and Scout
//     (2026-07-17), and that Bedrock is not OpenAI-compatible. Host documentation
//     read for the wire format: Groq (Bearer, stream_options.include_usage,
//     response_format json_schema, tool_choice none/auto/required, Retry-After)
//     and Together (response_format json_schema {name, schema}). The list price
//     in the model table for llama-4-scout ($0.10 / $0.30) is one aggregator's, as
//     summarised, and is not a quote from any host the operator will use.
//
// What is NOT supported, and why:
//
//   - Meta AI, the consumer assistant (Facebook, Instagram, WhatsApp, Messenger,
//     meta.ai), and a Meta account: they do not grant Model API or hosted Llama
//     access, and AlixAI never scrapes, automates or impersonates a consumer
//     product (additive plan 4.1). The billing notice in the shipped profile says
//     so and must be acknowledged before setup completes.
//   - Meta's own Llama API (api.llama.com): retired, see above. There is no
//     dialect for it.
//   - Llama on the Model API: not offered there. Llama is reachable only through a
//     third-party host (DialectOpenAICompat), whose terms, data handling, region
//     and billing are the operator's to review (X-H01) and are not Meta's.
//   - AWS Bedrock and any other provider-native (non OpenAI-compatible) API: a
//     different wire format and credential kind; a separate adapter would be
//     needed.
//   - The Model API's contributor tier: refused unless Config.AllowContributorTier
//     is set. The pages read do not say what it asks in return (data use, review,
//     eligibility); a price that low next to the standard tier is read as a
//     reason to ask before sending data there.
//   - Image, video, audio and PDF input, Muse Image, Muse Voice Transcribe and SAM
//     (ADR-0024 keeps non-text parts off in the first release), embeddings,
//     the Responses and Messages protocols, files, search grounding, tool search,
//     prompt caching, computer use and every other route: none is in the route
//     table.
//
// # Contract decisions
//
//   - One request per call. Generate and GenerateStream make exactly one POST
//     (plus bounded retries of a refusal made before processing); a tool call in
//     the answer is returned as a proposal and is never followed up.
//   - Capabilities are honest for a generic service: text in and out; a JSON schema
//     is sent as response_format json_schema WITHOUT strict, and the capability is
//     declared schema_best_effort, tool arguments strict_arguments=false. The
//     caller validates the document or the arguments against the schema it owns.
//     (Meta documents constrained decoding for strict:true over a narrower schema
//     subset; adopting it needs a schema conversion like adapters/external/openai
//     and a live check, X-H02.)
//   - Fail closed instead of dropping: non-text parts, grammar constraints, a
//     schema with an external reference, more tools than Config.MaxTools, more
//     output tokens than the model's cap, and (Model API) stop sequences are
//     refused before anything is sent. On a service that accepts only
//     tool_choice "auto" (the Model API, or Config.NoForcedToolChoice) a required
//     proposal goes out as auto and is enforced on the answer, and "none" leaves
//     the tools out.
//   - The request body carries only model, messages, the output cap
//     (max_completion_tokens on the Model API, max_tokens elsewhere), temperature,
//     stop (hosted Llama), stream, stream_options.include_usage, response_format,
//     tools, tool_choice and an operator-set reasoning_effort. Never store,
//     user, safety_identifier, prompt_cache_*, previous_response_id, logprobs,
//     service tier, plugins, provider routing, fallback model lists or web search
//     options. An answer that carries host-run output (executed_tools,
//     function_call, audio, images, search_results, a non-text content part or a
//     tool call that is not a function) is rejected.
//   - Refusals and truncation are never results: a content-filter finish under a
//     schema is schema_invalid; a truncated structured answer fails to parse; a
//     stream without a finish reason, without usage, or without the [DONE]
//     sentinel is an error.
//   - Retries only for 429 rate limits and 503, only before the first event is
//     read, at most MaxRetries (hard cap 5), honouring Retry-After as the minimum
//     and refusing to sit out one longer than Retry.MaxRetryAfter. A transport
//     failure after bytes were sent is never retried here (the router decides).
//   - Errors carry provider_error_kind and sent details so the router's sender can
//     classify them without guessing. A response body is never copied into an
//     error, a log or a detail.
//
// # Credentials and network
//
// The key is resolved through KeySource on every call (internal/credentials,
// scope-bound, audited, revocable) and put only in the Authorization header of an
// allowlisted route. There is no configuration field that holds a key. The
// request is dialled through internal/egress (egress.ProviderRegistry): default
// deny, exact host and port, certificates verified, no proxy, no redirect
// followed. The route table is chat completions, the model list and one anonymous
// health route (/status on the Model API, the model list elsewhere), under the
// validated API prefix.
package meta
