// Package anthropic is the Anthropic provider adapter (task X-19, additive phase
// C): it implements the extended provider contract of internal/providers
// (MultimodalGenerator, CapabilityProvider, AIRuntimeProvider) over the
// Anthropic Messages API. It is DISABLED by default and absent from a binary
// built with -tags noexternal (ADR-0022); only internal/cli/providers_external.go
// builds it, and only when external mode is enabled for the deployment. It
// shares the bounded SSE reader and retry policy of adapters/external with the
// other hosted adapters and imports no other provider package.
//
// # What it is
//
// An interchangeable inference backend. It never executes a tool, never owns a
// permission, fact or commit, and treats everything the provider returns as
// untrusted data: text, tool-use proposals and structured output go back to the
// caller, which validates them and goes through the normal prepare / confirm /
// commit path (master plan 3, additive plan 7 and 9). The router (internal/modes)
// decides whether a request may be sent at all; this package receives an already
// admitted, filtered request.
//
// # Billing boundary (additive plan 4.1)
//
// A Claude consumer subscription (claude.ai) and Anthropic Console / API billing
// are separate products: a subscription does not include API access. The
// provider profile this package ships (ProfileJSON) says so in its billing
// notice, and the setup flow must show it before a key is stored.
//
// # Research record (fetched 2026-10-08, as summarised by a fetch tool)
//
// The figures below were read from the vendor's pages on the date above through
// a summarising fetch tool, which sometimes mislabels pages, plus the bundled
// API reference of the build tooling (cached 2026-09-25). They are not verified
// by AlixAI; live behaviour is owed to X-H02 and the terms to X-H01.
//
//	https://platform.claude.com/docs/en/build-with-claude/structured-outputs
//	https://platform.claude.com/docs/en/build-with-claude/streaming
//	https://platform.claude.com/docs/en/api/errors
//	https://platform.claude.com/docs/en/api/rate-limits
//
// Endpoint: POST /v1/messages with the headers x-api-key and anthropic-version:
// 2023-06-01. No anthropic-beta header is ever sent.
//
//   - Structured output: output_config.format = {type: json_schema, schema}, GA,
//     no beta header (the older output_format parameter is deprecated). Subset:
//     objects must carry additionalProperties:false; no recursive schemas, no
//     external $ref, no numeric or string bounds, minItems only 0 or 1; optional
//     properties are allowed. A refusal (stop_reason refusal) or max_tokens may
//     leave a document that does not match.
//   - Tool use: tools = [{name, description, input_schema, strict: true}]; at
//     most 20 strict tools, 24 optional parameters and 16 union types per
//     request ("Schema is too complex" is a 400). Answer blocks {type: tool_use,
//     id, name, input (an object)}; the result goes back in a user message as a
//     tool_result block, all results of one turn in the same message. The newest
//     models (Opus 5.5, Sonnet 5.5, Fable 5.1) answer 400 to tool_choice any or
//     tool; auto and none are accepted, so a required proposal is enforced after
//     the answer (see Config.ForcedToolChoice).
//   - Streaming: message_start (input tokens), content_block_start / _delta /
//     _stop (text_delta, input_json_delta, thinking_delta, signature_delta),
//     message_delta (stop_reason, cumulative output tokens), message_stop, ping,
//     and error events that can follow a 200 (overloaded_error, ...).
//   - Usage: usage.input_tokens and usage.output_tokens (output includes thinking
//     tokens), plus cache_creation_input_tokens and cache_read_input_tokens. The
//     API reports tokens, not money; cost is computed from the price table and
//     marked Estimated.
//   - Rate limits: anthropic-ratelimit-{requests,tokens,input-tokens,
//     output-tokens}-{limit,remaining,reset} (reset is an RFC 3339 timestamp) and
//     retry-after on 429. Errors are {"type":"error","error":{"type","message"},
//     "request_id"}: 400 invalid_request_error, 401 authentication_error, 402
//     billing_error, 403 permission_error, 404 not_found_error, 413
//     request_too_large, 429 rate_limit_error, 500 api_error, 504 timeout_error,
//     529 overloaded_error. A spend-cap 429 carries error.details.error_code
//     enforced_spend_limit_reached and no retry-after.
//   - Authentication: API keys. Anthropic documents no OAuth flow for API access,
//     so the first release uses the manual key held by internal/credentials. Do
//     not describe it as OAuth.
//
// # Contract decisions
//
//   - One request per call. Generate and GenerateStream make exactly one POST
//     (plus bounded retries of a refusal made before processing); a tool_use
//     block in the answer is returned as a proposal and is never followed up.
//   - Only custom (function) tools are sent, always strict. No thinking,
//     metadata, service tier, container, MCP server, context management, cache
//     control or beta feature is ever set; the route table has no batch, file,
//     skill, agent, session, memory or admin route, and a provider-run block
//     (server_tool_use, *_tool_result, mcp_tool_use, pause_turn, ...) rejects the
//     answer. Thinking and redacted_thinking blocks are ignored, never carried.
//   - Fail closed instead of dropping: non-text parts (ADR-0024), grammar
//     constraints, a temperature the model rejects, an effort on a model without
//     one, a schema outside the subset (open maps, recursion, external
//     references, over the complexity limits), a system message after the first
//     turn and a conversation that ends with an assistant turn (prefill) are
//     refused before anything is sent. Strict is never switched off.
//   - Refusals and truncation are never results: a refusal under a schema is
//     schema_invalid; a truncated structured answer fails to parse.
//   - Retries only for 429 rate_limit and 503 / 529 overload, only before the
//     first event is read, at most MaxRetries (hard cap 5), honouring Retry-After
//     as the minimum and refusing to sit out one longer than Retry.MaxRetryAfter.
//     A transport failure after bytes were sent is never retried here (the
//     router decides; the outcome may be unknown).
//   - Errors carry provider_error_kind and sent details so the router's sender
//     can classify them without guessing. A response body is never copied into an
//     error, a log or a detail.
//   - Live risks that fixtures cannot show (owed to X-H02): a multi-turn tool
//     exchange with an always-thinking model is sent without the thinking blocks
//     of the earlier turn (the single-step proposal pattern does not need them);
//     the exact per-model limits and whether pattern is accepted in a schema
//     (it is stripped).
//
// # Credentials and network
//
// The key is resolved through KeySource on every call (internal/credentials,
// scope-bound, audited, revocable) and put only in the x-api-key header of an
// allowlisted route. There is no configuration field that holds a key. The
// request is dialled through internal/egress (egress.ProviderRegistry): default
// deny, exact host and port, certificates verified, no proxy, no redirect
// followed.
package anthropic
