// Package mistral is the external speech-to-text adapter for Mistral AI's Audio
// Transcriptions API (Voxtral). It implements internal/providers/voice.STT for a
// finalised clip (no streaming). It is DISABLED by default and only built from
// internal/cli/providers_external_stt.go when external mode is on; a binary
// built with -tags noexternal does not link it.
//
// # Research record (checked 2026-10-26)
//
//	https://docs.mistral.ai/api/endpoint/audio/transcriptions
//	https://docs.mistral.ai/studio/audio/speech-to-text/transcribe-files
//	https://docs.mistral.ai/capabilities/audio_transcription/basic_transcription
//
// Endpoint: POST /v1/audio/transcriptions (multipart) at https://api.mistral.ai.
// Current offline transcription models: voxtral-mini-transcribe-3 (alias of
// voxtral-mini-transcribe-3-0 / voxtral-mini-transcribe-latest) and
// voxtral-small-transcribe-3. Optional fields include language, diarize,
// timestamp_granularities=segment, context_bias. Authentication: API key in
// Authorization: Bearer (same key as chat). Data location defaults to the EU;
// see adapters/external/mistral for retention and training terms.
//
// TTS is a separate package: adapters/external/tts/mistral (POST /v1/audio/speech).
//
// Provider id: "mistral" (same slug as the chat adapter; role stt on the profile).
// Price is the profile's audio_seconds_in; this package does not price calls.
package mistral
