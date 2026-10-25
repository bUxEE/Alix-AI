package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/models/registry"
)

// fakeModelID is the one id accepted without a registry entry, and only with
// the fake provider: it names no model.
const fakeModelID = "fake"

// resolved is what the registry and the artefact say about the model under
// test.
type resolved struct {
	Model ModelInfo
	// Entry is the registry entry, nil for the fake model.
	Entry *models.Entry
	// ContextBudget and NativeMax come from the entry; nil when unknown.
	ContextBudget *int
	NativeMax     *int
}

// ResolveModel looks the model up in the embedded registry and, when
// artifactPath is given, hashes the file with models.HashFile. The id must be
// in the registry; the one exception is the fake provider's "fake" model. A
// computed hash that differs from the registry's pinned sha256 is an error:
// numbers measured on a file other than the one the registry names are not
// evidence about that entry (SEC-063: the pinned hash is the authority).
func ResolveModel(ctx context.Context, provider, id, artifactPath, runtimeName string) (resolved, error) {
	reg, err := models.LoadFS(registry.FS)
	if err != nil {
		return resolved{}, fmt.Errorf("model registry: %w", err)
	}
	return resolveModel(ctx, reg, provider, id, artifactPath, runtimeName)
}

// resolveModel is ResolveModel over a given registry.
func resolveModel(ctx context.Context, reg *models.Registry, provider, id, artifactPath, runtimeName string) (resolved, error) {
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
		out.ContextBudget = entry.Context.BudgetTokens
		out.NativeMax = entry.Context.NativeMaxTokens
		if entry.Artifact.SHA256 != nil {
			out.Model.ArtifactSHA256 = strPtr(*entry.Artifact.SHA256)
			out.Model.ArtifactHashSource = HashRegistry
			out.Model.ArtifactSizeBytes = entry.Artifact.SizeBytes
		}
	}

	if artifactPath != "" {
		sum, size, err := models.HashFile(ctx, artifactPath)
		if err != nil {
			return resolved{}, fmt.Errorf("hashing --artifact: %w", err)
		}
		if out.Entry != nil && out.Entry.Artifact.SHA256 != nil && *out.Entry.Artifact.SHA256 != sum {
			return resolved{}, fmt.Errorf(
				"the file's SHA-256 %s does not match the registry's pinned %s for %s: refusing to benchmark a different artefact",
				sum, *out.Entry.Artifact.SHA256, id)
		}
		out.Model.ArtifactSHA256 = strPtr(sum)
		out.Model.ArtifactHashSource = HashComputed
		out.Model.ArtifactSizeBytes = &size
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
