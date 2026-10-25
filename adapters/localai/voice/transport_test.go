package voice

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/adapters/localai"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providers"
	pv "github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// SEC-067: nothing outside the audio allowlist can be called through this
// client, whatever the caller does, and the whole test suite stays inside it
// (TestMain asserts the recorded paths of every test server).
func TestOnlyAllowlistedEndpointsCalled(t *testing.T) {
	want := []string{"POST /v1/audio/speech", "POST /v1/audio/transcriptions", "POST /v1/vad"}
	got := AllowedEndpoints()
	if len(got) != len(want) {
		t.Fatalf("allowlist = %v, want exactly %v", got, want)
	}
	have := map[string]bool{}
	for _, e := range got {
		have[e] = true
		if !strings.HasPrefix(e, "POST /v1/audio/") && e != "POST /v1/vad" {
			t.Errorf("%q is not an audio route", e)
		}
	}
	for _, e := range want {
		if !have[e] {
			t.Errorf("allowlist lacks %q", e)
		}
	}
	// The two allowlists are separate: nothing is shared, so neither can widen
	// the other.
	for _, e := range localai.AllowedEndpoints() {
		if have[e] {
			t.Errorf("%q is on both the voice and the text allowlist", e)
		}
	}
	got[0] = "GET /admin" // the returned slice is a copy
	if AllowedEndpoints()[0] == "GET /admin" {
		t.Error("AllowedEndpoints exposes the table itself")
	}

	srv := newStub(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, `{}`) })
	c, key := srv.client(t)
	refused := []struct{ method, path string }{
		{"GET", "/v1/audio/voices"},
		{"POST", "/tts"},
		{"POST", "/vad"},
		{"POST", "/audio/speech"},
		{"POST", "/audio/transcriptions"},
		{"GET", "/v1/audio/transcriptions"}, // right path, wrong method
		{"PUT", "/v1/vad"},
		{"POST", "/v1/chat/completions"},
		{"POST", "/v1/embeddings"},
		{"GET", "/v1/models"},
		{"GET", "/readyz"},
		{"GET", "/version"},
		{"POST", "/models/apply"},
		{"POST", "/models/import"},
		{"DELETE", "/models/galleries"},
		{"GET", "/backends"},
		{"GET", "/api/settings"},
		{"POST", "/v1/audio/speech/"},         // trailing slash
		{"POST", "/v1/Audio/speech"},          // case
		{"POST", "/v1/audio/speech?x=1"},      // query
		{"POST", "/v1/audio/../models/apply"}, // not cleaned for it
		{"POST", "/v1/audio/%73peech"},        // escaped path
	}
	for _, r := range refused {
		req, err := http.NewRequest(r.method, srv.URL+r.path, bytes.NewReader([]byte("x")))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.hc.Do(req)
		if err == nil {
			resp.Body.Close()
			t.Errorf("%s %s was sent", r.method, r.path)
			continue
		}
		if !errors.Is(err, errForbiddenEndpoint) {
			t.Errorf("%s %s: error = %v, want the allowlist refusal", r.method, r.path, err)
		}
	}
	// Another host or scheme is refused too, key or no key.
	for _, u := range []string{"http://127.0.0.2:" + portOf(t, srv.URL) + "/v1/vad", "https://" + strings.TrimPrefix(srv.URL, "http://") + "/v1/vad", "http://example.com/v1/vad"} {
		req, _ := http.NewRequest("POST", u, nil)
		if resp, err := c.hc.Do(req); err == nil {
			resp.Body.Close()
			t.Errorf("request to %s was sent", u)
		}
	}
	if n := len(srv.requests()); n != 0 {
		t.Fatalf("the runtime received %d requests that the guard should have refused", n)
	}

	// The three routes work, carry the key, and nothing else is called.
	f := pv.PCM16Mono(16000)
	pcm := tonePCM(f, 100*time.Millisecond)
	for _, call := range roleCalls() {
		_ = call.run(c)
	}
	_, _ = c.Transcribe(bg, testScope(), pv.TranscribeRequest{Audio: pcm, Format: f})
	reqs := srv.requests()
	if len(reqs) == 0 {
		t.Fatal("no request reached the runtime")
	}
	for _, r := range reqs {
		if !have[r.Method+" "+r.Path] {
			t.Errorf("runtime received %s %s", r.Method, r.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+key {
			t.Errorf("%s %s carried no key", r.Method, r.Path)
		}
	}
}

func portOf(t *testing.T, rawURL string) string {
	t.Helper()
	_, p, err := net.SplitHostPort(strings.TrimPrefix(rawURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The key a caller never configured is never invented, and a header a caller
// puts on a request is replaced by the guard.
func TestKeyHandling(t *testing.T) {
	srv := newStub(t, validTranscript)
	f := pv.PCM16Mono(16000)
	pcm := tonePCM(f, 100*time.Millisecond)

	t.Run("no key configured sends no Authorization header", func(t *testing.T) {
		c, err := New(localai.Config{Endpoint: srv.URL}, testVoiceConfig())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Transcribe(bg, testScope(), pv.TranscribeRequest{Audio: pcm, Format: f}); err != nil {
			t.Fatal(err)
		}
		reqs := srv.requests()
		if h := reqs[len(reqs)-1].Header.Get("Authorization"); h != "" {
			t.Errorf("Authorization = %q", h)
		}
	})
	t.Run("env reference", func(t *testing.T) {
		t.Setenv("ALIXAI_TEST_VOICE_KEY", "env-key-value")
		c, err := New(localai.Config{Endpoint: srv.URL, APIKeyRef: "env:ALIXAI_TEST_VOICE_KEY"}, testVoiceConfig())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Transcribe(bg, testScope(), pv.TranscribeRequest{Audio: pcm, Format: f}); err != nil {
			t.Fatal(err)
		}
		reqs := srv.requests()
		if h := reqs[len(reqs)-1].Header.Get("Authorization"); h != "Bearer env-key-value" {
			t.Errorf("Authorization = %q", h)
		}
	})
	t.Run("file reference", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "key")
		if err := os.WriteFile(path, []byte("file-key-value\nsecond line ignored\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := New(localai.Config{Endpoint: srv.URL, APIKeyRef: "file:" + path}, testVoiceConfig())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Transcribe(bg, testScope(), pv.TranscribeRequest{Audio: pcm, Format: f}); err != nil {
			t.Fatal(err)
		}
		reqs := srv.requests()
		if h := reqs[len(reqs)-1].Header.Get("Authorization"); h != "Bearer file-key-value" {
			t.Errorf("Authorization = %q", h)
		}
	})
	t.Run("a bad reference is refused at construction without echoing it", func(t *testing.T) {
		for _, ref := range []string{"env:ALIXAI_TEST_UNSET_VAR", "literal-key-pasted-by-mistake", "file:/does/not/exist", "env:bad name"} {
			_, err := New(localai.Config{Endpoint: srv.URL, APIKeyRef: ref}, testVoiceConfig())
			wantCode(t, err, contracts.ErrValidationFailed)
			if strings.Contains(err.Error(), "literal-key-pasted-by-mistake") {
				t.Errorf("error repeats the reference: %v", err)
			}
		}
	})
	t.Run("a key file open to others is refused", func(t *testing.T) {
		if err := keyFileModeOK(0o644, "linux"); err == nil {
			t.Error("a world-readable key file was accepted on linux")
		}
		if err := keyFileModeOK(0o600, "linux"); err != nil {
			t.Errorf("an owner-only key file was refused: %v", err)
		}
		if err := keyFileModeOK(0o666, "windows"); err != nil {
			t.Errorf("the windows mode check must be skipped: %v", err)
		}
	})
}

// SEC-082: the key and the response bodies never appear in an error, a log
// line or a formatted client.
func TestKeyAndBodiesNeverLogged(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, promptMarker, http.StatusInternalServerError)
	})
	key := newTestKey(t)
	c, err := New(localai.Config{Endpoint: srv.URL}, testVoiceConfig(), WithAPIKey(key), WithLogger(log))
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range roleCalls() {
		if err := call.run(c); err == nil {
			t.Fatalf("%s: want a failure", call.name)
		} else if strings.Contains(err.Error(), key) || strings.Contains(err.Error(), promptMarker) {
			t.Errorf("%s: error leaks: %v", call.name, err)
		}
	}
	out := logs.String()
	if !strings.Contains(out, "localai voice request") {
		t.Fatalf("no debug record was written: %q", out)
	}
	for _, secret := range []string{key, promptMarker, "Bearer"} {
		if strings.Contains(out, secret) {
			t.Errorf("log output contains %q", secret)
		}
	}
	shown := fmt.Sprintf("%v %+v %#v %s", c, c, c, c.hc.Transport.(*guardTransport).key)
	if strings.Contains(shown, key) || strings.Contains(fmt.Sprintf("%#v %v", newAPIKey(key), []any{newAPIKey(key)}), key) {
		t.Error("a formatted client or key reveals the value")
	}
}

// SEC-065: the endpoint policy of the text client applies to the audio client.
func TestEndpointPolicyAndConfig(t *testing.T) {
	bad := []localai.Config{
		{Endpoint: "http://192.168.1.10:8080"},                                       // plaintext off-box
		{Endpoint: "http://example.com"},                                             // plaintext to a name
		{Endpoint: "http://127.0.0.1:8080/prefix"},                                   // path prefix
		{Endpoint: "http://user:pw@127.0.0.1:8080"},                                  // credentials
		{Endpoint: "ftp://127.0.0.1"},                                                // scheme
		{Endpoint: "http://127.0.0.1:8080", TLS: localai.TLSConfig{ServerName: "x"}}, // TLS on plaintext
	}
	for _, rt := range bad {
		if _, err := New(rt, testVoiceConfig()); !contracts.HasCode(err, contracts.ErrValidationFailed) {
			t.Errorf("%+v: error = %v, want validation_failed", rt, err)
		}
	}
	good := []localai.Config{{}, {Endpoint: "http://127.0.0.1:8080"}, {Endpoint: "http://localhost:9"}, {Endpoint: "https://voice.example.org"}, {Endpoint: "http://[::1]:8080"}}
	for _, rt := range good {
		if _, err := New(rt, testVoiceConfig()); err != nil {
			t.Errorf("%+v: %v", rt, err)
		}
	}
	badCfg := []Config{
		{Models: ModelSet{STT: "bad name with spaces"}},
		{Models: ModelSet{TTS: "../escape"}},
		{Profiles: map[string]ModelSet{"Bad Profile": {}}},
		{Profiles: map[string]ModelSet{"ok": {VAD: "x y"}}},
		{MaxAudioBytes: -1},
	}
	for _, cfg := range badCfg {
		if _, err := New(localai.Config{}, cfg); !contracts.HasCode(err, contracts.ErrValidationFailed) {
			t.Errorf("%+v: error = %v, want validation_failed", cfg, err)
		}
	}
}

// The dial-time check refuses a plaintext connection to anything but loopback,
// whatever name the endpoint used.
func TestLoopbackOnlyDialControl(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:80": true, "[::1]:80": true, "127.9.9.9:1": true, "[::ffff:127.0.0.1]:80": true,
		"192.168.0.2:80": false, "8.8.8.8:80": false, "[2001:db8::1]:80": false, "not-an-ip:80": false, "garbage": false,
	} {
		if err := loopbackOnly("tcp", addr, nil); (err == nil) != ok {
			t.Errorf("loopbackOnly(%q) = %v, want allowed=%v", addr, err, ok)
		}
	}
}

func TestProfilesAndRoles(t *testing.T) {
	srv := newStub(t, validTranscript)
	f := pv.PCM16Mono(16000)
	pcm := tonePCM(f, 100*time.Millisecond)

	// A deployment with STT only: TTS and VAD are refused, never defaulted.
	c, _ := srv.client(t, func(_ *localai.Config, cfg *Config) { cfg.Models = ModelSet{STT: "only-stt"} })
	if _, err := c.Transcribe(bg, testScope(), pv.TranscribeRequest{Audio: pcm, Format: f}); err != nil {
		t.Fatal(err)
	}
	_, err := c.SynthesizeStream(bg, testScope(), ttsReq("hi"))
	wantCode(t, err, contracts.ErrValidationFailed)
	_, err = c.DetectSpeech(bg, testScope(), pv.DetectRequest{Audio: pcm, Format: f})
	wantCode(t, err, contracts.ErrValidationFailed)
	if n := len(srv.requests()); n != 1 {
		t.Errorf("runtime received %d requests, want only the STT one", n)
	}
	// The configuration is copied: later edits by the caller do not reach it.
	cfg := testVoiceConfig()
	c2, _ := New(localai.Config{Endpoint: srv.URL}, cfg)
	cfg.Profiles["it-fast"] = ModelSet{STT: "changed"}
	if m, _ := c2.model("stt", "it-fast"); m != "stt-it" {
		t.Errorf("model = %q, the client shares the caller's map", m)
	}
}

// One client serves all three roles through the registry's typed lookups.
func TestVoiceProvidersRegister(t *testing.T) {
	srv := newStub(t, validTranscript)
	c, _ := srv.client(t)
	reg := providers.NewRegistry(clock.Real{})
	for _, role := range pv.Roles() {
		if err := pv.Register(reg, role, c); err != nil {
			t.Fatalf("Register(%s): %v", role, err)
		}
	}
	if _, err := pv.LookupSTT(reg); err != nil {
		t.Error(err)
	}
	if _, err := pv.LookupTTS(reg); err != nil {
		t.Error(err)
	}
	if _, err := pv.LookupVAD(reg); err != nil {
		t.Error(err)
	}
}

// Tier 4 of docs/architecture/module-map.md: the base packages, the provider
// interfaces and package localai itself, nothing else from this module, and no
// shell-out.
func TestImportsStayWithinTier(t *testing.T) {
	const modulePath = "github.com/buxee/alix-ai/"
	prod := map[string]bool{
		"adapters/localai":         true,
		"pkg/contracts":            true,
		"internal/clock":           true,
		"internal/providers":       true,
		"internal/providers/voice": true,
	}
	test := map[string]bool{"internal/providers/voice/fake": true}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) < 10 {
		t.Fatalf("found %d files (%v)", len(files), err)
	}
	for _, f := range files {
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		isTest := strings.HasSuffix(f, "_test.go")
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == "os/exec" || p == "plugin" || p == "io/ioutil" {
				t.Errorf("%s imports %s", f, p)
			}
			if !strings.HasPrefix(p, modulePath) {
				continue
			}
			rel := strings.TrimPrefix(p, modulePath)
			if !prod[rel] && !(isTest && test[rel]) {
				t.Errorf("%s imports %s, which module-map.md does not allow for adapters/localai/voice", f, rel)
			}
		}
	}
}

// Raw audio is never written to disk by the adapter: no non-test file creates,
// writes or renames a file. (The only file the package opens is the API key
// file, read-only.)
func TestNoAudioWrittenToDisk(t *testing.T) {
	banned := map[string]bool{
		"Create": true, "CreateTemp": true, "MkdirTemp": true, "WriteFile": true, "OpenFile": true,
		"Mkdir": true, "MkdirAll": true, "Rename": true, "Truncate": true, "Symlink": true, "Link": true,
	}
	files, _ := filepath.Glob("*.go")
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		scanned++
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "os" && banned[sel.Sel.Name] {
					t.Errorf("%s uses os.%s: the adapter must not write to disk", f, sel.Sel.Name)
				}
			}
			return true
		})
	}
	if scanned < 8 {
		t.Fatalf("scanned only %d files", scanned)
	}
}
