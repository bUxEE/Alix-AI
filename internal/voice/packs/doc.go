// Package packs is the language-pack packaging of the voice profile (master plan
// 13, 16.3, 17.2; ADR 0015): the manifest of a pack, the store that installs and
// verifies it, and the PackCatalog and PackLoader that internal/voice/profile
// needs.
//
// # Manifests
//
// A pack is one YAML manifest in models/voice-packs/, validated against
// pack-manifest.schema.json: its language, the STT, TTS and VAD models it
// provides (any non-empty subset: STT-only is valid), each model's files with a
// pinned size and SHA-256, the recorded licences, the quality scores and the
// resource needs (null until M4-06 measures them). The manifests are embedded in
// the binary, and only a pack in that set can be installed: there is no way to
// install, or activate, a pack the release does not approve.
//
// # Installing and activating
//
// Nothing here downloads anything. The operator fetches the pinned files (the
// manifest says where) and Install copies them into the store, checking every
// file's size and SHA-256 as it copies. Install needs the operator's explicit
// approval of that pack and refuses without it before it touches the disk, and it
// never activates: activation is a separate explicit step.
//
// Activation is persisted as a marker file, created exclusively, so at most one
// pack is active across processes and a second activation is refused with
// conflict until the operator deactivates the first. The files are verified again
// at activation (Loader.Load), so a file changed after install keeps the pack
// from loading.
//
// This package imports no network or process-execution package: a runtime
// connection is the provider factory's business, not the pack store's.
package packs
