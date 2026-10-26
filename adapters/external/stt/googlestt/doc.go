// Package googlestt is the external speech-to-text adapter for Google Cloud
// Speech-to-Text API v2 (Recognize). It implements internal/providers/voice.STT
// for a finalised clip. Disabled by default; wired from
// internal/cli/providers_external_stt.go.
//
// # Research record (checked 2026-10-26)
//
//	https://docs.cloud.google.com/speech-to-text/docs/models/chirp-3
//	https://docs.cloud.google.com/speech-to-text/docs/transcription-model
//	https://docs.cloud.google.com/speech-to-text/docs/locations
//	https://docs.cloud.google.com/speech-to-text/docs/reference/rest/v2/projects.locations.recognizers/recognize
//
// Endpoint: POST
// https://{location}-speech.googleapis.com/v2/projects/{project}/locations/{location}/recognizers/{recognizer}:recognize
// Model: chirp_3 (GA in us and eu multi-regions). Auth: Authorization: Bearer
// <OAuth access token> with cloud-platform scope (google-stt connection), or an
// API key when the operator configures one as the credential (v2 primarily
// documents OAuth / ADC; API key support is operator-dependent).
//
// Provider id: "google-stt". Price is the profile's audio_seconds_in.
package googlestt
