package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	vfake "github.com/buxee/alix-ai/internal/providers/voice/fake"
	"github.com/buxee/alix-ai/internal/voice/packs"
	"github.com/buxee/alix-ai/internal/voice/profile"
)

// voiceTestPackBytes is the one file of a voiceTest pack: small and different per pack.
func voiceTestPackBytes(id string) []byte { return []byte("stt model bytes of pack " + id) }

// voiceTestManifest is an STT-only manifest for pack id.
func voiceTestManifest(id string) []byte {
	data := voiceTestPackBytes(id)
	sum := sha256.Sum256(data)
	return []byte(fmt.Sprintf(`id: %[1]s
name: "Test pack %[1]s"
version: "1"
language: %[1]s
status: candidate
models:
  stt:
    name: "stt model"
    engine: whisper.cpp
    registry_id: null
    files:
      - name: stt-%[1]s.bin
        url: https://example.test/stt-%[1]s.bin
        revision: 0123456789abcdef0123456789abcdef01234567
        size_bytes: %[2]d
        sha256: %[3]s
    licence:
      spdx: MIT
      url: https://example.test/licence
      redistribution_notes: "test licence"
  vad:
    name: "vad model"
    engine: silero-vad
    registry_id: null
    files:
      - name: vad-%[1]s.bin
        url: https://example.test/vad-%[1]s.bin
        revision: 0123456789abcdef0123456789abcdef01234567
        size_bytes: %[2]d
        sha256: %[3]s
    licence:
      spdx: MIT
      url: https://example.test/licence
      redistribution_notes: "test licence"
quality:
  stt_wer: null
  tts_time_to_first_audio_ms: null
  tts_real_time_factor: null
  measurement: null
resources:
  disk_bytes: %[4]d
  steady_rss_mb: null
  peak_rss_mb: null
  cpu_cores: null
notes: []
`, id, len(data), hex.EncodeToString(sum[:]), 2*len(data)))
}

// voiceUseTestManifests makes the voice commands approve the test packs "en" and
// "it" instead of the manifests embedded in the binary.
func voiceUseTestManifests(t *testing.T) {
	t.Helper()
	old := voiceShippedManifests
	t.Cleanup(func() { voiceShippedManifests = old })
	voiceShippedManifests = func() (packs.Manifests, error) {
		return packs.LoadManifests(fstest.MapFS{
			"en.yaml": {Data: voiceTestManifest("en")},
			"it.yaml": {Data: voiceTestManifest("it")},
		})
	}
}

// voiceFetched writes the files of pack id into a new directory and returns it.
func voiceFetched(t *testing.T, id string) string {
	t.Helper()
	dir := t.TempDir()
	data := voiceTestPackBytes(id)
	for _, name := range []string{"stt-" + id + ".bin", "vad-" + id + ".bin"} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

var voiceNoEnv = func(string) (string, bool) { return "", false }

// voiceRun runs a command on the registered dispatcher against the production
// configuration registry, which is the one the voice section is registered on.
func voiceRun(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Default.Run(context.Background(), args, &Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, LookupEnv: voiceNoEnv, Config: config.Default})
	return code, out.String(), errb.String()
}

// voiceResetConfig puts the production registry back to its defaults when the
// test ends, and now.
func voiceResetConfig(t *testing.T) {
	t.Helper()
	reset := func() {
		if err := config.Default.LoadFrom("", voiceNoEnv); err != nil {
			t.Fatal(err)
		}
	}
	reset()
	t.Cleanup(reset)
}

func voiceDecode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
	return m
}

func voiceListPacks(t *testing.T, dataDir string) (active string, packsByID map[string]map[string]any) {
	t.Helper()
	code, out, errb := voiceRun(t, "voice", "list", "-data-dir", dataDir)
	if code != ExitOK {
		t.Fatalf("voice list = %d: %s", code, errb)
	}
	m := voiceDecode(t, out)
	packsByID = map[string]map[string]any{}
	for _, p := range m["packs"].([]any) {
		pm := p.(map[string]any)
		packsByID[pm["id"].(string)] = pm
	}
	return m["active"].(string), packsByID
}

// The commands install a pack only on the operator's explicit approval, verify
// the hashes, activate one pack at a time and refuse a second.
func TestVoiceInstallActivateDeactivate(t *testing.T) {
	voiceUseTestManifests(t)
	voiceResetConfig(t)
	dataDir := t.TempDir()
	store := filepath.Join(dataDir, "voice-packs")

	active, listed := voiceListPacks(t, dataDir)
	if active != "" || len(listed) != 2 || listed["en"]["installed"] != false || listed["en"]["approved"] != true {
		t.Fatalf("fresh list: active %q, %v", active, listed)
	}

	// No approval: the plan is printed, the exit is 1 and nothing is written.
	code, out, errb := voiceRun(t, "voice", "install", "-data-dir", dataDir, "en")
	if code != ExitError || !strings.Contains(errb, "approval") {
		t.Fatalf("install without approval = %d, stderr %q", code, errb)
	}
	plan := voiceDecode(t, out)
	if plan["installed"] != false || plan["plan"].(map[string]any)["id"] != "en" || !strings.Contains(out, "example.test/stt-en.bin") || !strings.Contains(out, "test licence") {
		t.Errorf("the refusal does not show the plan:\n%s", out)
	}
	if _, err := os.Stat(store); !os.IsNotExist(err) {
		t.Errorf("a refused install touched the disk: %v", err)
	}
	// Approval needs the files; approval of another pack is not approval.
	if code, _, _ := voiceRun(t, "voice", "install", "-data-dir", dataDir, "-approve", "en", "en"); code != ExitUsage {
		t.Errorf("-approve without -from = %d, want usage", code)
	}
	if code, _, _ := voiceRun(t, "voice", "install", "-data-dir", dataDir, "-from", voiceFetched(t, "en"), "-approve", "it", "en"); code != ExitError {
		t.Errorf("approval of another pack = %d, want 1", code)
	}

	// A file that is not the pinned one installs nothing.
	bad := voiceFetched(t, "en")
	if err := os.WriteFile(filepath.Join(bad, "stt-en.bin"), []byte("stt model bytes of pack XX"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errb := voiceRun(t, "voice", "install", "-data-dir", dataDir, "-from", bad, "-approve", "en", "en"); code != ExitError || !strings.Contains(errb, "sha256") {
		t.Errorf("tampered install = %d, stderr %q", code, errb)
	}
	if _, listed := voiceListPacks(t, dataDir); listed["en"]["installed"] != false {
		t.Error("a pack whose hash did not match is listed as installed")
	}

	for _, id := range []string{"en", "it"} {
		code, out, errb := voiceRun(t, "voice", "install", "-data-dir", dataDir, "-from", voiceFetched(t, id), "-approve", id, id)
		if code != ExitOK {
			t.Fatalf("install %s = %d: %s", id, code, errb)
		}
		if m := voiceDecode(t, out); m["installed"] != true || m["active"] != false {
			t.Errorf("install %s: %s", id, out)
		}
	}
	if active, listed := voiceListPacks(t, dataDir); active != "" || listed["en"]["installed"] != true || listed["en"]["active"] != false {
		t.Fatalf("installing activated something: active %q, %v", active, listed)
	}

	if code, out, errb := voiceRun(t, "voice", "activate", "-data-dir", dataDir, "en"); code != ExitOK || !strings.Contains(out, "next start") {
		t.Fatalf("activate en = %d: %s %s", code, out, errb)
	}
	code, _, errb = voiceRun(t, "voice", "activate", "-data-dir", dataDir, "it")
	if code != ExitError || !strings.Contains(errb, "deactivate") {
		t.Errorf("second activation = %d, stderr %q: want it refused", code, errb)
	}
	if active, listed := voiceListPacks(t, dataDir); active != "en" || listed["en"]["active"] != true || listed["it"]["active"] != false {
		t.Errorf("after the refused activation: active %q, %v", active, listed)
	}
	if code, _, errb := voiceRun(t, "voice", "deactivate", "-data-dir", dataDir); code != ExitOK {
		t.Fatalf("deactivate = %d: %s", code, errb)
	}
	if code, _, errb := voiceRun(t, "voice", "activate", "-data-dir", dataDir, "it"); code != ExitOK {
		t.Fatalf("activate it after deactivation = %d: %s", code, errb)
	}

	// A file damaged after install keeps the pack from activating.
	if code, _, _ := voiceRun(t, "voice", "deactivate", "-data-dir", dataDir); code != ExitOK {
		t.Fatal("deactivate failed")
	}
	if err := os.WriteFile(filepath.Join(store, "en", "stt-en.bin"), []byte("stt model bytes of pack XX"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errb := voiceRun(t, "voice", "activate", "-data-dir", dataDir, "en"); code != ExitError || !strings.Contains(errb, "sha256") {
		t.Errorf("activation of a damaged pack = %d, stderr %q", code, errb)
	}
	if code, _, _ := voiceRun(t, "voice", "activate", "-data-dir", dataDir); code != ExitUsage {
		t.Errorf("activate with no pack = %d, want usage", code)
	}
}

// The `voice:` section is accepted by the configuration once this package is
// linked (profile registers it), and an unknown key in it is still refused.
func TestVoiceConfigSectionIsAccepted(t *testing.T) {
	voiceResetConfig(t)
	path := writeConfig(t, "voice:\n  enabled: false\n  active_pack: \"\"\n")
	if code, _, errb := voiceRun(t, "config", "check", "-config", path); code != ExitOK {
		t.Fatalf("config check with a voice section = %d: %s", code, errb)
	}
	path = writeConfig(t, "voice:\n  enabled: false\n  surprise: 1\n")
	if code, _, _ := voiceRun(t, "config", "check", "-config", path); code != ExitError {
		t.Errorf("a voice section with an unknown key = %d, want 1", code)
	}
	path = writeConfig(t, "voice:\n  active_pack: Bad_Pack\n")
	if code, _, _ := voiceRun(t, "config", "check", "-config", path); code != ExitError {
		t.Errorf("a malformed active_pack = %d, want 1", code)
	}
}

func voiceEnableConfig(t *testing.T, activePack string) {
	t.Helper()
	y := "voice:\n  enabled: true\n"
	if activePack != "" {
		y += "  active_pack: " + activePack + "\n"
	}
	if err := config.Default.LoadFrom(writeConfig(t, y), voiceNoEnv); err != nil {
		t.Fatal(err)
	}
}

type voiceLogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *voiceLogBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *voiceLogBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type voiceServeRig struct {
	deps   serveDeps
	log    *voiceLogBuf
	reg    *providers.Registry
	stops  []func()
	dataDr string
}

func newVoiceServeRig(t *testing.T) *voiceServeRig {
	t.Helper()
	r := &voiceServeRig{log: &voiceLogBuf{}, reg: providers.NewRegistry(clock.Real{}), dataDr: filepath.Join(t.TempDir(), "data")}
	r.deps = serveDeps{
		Log:       slog.New(slog.NewTextHandler(r.log, nil)),
		DataDir:   r.dataDr,
		Providers: r.reg,
		AddService: func(start func(ctx context.Context) (stop func())) {
			r.stops = append(r.stops, start(context.Background()))
		},
	}
	return r
}

func (r *voiceServeRig) roles() []providers.Role {
	var out []providers.Role
	for _, role := range voice.Roles() {
		if r.reg.Has(role) {
			out = append(out, role)
		}
	}
	return out
}

// installVoicePack installs pack id into the rig's store and, when activate is
// set, activates it, the way the commands do.
func (r *voiceServeRig) installVoicePack(t *testing.T, id string, activate bool) {
	t.Helper()
	st, err := voiceOpenStore(filepath.Join(r.dataDr, voicePackDirName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Install(context.Background(), packs.InstallRequest{PackID: id, From: voiceFetched(t, id), Approval: packs.Approval{PackID: id}}); err != nil {
		t.Fatal(err)
	}
	if activate {
		if _, err := st.Activate(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
}

func voiceUseFactory(t *testing.T, f profile.ProviderFactory) {
	t.Helper()
	old := voiceProviderFactory
	t.Cleanup(func() { voiceProviderFactory = old })
	voiceProviderFactory = f
}

// With voice disabled (the default) the provider does nothing at all: no
// directory is made or read, no factory is called, no role is registered.
func TestVoiceServeProviderDisabledDoesNothing(t *testing.T) {
	voiceUseTestManifests(t)
	voiceResetConfig(t)
	var factoryCalls int
	voiceUseFactory(t, func(context.Context) (profile.Providers, error) { factoryCalls++; return profile.Providers{}, nil })
	r := newVoiceServeRig(t)
	if err := voiceServeProvide(context.Background(), r.deps); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.dataDr); !os.IsNotExist(err) {
		t.Errorf("a disabled voice profile touched the data directory: %v", err)
	}
	if factoryCalls != 0 || len(r.roles()) != 0 || len(r.stops) != 0 {
		t.Errorf("a disabled voice profile built something: factory %d, roles %v, services %d", factoryCalls, r.roles(), len(r.stops))
	}
	if !strings.Contains(strings.Join(serveProviderNames(), ","), "voice") {
		t.Errorf("serve providers %v do not include voice", serveProviderNames())
	}
}

func serveProviderNames() []string {
	names, _ := registeredServeProviders()
	return names
}

// Voice that is wanted and cannot come up never stops serve: it is logged and
// the deployment runs text-only. And when it can, it is loaded with the pack the
// operator activated.
func TestVoiceServeProviderLoadsOrRunsTextOnly(t *testing.T) {
	voiceUseTestManifests(t)

	t.Run("no pack installed", func(t *testing.T) {
		voiceResetConfig(t)
		voiceEnableConfig(t, "")
		voiceUseFactory(t, func(context.Context) (profile.Providers, error) {
			t.Error("the factory ran with no pack installed")
			return profile.Providers{}, nil
		})
		r := newVoiceServeRig(t)
		if err := voiceServeProvide(context.Background(), r.deps); err != nil || len(r.roles()) != 0 {
			t.Fatalf("err %v, roles %v", err, r.roles())
		}
		if !strings.Contains(r.log.String(), "not_installed") {
			t.Errorf("log does not say why voice is absent: %s", r.log.String())
		}
	})
	t.Run("no runtime adapter linked", func(t *testing.T) {
		voiceResetConfig(t)
		voiceEnableConfig(t, "")
		r := newVoiceServeRig(t)
		r.installVoicePack(t, "en", true)
		if err := voiceServeProvide(context.Background(), r.deps); err != nil {
			t.Fatalf("a voice profile that cannot load stopped serve: %v", err)
		}
		if len(r.roles()) != 0 || !strings.Contains(r.log.String(), "running text-only") || !strings.Contains(r.log.String(), "no voice runtime adapter") {
			t.Errorf("roles %v, log: %s", r.roles(), r.log.String())
		}
	})
	t.Run("the configuration and the recorded activation disagree", func(t *testing.T) {
		voiceResetConfig(t)
		voiceEnableConfig(t, "it")
		voiceUseFactory(t, func(context.Context) (profile.Providers, error) {
			t.Error("the factory ran although the activation is ambiguous")
			return profile.Providers{}, nil
		})
		r := newVoiceServeRig(t)
		r.installVoicePack(t, "en", true)
		if err := voiceServeProvide(context.Background(), r.deps); err != nil || len(r.roles()) != 0 {
			t.Fatalf("err %v, roles %v", err, r.roles())
		}
		if !strings.Contains(r.log.String(), "differ") {
			t.Errorf("log: %s", r.log.String())
		}
	})
	t.Run("a pack that no longer verifies", func(t *testing.T) {
		voiceResetConfig(t)
		voiceEnableConfig(t, "")
		fake := vfake.New()
		voiceUseFactory(t, func(context.Context) (profile.Providers, error) { return profile.Providers{STT: fake}, nil })
		r := newVoiceServeRig(t)
		r.installVoicePack(t, "en", true)
		if err := os.WriteFile(filepath.Join(r.dataDr, voicePackDirName, "en", "stt-en.bin"), []byte("tampered"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := voiceServeProvide(context.Background(), r.deps); err != nil || len(r.roles()) != 0 {
			t.Fatalf("err %v, roles %v: a damaged pack was loaded", err, r.roles())
		}
		if !strings.Contains(r.log.String(), "running text-only") {
			t.Errorf("log: %s", r.log.String())
		}
	})
	t.Run("loaded with the activated pack", func(t *testing.T) {
		voiceResetConfig(t)
		voiceEnableConfig(t, "")
		fake := vfake.New()
		var closed int
		voiceUseFactory(t, func(context.Context) (profile.Providers, error) {
			return profile.Providers{STT: fake, Close: func() error { closed++; return nil }}, nil
		})
		r := newVoiceServeRig(t)
		r.installVoicePack(t, "en", true)
		if err := voiceServeProvide(context.Background(), r.deps); err != nil {
			t.Fatal(err)
		}
		if got := r.roles(); len(got) != 1 || got[0] != providers.RoleSTT {
			t.Fatalf("registered roles = %v, want stt only (the factory provided no TTS)", got)
		}
		if len(r.stops) != 1 || !strings.Contains(r.log.String(), "active_pack=en") {
			t.Fatalf("services %d, log: %s", len(r.stops), r.log.String())
		}
		r.stops[0]()
		if closed != 1 {
			t.Errorf("the profile released its connections %d times at shutdown, want once", closed)
		}
	})
}
