package packs_test

import (
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/voice/packs"
	"github.com/buxee/alix-ai/models/registry"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// TestPackManifestValid: the manifests that ship in the binary (en and it) pass
// the schema and the pack rules, name the three voice roles with pinned files
// and recorded licences, and a manifest that breaks a rule is refused. An
// STT-only pack is valid.
func TestPackManifestValid(t *testing.T) {
	shipped, err := packs.Shipped()
	if err != nil {
		t.Fatalf("shipped manifests: %v", err)
	}
	all := shipped.All()
	if len(all) < 2 {
		t.Fatalf("%d shipped manifests, want at least en and it", len(all))
	}
	for _, id := range []string{"en", "it"} {
		m, ok := shipped.Lookup(id)
		if !ok {
			t.Fatalf("no shipped pack %q", id)
		}
		want := []providers.Role{providers.RoleSTT, providers.RoleTTS, providers.RoleVAD}
		if got := m.Roles(); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Errorf("%s provides %v, want %v", id, got, want)
		}
		if m.Language != id {
			t.Errorf("%s serves language %q", id, m.Language)
		}
		for _, rf := range m.Files() {
			f := rf.File
			if len(f.SHA256) != 64 || f.SizeBytes <= 0 || f.Revision == "" || !strings.HasPrefix(f.URL, "https://") {
				t.Errorf("%s %s: not fully pinned: %+v", id, f.Name, f)
			}
		}
		for _, mod := range []*packs.Model{m.Models.STT, m.Models.TTS, m.Models.VAD} {
			if mod.Licence.SPDX == "" || mod.Licence.URL == "" || mod.Licence.RedistributionNotes == "" {
				t.Errorf("%s %s: licence is not recorded", id, mod.Name)
			}
		}
		if m.Digest == "" {
			t.Errorf("%s has no digest", id)
		}
	}
	if err := shipped.CheckRegistry(mustRegistry(t)); err != nil {
		t.Errorf("shipped manifests disagree with the registry: %v", err)
	}

	valid := newTestPack("it-fast", "it-IT", "stt", "tts", "vad")
	if _, err := packs.ParseManifest("it-fast.yaml", []byte(valid.yaml)); err != nil {
		t.Fatalf("a hyphenated pack id was refused: %v", err)
	}
	sttOnly := newTestPack("en-stt", "en", "stt")
	m, err := packs.ParseManifest("en-stt.yaml", []byte(sttOnly.yaml))
	if err != nil {
		t.Fatalf("an STT-only pack was refused: %v", err)
	}
	if got := m.Pack(); len(got.Roles) != 1 || got.Roles[0] != providers.RoleSTT {
		t.Errorf("STT-only pack roles = %v", got.Roles)
	}

	sttLine := "        sha256: "
	for _, tt := range []struct {
		name string
		file string
		edit func(string) string
		want string
	}{
		{"missing sha256", "it-fast.yaml", func(s string) string { return dropLineWith(s, sttLine) }, "sha256"},
		{"short sha256", "it-fast.yaml", func(s string) string { return strings.Replace(s, sttLine, sttLine+"abc", 1) }, "sha256"},
		{"unknown key", "it-fast.yaml", func(s string) string {
			return strings.Replace(s, "status: candidate", "status: candidate\nsurprise: 1", 1)
		}, "surprise"},
		{"unknown role", "it-fast.yaml", func(s string) string { return strings.Replace(s, "  vad:\n", "  asr:\n", 1) }, "asr"},
		{"uppercase id", "It.yaml", func(s string) string { return strings.Replace(s, "id: it-fast", "id: It", 1) }, "id"},
		{"id is not the file name", "it.yaml", func(s string) string { return s }, "must match the file name"},
		{"no models", "it-fast.yaml", func(s string) string { return cutModels(s) }, "models"},
		{"path in a file name", "it-fast.yaml", func(s string) string {
			return strings.Replace(s, "name: stt-it-fast.bin", "name: ../stt-it-fast.bin", 1)
		}, "name"},
		{"plain http url", "it-fast.yaml", func(s string) string {
			return strings.Replace(s, "url: https://example.test/stt", "url: http://example.test/stt", 1)
		}, "url"},
		{"disk bytes disagree", "it-fast.yaml", func(s string) string { return strings.Replace(s, "disk_bytes: ", "disk_bytes: 9", 1) }, "disk_bytes"},
		{"missing licence", "it-fast.yaml", func(s string) string { return dropLineWith(s, "redistribution_notes") }, "redistribution_notes"},
		{"missing quality", "it-fast.yaml", func(s string) string { return strings.Replace(s, "quality:", "qualityx:", 1) }, "quality"},
		{"duplicate key", "it-fast.yaml", func(s string) string {
			return strings.Replace(s, "language: it-IT", "language: it-IT\nlanguage: it", 1)
		}, "defined twice"},
		{"anchor", "it-fast.yaml", func(s string) string { return strings.Replace(s, "version: \"1\"", "version: &v \"1\"", 1) }, "anchors"},
		{"bad language", "it-fast.yaml", func(s string) string { return strings.Replace(s, "language: it-IT", "language: \"1\"", 1) }, "language"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := packs.ParseManifest(tt.file, []byte(tt.edit(valid.yaml)))
			wantCode(t, err, contracts.ErrSchemaInvalid)
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}

	t.Run("a bad manifest rejects the whole set", func(t *testing.T) {
		good := newTestPack("en", "en", "stt")
		bad := newTestPack("it", "it", "stt")
		bad.yaml = strings.Replace(bad.yaml, "status: candidate", "status: nonsense", 1)
		fsys := mapFS(good, bad)
		_, err := packs.LoadManifests(fsys)
		wantCode(t, err, contracts.ErrSchemaInvalid)
	})
}

func TestManifestRegistryCrossCheck(t *testing.T) {
	reg := mustRegistry(t)
	entry, ok := reg.Get("qwen3.5-0.8b-q4_k_m")
	if !ok || entry.Artifact.SHA256 == nil {
		t.Fatal("test needs a pinned registry entry")
	}
	base := newTestPack("en", "en", "stt")
	withRef := func(id, sha string) packs.Manifest {
		y := strings.Replace(base.yaml, "registry_id: null", "registry_id: "+id, 1)
		if sha != "" {
			y = replaceSHA(y, sha)
		}
		m, err := packs.ParseManifest("en.yaml", []byte(y))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if err := withRef("qwen3.5-0.8b-q4_k_m", *entry.Artifact.SHA256).CheckRegistry(reg); err != nil {
		t.Errorf("a manifest whose file is the registry's pinned file was refused: %v", err)
	}
	wantCode(t, withRef("qwen3.5-0.8b-q4_k_m", "").CheckRegistry(reg), contracts.ErrSchemaInvalid)
	wantCode(t, withRef("no-such-model", "").CheckRegistry(reg), contracts.ErrSchemaInvalid)
	wantCode(t, withRef("no-such-model", "").CheckRegistry(nil), contracts.ErrSchemaInvalid)
}

func mustRegistry(t testing.TB) *models.Registry {
	t.Helper()
	reg, err := models.LoadFS(registry.FS)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	return reg
}

func dropLineWith(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if !strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// cutModels removes every role from the models mapping.
func cutModels(s string) string {
	start := strings.Index(s, "models:\n")
	end := strings.Index(s, "quality:")
	return s[:start] + "models: {}\n" + s[end:]
}

func replaceSHA(y, sha string) string {
	lines := strings.Split(y, "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "sha256:") {
			lines[i] = "        sha256: " + sha
		}
	}
	return strings.Join(lines, "\n")
}
