// Package registry carries the shipped model registry (the *.yaml files in this
// directory) inside the AlixAI binary.
//
// A registry that lives only as loose files beside the model files could be
// replaced together with a tampered model, which would defeat the hash check
// (SEC-063, master plan 16.3). Embedding it ties the pinned hashes to the
// release that was built and signed. Load it with models.LoadFS(registry.FS);
// models.LoadDir is for tools that edit the files.
package registry

import "embed"

// FS holds every registry entry file, at its root.
//
//go:embed *.yaml
var FS embed.FS
