// Package mistral is the external text-to-speech adapter for Mistral AI's
// Audio Speech API (Voxtral TTS). It implements internal/providers/voice.TTS.
// It is DISABLED by default and only wired from internal/cli when external mode
// is on; a binary built with -tags noexternal does not link it.
//
// # Research record (checked 2026-10-26)
//
//	https://docs.mistral.ai/api/endpoint/audio/speech
//	https://docs.mistral.ai/studio/audio/text-to-speech/generate-speech
//	https://docs.mistral.ai/studio/audio/specs-and-limits/audio-formats
//	https://docs.mistral.ai/studio/audio/text-to-speech/overview
//
// Endpoint: POST /v1/audio/speech (JSON) at https://api.mistral.ai. Model:
// voxtral-mini-tts-3-0 (alias voxtral-mini-tts-3 / voxtral-mini-tts-latest as
// documented). Request fields: model, input, voice_id (required; preset slug
// e.g. gb_jane_neutral or a workspace voice id), response_format. Response is
// JSON { "audio_data": "<base64>" } (not a raw audio body). Formats offered:
// mp3 (default), wav, pcm, flac, opus. Generated speech is mono at 24 kHz.
// Official audio-formats page describes pcm as raw samples with no file header;
// an older speech page labelled pcm as float32 LE (streaming paths). This
// adapter requests response_format=wav and strips the RIFF/WAVE container to
// signed 16-bit little-endian mono PCM (LINEAR16) at 24 kHz — no MP3/FLAC/Opus
// decoding. Authentication: API key in Authorization: Bearer (same key as chat
// / STT). Data location defaults to the EU; see adapters/external/mistral.
//
// Provider id: "mistral" (same slug as chat and STT; role tts on the profile).
// Prefer a characters_in price on the profile (ADR 0043).
package mistral
