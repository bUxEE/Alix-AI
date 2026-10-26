# 0039 — Dictation without VAD (amendment of 0015 for user-recorded clips)

- **Status:** accepted
- **Date:** 2026-10-26
- **Decided in:** release 1.1.0

## Context
ADR [0015](0015-voice-models-lite.md) forbids "STT without the VAD in front" because Whisper
and similar models invent text on silence and noise when fed an open microphone. That rule is
correct for **voice-conversation mode**, where the local VAD cuts utterances before recognition.

Dictation (record-to-text, 1.1.0) is a different path: the user presses record, stops, and the
clip becomes editable text in the chat input. There is no open microphone stream and no
voice-conversation WebSocket. VAD would only re-segment a clip the user already bounded.

External STT may receive a user-recorded clip under the same egress, modality and budget checks
as VAD-cut audio (`extmodal.WithUserSegmented`), with audio egress still off by default.

## Options
1. **Require VAD for dictation too.** Forces a VAD model on every STT-only pack and every
   light-external deployment that only wants record-to-text.
2. **Allow STT-only for dictation; keep VAD mandatory for voice conversation (chosen).**
   Mitigations replace the VAD for this path only.
3. **Drop the VAD rule entirely.** Unsafe for conversation mode (0015 evidence).

## Decision
Amend 0015 for dictation only:

1. Voice-conversation mode still requires a VAD in front of STT (pack and session Open refuse
   an STT-only pack for the WebSocket).
2. Dictation may use an STT-only stack. Segmentation is `user_recorded` (host record/stop).
3. Mitigations: accept only WAV PCM s16le mono 16 kHz; reject clips shorter than 300 ms, longer
   than the configured maximum (default 60 s, hard ceiling 300 s), or silent (RMS/peak energy);
   never retain audio (zero the buffer); never log transcript or audio; the transcript is always
   editable and never auto-sent as a turn.
4. External STT for a dictation clip uses `WithUserSegmented` under the existing raw-audio,
   data-egress (audio class, audio setting off by default), modality and `audio_seconds_in`
   budget rules.

## Consequences
- Easier: STT-only packs (`en-stt` and light-external setups) support dictation without a VAD.
- Harder: two segmentation markers in `extmodal` (VAD vs user); conversation Open must keep
  refusing STT without VAD.
- Forbidden: treating dictation as a bypass of audio egress or budget; auto-submitting a
  dictation transcript; retaining raw audio after recognition.

## Reversal plan
Remove `WithUserSegmented` and the dictation service; restore "VAD always" in pack/session
checks. Routes and schemas stay (additive 1.1.0); the host hides the record control when
capabilities report unavailable.
