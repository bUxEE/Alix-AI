// Package gemini is the Google Gemini provider adapter (task X-20, additive
// phase C): it implements the extended provider contract of internal/providers
// (MultimodalGenerator, CapabilityProvider, AIRuntimeProvider) over the Gemini
// API generateContent method. It is DISABLED by default and absent from a binary
// built with -tags noexternal (ADR-0022); only internal/cli/providers_external.go
// builds it, and only when external mode is enabled for the deployment. It
// shares the bounded SSE reader and retry policy of adapters/external with the
// other hosted adapters and imports no other provider package.
//
// # What it is
//
// An interchangeable inference backend. It never executes a tool, never owns a
// permission, fact or commit, and treats everything the provider returns as
// untrusted data: text, function-call proposals and structured output go back to
// the caller, which validates them and goes through the normal prepare / confirm
// / commit path (master plan 3, additive plan 7 and 9). The router
// (internal/modes) decides whether a request may be sent at all; this package
// receives an already admitted, filtered request.
//
// # Billing boundary (additive plan 4.1)
//
// Gemini API access is tied to a Google Cloud project and that project's quota
// and billing configuration; a Gemini or Google One consumer subscription does
// not include API access. On the free tier Google may use the content to improve
// its products, on the paid tier it states it does not. The provider profile
// this package ships (ProfileJSON) says so in its billing notice, and the setup
// flow must show it before a credential is stored.
//
// # Authentication: API key and Google OAuth (AuthSupport)
//
// Both are supported and the account chooses per connection (Config.Auth).
// OAuth is the preferred kind (additive plan 11.1); the manual key is the
// fallback and the first release path until live consent is verified (X-H02).
//
//   - API key: held by internal/credentials (KeySource, ServiceKeys), sent in the
//     x-goog-api-key header only. The older "?key=" query parameter is never
//     used: the route table refuses any query but the constant alt=sse.
//   - OAuth: the shared engine in internal/oauth runs the authorization code
//     flow with PKCE and refreshes the token; this package asks it for the access
//     value of one call through TokenSource (the composition root wraps
//     Engine.Token; the wrapper is proved to fit in oauth_engine_test.go). The
//     token goes in "Authorization: Bearer", the Cloud project in
//     x-goog-user-project (Config.UserProject). After a 401 the adapter asks for
//     a forced refresh once and retries once. The exported constants
//     (OAuthAuthorizationEndpoint, OAuthTokenEndpoint, OAuthRevocationEndpoint,
//     InferenceScopes, OAuthAuthParams, OAuthConsentNotes) are what the
//     composition root turns into the engine's ProviderConfig.
//
// # Research record (fetched 2026-10-08, as summarised by a fetch tool)
//
// The figures below were read from the vendor's pages on the date above through
// a summarising fetch tool, which sometimes mislabels pages (the structured
// output and function calling pages came back in the vocabulary of a newer
// "Interactions" API and were not used for the wire format). They are not
// verified by AlixAI; live behaviour is owed to X-H02 and the terms to X-H01.
//
//	https://ai.google.dev/gemini-api/docs/oauth
//	https://ai.google.dev/api/generate-content
//	https://ai.google.dev/api/caching            (Tool, FunctionCallingConfig)
//	https://ai.google.dev/gemini-api/docs/pricing
//	https://ai.google.dev/gemini-api/docs/models
//	https://ai.google.dev/gemini-api/docs/rate-limits
//	https://ai.google.dev/gemini-api/docs/thinking
//
// OAuth (read 2026-10-08): scopes named are
// https://www.googleapis.com/auth/cloud-platform (gcloud based setup) and
// https://www.googleapis.com/auth/generative-language.retriever (retrieval; the
// programmatic example uses it alone). The page names no scope specific to
// generateContent, so the "inference" capability requests cloud-platform, the
// one named for calling the API, and that is broad; the narrowest scope that
// works is to be confirmed live. Consent screen: the Cloud project needs an
// OAuth consent screen of user type External; while it is in testing each user
// must be a listed test user; an unverified app shows "Google hasn't verified
// this app", which is expected in testing; a production app must add and verify
// its scopes with Google. Live consent needs a human (X-H02).
//
// Endpoint: POST https://generativelanguage.googleapis.com/v1beta/models/{model}:
// generateContent and :streamGenerateContent?alt=sse. The model is in the path.
//
//   - Request: contents [{role: user|model, parts}], systemInstruction {parts},
//     tools [{functionDeclarations [{name, description, parametersJsonSchema}]}],
//     toolConfig.functionCallingConfig.mode (AUTO, ANY, NONE, VALIDATED; ANY and
//     VALIDATED constrain a call to the declared schema), generationConfig
//     {maxOutputTokens, temperature (0 to 2), stopSequences (at most 5),
//     responseMimeType application/json, responseJsonSchema}. The reference lists
//     responseSchema (a smaller OpenAPI subset) too; responseJsonSchema and
//     parametersJsonSchema (full JSON Schema) are used from memory of the API, not
//     read in the fetched excerpts (see the live risks).
//   - Structured output subset: types, properties, required, additionalProperties,
//     items, minItems, maxItems, enum, minimum, maximum, title, description,
//     format (date-time, date, time), anyOf, "$ref": "#" for recursion. See
//     schema.go for what the conversion strips and refuses.
//   - Answer: candidates[0].content.parts (text, thought, thoughtSignature,
//     functionCall {id?, name, args}), finishReason (STOP, MAX_TOKENS, SAFETY,
//     RECITATION, LANGUAGE, OTHER, BLOCKLIST, PROHIBITED_CONTENT, SPII,
//     MALFORMED_FUNCTION_CALL, UNEXPECTED_TOOL_CALL, TOO_MANY_TOOL_CALLS,
//     MISSING_THOUGHT_SIGNATURE, MALFORMED_RESPONSE, ...), usageMetadata
//     {promptTokenCount, candidatesTokenCount, thoughtsTokenCount,
//     cachedContentTokenCount, toolUsePromptTokenCount}, promptFeedback.blockReason,
//     modelVersion, responseId. A function result goes back as a functionResponse
//     part {name, response (an object), id?} in a user content.
//   - Streaming: every server-sent event is a whole GenerateContentResponse; the
//     last one carries finishReason and the final usage; there is no terminator
//     event, the connection just ends.
//   - Usage: tokens, not money; thinking tokens bill as output. Cost is computed
//     from the price table and marked Estimated. Prices (USD per 1M tokens,
//     standard paid tier): gemini-3.5-flash-lite 0.30 in / 2.50 out; gemini-3.5-
//     flash 1.50 / 9.00; gemini-3.8-flash 0.75 / 3.75 until 2026-12-31 and 1.50 /
//     7.50 from 2027-01-01 (the table holds the higher rate so an estimate stays an
//     upper bound); gemini-2.5-flash-lite 0.10 / 0.40; gemini-2.5-flash 0.30 /
//     2.50. Limits read only for gemini-3.5-flash-lite: 1,048,576 in / 65,536 out.
//   - Rate limits: requests, input tokens and requests per day, per project (the
//     daily quota resets at midnight Pacific time), plus spend based limits over a
//     rolling 10 minutes; the page states no rate-limit headers.
//   - Errors: {"error":{"code","message","status","details"}} with status
//     INVALID_ARGUMENT, FAILED_PRECONDITION, PERMISSION_DENIED, NOT_FOUND,
//     RESOURCE_EXHAUSTED, INTERNAL, UNAVAILABLE, DEADLINE_EXCEEDED and details
//     ErrorInfo (reason), QuotaFailure (violations[].quotaId), RetryInfo
//     (retryDelay such as "34s"). These shapes are from memory of the API and not
//     checked against the fetched pages.
//
// # Contract decisions
//
//   - One request per call. Generate and GenerateStream make exactly one POST
//     (plus bounded retries of a refusal made before processing); a functionCall
//     in the answer is returned as a proposal and is never followed up.
//   - Only function declarations are sent. No grounding, code execution, url
//     context, file search, cached content, safety override, thinking setting or
//     candidate count is ever set; the route table has no cached contents, files,
//     tuned models, batch, embedding, count tokens, live or interactions route,
//     and a part that was not requested (inlineData, fileData, executableCode,
//     codeExecutionResult, functionResponse, ...) or provider tool metadata
//     rejects the answer. Thought parts are ignored, never carried.
//   - Tool mode: a required proposal is ANY, a cancellation is NONE, otherwise
//     VALIDATED (Config.ToolMode AUTO opts out and the capability record then
//     stops claiming strict arguments). The profile declares strict arguments on
//     the strength of ANY and VALIDATED; the caller validates anyway.
//   - Fail closed instead of dropping: non-text parts (ADR-0024), grammar
//     constraints, a temperature above 2, more than 5 stop sequences, structured
//     output together with tools, a schema outside the subset (open maps,
//     recursion, external references, allOf), a system message after the first
//     turn, a tool result that answers no earlier proposal and a conversation that
//     ends with an assistant turn are refused before anything is sent.
//   - Refusals and truncation are never results: a filtered answer under a schema
//     is schema_invalid; a truncated structured answer fails to parse; the
//     finishReasons that say the model failed (MALFORMED_FUNCTION_CALL,
//     MISSING_THOUGHT_SIGNATURE, ...) are errors.
//   - A function call without an id gets a synthesised "gem_<responseId>_<n>" id,
//     which is never sent back to the API (the call and its result pair by name and
//     order there). The thoughtSignature a thinking model attaches to a call is kept
//     in a small bounded in-memory map keyed by tenant and call id and put back when
//     the same proposal is echoed; it is never logged or returned.
//   - Retries only for a rate limit (429 that is not a daily or billing quota) and
//     503, only before the first event is read, at most MaxRetries (hard cap 5),
//     honouring the retry delay as the minimum and refusing to sit out one longer
//     than Retry.MaxRetryAfter. A 500 is not retried (it may have been processed),
//     and neither is a transport failure after bytes were sent (the router decides).
//   - Errors carry provider_error_kind and sent details so the router's sender can
//     classify them without guessing. A response body is never copied into an
//     error, a log or a detail. An invalid key is a 400 with ErrorInfo reason
//     API_KEY_INVALID and is mapped to authentication_failed. The two 429 kinds
//     share the status RESOURCE_EXHAUSTED; a per-day QuotaFailure or a message
//     about billing, spend or prepayment is a quota failure (never retried), any
//     other is a rate limit. That discriminator is a judgement, unverified.
//   - Live risks that fixtures cannot show (owed to X-H02): the field names
//     responseJsonSchema and parametersJsonSchema and the VALIDATED mode on the
//     chosen model; whether structured output may be combined with tools (refused
//     here); the rule for thought signatures on the newest models (a lost
//     signature after a restart may be a 400 on a multi-turn tool exchange; no
//     documented skip value was found); the narrowest OAuth scope; whether the
//     x-goog-user-project header is required with a token; the real 429 shapes.
//
// # Credentials and network
//
// The credential is resolved on every call (internal/credentials, scope-bound,
// audited, revocable; or the OAuth engine) and put only in a header of an
// allowlisted route. There is no configuration field that holds a key or a token.
// The request is dialled through internal/egress (egress.ProviderRegistry):
// default deny, exact host and port, certificates verified, no proxy, no
// redirect followed. The OAuth token endpoints must be on the application's
// allowlist too (the engine dials them).
package gemini
