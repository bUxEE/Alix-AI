// Package openai is the external speech-to-text adapter for OpenAI's Audio
// Transcriptions API. It implements internal/providers/voice.STT for a
// finalised clip (no streaming). It is DISABLED by default and only built from
// internal/cli/providers_external_stt.go when external mode is on; a binary
// built with -tags noexternal does not link it.
//
// # Research record (checked 2026-10-26)
//
//	https://developers.openai.com/api/docs/guides/speech-to-text
//	https://developers.openai.com/api/reference/resources/audio/subresources/transcriptions/methods/create
//	https://developers.openai.com/api/docs/deprecations
//
// Endpoint: POST /v1/audio/transcriptions (multipart). Recommended model for
// recorded files: gpt-transcribe. Still accepted until 2027-02-26: whisper-1,
// gpt-4o-transcribe, gpt-4o-mini-transcribe. Files up to 25 MB; wav is listed.
// Authentication: API key in Authorization: Bearer (the SIWC / plan token covers
// eligible Responses API requests only — transcriptions need a key).
//
// Provider id: "openai" (same slug as the chat adapter; role stt on the profile).
// Price is the profile's audio_seconds_in; this package does not price calls.
package openai
