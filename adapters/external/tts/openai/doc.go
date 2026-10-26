// Package openai is the external text-to-speech adapter for OpenAI's Audio
// Speech API. It implements internal/providers/voice.TTS. It is DISABLED by
// default and only wired from internal/cli when external mode is on; a binary
// built with -tags noexternal does not link it.
//
// # Research record (checked 2026-10-26)
//
//	https://platform.openai.com/docs/api-reference/audio/createSpeech
//	https://platform.openai.com/docs/guides/text-to-speech
//
// Endpoint: POST /v1/audio/speech (JSON). Models: gpt-4o-mini-tts (default),
// gpt-4o-mini-tts-2025-12-15, tts-1, tts-1-hd. Built-in voices include alloy,
// ash, ballad, coral, echo, fable, onyx, nova, sage, shimmer, verse, marin,
// cedar (tts-1 / tts-1-hd support a smaller set). response_format pcm returns
// raw 24 kHz 16-bit signed little-endian mono PCM (no WAV header). Maximum
// input length: 4096 characters. Authentication: API key in Authorization:
// Bearer (the SIWC / plan token covers eligible Responses API requests only —
// speech needs a key).
//
// Provider id: "openai" (same slug as the chat and STT adapters; role tts on
// the profile). Metering unit needed by the price table: characters. The
// schemas/providers meterUnit enum has no characters unit as of this check
// (only toks_*, images_*, audio_seconds_*, file_bytes_in, requests) — report
// only; schemas are owned by the sibling agent.
package openai
