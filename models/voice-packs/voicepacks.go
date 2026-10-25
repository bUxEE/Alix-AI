// Package voicepacks carries the shipped language-pack manifests (the *.yaml
// files in this directory) and their schema inside the AlixAI binary.
//
// A manifest that lived only as a loose file beside the model files could be
// replaced together with a tampered model, which would defeat the hash check
// (master plan 16.3). Embedding it ties the pinned hashes to the release that
// was built and signed. internal/voice/packs installs only a manifest from this
// set. The model files themselves are never committed: the manifest pins them.
package voicepacks

import "embed"

// FS holds every pack manifest, at its root, and the manifest schema.
//
//go:embed *.yaml pack-manifest.schema.json
var FS embed.FS

// SchemaFile is the name of the manifest schema inside FS.
const SchemaFile = "pack-manifest.schema.json"
