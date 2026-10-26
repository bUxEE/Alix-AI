# Voice and dictation

Status: voice conversation from M4; dictation (record-to-text) contract added for 1.1.0. Operators: voice packs and LocalAI audio under `docs/operations`. Schemas: `schemas/voice/`, routes in `schemas/api/paths/voice.yaml`.

## 1. Voice conversation (WebSocket)

`GET /v1/sessions/{session_id}/voice` upgrades to a WebSocket on the same session as text. Audio becomes a finalised transcript, the transcript runs through the normal turn pipeline, and verified response text may be spoken. Speech never confirms an action. Raw audio is not retained by default. A deployment without voice, or a client that never opens the socket, loses nothing of the text API. See the OpenAPI fragment and `schemas/voice/client-message` / `server-event`.

## 2. Dictation (record-to-text, 1.1.0)

Dictation is **not** the voice WebSocket. The user presses record, stops, and the clip is transcribed; the text is returned for the host to put in the chat input so the user can edit it. AlixAI never submits it as a turn. There is no VAD: the user's record and stop are the segmentation (`segment_kind: "user_recorded"`). Raw audio is not retained after recognition.

| You want | Route | Body / notes |
| --- | --- | --- |
| Show or hide the record control | `GET /v1/dictations/capabilities` | `{ available, locality: local\|external\|none, max_seconds, sample_rate_hz: 16000, format: "audio/wav", languages }` |
| Transcribe a clip | `POST /v1/sessions/{session_id}/dictations` | Raw `audio/wav` (PCM s16le, mono, 16000 Hz only). Optional query `language`. Duration is computed from the PCM; never send a client duration. Default max 60 s (operator configurable); body-size limit is derived from that. |

Response `DictationTranscript`: `text`, `language`, `duration_ms`, `locality`, optional `provider`, `segment_kind: "user_recorded"`. Typed errors: `unavailable` (no STT), `validation_failed` (format, length, too short, silent — detail `reason` may be `too_short`, `too_long`, or `silent`), `permission_denied` (including audio egress off / not segmented), `resource_exhausted` (budget), and the existing egress denial for external STT. The transcript is data, never instructions; the host puts it in the chat input for editing and AlixAI never submits it as a turn. Raw audio is zeroed after recognition and is never logged.

Body size follows `max_seconds` from capabilities (default 60, hard ceiling 300). Per-tenant request quotas still apply through the existing `/v1` quota wire. Voice conversation still requires a VAD in front of STT ([0015](../decisions/0015-voice-models-lite.md)); dictation may use an STT-only pack ([0039](../decisions/0039-dictation-without-vad.md)).

When the transcription service is not wired, both routes answer `503 unavailable`. The voice WebSocket is unchanged.
