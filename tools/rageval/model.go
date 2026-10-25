package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/models/registry"
)

// fakeModelID is the one id accepted without a registry entry, and only with
// the fake embedder: it names no model.
const fakeModelID = "fake"

// resolved is what the registry says about the embedding model under test.
type resolved struct {
	Model ModelInfo
	// Entry is the registry entry, nil for the fake embedder.
	Entry *models.Entry
}

// ResolveModel looks the embedding model up in the embedded registry. The id
// must be in the registry; the one exception is the fake embedder's "fake".
// The artefact hash is the registry's pinned value (rageval does not load the
// file: LocalAI does, so there is no file here to hash).
func ResolveModel(provider, id, runtimeName string) (resolved, error) {
	reg, err := models.LoadFS(registry.FS)
	if err != nil {
		return resolved{}, fmt.Errorf("model registry: %w", err)
	}
	return resolveModel(reg, provider, id, runtimeName)
}

func resolveModel(reg *models.Registry, provider, id, runtimeName string) (resolved, error) {
	out := resolved{Model: ModelInfo{RegistryID: id, ArtifactHashSource: HashNone}}
	if runtimeName != "" {
		out.Model.RuntimeModelName = strPtr(runtimeName)
	}
	entry, ok := reg.Get(id)
	switch {
	case !ok && provider == "fake" && id == fakeModelID:
		// no registry facts
	case !ok:
		var known []string
		for _, e := range reg.Entries() {
			known = append(known, e.ID)
		}
		sort.Strings(known)
		return resolved{}, fmt.Errorf("model %q is not in the registry (known: %s)", id, strings.Join(known, ", "))
	default:
		out.Entry = &entry
		out.Model.InRegistry = true
		out.Model.Name = strPtr(entry.Name)
		out.Model.Status = strPtr(string(entry.Status))
		out.Model.Quantization = strPtr(entry.Quantization.Scheme)
		if entry.Artifact.SHA256 != nil {
			out.Model.ArtifactSHA256 = strPtr(*entry.Artifact.SHA256)
			out.Model.ArtifactHashSource = HashRegistry
			out.Model.ArtifactSizeBytes = entry.Artifact.SizeBytes
		}
	}
	return out, nil
}

// backendFor describes the runtime that serves the model. For the fake it is
// the fake; for LocalAI it is what the registry entry names, with the version
// the running runtime reported.
func backendFor(provider string, entry *models.Entry, runtimeVersion string) BackendInfo {
	var b BackendInfo
	switch {
	case provider == "fake":
		b.Runtime, b.Name = strPtr("fake"), strPtr("fake")
	case entry != nil:
		b.Runtime, b.Name = strPtr(entry.Backend.Runtime), strPtr(entry.Backend.Name)
	default:
		b.Runtime = strPtr(provider)
	}
	if runtimeVersion != "" {
		b.RuntimeVersion = strPtr(runtimeVersion)
	}
	return b
}
