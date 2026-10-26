// Package elevenlabs is the external text-to-speech adapter for ElevenLabs
// Text-to-Speech. It implements internal/providers/voice.TTS. Disabled by
// default; wired from internal/cli when external mode is on.
//
// # Research record (checked 2026-10-26)
//
//	https://elevenlabs.io/docs/api-reference/text-to-speech/convert
//	https://elevenlabs.io/docs/overview/models
//	https://help.elevenlabs.io/hc/en-us/articles/13298164480913-What-s-the-maximum-amount-of-characters-and-text-I-can-generate
//
// Endpoint: POST https://api.elevenlabs.io/v1/text-to-speech/{voice_id}
// with query output_format=pcm_16000 or pcm_24000 (raw s16le mono PCM at that
// rate). Auth: xi-api-key header. Models: eleven_multilingual_v2 (default;
// 10_000 character limit), eleven_flash_v2_5 (40_000). Body fields: text,
// model_id. Optional language_code (ISO 639-1; not supported for multilingual_v2).
//
// Provider id: "elevenlabs". Metering unit: characters (schemas/providers
// meterUnit has no characters unit as of this check — report only).
package elevenlabs
