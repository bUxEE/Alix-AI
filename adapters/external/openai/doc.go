// Package openai is the first external provider adapter (task X-10, ADR-0021):
// it implements the extended provider contract of internal/providers
// (MultimodalGenerator, CapabilityProvider, AIRuntimeProvider, Embedder) over
// the OpenAI API. It is DISABLED by default and absent from a binary built with
// -tags noexternal (ADR-0022); only internal/cli/providers_external.go builds
// it, and only when external mode is enabled for the deployment.
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
// # Research record (fetched 2026-10-07, as summarised by a fetch tool)
//
// The figures below were read from the vendor's pages on the date above through
// a summarising fetch tool, which sometimes mislabels pages. They are not
// verified by AlixAI; live behaviour is owed to X-H02 and the terms to X-H01.
//
//	https://developers.openai.com/api/docs/guides/structured-outputs
//	https://developers.openai.com/api/docs/guides/function-calling
//	https://developers.openai.com/api/docs/guides/streaming-responses
//	https://developers.openai.com/api/docs/guides/rate-limits
//	https://developers.openai.com/api/docs/guides/error-codes
//	https://developers.openai.com/api/reference/resources/responses/methods/create
//	https://developers.openai.com/api/docs/pricing
//
// Endpoint family: the Responses API (POST /v1/responses), which the vendor
// recommends for new projects; Chat Completions stays supported and is the
// fallback if Responses is withdrawn. Both support strict structured outputs.
// Responses accepts "store": false, which this adapter always sends (a stored
// response is retained for at least 30 days).
//
//   - Strict structured output: text.format = {type: json_schema, name, schema,
//     strict: true}. Rules: the root is an object; every object has
//     additionalProperties:false and lists every property in required; nesting
//     at most 10 levels and 5000 properties; at most 1000 enum values; composition
//     keywords such as allOf, not and dependentRequired are unsupported; $ref and
//     $defs are supported. The two readings of the page disagreed on whether
//     pattern, format and the numeric and size bounds are accepted; the adapter
//     takes the conservative reading and strips them (see strict.go). A refusal
//     arrives as a content part of type "refusal" and does not follow the schema.
//   - Function calling: tools = [{type: function, name, description, parameters,
//     strict: true}]; output items {type: function_call, call_id, name,
//     arguments}; tool_choice auto, none or required. A result goes back as a
//     function_call_output item, which this adapter only does when the CALLER
//     sends a tool message: it never runs a loop of its own.
//   - Streaming: server-sent events; response.output_text.delta,
//     response.output_item.added, response.function_call_arguments.delta and
//     .done, response.refusal.delta, response.completed, response.incomplete,
//     response.failed and error. Usage arrives only in response.completed. There
//     is no [DONE] sentinel and no stream_options.
//   - Usage: usage.input_tokens and usage.output_tokens (output includes
//     reasoning tokens). The API reports tokens, not money; cost is computed from
//     the price table and marked Estimated.
//   - Rate limits: headers x-ratelimit-{limit,remaining,reset}-{requests,tokens}
//     (reset as a duration such as "6m0s") and Retry-After on 429 and 503. Errors
//     are {"error": {message, type, param, code}}: 401 invalid key, 403 region or
//     permission, 429 rate_limit_exceeded or insufficient_quota, 5xx server_error
//     and server_is_overloaded.
//   - Authentication: API keys. OpenAI documents no OAuth flow for API access (the
//     auth page returned 404 on 2026-10-03; a third-party summary says so;
//     re-checked here without contrary evidence), so the first release uses the
//     manual key held by internal/credentials. Do not describe it as OAuth.
//
// # Contract decisions
//
//   - One request per call. Generate and GenerateStream make exactly one POST
//     (plus bounded retries of a refusal made before processing); a function call
//     in the answer is returned as a proposal and is never followed up.
//   - Only the function tool type is sent. No previous_response_id, conversation,
//     background, include, prompt, max_tool_calls, metadata or user identifier is
//     ever set; the route table has no file, vector store, assistant, thread or
//     conversation route, and a hosted-tool item in an answer rejects the answer.
//   - Fail closed instead of dropping: non-text parts (ADR-0024), grammar
//     constraints, stop sequences (the API has none) and a temperature on a
//     reasoning model are refused before anything is sent. A schema that cannot be
//     converted to the strict subset is refused; strict is never switched off.
//   - Refusals and truncation are never results: a refusal under a schema is
//     schema_invalid; a truncated structured answer fails to parse.
//   - Retries only for 429 rate_limit and 503 overload, only before the first
//     event is read, at most MaxRetries (hard cap 5), honouring Retry-After as the
//     minimum and refusing to sit out one longer than Retry.MaxRetryAfter. A
//     transport failure after bytes were sent is never retried here (the router
//     decides; the outcome may be unknown).
//   - Errors carry provider_error_kind and sent details so the router's sender can
//     classify them without guessing. A response body is never copied into an
//     error, a log or a detail.
//   - Reasoning models and store:false: a multi-turn tool exchange with a
//     reasoning model normally also returns the reasoning items; this adapter does
//     not carry them. Single-step proposals, which is the AlixAI pattern, do not
//     need them.
//
// # Credentials and network
//
// The key is resolved through KeySource on every call (internal/credentials,
// scope-bound, audited, revocable) and put only in the Authorization header of an
// allowlisted route. There is no configuration field that holds a key. The
// request is dialled through internal/egress (egress.ProviderRegistry): default
// deny, exact host and port, certificates verified, no proxy, no redirect
// followed.
package openai
