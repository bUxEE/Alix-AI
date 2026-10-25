package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/models/registry"
)

// The registry changes under these tests (M0-13 pins hashes, measures and
// promotes or rejects candidates), so nothing here names a model or states
// what an entry says: every expectation is read from the registry itself.

func registryEntries(t testing.TB) []models.Entry {
	t.Helper()
	reg, err := models.LoadFS(registry.FS)
	if err != nil {
		t.Fatalf("model registry: %v", err)
	}
	entries := reg.Entries()
	if len(entries) == 0 {
		t.Fatal("the model registry is empty")
	}
	return entries
}

// anyEntry returns the first registry entry (by id).
func anyEntry(t testing.TB) models.Entry { return registryEntries(t)[0] }

// pinnedEntry returns an entry that pins a sha256, or false when none does.
func pinnedEntry(t testing.TB) (models.Entry, bool) {
	for _, e := range registryEntries(t) {
		if e.Artifact.SHA256 != nil {
			return e, true
		}
	}
	return models.Entry{}, false
}

// registryWithPin returns the shipped registry with entry id pinned to the given
// hash and size, so the match path can be tested without a model file.
func registryWithPin(t testing.TB, id, sum string, size int64) *models.Registry {
	t.Helper()
	files := fstest.MapFS{}
	des, err := fs.ReadDir(registry.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, de := range des {
		b, err := fs.ReadFile(registry.FS, de.Name())
		if err != nil {
			t.Fatal(err)
		}
		if de.Name() == id+".yaml" {
			lines := strings.Split(string(b), "\n")
			for i, l := range lines {
				switch {
				case strings.HasPrefix(l, "  sha256:"):
					lines[i] = "  sha256: " + sum
				case strings.HasPrefix(l, "  size_bytes:"):
					lines[i] = fmt.Sprintf("  size_bytes: %d", size)
				}
			}
			b = []byte(strings.Join(lines, "\n"))
		}
		files[de.Name()] = &fstest.MapFile{Data: b}
	}
	reg, err := models.LoadFS(files)
	if err != nil {
		t.Fatalf("the modified registry does not load: %v", err)
	}
	return reg
}

func TestResolveModel(t *testing.T) {
	ctx := context.Background()

	t.Run("a registry id carries its facts into the result", func(t *testing.T) {
		e := anyEntry(t)
		m, err := ResolveModel(ctx, "localai", e.ID, "", "local-name")
		if err != nil {
			t.Fatal(err)
		}
		got := m.Model
		if got.RegistryID != e.ID || !got.InRegistry || got.Name == nil || *got.Name != e.Name ||
			got.Status == nil || *got.Status != string(e.Status) || got.Quantization == nil || *got.Quantization != e.Quantization.Scheme {
			t.Errorf("model = %+v, entry = %s %s %s", got, e.Name, e.Status, e.Quantization.Scheme)
		}
		if got.RuntimeModelName == nil || *got.RuntimeModelName != "local-name" {
			t.Errorf("runtime model name = %v", got.RuntimeModelName)
		}
		if e.Artifact.SHA256 != nil {
			if got.ArtifactHashSource != HashRegistry || got.ArtifactSHA256 == nil || *got.ArtifactSHA256 != *e.Artifact.SHA256 {
				t.Errorf("a pinned hash must be carried from the registry: %+v", got)
			}
		} else if got.ArtifactHashSource != HashNone || got.ArtifactSHA256 != nil {
			t.Errorf("an unpinned entry records no hash: %+v", got)
		}
		if (m.NativeMax == nil) != (e.Context.NativeMaxTokens == nil) || (m.NativeMax != nil && *m.NativeMax != *e.Context.NativeMaxTokens) {
			t.Errorf("native max = %v, entry says %v", m.NativeMax, e.Context.NativeMaxTokens)
		}
		if (m.ContextBudget == nil) != (e.Context.BudgetTokens == nil) {
			t.Errorf("context budget = %v, entry says %v", m.ContextBudget, e.Context.BudgetTokens)
		}
		if m.Entry == nil || m.Entry.ID != e.ID {
			t.Error("the entry must be returned")
		}
	})

	t.Run("an unknown id is refused for LocalAI and lists what is known", func(t *testing.T) {
		_, err := ResolveModel(ctx, "localai", "my-own-model", "", "")
		if err == nil || !strings.Contains(err.Error(), "not in the registry") {
			t.Fatalf("err = %v", err)
		}
		for _, e := range registryEntries(t) {
			if !strings.Contains(err.Error(), e.ID) {
				t.Errorf("the error does not list %s: %v", e.ID, err)
			}
		}
	})

	t.Run("only the fake provider may use the id fake", func(t *testing.T) {
		m, err := ResolveModel(ctx, "fake", "fake", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if m.Model.InRegistry || m.Entry != nil || m.Model.ArtifactSHA256 != nil || m.Model.ArtifactHashSource != HashNone || m.Model.Name != nil {
			t.Errorf("model = %+v", m.Model)
		}
		if _, err := ResolveModel(ctx, "localai", "fake", "", ""); err == nil {
			t.Error("LocalAI must not accept the placeholder model")
		}
		if _, err := ResolveModel(ctx, "fake", "fake-two", "", ""); err == nil {
			t.Error("the fake provider accepts the id fake and registry ids, nothing else")
		}
		if _, err := ResolveModel(ctx, "fake", anyEntry(t).ID, "", ""); err != nil {
			t.Errorf("the fake provider may borrow a registry entry: %v", err)
		}
	})

	t.Run("an artefact is hashed with models.HashFile", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "model.gguf")
		content := []byte("not really a model")
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(content)
		m, err := ResolveModel(ctx, "fake", "fake", path, "")
		if err != nil {
			t.Fatal(err)
		}
		if m.Model.ArtifactHashSource != HashComputed || m.Model.ArtifactSHA256 == nil || *m.Model.ArtifactSHA256 != hex.EncodeToString(sum[:]) ||
			m.Model.ArtifactSizeBytes == nil || *m.Model.ArtifactSizeBytes != int64(len(content)) {
			t.Errorf("model = %+v", m.Model)
		}
	})

	t.Run("a file that is not the pinned artefact is refused", func(t *testing.T) {
		e, ok := pinnedEntry(t)
		if !ok {
			t.Skip("no registry entry pins a sha256 at the moment")
		}
		path := filepath.Join(t.TempDir(), "other.gguf")
		if err := os.WriteFile(path, []byte("a different file"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := ResolveModel(ctx, "localai", e.ID, path, "")
		if err == nil || !strings.Contains(err.Error(), "does not match the registry") || !strings.Contains(err.Error(), "refusing to benchmark") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("a file with the pinned hash is accepted and recorded as computed", func(t *testing.T) {
		content := []byte("pretend this is a GGUF file")
		sum := sha256.Sum256(content)
		hexSum := hex.EncodeToString(sum[:])
		e := anyEntry(t)
		reg := registryWithPin(t, e.ID, hexSum, int64(len(content)))
		path := filepath.Join(t.TempDir(), "m.gguf")
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		m, err := resolveModel(ctx, reg, "localai", e.ID, path, "")
		if err != nil {
			t.Fatal(err)
		}
		if m.Model.ArtifactHashSource != HashComputed || m.Model.ArtifactSHA256 == nil || *m.Model.ArtifactSHA256 != hexSum ||
			m.Model.ArtifactSizeBytes == nil || *m.Model.ArtifactSizeBytes != int64(len(content)) {
			t.Errorf("model = %+v", m.Model)
		}
		// The same entry refuses a different file.
		other := filepath.Join(t.TempDir(), "other.gguf")
		if err := os.WriteFile(other, []byte("something else"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveModel(ctx, reg, "localai", e.ID, other, ""); err == nil || !strings.Contains(err.Error(), "refusing to benchmark") {
			t.Errorf("a file other than the pinned artefact must be refused, got %v", err)
		}
	})

	t.Run("a missing or non-regular artefact is an error", func(t *testing.T) {
		if _, err := ResolveModel(ctx, "fake", "fake", filepath.Join(t.TempDir(), "nope.gguf"), ""); err == nil {
			t.Error("a missing file must be an error")
		}
		if _, err := ResolveModel(ctx, "fake", "fake", t.TempDir(), ""); err == nil {
			t.Error("a directory must be an error")
		}
	})
}

func TestBackendFor(t *testing.T) {
	e := anyEntry(t)
	m, err := ResolveModel(context.Background(), "localai", e.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	b := backendFor("localai", m.Entry, "v4.10.0")
	if b.Runtime == nil || *b.Runtime != e.Backend.Runtime || b.Name == nil || *b.Name != e.Backend.Name || b.RuntimeVersion == nil || *b.RuntimeVersion != "v4.10.0" {
		t.Errorf("backend = %+v, entry says %s/%s", b, e.Backend.Runtime, e.Backend.Name)
	}
	f := backendFor("fake", nil, "")
	if *f.Runtime != "fake" || *f.Name != "fake" || f.RuntimeVersion != nil {
		t.Errorf("fake backend = %+v", f)
	}
	if b := backendFor("fake", m.Entry, ""); *b.Runtime != "fake" {
		t.Error("a fake run is served by the fake even when it borrows a registry entry")
	}
	if b := backendFor("localai", nil, ""); b.Runtime == nil || *b.Runtime != "localai" || b.Name != nil {
		t.Errorf("a runtime with no registry entry is named by its provider only: %+v", b)
	}
}
