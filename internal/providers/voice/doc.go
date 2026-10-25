// Package voice defines the speech roles of the AI runtime seam: STT
// (streaming and batch), TTS (streaming) and VAD. They are the typed
// interfaces behind the reserved providers.RoleSTT, RoleTTS and RoleVAD
// (master plan sections 2, 8.1 and 13). Like internal/providers it holds
// interfaces, request validation, size limits and small helpers only: no HTTP,
// no runtime-specific code, no goroutines. adapters/localai/voice implements
// the roles, and the sub-package fake implements all of them
// deterministically for tests.
//
// # Optional by construction
//
// Voice is an installable interface to the same application session, and a
// text-only deployment carries none of it. Nothing in this package starts work
// on import. An implementation costs nothing until internal/voice/profile
// decides that voice is installed and enabled and constructs the providers; a
// role that is not registered degrades as master plan section 18 says
// (text input only, verified text only, manual turn taking), which
// providers.Lookup reports through the error's "effect" detail.
//
// # Roles
//
//   - STT turns audio into text, either one buffer at a time (Transcribe) or as
//     a live stream (TranscribeStream: audio in through Send, interim and final
//     transcript events out through Recv).
//   - TTS turns text into audio as a stream (SynthesizeStream). The text must
//     already be verified response text: the voice session service, not this
//     package, owns that gate. Closing the stream cancels playback at once.
//   - VAD finds the speech segments of an audio buffer (DetectSpeech).
//
// A role needs the typed access in this package: the registry accepts any
// non-nil value for a reserved role, so Register type-checks first and the
// Lookup helpers (LookupSTT, LookupTTS, LookupVAD) check again on the way out.
//
// # Authority
//
// A transcript is the output of a model and is untrusted input to the turn
// pipeline, exactly like user text: it never grants a permission and is never
// executed. The guarantees this package gives callers are deliberately small:
//
//   - Every call validates the contracts.Scope first; a missing scope is an
//     error, never a default.
//   - A stream is complete only when its Done event or chunk was received.
//     CollectTranscript and CollectAudio return partial output as an error,
//     never as a success.
//   - Audio is capped (Limits) before it reaches the runtime, and an oversize
//     request is validation_failed, not resource_exhausted: retrying the
//     identical request can never succeed.
//
// # Privacy
//
// Raw audio is not retained by default (master plan section 13). Audio fields
// carry the json:"-" tag so a request that is logged, audited or serialised by
// mistake does not leak it, and the fake records sizes, never samples.
//
// # Errors
//
// Every failure is a *contracts.Error: unavailable (runtime unreachable),
// timeout, resource_exhausted (model load failure), schema_invalid (the runtime
// answered malformed), validation_failed (the request itself is illegal) and
// scope_missing / scope_violation from the scope. Context cancellation by the
// caller stays a plain error wrapping context.Canceled (providers.ContextError).
package voice
