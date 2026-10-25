// Package extmodal is the multimodal and voice layer of External Provider Mode (additive plan
// section 10, ADR-0024, task X-15). It gates and handles the DATA of four modalities that an
// external provider can serve: vision (images and files in), image generation, speech to text and
// text to speech. It makes no provider call of its own: a caller hands it the provider's
// providers.MultimodalGenerator, voice.STT or voice.TTS, and the package decides whether the call
// may happen and what comes back.
//
// # Off by default
//
// Every modality is OFF. A modality runs only when the provider level and the tenant level of the
// Policy both enable it (the zero Policy enables nothing), the provider profile declares it with its
// media, voice or image-generation block and the matching role, the runtime capabilities support it,
// and the provider's retention of that kind of media is known. Each failure is a typed result:
// ReasonModalityOff (permission_denied) for policy, an UnsupportedError (validation_failed,
// providers.IsUnsupported) for a model that cannot, ReasonRetentionUndisclosed for a retention the
// profile does not state. Admit returns the Disclosure to show before data leaves: what the provider
// keeps, and for how long.
//
// # Voice
//
// The finalised local transcript is used before anything else: a transcript, or a local STT, means no
// audio leaves. Raw audio goes to an external STT only when no local STT is installed, the provider
// and the tenant both set the explicit RawAudio switch, the utterance was cut by the local VAD, the
// provider accepts its size and the data-egress guard allows it (Deps.Audio: the audio of the class its
// session states, under the audio setting of the egress policy, which is off by default). External TTS receives only text that was attested as a verified
// response (Service.Attest takes the verify.Outcome of the turn; a piece of an attested text is
// verified too, because the voice session speaks long answers in pieces). Wake word and microphone
// state never reach this package. NewSTT treats audio as VAD-cut only when the call's context carries
// WithVADSegmented (a session that does not cut utterances never sets it, so its raw audio stays local).
// The decorators NewSTT and NewTTS implement voice.STT and voice.TTS
// so the composition root can plug them through the voice provider factory; wiring them is not done
// here.
//
// # Images and files
//
// Only AlixAI file references are used (fil_ ids): each is resolved from scratch through the files
// service in the caller's scope, which enforces scope and expiry, and the bytes are handed to the
// provider adapter by that reference, never as a path or a URL. A file must carry an explicit
// approval for the provider; an application-private file (origin application, treatment
// application_object) also needs the explicit egress permission of the approval; a file approved as
// excerpt-only is sent as a bounded transformed excerpt, never as itself. Text that leaves (prompt,
// excerpt, speech) passes the TextEgress seam, which the composition root backs with the data-egress
// guard.
//
// # Budget
//
// Every external call (vision, image generation, speech, speech to text) is reserved on the provider budget
// (Deps.Budgets) right before it is made, against the egress decision of what it sends (its text, or for speech
// to text its audio), and settled with what it consumed, as the text router does: no budget, a refused reservation
// or a call with no upper bound means no call. Speech to text is bounded by the length of the audio, rounded up to
// whole seconds, at the profile's price per input audio second.
//
// # Output
//
// A provider reply is untrusted data. Every part is validated first (a provider cannot name a
// reference, a path or a URL; media types are an allowlist without SVG or HTML); produced media is
// then stored locally through the narrow GeneratedStore (Put and Delete, origin generated, a name
// chosen here) and only the AlixAI-managed FileRef is returned, so a provider never writes into
// application storage and nothing it produced is reachable except through the local store. Markdown
// and HTML text become sanitised markdown blocks (raw HTML, images and unsafe links are removed),
// and block JSON is validated by internal/blocks, charts against the turn's facts; an invalid block
// is dropped and reported, never repaired.
package extmodal
