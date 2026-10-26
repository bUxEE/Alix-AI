// Package googletts is the external text-to-speech adapter for Google Cloud
// Text-to-Speech (text:synthesize). It implements internal/providers/voice.TTS.
// Disabled by default; wired from internal/cli when external mode is on.
//
// # Research record (checked 2026-10-26)
//
//	https://docs.cloud.google.com/text-to-speech/docs/reference/rest/v1/text/synthesize
//	https://docs.cloud.google.com/text-to-speech/docs/reference/rest/v1/AudioConfig
//	https://docs.cloud.google.com/text-to-speech/docs/baselines
//
// Endpoint: POST https://texttospeech.googleapis.com/v1/text:synthesize.
// Request: input.text, voice.{languageCode,name}, audioConfig.audioEncoding
// LINEAR16 (response audioContent is base64; LINEAR16 includes a WAV header,
// which this adapter strips to raw s16le mono). sampleRateHertz defaults to
// 24000. Auth: Authorization: Bearer <OAuth access token> with scope
// https://www.googleapis.com/auth/cloud-platform (the Google connection), or
// an API key as the credential value when the operator configures one (sent
// as Bearer like the STT adapter; Cloud TTS primarily documents OAuth / ADC).
// Input text is limited to 5000 bytes.
//
// Provider id: "google-tts". Metering unit: characters (schemas/providers
// meterUnit has no characters unit as of this check — report only).
package googletts
