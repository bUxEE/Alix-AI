// Package session is the voice session service (master plan 13 and 14.2,
// "Voice"): the interface that lets a user talk to the SAME conversation
// session as the text API does.
//
//	audio -> speech recognition -> final transcript -> normal turn pipeline
//	      -> verified response text -> speech synthesis -> audio
//
// A Service opens Sessions; the gateway upgrades a request to a WebSocket
// (GET /v1/sessions/{session_id}/voice) and a Session serves it with the JSON
// messages of schemas/voice. The package owns the protocol and the rules of
// section 13, one per requirement; it holds no model, no HTTP and no transport
// of its own: the speech roles are the gated providers the voice profile
// (internal/voice/profile) registers, the turn pipeline is a TurnRunner (the
// dialogue engine, wrapped by the gateway), and the connection is a Transport.
//
// # The requirements of section 13
//
//   - Voice and text share one session and its structured goal state. The
//     final transcript runs as the user's message on the session the verified
//     context token names, under exactly that scope (cloned, so nothing the
//     caller does afterwards reaches it). No second conversation is minted.
//     (TestVoiceAndTextShareSession)
//   - The finalised transcript is visible to the host: interim hypotheses
//     stream for display, one transcript.final precedes the turn it starts.
//     (TestFinalTranscriptVisibleToHost)
//   - The user may correct the transcript before any consequential
//     confirmation: transcript.correct runs the corrected text as a new message
//     on the same session, the pipeline's own correction rules void what was
//     prepared from the old text, and the interface is told which preparation
//     was superseded. Speech has no way to confirm at all: TurnRunner carries
//     text and nothing else, so a transcript can start a goal, answer a question
//     or correct a value, never confirm, reject or choose.
//     (TestTranscriptCorrectionBeforeConfirm)
//   - Speech begins only after the material facts are verified: it starts from
//     a verify.Outcome and nothing else, after the turn has returned, only when
//     its provenance is coherent, and the model's text is verified once more
//     against the directive and the receipt the turn produced. Text that does
//     not pass is delivered and not spoken. (TestTTSOnlyAfterVerification)
//   - Playback can be cancelled immediately, by the client and by barge-in (a
//     new capture, a submission or a correction): synthesis is stopped before
//     anything else happens, and no audio of the playback follows its
//     playback.cancelled event. (TestCancelPlaybackImmediate)
//   - Raw audio is not retained by default. The session holds an utterance's
//     audio only while it is recognised (and only when a tenant retains it or a
//     speaker recogniser needs it), zeroes it afterwards, and puts none in a log
//     or an event. A tenant whose AudioPolicy says so has the audio handed to the
//     deployment's AudioRetainer, and a deployment with no retainer cannot open a
//     session for it. (TestRawAudioNotRetained)
//   - Language packs are installed and activated explicitly, one at a time
//     (internal/voice/profile): a session opens only while a pack that provides
//     recognition is active, and a pack taken away cancels the live streams
//     (Service.Packs, Service.CancelLiveStreams). The profile's gate is checked
//     when a stream opens and a stream already open keeps running, which is why
//     the service has to cancel them itself.
//   - Voice workers are absent from text-only profiles: a Service is built only
//     when the profile is loaded, owns no goroutine, and opening a Session
//     starts none until it is served. (TestTextOnlyHasNoVoiceWorkers)
//   - Speaker recognition may improve convenience but cannot replace
//     application authorization: an optional SpeakerRecognizer's hint is
//     reported beside the transcript as a hint and used for nothing else; the
//     scope of every turn is the token's. (TestSpeakerRecognitionNeverAuthorizes)
//
// # When a speech component fails
//
// Failure of speech leaves text and application control functional (section
// 18). Every failure is a typed error event carrying the degradation effect
// (text_input_only, verified_text_only, manual_turn_taking); the session stays
// open, the text API never noticed, and a recognition that fails part way yields
// no transcript at all. (TestSTTDownTextStillWorks)
//
// # Concurrency
//
// One goroutine reads the client's messages and never waits on the pipeline, so
// a cancellation is handled at once. A capture is read by a goroutine of its
// own; finalised utterances run the pipeline one at a time, in order, on one
// worker; a playback runs on its own goroutine. All writes to the connection go
// through one lock, and the check that a playback was cancelled is made under
// it, which is what guarantees that nothing follows playback.cancelled. A new
// utterance advances a counter (the barge-in clock): speech for a response whose
// turn began before the newest utterance is never started.
package session

// FullDuplexMeasured says whether a measured end-to-end test has proven full
// duplex voice (speech in both directions at once, with the latency and the
// echo and interruption behaviour that claim needs). It is false: this service
// is turn taking with barge-in, and it claims nothing more.
//
// ADR 0035 requires the AG-UI capabilities answer to state voice honestly. The
// answer (agui.NotClaimed, "voice_full_duplex"), docs/integration/agui.md and
// this constant move together: TestVoiceCapabilitiesNeverClaimFullDuplexUntilMeasured
// (internal/gateway) fails when they disagree, so flipping any one of them
// without the others cannot go unnoticed.
const FullDuplexMeasured = false
