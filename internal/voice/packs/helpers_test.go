package packs_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/voice/packs"
	"github.com/buxee/alix-ai/pkg/contracts"
)

var testNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// testPack is a small pack built in memory: a manifest and the files it pins.
type testPack struct {
	id    string
	yaml  string
	files map[string][]byte // by manifest file name
	names []string          // in manifest order
}

var testRoleEngine = map[string]string{"stt": "whisper.cpp", "tts": "piper", "vad": "silero-vad"}

// newTestPack builds the manifest and the files of a pack providing roles. The
// bytes of each file are small and different per pack and role, so a file from
// one pack is never the pinned file of another.
func newTestPack(id, lang string, roles ...string) testPack {
	tp := testPack{id: id, files: map[string][]byte{}}
	var total int
	var models strings.Builder
	for _, role := range roles {
		name := fmt.Sprintf("%s-%s.bin", role, id)
		data := []byte(fmt.Sprintf("%s model bytes of pack %s", role, id))
		sum := sha256.Sum256(data)
		tp.files[name] = data
		tp.names = append(tp.names, name)
		total += len(data)
		fmt.Fprintf(&models, `  %s:
    name: "%s model"
    engine: %s
    registry_id: null
    files:
      - name: %s
        url: https://example.test/%s
        revision: 0123456789abcdef0123456789abcdef01234567
        size_bytes: %d
        sha256: %s
    licence:
      spdx: MIT
      url: https://example.test/licence
      redistribution_notes: "test licence"
`, role, role, testRoleEngine[role], name, name, len(data), hex.EncodeToString(sum[:]))
	}
	tp.yaml = fmt.Sprintf(`id: %s
name: "Test pack %s"
version: "1"
language: %s
status: candidate
models:
%squality:
  stt_wer: null
  tts_time_to_first_audio_ms: null
  tts_real_time_factor: null
  measurement: null
resources:
  disk_bytes: %d
  steady_rss_mb: null
  peak_rss_mb: null
  cpu_cores: null
notes: []
`, id, id, lang, models.String(), total)
	return tp
}

func mapFS(tps ...testPack) fstest.MapFS {
	fsys := fstest.MapFS{}
	for _, tp := range tps {
		fsys[tp.id+".yaml"] = &fstest.MapFile{Data: []byte(tp.yaml)}
	}
	return fsys
}

// manifestSet loads the manifests of packs as the approved set.
func manifestSet(t testing.TB, tps ...testPack) packs.Manifests {
	t.Helper()
	set, err := packs.LoadManifests(mapFS(tps...))
	if err != nil {
		t.Fatalf("test manifests: %v", err)
	}
	return set
}

// newTestStore returns a store over an empty temporary directory.
func newTestStore(t testing.TB, tps ...testPack) (*packs.Store, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "voice-packs")
	st, err := packs.NewStore(root, manifestSet(t, tps...), clock.NewFake(testNow))
	if err != nil {
		t.Fatal(err)
	}
	return st, root
}

// fetched writes the pack's files into a new directory, as an operator who
// downloaded them would have, and returns it.
func fetched(t testing.TB, tp testPack) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range tp.files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func approve(id string) packs.Approval { return packs.Approval{PackID: id} }

// mustInstall installs tp with approval and fails the test otherwise.
func mustInstall(t testing.TB, st *packs.Store, tp testPack) {
	t.Helper()
	_, err := st.Install(t.Context(), packs.InstallRequest{PackID: tp.id, From: fetched(t, tp), Approval: approve(tp.id)})
	if err != nil {
		t.Fatalf("install %s: %v", tp.id, err)
	}
}

func wantCode(t testing.TB, err error, code contracts.ErrCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("got no error, want code %s", code)
	}
	if !contracts.HasCode(err, code) {
		t.Fatalf("got %v, want code %s", err, code)
	}
}

// entries lists the names directly inside dir (nil when it does not exist).
func entries(t testing.TB, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, de := range des {
		out = append(out, de.Name())
	}
	return out
}
