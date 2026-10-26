// Package elevenlabs is the external speech-to-text adapter for ElevenLabs
// Speech-to-Text. It implements internal/providers/voice.STT for a finalised
// clip. Disabled by default; wired from internal/cli/providers_external_stt.go.
//
// # Research record (checked 2026-10-26)
//
//	https://elevenlabs.io/docs/capabilities/speech-to-text
//	https://elevenlabs.io/docs/overview/models (Scribe models)
//	https://elevenlabs.io/docs/api-reference/speech-to-text/convert
//
// Endpoint: POST https://api.elevenlabs.io/v1/speech-to-text (multipart).
// Auth: xi-api-key header. model_id: scribe_v2 (default; current batch Scribe)
// or scribe_v1 (legacy). Optional language_code. File field required.
//
// Provider id: "elevenlabs". Price is the profile's audio_seconds_in.
package elevenlabs
