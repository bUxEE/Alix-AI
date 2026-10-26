// Package deepgram is the external speech-to-text adapter for Deepgram's
// pre-recorded Listen API. It implements internal/providers/voice.STT for a
// finalised clip (no streaming). Disabled by default; wired only from
// internal/cli/providers_external_stt.go.
//
// # Research record (checked 2026-10-26)
//
//	https://developers.deepgram.com/reference/speech-to-text/listen-pre-recorded
//	https://developers.deepgram.com/docs/pre-recorded-audio
//	https://developers.deepgram.com/docs/models-languages-overview
//
// Endpoint: POST https://api.deepgram.com/v1/listen?model=nova-3 (optional
// language=). Auth: Authorization: Token <api_key>. Body: raw audio with
// Content-Type: audio/wav. Default model nova-3.
//
// Provider id: "deepgram". Price is the profile's audio_seconds_in.
package deepgram
