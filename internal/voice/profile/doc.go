// Package profile is the installable voice profile (master plan sections 2,
// 13 and 17.2): the one place that decides whether any speech component
// exists in this process.
//
// # Absent unless installed and enabled
//
// A text-only deployment carries no speech-model cost. Load therefore builds
// nothing unless both conditions hold, checked cheapest first:
//
//  1. voice is enabled (the voice.enabled setting, off by default). When it is
//     not, Load returns at once: it does not consult the language-pack
//     catalog, does not call the provider factory, does not touch the provider
//     registry, allocates nothing and starts no goroutine. Nothing else in the
//     package does any of that at import time either.
//  2. a language pack is installed (the catalog lists at least one). When none
//     is, the answer is the same: no factory call, nothing registered.
//
// Only then is the provider factory called (that is where a runtime connection
// would be made) and the STT, TTS and VAD roles registered. The profile never
// starts a goroutine of its own, enabled or not: streams and pack loading run
// in the caller's goroutines.
//
// An absent profile is a *Profile that is nil together with a Status saying
// why. Every method that is useful on an absent profile is nil-safe. A profile
// that is wanted but cannot come up (misconfigured, a runtime that refuses,
// a configured pack that will not activate) is an error, never an absence:
// the caller logs it and runs text-only, the section 18 behaviour.
//
// # Language packs
//
// A language pack is the installed models for one language (master plan 13:
// "language packs are installed and activated explicitly"; 17.2: "language
// packs loaded individually"). Installing packs is another task's business
// (internal/voice/packs, M4-05); this package defines the registry interface
// it implements (PackRegistry), the catalog it needs to read (PackCatalog) and
// the loader hook adapters implement (PackLoader), and ships the one
// implementation of the activation rule (Packs).
//
// Activation is explicit and one pack at a time. Activate is serialised, at
// most one pack is ever loaded, and activating a second pack while another is
// active is refused with a conflict rather than swapped: the active pack may be
// serving a live voice session, so the operator (or the session service)
// deactivates it first. Nothing is activated by default and nothing is guessed;
// an unknown, uninstalled or malformed pack never loads.
//
// # What a loaded profile guarantees
//
// The providers registered for the voice roles are gated: a call is refused
// with unavailable, carrying the section 18 effect for the role (text input
// only, verified text only, manual turn taking), unless the profile is open,
// a pack is active and that pack provides the role. The gated providers also
// report that state through their health probe, so the registry's readiness
// view shows a voice role that cannot serve instead of looking healthy. The
// registry has no Unregister, so a profile is loaded once per process and Close
// disables the gates rather than removing the registration.
package profile
