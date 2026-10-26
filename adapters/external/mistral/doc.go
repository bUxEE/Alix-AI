// Package mistral is a thin adapter for Mistral AI's OpenAI-compatible API
// (https://docs.mistral.ai). It is disabled by default and absent from a binary
// built with -tags noexternal (ADR-0022).
//
// # Research record (checked 2026-10-26)
//
// Official sources (fetched on that date; operator must re-check before relying
// on retention / training / residency claims):
//
//	https://docs.mistral.ai/api/endpoint/chat
//	https://docs.mistral.ai/api/endpoint/models
//	https://docs.mistral.ai/studio/conversations/function-calling
//	https://docs.mistral.ai/api/endpoint/audio/transcriptions
//	https://docs.mistral.ai/api/endpoint/audio/speech
//	https://docs.mistral.ai/admin/monitor-comply/zero-data-retention
//	https://help.mistral.ai/en/articles/347629-where-do-you-store-my-data-or-my-organization-s-data
//	https://help.mistral.ai/en/articles/455207-can-i-opt-out-of-my-input-or-output-data-being-used-for-training
//	https://legal.mistral.ai/terms/privacy-policy/?lang=en
//
// Base URL: https://api.mistral.ai (API paths under /v1). Chat Completions:
// POST /v1/chat/completions. Model list / key probe: GET /v1/models.
// Authentication: API key in Authorization: Bearer. No official third-party
// OAuth for API access is documented for AlixAI to use; manual key only.
//
// Current chat model aliases (non-exhaustive; prefer GET /v1/models for an
// account): mistral-large-latest, mistral-medium-latest, mistral-small-latest,
// ministral-14b-latest, ministral-8b-latest, ministral-3b-latest,
// codestral-latest, magistral-medium-latest, magistral-small-latest. Tool
// calling (tools / tool_choice) and structured output
// (response_format type json_object or json_schema) are supported on capable
// chat models. Parallel tool calls are supported; docs cap tools per request
// at 128.
//
// Data location: by default Mistral hosts organisation data in the EU; an
// explicit US API endpoint stores data in the US. Some features may temporarily
// transfer data outside the EU under GDPR Article 46 safeguards (SCCs), per the
// help-centre article above.
//
// Retention (API, privacy policy): Input and Output for most APIs are kept for
// the time needed to generate the Output and then for thirty (30) rolling days
// to monitor abuse, unless zero data retention (ZDR) is activated for the
// Organisation. ZDR (paid plans, request to Mistral) covers listed stateless
// endpoints including /v1/chat/completions, /v1/audio/transcriptions and
// /v1/audio/speech; it does not cover Agents, Files, Conversations, Libraries
// or Vibe. Training use for Studio/API is controllable in Admin → Privacy
// ("Anonymous improvement data"); Vibe and API toggles are separate.
//
// Speech: official STT is POST /v1/audio/transcriptions (Voxtral Transcribe 3
// models, e.g. voxtral-mini-transcribe-3) — adapters/external/stt/mistral.
// Official TTS is POST /v1/audio/speech (e.g. voxtral-mini-tts-3-0) —
// adapters/external/tts/mistral.
package mistral
