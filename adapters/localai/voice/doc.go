// Package voice implements the speech roles of internal/providers/voice (STT
// batch and streaming, streaming TTS, VAD) over the audio routes of LocalAI.
// It is the "separate STT/TTS adapter" of master plan section 21 milestone M4:
// used when the Realtime pipeline (adapters/localai/realtime) is unavailable or
// not configured. Like the rest of adapters/localai it is the only code that
// knows LocalAI exists; nothing outside adapters/localai and the composition
// roots (cmd/, internal/cli/, tools/) imports it.
//
// New takes the localai.Config the text client was built from (endpoint, TLS,
// key reference, request timeout, response cap: one runtime, one endpoint
// policy) and a voice Config (which model serves STT, TTS and VAD, optionally
// per profile, and the audio limits). One Client implements all three roles;
// register it per role with voice.Register. Voice is optional: a deployment
// without voice models builds no Client, and a Client with no model configured
// for a role refuses that role (validation_failed) instead of defaulting.
//
// # Pinned runtime
//
// Read on 2026-10-04 from the LocalAI v4.10.0 sources (the version pinned by
// deploy/container/localai) and the current documentation:
//
//   - transcription: https://localai.io/docs/features/audio-to-text/ and
//     https://github.com/mudler/LocalAI/blob/v4.10.0/core/http/endpoints/openai/transcription.go
//     (form fields, JSON and server-sent-events answers), with the result types in
//     https://github.com/mudler/LocalAI/blob/v4.10.0/core/schema/transcription.go
//   - text to speech: https://localai.io/docs/features/text-to-audio/ and
//     https://github.com/mudler/LocalAI/blob/v4.10.0/core/http/endpoints/localai/tts.go
//     (streaming and whole-file answers), with the streamed WAV framing built in
//     https://github.com/mudler/LocalAI/blob/v4.10.0/core/backend/tts.go
//     (ModelTTSStream)
//   - voice activity detection: https://localai.io/docs/features/voice-activity-detection/ and
//     https://github.com/mudler/LocalAI/blob/v4.10.0/core/http/endpoints/localai/vad.go
//   - request and response types (TTSRequest, VADRequest, VADResponse):
//     https://github.com/mudler/LocalAI/blob/v4.10.0/core/schema/localai.go
//   - route table: https://github.com/mudler/LocalAI/blob/v4.10.0/core/http/routes/openai.go
//     and https://github.com/mudler/LocalAI/blob/v4.10.0/core/http/routes/localai.go
//
// # Endpoint allowlist
//
// The client may call exactly the routes in AllowedEndpoints, all of them POST
// and all of them authenticated:
//
//	/v1/audio/transcriptions   speech to text (multipart upload)
//	/v1/audio/speech           text to speech (JSON, audio answer)
//	/v1/vad                    voice activity detection (JSON)
//
// This table is separate from the five-route table of package localai (health,
// version, model list, chat, embeddings): neither can widen the other. The
// aliases (/tts, /vad, /audio/...), voice listing, model install, backends,
// galleries, settings, agents and every other route are absent, so they cannot
// be reached. The check is in the transport every request passes through, not in
// the callers: method and path must match exactly (no query, no escaped path),
// the scheme and host must be the configured ones, redirects are never followed,
// and the Authorization header is attached there and only there. Voice has no
// health route of its own: the runtime's health is the text client's.
//
// The transport (loopback dial check, proxy-free, no redirects, key handling,
// error classification) is a deliberate small copy of package localai's: its
// versions are unexported, and widening that package was outside this task. The
// endpoint is vetted by localai.Config.Validate, so the policy cannot drift.
//
// # Speech to text
//
// Transcribe sends one multipart/form-data POST: the fields model,
// response_format=verbose_json, temperature=0 and, when a language is asked for,
// language (the primary subtag of the BCP 47 tag: "it-IT" is sent as "it"), and
// the audio as a file part named audio.wav. The PCM is wrapped in a canonical
// WAV header in memory (LocalAI converts any container itself); the file name is
// fixed, never derived from the caller. The answer's text is returned trimmed;
// the duration reported is that of the audio sent, not a number the runtime
// claims.
//
// LocalAI's transcription route takes a complete file, so live input cannot be
// streamed to it. TranscribeStream buffers Send chunks in memory (bounded by the
// limits) until CloseSend; the first Recv after that sends the audio with
// "stream": true and relays the runtime's output: each "transcript.text.delta"
// event becomes an interim event carrying the text so far, the
// "transcript.text.done" event becomes the final event, then Done. The
// transcript is complete only when both the done event and the [DONE] marker
// arrived; an error event, a body that ends early or a [DONE] without a final
// transcript is an error and the text received so far is partial. A runtime
// that ignores "stream" and answers with JSON yields the same events, without
// interims. No goroutine is started: Recv waits on the stream's own channels.
// Audio in, in real time, is the Realtime adapter's job.
//
// # Text to speech
//
// SynthesizeStream sends a JSON POST (model, input, voice, language,
// response_format=wav, stream=true, and sample_rate when the caller asked for a
// format) and returns once the runtime answers 200 with audio; Recv then reads
// the WAV header and the samples. The format is the one the header states, never
// the one requested: a streamed answer is a 44-byte header whose size fields hold
// 0xFFFFFFFF, followed by raw 16-bit mono PCM at the model's rate. A complete
// file (a backend that cannot stream, or Config.TTSNoStream) is accepted too,
// with extra header chunks skipped and the stated length honoured. Every chunk
// holds whole sample frames (a half sample is carried to the next read), the
// last chunk has Done set and carries no samples, and Close drops the connection,
// which stops the runtime's output at once and unblocks a waiting Recv.
//
// Completion is the end of the body. A cut connection, a short file and a header
// that ends early are unavailable; an empty answer, a half sample, a format
// other than 16-bit PCM and an unusable rate are schema_invalid; audio past the
// response cap is resource_exhausted. One gap cannot be closed: if the runtime
// fails after it has sent the header, it can only end the chunked body, which
// looks like a clean short end. Zero audio is detected (schema_invalid); a
// truncated tail is not. The caller must treat TTS output as best effort, and
// the verified text is always returned beside it (master plan section 18).
//
// # Voice activity detection
//
// DetectSpeech sends {"model", "audio": [float32...]} and reads {"segments":
// [{"start", "end"}]} with bounds in seconds. LocalAI's VAD takes 16 kHz mono
// samples only, so any other format is refused with validation_failed rather
// than resampled; the session service must capture VAD audio at 16 kHz mono.
// The "segments" key must be present (LocalAI sends null when it found no
// speech), and the list is checked against the audio with
// voice.DetectResult.Check (ordered, inside the audio) before it is returned.
//
// # Privacy
//
// Audio never leaves the machine (the endpoint policy above), and the adapter
// writes none of it to disk: requests are built in memory, a test fails if a
// non-test file creates or writes a file, and the audio fields are never logged.
// The runtime itself, on the same machine, writes an uploaded transcription file
// to a temporary directory (os.MkdirTemp in the v4.10.0 handler) and removes it
// afterwards; a deployment that keeps raw audio off disk mounts the runtime's
// temporary directory on tmpfs.
//
// # Limits
//
// voice.Limits (Config.MaxAudioBytes, MaxChunkBytes, MaxTextRunes; defaults
// from voice.DefaultLimits) are applied before anything is sent, and an
// oversize request is validation_failed. Response bodies and synthesized audio
// are capped by localai.Config.MaxResponseBytes. Config.RequestTimeout bounds a
// call, a stream's request included (for TranscribeStream it starts at the
// request, after CloseSend, not at the open).
//
// # Errors
//
// Every failure is a *contracts.Error, classified by HTTP status first and, for
// 500 and 503, by short phrases from the body; a body is never copied into an
// error (it can hold the transcript or the spoken text):
//
//	connection refused, reset, EOF, 502, 503, 404, stream cut      unavailable
//	deadline, 504, 408, network timeout                            timeout
//	429, 507, out of memory, model load failure, audio past cap    resource_exhausted
//	413, an oversize or malformed request, a role with no model    validation_failed
//	malformed or incomplete runtime answer, bad audio header,
//	  empty audio, VAD segments that do not fit the audio          schema_invalid
//	401, 403, other 4xx, other 5xx                                 internal
//
// Caller cancellation stays a plain error wrapping context.Canceled; a deadline
// is a timeout that still matches context.DeadlineExceeded; Recv after Close is
// providers.ErrStreamClosed. The adapter never retries.
//
// # Logging
//
// An optional *slog.Logger receives debug records with the method, path, status,
// duration and error code. Audio, transcripts, spoken text, response bodies and
// the key are never logged.
package voice
