package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/buxee/alix-ai/adapters/localai"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/models/registry"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/tools/devkeys"
)

const (
	// serveModelID is the shipped registry entry the fixtures install. Its
	// artefact file name is read from the entry, not hard-coded here.
	serveModelID = "granite-4.0-350m-q4_k_m"
	serveVersion = "v4.10.0"
)

// syncBuf is a writer the server goroutines and the test can share.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// testKey is a throwaway API key for one test: random, never a credential.
func testKey(t testing.TB) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "TESTONLY-" + hex.EncodeToString(b)
}

// stubRuntime is a plain-HTTP loopback stand-in for LocalAI that records the
// paths it was asked for. (Plain HTTP: a TLS-intercepting security product on a
// developer machine breaks loopback TLS, and TLS is not what is under test.)
type stubRuntime struct {
	*httptest.Server
	mu      sync.Mutex
	paths   []string
	auth    []string
	version string
	chat    string   // the content of the assistant message
	models  []string // the model each chat request named
}

func newStubRuntime(t testing.TB) *stubRuntime {
	t.Helper()
	rt := &stubRuntime{version: serveVersion, chat: validProposal()}
	rt.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ := io.ReadAll(r.Body)
		var named struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(reqBody, &named)
		rt.mu.Lock()
		rt.paths = append(rt.paths, r.Method+" "+r.URL.Path)
		rt.auth = append(rt.auth, r.Header.Get("Authorization"))
		if r.URL.Path == "/v1/chat/completions" {
			rt.models = append(rt.models, named.Model)
		}
		version, chat := rt.version, rt.chat
		rt.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/readyz":
			w.WriteHeader(http.StatusOK)
		case "/version":
			_, _ = fmt.Fprintf(w, `{"version":%q}`, version+" (0123456)")
		case "/v1/models":
			_, _ = fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model"}]}`, serveModelID)
		case "/v1/chat/completions":
			body, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-1", "model": serveModelID,
				"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
					"message": map[string]any{"role": "assistant", "content": chat}}},
				"usage": map[string]any{"prompt_tokens": 120, "completion_tokens": 30},
			})
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(rt.Close)
	return rt
}

func (rt *stubRuntime) requested() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]string(nil), rt.paths...)
}

// chatModels returns the model name each chat request carried.
func (rt *stubRuntime) chatModels() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]string(nil), rt.models...)
}

func (rt *stubRuntime) sentAuth() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]string(nil), rt.auth...)
}

func (rt *stubRuntime) setChat(content string) {
	rt.mu.Lock()
	rt.chat = content
	rt.mu.Unlock()
}

func validProposal() string {
	return `{"capability_id":"demo.create","fields":{"count":3},"entity_refs":[],"intent":"task","uncertainty":{"overall":0.9}}`
}

// artefact is the model file the fixtures install: its registry file name and
// its content.
type artefact struct {
	name    string
	content []byte
}

// shippedArtefactName reads the artefact file name of the shipped entry.
func shippedArtefactName(t testing.TB) string { return serveWarmArtefactNameFor(t, serveModelID) }

// serveWarmArtefactNameFor reads the artefact file name of the shipped entry id.
func serveWarmArtefactNameFor(t testing.TB, id string) string {
	t.Helper()
	raw, err := fs.ReadFile(registry.FS, id+".yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^  filename: (\S+)$`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("the shipped entry has no artifact.filename line")
	}
	return string(m[1])
}

// installModel writes content as the entry's artefact into a fresh models
// directory and returns the directory.
func installModel(t testing.TB, a artefact) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, a.name), a.content, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// registryFor returns a loadModelRegistry replacement: the shipped registry,
// with the one entry the fixtures use re-pinned to content and qualified for
// versions. Every other entry is shipped as is, so the registry-wide rules
// (fallbacks, defaults) still hold.
func registryFor(t testing.TB, content []byte, versions ...string) func() (*models.Registry, error) {
	t.Helper()
	return serveWarmRegistryForIDs(t, []string{serveModelID}, content, versions...)
}

// serveWarmRegistryForIDs is registryFor for several entries: each of ids is re-pinned to
// content and qualified for versions.
func serveWarmRegistryForIDs(t testing.TB, ids []string, content []byte, versions ...string) func() (*models.Registry, error) {
	t.Helper()
	entries, err := fs.ReadDir(registry.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	mem := fstest.MapFS{}
	for _, e := range entries {
		raw, err := fs.ReadFile(registry.FS, e.Name())
		if err != nil {
			t.Fatal(err)
		}
		// Fixture world: every controller candidate is qualified (the shipped statuses record the live
		// spike's real verdicts — M0-13 rejected them — and tests that need a verified model must not
		// depend on them). The shipped registry itself is exercised by the shipped-registry test below.
		raw = regexp.MustCompile(`(?m)^status: rejected$`).ReplaceAll(raw, []byte("status: qualified-emulated"))
		mem[e.Name()] = &fstest.MapFile{Data: raw}
	}
	sum := sha256Hex(content)
	edit := func(raw []byte, re, repl string) []byte {
		rx := regexp.MustCompile(re)
		if len(rx.FindAll(raw, -1)) != 1 {
			t.Fatalf("the shipped entry no longer has exactly one match for %s", re)
		}
		return rx.ReplaceAll(raw, []byte(repl))
	}
	quoted := make([]string, len(versions))
	for i, v := range versions {
		quoted[i] = fmt.Sprintf("%q", v)
	}
	for _, id := range ids {
		raw := mem[id+".yaml"].Data
		raw = edit(raw, `(?m)^  size_bytes: \d+$`, fmt.Sprintf("  size_bytes: %d", len(content)))
		raw = edit(raw, `(?m)^  sha256: [0-9a-f]{64}$`, "  sha256: "+sum)
		raw = edit(raw, `(?m)^  allowed_runtime_versions: \[\]$`, "  allowed_runtime_versions: ["+strings.Join(quoted, ", ")+"]")
		// The fixture is a SYNTHETIC qualified entry: whatever the shipped entry's real status is (the live
		// spike M0-13 measured the Granite 350M controller and marked it `rejected`), these tests need a
		// verified, qualified model to exercise the preflight, so the status is forced here and nowhere else.
		raw = edit(raw, `(?m)^status: \S+$`, "status: qualified-emulated")
		mem[id+".yaml"] = &fstest.MapFile{Data: raw}
	}
	return func() (*models.Registry, error) { return models.LoadFS(mem) }
}

// useRegistry replaces the registry loader for one test.
func useRegistry(t testing.TB, load func() (*models.Registry, error)) {
	t.Helper()
	old := loadModelRegistry
	loadModelRegistry = load
	t.Cleanup(func() { loadModelRegistry = old })
}

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fixture is a stub runtime plus a verified model installation.
type fixture struct {
	rt      *stubRuntime
	dir     string
	art     artefact
	content []byte
}

func newFixture(t testing.TB, versions ...string) *fixture {
	t.Helper()
	f := &fixture{rt: newStubRuntime(t), content: []byte("not a real model, only bytes with a known hash")}
	f.art = artefact{name: shippedArtefactName(t), content: f.content}
	f.dir = installModel(t, f.art)
	if len(versions) == 0 {
		versions = []string{serveVersion}
	}
	useRegistry(t, registryFor(t, f.content, versions...))
	return f
}

func (f *fixture) localai() localai.Config {
	return localai.Config{Endpoint: f.rt.URL, Models: localai.ModelSet{Controller: serveModelID}}
}

func (f *fixture) inference() inferenceConfig {
	return inferenceConfig{ModelsDir: f.dir, Models: []string{serveModelID}}
}

// SEC-063: `alixai serve` verifies the installed model against the registry
// before the inference path starts. Every mismatch leaves the controller
// unregistered and the runtime untouched, and reports unavailable.
func TestSecServePreflightRefusesUnverifiedModel(t *testing.T) {
	tests := []struct {
		name string
		// arrange changes the fixture; it returns the inference section to use.
		arrange   func(t *testing.T, f *fixture) inferenceConfig
		wantErr   error // a models sentinel the cause must wrap, if any
		wantInOff string
	}{
		{
			name: "same size, different bytes",
			arrange: func(t *testing.T, f *fixture) inferenceConfig {
				tampered := bytes.Clone(f.content)
				tampered[0] ^= 0xff
				f.dir = installModel(t, artefact{name: f.art.name, content: tampered})
				return f.inference()
			},
			wantErr:   models.ErrHashMismatch,
			wantInOff: "model verification failed",
		},
		{
			name: "different size",
			arrange: func(t *testing.T, f *fixture) inferenceConfig {
				f.dir = installModel(t, artefact{name: f.art.name, content: append(bytes.Clone(f.content), 'x')})
				return f.inference()
			},
			wantErr:   models.ErrSizeMismatch,
			wantInOff: "model verification failed",
		},
		{
			name: "file is missing",
			arrange: func(t *testing.T, f *fixture) inferenceConfig {
				f.dir = t.TempDir()
				return f.inference()
			},
			wantErr:   models.ErrArtefactUnreadable,
			wantInOff: "model verification failed",
		},
		{
			name: "model id is not in the registry",
			arrange: func(t *testing.T, f *fixture) inferenceConfig {
				inf := f.inference()
				inf.Models = []string{"no-such-model"}
				return inf
			},
			wantInOff: "model verification failed",
		},
		{
			name: "one good model does not excuse a bad one",
			arrange: func(t *testing.T, f *fixture) inferenceConfig {
				inf := f.inference()
				inf.Models = []string{serveModelID, "granite-4.0-1b-q4_k_m"} // shipped entry, not installed
				return inf
			},
			wantErr:   models.ErrArtefactUnreadable,
			wantInOff: "model verification failed",
		},
		{
			name: "no models configured",
			arrange: func(t *testing.T, f *fixture) inferenceConfig {
				return inferenceConfig{ModelsDir: f.dir}
			},
			wantInOff: "inference.models",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			inf := tc.arrange(t, f)
			reg, off := startInference(context.Background(), discardLog(), clock.Real{}, f.localai(), inf)
			if off == nil {
				t.Fatal("startInference accepted the model; the inference path must stay off")
			}
			if !strings.Contains(off.reason, tc.wantInOff) {
				t.Errorf("reason = %q, want it to mention %q", off.reason, tc.wantInOff)
			}
			if tc.wantErr != nil {
				if !errors.Is(off.err, tc.wantErr) {
					t.Errorf("cause = %v, want it to wrap %v", off.err, tc.wantErr)
				}
				if got := contracts.CodeOf(off.err); got != contracts.ErrUnavailable {
					t.Errorf("code = %s, want %s", got, contracts.ErrUnavailable)
				}
			}
			if reg.Has(providers.RoleController) || reg.Usable(providers.RoleController) {
				t.Error("a controller is registered although verification failed")
			}
			if _, err := reg.TextGenerator(providers.RoleController); err == nil {
				t.Error("the registry hands out a text generator although verification failed")
			}
			if got := f.rt.requested(); len(got) != 0 {
				t.Errorf("the runtime was contacted before verification passed: %v", got)
			}
		})
	}
}

// Against the registry as shipped, no controller can start the inference path: the live spike (M0-13)
// measured every controller candidate against the fixed Lite bar and none passed, so each is `rejected`
// and the preflight refuses on STATUS before it looks at the file; the runtime is never contacted. The
// weights pinned in the registry cannot be reproduced here, and the status refusal is the stronger
// guarantee (a rejected model is never activated, whatever file is installed).
func TestServePreflightWithShippedRegistryRefusesUnmatchedArtefact(t *testing.T) {
	rt := newStubRuntime(t)
	useRegistry(t, func() (*models.Registry, error) { return models.LoadFS(registry.FS) })
	dir := installModel(t, artefact{name: shippedArtefactName(t), content: []byte("whatever")})
	reg, off := startInference(context.Background(), discardLog(), clock.Real{},
		localai.Config{Endpoint: rt.URL, Models: localai.ModelSet{Controller: serveModelID}},
		inferenceConfig{ModelsDir: dir, Models: []string{serveModelID}})
	if off == nil {
		t.Fatal("the inference path started with a file that is not the pinned artefact")
	}
	if !errors.Is(off.err, models.ErrNotActivatable) {
		t.Errorf("cause = %v, want it to wrap %v", off.err, models.ErrNotActivatable)
	}
	if reg.Has(providers.RoleController) {
		t.Error("a controller is registered")
	}
	if got := rt.requested(); len(got) != 0 {
		t.Errorf("the runtime was contacted: %v", got)
	}
}

func TestServePreflightStartsVerifiedInference(t *testing.T) {
	f := newFixture(t)
	reg, off := startInference(context.Background(), discardLog(), clock.Real{}, f.localai(), f.inference())
	if off != nil {
		t.Fatalf("startInference refused a verified model: %s: %v", off.reason, off.err)
	}
	if !reg.Usable(providers.RoleController) {
		t.Fatalf("controller status = %+v, want usable", reg.Status(providers.RoleController))
	}
	st := reg.Status(providers.RoleController)
	if st.Provider != localai.ProviderName || st.Version != serveVersion || st.Status != providers.HealthOK {
		t.Errorf("status = %+v", st)
	}
	// Only the three read routes ran: nothing generated, nothing installed.
	for _, p := range f.rt.requested() {
		switch p {
		case "GET /readyz", "GET /version", "GET /v1/models":
		default:
			t.Errorf("unexpected runtime request %q", p)
		}
	}
	if reg.Has(providers.RoleEmbedder) || reg.Has(providers.RoleRouter) {
		t.Error("roles nobody configured were registered")
	}
}

// QualifiedVersions comes from the registry's allowed_runtime_versions; an
// operator list can narrow it and never widen it, and an empty result must not
// reach the adapter (where empty means "no check").
func TestServePreflightQualifiedVersions(t *testing.T) {
	tests := []struct {
		name       string
		registry   []string
		operator   []string
		runtime    string
		wantOff    bool
		wantUsable bool
	}{
		{name: "registry list, matching runtime", registry: []string{"v4.10.0"}, runtime: "v4.10.0", wantUsable: true},
		{name: "registry list, other runtime", registry: []string{"v4.10.0"}, runtime: "v4.11.2"},
		{name: "operator narrows the set", registry: []string{"v4.10.0", "v4.9.0"}, operator: []string{"v4.9.0"}, runtime: "v4.9.0", wantUsable: true},
		{name: "operator narrowed away the running version", registry: []string{"v4.10.0", "v4.9.0"}, operator: []string{"v4.9.0"}, runtime: "v4.10.0"},
		{name: "operator cannot widen the set", registry: []string{"v4.10.0"}, operator: []string{"v4.10.0", "v9.9.9"}, runtime: "v9.9.9"},
		{name: "operator and registry share nothing", registry: []string{"v4.10.0"}, operator: []string{"v4.9.0"}, wantOff: true},
		{name: "case and the v prefix are cosmetic", registry: []string{"v4.10.0"}, operator: []string{"V4.10.0"}, runtime: "v4.10.0", wantUsable: true},
		{name: "registry qualifies no version", registry: []string{}, wantOff: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.registry...)
			if len(tc.registry) == 0 {
				useRegistry(t, registryFor(t, f.content)) // newFixture defaults an empty list to one version
			}
			if tc.runtime != "" {
				f.rt.mu.Lock()
				f.rt.version = tc.runtime
				f.rt.mu.Unlock()
			}
			lcfg := f.localai()
			lcfg.QualifiedVersions = tc.operator
			reg, off := startInference(context.Background(), discardLog(), clock.Real{}, lcfg, f.inference())
			if (off != nil) != tc.wantOff {
				t.Fatalf("off = %v, want off=%v", off, tc.wantOff)
			}
			if tc.wantOff {
				if reg.Has(providers.RoleController) {
					t.Error("a controller is registered although no version is qualified")
				}
				if got := f.rt.requested(); len(got) != 0 {
					t.Errorf("the runtime was contacted: %v", got)
				}
				return
			}
			if got := reg.Usable(providers.RoleController); got != tc.wantUsable {
				st := reg.Status(providers.RoleController)
				t.Errorf("usable = %v, want %v (status %s: %s)", got, tc.wantUsable, st.Status, st.Reason)
			}
		})
	}
}

func TestServePreflightNeedsConfiguration(t *testing.T) {
	f := newFixture(t)
	// An empty localai.models.controller is derived from the verified registry
	// id (M0-19; TestEmptyControllerNameIsDerivedFromTheVerifiedRegistryID runs it
	// through serve). The path stays off when nothing can be derived: two verified
	// entries serve the controller role and the operator has not said which.
	// Nothing is registered and the runtime is not contacted.
	t.Run("no controller model and two to choose from", func(t *testing.T) {
		const second = "granite-4.0-1b-q4_k_m"
		dir := t.TempDir()
		for _, id := range []string{serveModelID, second} {
			if err := os.WriteFile(filepath.Join(dir, serveWarmArtefactNameFor(t, id)), f.content, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		useRegistry(t, serveWarmRegistryForIDs(t, []string{serveModelID, second}, f.content, serveVersion))
		lcfg := f.localai()
		lcfg.Models.Controller = ""
		before := len(f.rt.requested())
		reg, off := startInference(context.Background(), discardLog(), clock.Real{}, lcfg,
			inferenceConfig{ModelsDir: dir, Models: []string{serveModelID, second}})
		if off == nil || reg.Has(providers.RoleController) {
			t.Fatalf("off = %v, controller registered = %v", off, reg.Has(providers.RoleController))
		}
		if !strings.Contains(off.err.Error(), "several controller models") {
			t.Errorf("cause = %v, want it to say that several entries serve the controller role", off.err)
		}
		if got := f.rt.requested(); len(got) != before {
			t.Errorf("the runtime was contacted: %v", got[before:])
		}
	})
	t.Run("registry cannot be loaded", func(t *testing.T) {
		useRegistry(t, func() (*models.Registry, error) { return nil, errors.New("boom") })
		reg, off := startInference(context.Background(), discardLog(), clock.Real{}, f.localai(), f.inference())
		if off == nil || reg.Has(providers.RoleController) {
			t.Fatalf("off = %v, controller registered = %v", off, reg.Has(providers.RoleController))
		}
	})
	t.Run("adapter refuses the endpoint", func(t *testing.T) {
		useRegistry(t, registryFor(t, f.content, serveVersion))
		lcfg := f.localai()
		lcfg.Endpoint = "http://203.0.113.9:8080" // not loopback, not TLS
		reg, off := startInference(context.Background(), discardLog(), clock.Real{}, lcfg, f.inference())
		if off == nil || reg.Has(providers.RoleController) {
			t.Fatalf("off = %v, controller registered = %v", off, reg.Has(providers.RoleController))
		}
	})
}

func TestIntersectVersions(t *testing.T) {
	tests := []struct {
		a, b, want []string
	}{
		{[]string{"v4.10.0", "v4.9.0"}, []string{"v4.9.0"}, []string{"v4.9.0"}},
		{[]string{"v4.10.0"}, []string{"4.10.0"}, []string{"v4.10.0"}},
		{[]string{"4.10.0"}, []string{"V4.10.0"}, []string{"4.10.0"}},
		{[]string{"v4.10.0"}, []string{"v4.9.0"}, nil},
		{nil, []string{"v4.9.0"}, nil},
		{[]string{"v4.9.0"}, nil, nil},
	}
	for _, tc := range tests {
		got := intersectVersions(tc.a, tc.b)
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("intersectVersions(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestInferenceConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     inferenceConfig
		wantErr string
	}{
		{name: "empty is valid (inference stays off)"},
		{name: "ok", cfg: inferenceConfig{ModelsDir: "data/models", Models: []string{serveModelID}}},
		{name: "models without a directory", cfg: inferenceConfig{Models: []string{serveModelID}}, wantErr: "models_dir is required"},
		{name: "not an id", cfg: inferenceConfig{ModelsDir: "d", Models: []string{"../etc/passwd"}}, wantErr: "not a registry entry id"},
		{name: "upper case", cfg: inferenceConfig{ModelsDir: "d", Models: []string{"Granite"}}, wantErr: "not a registry entry id"},
		{name: "listed twice", cfg: inferenceConfig{ModelsDir: "d", Models: []string{serveModelID, serveModelID}}, wantErr: "listed twice"},
		{name: "NUL in the directory", cfg: inferenceConfig{ModelsDir: "d\x00"}, wantErr: "NUL"},
	}
	for _, tc := range tests {
		err := tc.cfg.Validate()
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: error = %v, want it to contain %q", tc.name, err, tc.wantErr)
		}
	}
}

// ---- serve end to end -------------------------------------------------------

// serveEnv builds an Env over its own registry that knows every section serve
// reads, and puts the shared section variables back when the test ends.
func serveEnv(t *testing.T, stdout, stderr io.Writer) *Env {
	t.Helper()
	oldLocal, oldInf, oldKeys, oldProfile := localaiSection, inferenceSection, keysSection, serveProfileSection
	oldGW, oldDev, oldID := *gateway.ConfigSection(), *gateway.DevSection(), *identity.ConfigSection()
	t.Cleanup(func() {
		localaiSection, inferenceSection, keysSection, serveProfileSection = oldLocal, oldInf, oldKeys, oldProfile
		*gateway.ConfigSection(), *gateway.DevSection(), *identity.ConfigSection() = oldGW, oldDev, oldID
	})
	reg := config.NewRegistry()
	registerServeSections(reg)
	return &Env{
		Stdin:     strings.NewReader(""),
		Stdout:    stdout,
		Stderr:    stderr,
		LookupEnv: func(string) (string, bool) { return "", false },
		Config:    reg,
	}
}

func yamlPath(p string) string { return fmt.Sprintf("%q", filepath.ToSlash(p)) }

// serveConfig writes a config file for a serve run on an ephemeral loopback
// port and returns its path and the data directory.
func serveConfig(t *testing.T, f *fixture, extra string) (cfgPath, dataDir string) {
	t.Helper()
	dataDir = filepath.Join(t.TempDir(), "data")
	doc := fmt.Sprintf(`data_dir: %s
server:
  listen: 127.0.0.1:0
localai:
  endpoint: %s
  api_key_ref: env:ALIXAI_TEST_LOCALAI_KEY
  models:
    controller: %s
inference:
  models_dir: %s
  models: [%s]
%s`, yamlPath(dataDir), f.rt.URL, serveModelID, yamlPath(f.dir), serveModelID, extra)
	cfgPath = filepath.Join(t.TempDir(), "alixai.yaml")
	if err := os.WriteFile(cfgPath, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, dataDir
}

// running is one `alixai serve` process-in-a-goroutine.
type running struct {
	base   string
	stderr *syncBuf
	stdout *syncBuf
	stop   func() error
}

var listeningRE = regexp.MustCompile(`listening on (http://\S+)`)

func startServe(t *testing.T, cfgPath string) *running {
	t.Helper()
	r := &running{stderr: &syncBuf{}, stdout: &syncBuf{}}
	env := serveEnv(t, r.stdout, r.stderr)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, env, []string{"-config", cfgPath}) }()
	r.stop = func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(30 * time.Second):
			return errors.New("serve did not stop")
		}
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = r.stop()
		}
	})
	origStop := r.stop
	r.stop = func() error { stopped = true; return origStop() }

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if m := listeningRE.FindStringSubmatch(r.stdout.String()); m != nil {
			r.base = m[1]
			return r
		}
		select {
		case err := <-done:
			stopped = true
			t.Fatalf("serve stopped before it listened: %v\nstderr:\n%s", err, r.stderr.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatalf("serve did not listen in time\nstdout: %s\nstderr:\n%s", r.stdout.String(), r.stderr.String())
	return nil
}

// serveWarmReadyzWait is how long do keeps asking a /readyz that is only waiting
// for the model to warm up.
const serveWarmReadyzWait = 20 * time.Second

// serveWarmStillWarming reports whether a /readyz body says the model is still
// loading: the inference_warmth check fails with "warming up". Any other
// failure, and a ready service, are final.
func serveWarmStillWarming(body []byte) bool {
	var rep gateway.ReadyReport
	if json.Unmarshal(body, &rep) != nil {
		return false
	}
	for _, c := range rep.Checks {
		if c.Name == serveReadinessCheckName && !c.OK && strings.Contains(c.Reason, "warming up") {
			return true
		}
	}
	return false
}

// do sends one request. A GET /readyz that is only waiting for the model to warm
// up (the first poll waits 1.5 s for it, then answers 503 "warming up") is asked
// again, for up to serveWarmReadyzWait, so a test that needs the first 200 does
// not depend on the warm-up finishing inside that wait on a loaded host. A test
// that is about the warm-up itself calls doOnce.
func (r *running) do(t *testing.T, method, path, body string) (int, http.Header, []byte) {
	t.Helper()
	code, hdr, b := r.doOnce(t, method, path, body)
	if method != http.MethodGet || path != "/readyz" {
		return code, hdr, b
	}
	deadline := time.Now().Add(serveWarmReadyzWait)
	for code == http.StatusServiceUnavailable && serveWarmStillWarming(b) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		code, hdr, b = r.doOnce(t, method, path, body)
	}
	return code, hdr, b
}

// doOnce sends one request and returns whatever answered.
func (r *running) doOnce(t *testing.T, method, path, body string) (int, http.Header, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, r.base+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

const demoPropose = `{"text":"Make 3 demos","today":"2026-10-12","capabilities":[{"id":"demo.create","description":"Create a demo","fields":[{"name":"count","type":"integer","required":true}]}]}`

// The composition root end to end: config, store, model preflight, the LocalAI
// adapter and the gateway. One text request goes through AlixAI and the
// runtime and comes back as a schema-validated TaskProposal; the API key reaches
// the runtime and nothing else.
func TestServeRunsOneTextRequestThroughTheRuntime(t *testing.T) {
	f := newFixture(t)
	key := testKey(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", key)
	cfg, dataDir := serveConfig(t, f, "dev:\n  spike_endpoint: true\n")
	srv := startServe(t, cfg)

	if code, _, body := srv.do(t, http.MethodGet, "/healthz", ""); code != http.StatusOK {
		t.Fatalf("/healthz = %d: %s", code, body)
	}
	// The first /readyz poll starts the warm-up and waits 1.5 s for it; a host that
	// is slow to answer needs another poll, so ask until the model is ready.
	var code int
	var body []byte
	pollUntil(t, 20*time.Second, "/readyz to turn ready", func() bool {
		code, _, body = srv.doOnce(t, http.MethodGet, "/readyz", "")
		return code == http.StatusOK
	})
	var ready gateway.ReadyReport
	if err := json.Unmarshal(body, &ready); err != nil {
		t.Fatal(err)
	}
	if !ready.Ready || ready.Status != providers.HealthOK {
		t.Errorf("readiness = %+v", ready)
	}
	names := map[string]bool{}
	for _, c := range ready.Checks {
		names[c.Name] = c.OK
	}
	if ok, found := names["store"]; !found || !ok {
		t.Errorf("checks = %+v, want a passing store check", ready.Checks)
	}
	if _, found := names["inference"]; found {
		t.Errorf("an inference check is reported although the path started: %+v", ready.Checks)
	}

	code, hdr, body := srv.do(t, http.MethodPost, "/v0/dev/propose", demoPropose)
	if code != http.StatusOK {
		t.Fatalf("POST /v0/dev/propose = %d: %s", code, body)
	}
	if hdr.Get("X-AlixAI-Dev-Only") != "true" {
		t.Error("the spike response is not marked dev-only")
	}
	var resp gateway.ProposeResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.DevOnly || !resp.SchemaValid || resp.Proposal.CapabilityID == nil || *resp.Proposal.CapabilityID != "demo.create" ||
		resp.Proposal.Intent != "task" || resp.Proposal.TraceID == "" || resp.Model != serveModelID {
		t.Errorf("response = %+v", resp)
	}

	// Invalid model output is a schema_invalid envelope with no part of it.
	f.rt.setChat(`{"capability_id":"rm.everything","fields":{},"entity_refs":[],"intent":"task","uncertainty":{"overall":1}}`)
	code, _, body = srv.do(t, http.MethodPost, "/v0/dev/propose", demoPropose)
	if code != http.StatusBadGateway {
		t.Fatalf("invalid model output: status = %d, want 502: %s", code, body)
	}
	var env contracts.Error
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Code != contracts.ErrSchemaInvalid || strings.Contains(string(body), "rm.everything") {
		t.Errorf("envelope = %s", body)
	}

	// The key reached the runtime on its authenticated calls, and nowhere else.
	sawKey := false
	for _, a := range f.rt.sentAuth() {
		if a == "Bearer "+key {
			sawKey = true
		}
	}
	if !sawKey {
		t.Error("the configured API key never reached the runtime")
	}
	if strings.Contains(string(body), key) || strings.Contains(srv.stderr.String(), key) || strings.Contains(srv.stdout.String(), key) {
		t.Error("the API key leaked into a response or a log")
	}

	if err := srv.stop(); err != nil {
		t.Fatalf("serve returned %v after a clean stop", err)
	}
	if !strings.Contains(srv.stderr.String(), `"spike_endpoint":true`) {
		t.Errorf("the startup line does not state that the spike endpoint is on:\n%s", srv.stderr.String())
	}

	// SEC-081: the data directory and the database are created owner-only
	// (Windows has no mode bits; its ACL is the store's concern).
	if runtime.GOOS != "windows" {
		for p, want := range map[string]fs.FileMode{dataDir: 0o700, filepath.Join(dataDir, "alixai.db"): 0o600} {
			info, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != want {
				t.Errorf("%s mode = %o, want %o", p, info.Mode().Perm(), want)
			}
		}
	}
}

// Without dev.spike_endpoint the route does not exist.
func TestServeSpikeEndpointIsOffByDefault(t *testing.T) {
	f := newFixture(t)
	cfg, _ := serveConfig(t, f, "")
	srv := startServe(t, cfg)
	code, _, body := srv.do(t, http.MethodPost, "/v0/dev/propose", demoPropose)
	if code != http.StatusNotFound {
		t.Fatalf("POST /v0/dev/propose = %d, want 404: %s", code, body)
	}
	if got := f.rt.requested(); containsPath(got, "POST /v1/chat/completions") {
		t.Errorf("the runtime was asked to generate: %v", got)
	}
	if strings.Contains(srv.stderr.String(), "dev.spike_endpoint is on") {
		t.Error("the dev warning was logged although the endpoint is off")
	}
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// SEC-063 at the process level: a tampered model keeps the service up but the
// inference path off. /readyz says so, the spike endpoint answers unavailable,
// and nothing reaches the runtime.
func TestSecServeWithTamperedModelStaysUpWithInferenceOff(t *testing.T) {
	f := newFixture(t)
	tampered := bytes.Clone(f.content)
	tampered[0] ^= 0xff
	f.dir = installModel(t, artefact{name: f.art.name, content: tampered})
	cfg, _ := serveConfig(t, f, "dev:\n  spike_endpoint: true\n")
	srv := startServe(t, cfg)

	code, _, body := srv.do(t, http.MethodGet, "/healthz", "")
	if code != http.StatusOK {
		t.Fatalf("/healthz = %d: %s", code, body)
	}
	code, _, body = srv.do(t, http.MethodGet, "/readyz", "")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d, want 503: %s", code, body)
	}
	var ready gateway.ReadyReport
	if err := json.Unmarshal(body, &ready); err != nil {
		t.Fatal(err)
	}
	var reported *gateway.CheckReport
	for i := range ready.Checks {
		if ready.Checks[i].Name == "inference" {
			reported = &ready.Checks[i]
		}
	}
	if ready.Ready || reported == nil || reported.OK || !strings.Contains(reported.Reason, "model verification failed") {
		t.Errorf("readiness = %+v", ready)
	}
	if strings.Contains(string(body), f.art.name) || strings.Contains(string(body), sha256Hex(f.content)) {
		t.Errorf("/readyz exposes verification detail to an anonymous caller: %s", body)
	}

	code, _, body = srv.do(t, http.MethodPost, "/v0/dev/propose", demoPropose)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("POST /v0/dev/propose = %d, want 503: %s", code, body)
	}
	var env contracts.Error
	if err := json.Unmarshal(body, &env); err != nil || env.Code != contracts.ErrUnavailable {
		t.Errorf("envelope = %s (%v)", body, err)
	}
	if got := f.rt.requested(); len(got) != 0 {
		t.Errorf("the runtime was contacted: %v", got)
	}
	if !strings.Contains(srv.stderr.String(), "the inference path is off") {
		t.Errorf("the log does not say the inference path is off:\n%s", srv.stderr.String())
	}
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
}

// SEC-093 at the composition root: a non-loopback listener without TLS is
// refused, before the data directory or the database is created.
func TestSecServeRefusesNonLoopbackWithoutTLS(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:0", ":0", "192.0.2.10:7470", "[::]:0"} {
		t.Run(listen, func(t *testing.T) {
			f := newFixture(t)
			cfg, dataDir := serveConfig(t, f, "")
			raw, err := os.ReadFile(cfg)
			if err != nil {
				t.Fatal(err)
			}
			raw = bytes.Replace(raw, []byte("listen: 127.0.0.1:0"), []byte(fmt.Sprintf("listen: %q", listen)), 1)
			if err := os.WriteFile(cfg, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			env := serveEnv(t, io.Discard, io.Discard)
			err = runServe(context.Background(), env, []string{"-config", cfg})
			if !errors.Is(err, gateway.ErrInsecureListener) {
				t.Fatalf("err = %v, want ErrInsecureListener", err)
			}
			if _, statErr := os.Stat(dataDir); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("the data directory exists after the refusal (stat: %v): the store was opened first", statErr)
			}
			if got := f.rt.requested(); len(got) != 0 {
				t.Errorf("the runtime was contacted: %v", got)
			}
		})
	}
}

// SEC-092: the unauthenticated spike endpoint is never served beyond loopback,
// even where TLS is configured for the listener.
func TestSecServeRefusesSpikeEndpointBeyondLoopback(t *testing.T) {
	f := newFixture(t)
	cfg, dataDir := serveConfig(t, f, "dev:\n  spike_endpoint: true\n")
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte("listen: 127.0.0.1:0"), []byte(`listen: "0.0.0.0:0"`), 1)
	raw = append(raw, "gateway:\n  tls:\n    cert_file: \"c.pem\"\n    key_file: \"k.pem\"\n"...)
	if err := os.WriteFile(cfg, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	env := serveEnv(t, io.Discard, io.Discard)
	err = runServe(context.Background(), env, []string{"-config", cfg})
	if !errors.Is(err, gateway.ErrSpikeNeedsLoopback) {
		t.Fatalf("err = %v, want ErrSpikeNeedsLoopback", err)
	}
	if _, statErr := os.Stat(dataDir); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the data directory exists after the refusal (stat: %v)", statErr)
	}
}

func TestServeRefusesUnixSocketUntilSupported(t *testing.T) {
	f := newFixture(t)
	cfg, dataDir := serveConfig(t, f, "")
	raw, _ := os.ReadFile(cfg)
	raw = bytes.Replace(raw, []byte("server:\n"), []byte("server:\n  unix_socket: \"/tmp/alixai.sock\"\n"), 1)
	if err := os.WriteFile(cfg, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	env := serveEnv(t, io.Discard, io.Discard)
	err := runServe(context.Background(), env, []string{"-config", cfg})
	if err == nil || !strings.Contains(err.Error(), "unix_socket") {
		t.Fatalf("err = %v, want a refusal that names server.unix_socket", err)
	}
	if _, statErr := os.Stat(dataDir); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the data directory exists after the refusal (stat: %v)", statErr)
	}
}

func TestServeUsage(t *testing.T) {
	env := serveEnv(t, io.Discard, io.Discard)
	err := runServe(context.Background(), env, []string{"extra"})
	if !errors.Is(err, ErrUsage) {
		t.Errorf("err = %v, want a usage error", err)
	}
}

// The composition root registers the localai and inference sections on the
// default registry (the adapter does not), next to the gateway's own.
func TestServeSectionsAreRegisteredOnDefault(t *testing.T) {
	have := map[string]bool{}
	for _, name := range config.Default.Sections() {
		have[name] = true
	}
	for _, want := range []string{localaiSectionName, inferenceSectionName, keysSectionName, serveProfileSectionName, identity.SectionName, gateway.SectionName, gateway.DevSectionName} {
		if !have[want] {
			t.Errorf("section %q is not registered on config.Default (have %v)", want, config.Default.Sections())
		}
	}
}

// `config check` runs the adapter's own validation through the registered
// section, so an unsafe runtime endpoint is refused before serve starts
// (SEC-065).
func TestServeConfigCheckValidatesLocalAISection(t *testing.T) {
	env := serveEnv(t, io.Discard, io.Discard)
	var errb bytes.Buffer
	env.Stderr = &errb
	cfg := writeConfig(t, "localai:\n  endpoint: http://203.0.113.9:8080\n")
	code := Default.Run(context.Background(), []string{"config", "check", "-config", cfg}, env)
	if code != ExitError || !strings.Contains(errb.String(), "localai") {
		t.Fatalf("config check: code = %d, stderr = %q; want a refusal that names the localai section", code, errb.String())
	}
}

// The log redactor must not be needed to keep the key out of the startup line:
// serve never logs the adapter configuration at all.
func TestServeStartupLogHasNoConfiguration(t *testing.T) {
	f := newFixture(t)
	key := testKey(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", key)
	cfg, _ := serveConfig(t, f, "")
	srv := startServe(t, cfg)
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	logs := srv.stderr.String()
	for _, banned := range []string{key, "api_key_ref", "ALIXAI_TEST_LOCALAI_KEY"} {
		if strings.Contains(logs, banned) {
			t.Errorf("the log mentions %q:\n%s", banned, logs)
		}
	}
	// The log is JSON lines through the redacting handler.
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Errorf("log line is not JSON: %q", line)
		}
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ---- the public API (M1-16) -------------------------------------------------

// The host application of these tests is the one the shipped example package
// describes (examples/packages/valid-minimal): its manifest declares the
// issuer, the audience and the key id of its tokens, and the operator registers
// the key for it with `alixai identity add-issuer-key -package`, exactly as in
// production. The key pair is generated per test; nothing here can mint a token
// for a real deployment.
const (
	hostPackageDir = "../../examples/packages/valid-minimal"
	hostApp        = "demo-library"
	hostIssuer     = "https://library.example.test"
	hostKeyID      = "demo-library-ctx-2026"
	hostAudience   = "alixai"
	// identityYAML is the identity section of a serve config: the deployment
	// and the audience the example package's tokens carry.
	identityYAML = "identity:\n  deployment_id: dep_cli\n  audience: alixai\n"
)

// newHostIssuer is the host application: a fresh key pair and a minter that
// stamps tokens with the real clock (serve reads the real one).
func newHostIssuer(t testing.TB) *identitytest.Issuer {
	t.Helper()
	return identitytest.NewIssuer(t, identitytest.IssuerConfig{
		ApplicationID: hostApp, Issuer: hostIssuer, KeyID: hostKeyID, Audience: hostAudience, Clock: clock.Real{},
	})
}

// runCLI runs one alixai command on the registered dispatcher, in an
// environment that knows the sections serve reads, and returns the exit code
// and what it printed.
func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut syncBuf
	code = Default.Run(context.Background(), args, serveEnv(t, &out, &errOut))
	return code, out.String(), errOut.String()
}

// provisionIssuer registers iss's public key the way an operator does.
func provisionIssuer(t *testing.T, cfgPath string, iss *identitytest.Issuer) {
	t.Helper()
	code, out, errOut := runCLI(t, "identity", "add-issuer-key", "-config", cfgPath, "-package", hostPackageDir,
		"-key-id", iss.KeyID, "-public-key", trust.EncodePublicKey(iss.Public))
	if code != ExitOK {
		t.Fatalf("identity add-issuer-key = %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
}

// call sends one request with a fresh single-use token ("" sends none).
func (r *running) call(t *testing.T, iss *identitytest.Issuer, method, path, body string) (int, []byte) {
	t.Helper()
	token := ""
	if iss != nil {
		token = iss.Token()
	}
	return r.callToken(t, token, method, path, body)
}

// callToken sends one request with the given bearer token ("" sends none).
func (r *running) callToken(t *testing.T, token, method, path, body string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, r.base+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// The composition root mounts the public API behind the identity middleware,
// over one package source: an application with nothing installed is not active,
// whatever its token says.
func TestServeMountsThePublicAPI(t *testing.T) {
	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	iss := newHostIssuer(t)
	cfg, _ := serveConfig(t, f, identityYAML)
	provisionIssuer(t, cfg, iss)
	srv := startServe(t, cfg)

	// No token, a stranger's token: refused before any handler.
	if code, body := srv.call(t, nil, http.MethodGet, "/v1/health", ""); code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/health without a token = %d: %s", code, body)
	}
	stranger := identitytest.NewIssuer(t, identitytest.IssuerConfig{ApplicationID: hostApp, Clock: clock.Real{}, Audience: hostAudience, KeyID: "stranger"})
	if code, _ := srv.call(t, stranger, http.MethodPost, "/v1/sessions", ""); code != http.StatusUnauthorized {
		t.Fatalf("a token from an unregistered key = %d", code)
	}

	code, body := srv.call(t, iss, http.MethodGet, "/v1/health", "")
	if code != http.StatusOK {
		t.Fatalf("GET /v1/health = %d: %s", code, body)
	}
	var health gateway.HealthBody
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatal(err)
	}
	if !health.Ready || health.Application.ApplicationID != hostApp || health.Application.Status != "not_installed" {
		t.Errorf("health = %+v", health)
	}
	if strings.Contains(string(body), "dep_cli") {
		t.Error("the deployment id is in a response")
	}

	// The package source is the one the engine, the registry and the coordinator
	// share: nothing installed is nothing active, so no traffic.
	code, body = srv.call(t, iss, http.MethodPost, "/v1/sessions", "")
	var env contracts.Error
	if err := json.Unmarshal(body, &env); err != nil || code != http.StatusForbidden || env.Code != contracts.ErrQuarantined {
		t.Fatalf("POST /v1/sessions for an application with no package = %d: %s", code, body)
	}
	if code, body := srv.call(t, iss, http.MethodGet, "/v1/applications/"+hostApp, ""); code != http.StatusOK || !strings.Contains(string(body), `"not_installed"`) {
		t.Fatalf("GET /v1/applications/%s = %d: %s", hostApp, code, body)
	}
	// Registration needs a package signed by a key the operator registered.
	if code, _ := srv.call(t, iss, http.MethodPost, "/v1/applications", `{"path":"/etc"}`); code != http.StatusForbidden {
		t.Fatalf("registration without the capability scope = %d", code)
	}

	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(srv.stderr.String(), `"api":true`) {
		t.Errorf("the startup line does not say the API is on:\n%s", srv.stderr.String())
	}
}

// With no identity configured the /v1 API refuses everything: the routes exist
// and none of them reaches a handler.
func TestServeRefusesTheAPIWithoutIdentity(t *testing.T) {
	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	cfg, _ := serveConfig(t, f, "")
	srv := startServe(t, cfg)
	other := identitytest.NewIssuer(t, identitytest.IssuerConfig{ApplicationID: "app_demo", Clock: clock.Real{}})
	code, body := srv.call(t, other, http.MethodPost, "/v1/sessions", "")
	var env contracts.Error
	if err := json.Unmarshal(body, &env); err != nil || code != http.StatusServiceUnavailable || env.Code != contracts.ErrUnavailable {
		t.Fatalf("POST /v1/sessions with no identity configured = %d: %s", code, body)
	}
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(srv.stderr.String(), "identity.deployment_id is not configured") || !strings.Contains(srv.stderr.String(), `"api":false`) {
		t.Errorf("the log does not say the API is off:\n%s", srv.stderr.String())
	}
}

const interruptedPrepared = `{"preparation_id":"prep_01M3Y7RKG0XFVC2RXX1YSN2CB7","capability_id":"items.create","state":"confirmed","summary":"Create an item named \"widget\"","effects":[{"operation":"create","entity_type":"item","description":"Create an item named \"widget\".","changes":[{"field":"name","value":{"kind":"text","value":"widget"}}],"external":false}],"risk_level":2,"data_versions":[{"entity_type":"item_collection","entity_id":"ten_acme:wsp_main","version":"v0"}],"prepared_at":"2026-10-02T12:00:00.000Z","expires_at":"2026-10-02T12:05:00.000Z","invalidation_conditions":["data_version_changed","session_ended"],"confirmation":{"mode":"explicit","bindings":["preparation_id","principal","session","data_versions"]},"idempotency_key":"idem_01M3Y7RKG0XFVC2RXX20SF770M"}`

// SEC-046: a commit that a dead process left in `committing` is resolved
// before the service accepts a request, by asking the host and never by
// committing again; with no adapter to ask, the outcome is uncertain.
func TestServeResolvesInterruptedCommitsBeforeServing(t *testing.T) {
	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	cfg, dataDir := serveConfig(t, f, "")
	dbPath := filepath.Join(dataDir, store.DefaultFileName)

	// A previous process: it wrote `committing` (with its audit event, which
	// this fixture skips) and died before it recorded an outcome.
	scope := contracts.Scope{DeploymentID: "dep_cli", ApplicationID: "app_demo", TenantID: "ten_acme", WorkspaceID: "wsp_main",
		PrincipalID: "usr_alice", Roles: []string{"member"}, CapabilityScopes: []string{"items.create"}, SessionID: "ses_dead_01"}
	var pa contracts.PreparedAction
	if err := json.Unmarshal([]byte(interruptedPrepared), &pa); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	seed := func() {
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		db, err := store.Open(context.Background(), store.Options{Path: dbPath})
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		scopeJSON, _ := json.Marshal(scope)
		past := time.Now().Add(-time.Hour).UnixMilli()
		_, err = store.InsertScoped(context.Background(), db, scope, "action_preparations",
			[]string{"id", "principal_id", "session_id", "scope_json", "capability_id", "state", "risk_level", "confirmation_mode", "confirmation_mac",
				"idempotency_key", "fields_json", "prepared_json", "snapshot_version", "data_digest", "prepared_at", "expires_at", "confirmed_at",
				"committing_at", "committing_from", "commit_attempts", "updated_at"},
			string(pa.PreparationID), scope.PrincipalID, scope.SessionID, string(scopeJSON), "items.create", "committing", 2, "explicit", strings.Repeat("a", 64),
			string(pa.IdempotencyKey), `{"name":"widget","quantity":3}`, interruptedPrepared, "v0", strings.Repeat("b", 64),
			past, past+300000, past, past, "confirmed", 1, past)
		if err != nil {
			t.Fatal(err)
		}
	}
	seed()

	srv := startServe(t, cfg)
	// The listening line is printed after the recovery: the row is already
	// resolved when the first request could have been served.
	state := func() (string, string) {
		db, err := store.Open(context.Background(), store.Options{Path: dbPath})
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var st string
		var rc sql.NullString
		if err := db.QueryRowContext(context.Background(), `SELECT state, receipt_id FROM action_preparations`).Scan(&st, &rc); err != nil {
			t.Fatal(err)
		}
		return st, rc.String
	}
	st, receipt := state()
	if st != "uncertain" || receipt == "" {
		t.Fatalf("after startup the interrupted commit is %q (receipt %q), want uncertain with a receipt", st, receipt)
	}
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(srv.stderr.String(), "interrupted commits resolved before serving") {
		t.Errorf("the recovery is not logged:\n%s", srv.stderr.String())
	}
}

// The periodic sweeps run as service jobs: a failing or panicking job does not
// stop the service or the other jobs, and they stop with the context.
func TestServiceJobsRunAndSurviveFailure(t *testing.T) {
	var good, bad, boom atomic.Int64
	logs := &syncBuf{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	wait := startServiceJobs(ctx, log,
		serviceJob{name: "good", every: 5 * time.Millisecond, run: func(context.Context) error { good.Add(1); return nil }},
		serviceJob{name: "bad", every: 5 * time.Millisecond, run: func(context.Context) error {
			bad.Add(1)
			return contracts.New(contracts.ErrUnavailable, "the store is busy")
		}},
		serviceJob{name: "boom", every: 5 * time.Millisecond, run: func(context.Context) error { boom.Add(1); panic("a job bug") }},
	)
	deadline := time.Now().Add(10 * time.Second)
	for (good.Load() < 3 || bad.Load() < 3 || boom.Load() < 3) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the service jobs did not stop with the context")
	}
	if good.Load() < 3 || bad.Load() < 3 || boom.Load() < 3 {
		t.Fatalf("runs: good %d, bad %d, boom %d: a failing job stopped the others", good.Load(), bad.Load(), boom.Load())
	}
	for _, want := range []string{`"job":"bad"`, `"code":"unavailable"`, `"job":"boom"`, "service job panicked"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log lacks %s:\n%s", want, logs.String())
		}
	}
	// Both sweeps of the coordinator are scheduled, on their own periods.
	w := &v1Wiring{}
	names := map[string]bool{}
	for _, j := range w.serviceJobs(log) {
		names[j.name] = true
		if j.every <= 0 {
			t.Errorf("job %s has no period", j.name)
		}
	}
	if !names["actions.recover"] || !names["actions.expire_due"] || len(names) != 2 {
		t.Errorf("service jobs = %v", names)
	}
}

// An audit registry assembled by hand must hold every event type the service
// appends; the core registry (the default) does.
func TestAuditRegistryMustHoldTheActionEventTypes(t *testing.T) {
	if err := checkAuditRegistry(audit.DefaultRegistry); err != nil {
		t.Fatalf("the default registry: %v", err)
	}
	if err := checkAuditRegistry(audit.NewRegistry()); err == nil {
		t.Fatal("an empty registry was accepted")
	}
	partial := audit.NewCoreRegistry()
	if err := checkAuditRegistry(partial); err == nil || !strings.Contains(err.Error(), "dialogue.turn") {
		t.Fatalf("a core registry without the dialogue event type = %v", err)
	}
	for _, name := range []audit.EventType{audit.EventActionPrepared, audit.EventActionConfirmed, audit.EventActionCommitting, audit.EventActionCommitted,
		audit.EventActionRejected, audit.EventActionExpired, audit.EventActionInvalidated, audit.EventActionUncertain} {
		found := false
		for _, r := range requiredAuditTypes {
			found = found || r == name
		}
		if !found {
			t.Errorf("%s is not among the required audit types", name)
		}
	}
}

// With no transport registered for the kind the manifest declares, an
// application has no adapter: unavailable, with the reason, never a default one
// and never another kind's. An application that is not active gets none either,
// and the adapter that is built is the adapters.Wrap wrapper, built once per
// package by the factory registered for the declared kind.
func TestAdapterSourceFailsClosed(t *testing.T) {
	ctx := context.Background()
	reg := transportsSwapDefault(t)
	clk := clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	dataDir := t.TempDir()
	db := storetest.Open(t, nil)
	keys := devTrust{}
	verifier := trust.NewVerifier(keys, clk)
	inst, err := apppkg.NewInstaller(apppkg.InstallerOptions{DB: db, Clock: clk, DataDir: dataDir, Verifier: verifier, CoreVersion: apppkg.DevCoreVersion})
	if err != nil {
		t.Fatal(err)
	}
	apps, err := gateway.NewApplications(inst, apppkg.Options{Verifier: verifier, CoreVersion: apppkg.DevCoreVersion}, clk, 0)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	src := &adapterSource{
		apps: apps, log: slog.New(slog.NewJSONHandler(logs, nil)), cache: map[string]cachedAdapter{},
		transports: transportsSource{installer: inst, dataDir: dataDir},
	}

	// Nothing installed: not active.
	if _, err := src.Adapter(ctx, "demo-library"); contracts.CodeOf(err) != contracts.ErrQuarantined {
		t.Fatalf("an application with nothing installed: %v", err)
	}
	// Installed is quarantined: still not active.
	installed, err := inst.Install(ctx, apppkg.FromDir("../../examples/packages/valid-minimal"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Adapter(ctx, "demo-library"); contracts.CodeOf(err) != contracts.ErrQuarantined {
		t.Fatalf("a quarantined application: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE apppkg_packages SET status = 'active', audit_report_id = 'rep_cli_01' WHERE application_id = 'demo-library'`); err != nil {
		t.Fatal(err)
	}
	// Active, declares an mcp endpoint, no transport registered: unavailable
	// with the reason, never a default adapter, and said in the log.
	_, err = src.Adapter(ctx, "demo-library")
	if reason, kind, ok := transportsUnavailableReason(err); contracts.CodeOf(err) != contracts.ErrUnavailable || !ok ||
		reason != transportsReasonNotRegistered || kind != TransportKindMCP || !strings.Contains(err.Error(), "no adapter transport") {
		t.Fatalf("an active application with no transport: %v", err)
	}
	if !strings.Contains(logs.String(), "no adapter for the application") || !strings.Contains(logs.String(), transportsReasonNotRegistered) {
		t.Errorf("the missing transport is not in the log: %s", logs.String())
	}
	// A transport of ANOTHER kind does not serve an mcp package.
	reg.register(TransportKindOpenAPI, func(context.Context, TransportRequest) (adapters.AppAdapter, error) {
		t.Error("the openapi transport was called for a package that declares mcp")
		return nil, errors.New("must not be called")
	})
	src.forget("demo-library")
	if _, err := src.Adapter(ctx, "demo-library"); contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Fatalf("a transport of another kind: %v", err)
	}
	// The factory of the declared kind gives the wrapped adapter, built once for
	// the package, from what the manifest declares.
	app := fake.NewApp(fake.WithClock(clk))
	opened := 0
	reg.register(TransportKindMCP, func(_ context.Context, req TransportRequest) (adapters.AppAdapter, error) {
		opened++
		if req.ApplicationID != "demo-library" || req.Kind != TransportKindMCP || req.Record.ManifestHash != installed.Record.ManifestHash ||
			req.Record.State != apppkg.StateActive || req.Package.ManifestHash != installed.Record.ManifestHash {
			t.Errorf("the transport was given %s %s %s %s", req.ApplicationID, req.Kind, req.Record.ManifestHash, req.Record.State)
		}
		if len(req.Endpoints) != 1 || req.Endpoints[0].ID != "library-mcp" {
			t.Errorf("the transport was given endpoints %+v", req.Endpoints)
		}
		if _, err := os.Stat(filepath.Join(req.Dir, "alixai.yaml")); err != nil {
			t.Errorf("the install directory %s is not the package's: %v", req.Dir, err)
		}
		return fake.NewAdapter(app), nil
	})
	src.forget("demo-library")
	ad, err := src.Adapter(ctx, "demo-library")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := src.Adapter(ctx, "demo-library")
	if again != ad || opened != 1 {
		t.Fatalf("adapters were built %d times, want once per package", opened)
	}
	// It is the wrapper: a scope of another application is refused before the
	// transport is called.
	other := contracts.Scope{DeploymentID: "d", ApplicationID: "someone-else", TenantID: "t", WorkspaceID: "w", PrincipalID: "p",
		Roles: []string{"r"}, CapabilityScopes: []string{fake.ReadCapability}, SessionID: "s"}
	if _, err := ad.Read(ctx, other, adapters.ReadRequest{CapabilityID: fake.ReadCapability, Fields: map[string]any{"item_id": "x"}}); contracts.CodeOf(err) != contracts.ErrScopeViolation {
		t.Fatalf("the adapter did not refuse another application's scope: %v", err)
	}
	if len(app.Calls()) != 0 {
		t.Fatal("the transport was called for a scope of another application")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("a transport registered twice did not panic")
			}
		}()
		reg.register(TransportKindMCP, func(context.Context, TransportRequest) (adapters.AppAdapter, error) { return nil, nil })
	}()
}

// devTrust trusts the published TEST-ONLY publisher key for every application.
type devTrust struct{}

func (devTrust) LookupKey(_ context.Context, app, id string) (trust.Key, error) {
	if id != devkeys.KeyID {
		return trust.Key{}, trust.ErrUnknownKey
	}
	return trust.Key{ApplicationID: app, KeyID: devkeys.KeyID, PublicKey: devkeys.PublicKey(), Status: trust.KeyActive}, nil
}

// ---- the checkpoint signing key and the issuer keys (M1-22) -----------------

// refusedServe runs serve with cfgPath and returns the error it stopped with. A
// serve that did start is stopped after a moment and reported as a failure by
// the caller (the error is nil and stdout says it listened).
func refusedServe(t *testing.T, cfgPath string) (err error, stdout, stderr string) {
	t.Helper()
	var out, errOut syncBuf
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = runServe(ctx, serveEnv(t, &out, &errOut), []string{"-config", cfgPath})
	return err, out.String(), errOut.String()
}

// pollUntil retries cond until it holds or the time is up.
func pollUntil(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A checkpoint signing key that is configured (or sits at the default place)
// and cannot be used stops serve from starting: a service that was told to sign
// never quietly runs without signing. The refusal comes before anything is
// listened on, and for a key file that cannot be read, before the store is
// touched. (A chmod 000 cannot be tested on Windows; a missing file, a
// directory, text that is not a key and a key of the wrong type are the
// unreadable cases that run everywhere.)
func TestServeRefusesUnreadableCheckpointKey(t *testing.T) {
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))

	ecdsaPEM := func() []byte {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	}()

	cases := []struct {
		name string
		// setup returns the config extra for the keys section.
		setup    func(t *testing.T, dir string) string
		wantText string
		// beforeStore: nothing may have been created in the data directory.
		beforeStore bool
	}{
		{"configured file is missing", func(t *testing.T, dir string) string {
			return "keys:\n  checkpoint_key_file: " + yamlPath(filepath.Join(dir, "nope.key")) + "\n"
		}, "cannot be used", true},
		{"configured file is a directory", func(t *testing.T, dir string) string {
			p := filepath.Join(dir, "adir.key")
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
			return "keys:\n  checkpoint_key_file: " + yamlPath(p) + "\n"
		}, "not a regular file", true},
		{"configured file is not a key", func(t *testing.T, dir string) string {
			p := filepath.Join(dir, "junk.key")
			if err := os.WriteFile(p, []byte("this is not a key\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return "keys:\n  checkpoint_key_file: " + yamlPath(p) + "\n"
		}, "no PKCS#8 PRIVATE KEY PEM block", true},
		{"configured key is not Ed25519", func(t *testing.T, dir string) string {
			p := filepath.Join(dir, "ecdsa.key")
			if err := os.WriteFile(p, ecdsaPEM, 0o600); err != nil {
				t.Fatal(err)
			}
			return "keys:\n  checkpoint_key_file: " + yamlPath(p) + "\n"
		}, "not an Ed25519 key", true},
		{"configured key is readable by other users", func(t *testing.T, dir string) string {
			if runtime.GOOS == "windows" {
				t.Skip("file modes are advisory on Windows")
			}
			_, priv, err := trust.GenerateKey()
			if err != nil {
				t.Fatal(err)
			}
			p, _, err := trust.WriteKeyFiles(dir, "loose", priv)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(p, 0o644); err != nil {
				t.Fatal(err)
			}
			return "keys:\n  checkpoint_key_file: " + yamlPath(p) + "\n"
		}, "accessible by other users", true},
		{"configured key file that cannot be opened", func(t *testing.T, dir string) string {
			if runtime.GOOS == "windows" || os.Geteuid() == 0 {
				t.Skip("a chmod 000 file is still readable on Windows and to root")
			}
			_, priv, err := trust.GenerateKey()
			if err != nil {
				t.Fatal(err)
			}
			p, _, err := trust.WriteKeyFiles(dir, "sealed", priv)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(p, 0); err != nil {
				t.Fatal(err)
			}
			return "keys:\n  checkpoint_key_file: " + yamlPath(p) + "\n"
		}, "cannot be used", true},
		{"a valid key that is not registered in the trust store", func(t *testing.T, dir string) string {
			_, priv, err := trust.GenerateKey()
			if err != nil {
				t.Fatal(err)
			}
			p, _, err := trust.WriteKeyFiles(dir, "unregistered", priv)
			if err != nil {
				t.Fatal(err)
			}
			return "keys:\n  checkpoint_key_file: " + yamlPath(p) + "\n"
		}, "not registered in the trust store", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			keyDir := filepath.Join(t.TempDir(), "keyfiles")
			if err := os.MkdirAll(keyDir, 0o700); err != nil {
				t.Fatal(err)
			}
			cfg, dataDir := serveConfig(t, f, c.setup(t, keyDir))
			err, stdout, stderr := refusedServe(t, cfg)
			if err == nil || !strings.Contains(err.Error(), c.wantText) {
				t.Fatalf("serve = %v, want a refusal containing %q\nstderr: %s", err, c.wantText, stderr)
			}
			if strings.Contains(stdout, "listening on") {
				t.Fatalf("serve listened although its checkpoint key is unusable:\n%s", stdout)
			}
			if c.beforeStore {
				if _, statErr := os.Stat(filepath.Join(dataDir, store.DefaultFileName)); statErr == nil {
					t.Error("the store was created for a service that refused to start")
				}
			}
			if strings.Contains(err.Error(), "BEGIN") || strings.Contains(stderr, "BEGIN") {
				t.Errorf("a refusal quotes key material: %v", err)
			}
		})
	}

	// A key that keys init made and registered, then damaged, swapped or revoked.
	t.Run("a provisioned key whose file was replaced by junk", func(t *testing.T) {
		f := newFixture(t)
		cfg, dataDir := serveConfig(t, f, "")
		if code, out, errOut := runCLI(t, "keys", "init", "-config", cfg); code != ExitOK {
			t.Fatalf("keys init = %d\n%s\n%s", code, out, errOut)
		}
		// No keys block at all: the default place counts as configured once the file is there.
		if err := os.WriteFile(filepath.Join(dataDir, "keys", defaultCheckpointKeyID+".key"), []byte("junk"), 0o600); err != nil {
			t.Fatal(err)
		}
		err, stdout, _ := refusedServe(t, cfg)
		if err == nil || strings.Contains(stdout, "listening on") {
			t.Fatalf("serve with a damaged default key file = %v\n%s", err, stdout)
		}
	})
	t.Run("a provisioned key that was revoked in the trust store", func(t *testing.T) {
		f := newFixture(t)
		cfg, dataDir := serveConfig(t, f, "")
		if code, out, errOut := runCLI(t, "keys", "init", "-config", cfg); code != ExitOK {
			t.Fatalf("keys init = %d\n%s\n%s", code, out, errOut)
		}
		if err := trust.NewStore(openData(t, dataDir), clock.Real{}).RevokeKey(context.Background(), trust.PlatformApplicationID, defaultCheckpointKeyID, "test"); err != nil {
			t.Fatal(err)
		}
		err, stdout, _ := refusedServe(t, cfg)
		if err == nil || !strings.Contains(err.Error(), "revoked") || strings.Contains(stdout, "listening on") {
			t.Fatalf("serve with a revoked checkpoint key = %v\n%s", err, stdout)
		}
	})
	t.Run("a key file that is not the registered key", func(t *testing.T) {
		f := newFixture(t)
		cfg, dataDir := serveConfig(t, f, "")
		if code, out, errOut := runCLI(t, "keys", "init", "-config", cfg); code != ExitOK {
			t.Fatalf("keys init = %d\n%s\n%s", code, out, errOut)
		}
		// Another key under the registered id: the file no longer matches the trust store.
		_, other, err := trust.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		pemBytes, err := trust.MarshalPrivateKeyPEM(other)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, "keys", defaultCheckpointKeyID+".key"), pemBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		err, stdout, _ := refusedServe(t, cfg)
		if err == nil || !strings.Contains(err.Error(), "does not match") || strings.Contains(stdout, "listening on") {
			t.Fatalf("serve with a swapped key file = %v\n%s", err, stdout)
		}
	})
}

// With no checkpoint key provisioned and none configured, serve runs, and says
// that checkpoints are not signed.
func TestServeWithoutACheckpointKeyRunsUnsignedAndSaysSo(t *testing.T) {
	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	cfg, _ := serveConfig(t, f, "")
	srv := startServe(t, cfg)
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(srv.stderr.String(), "audit checkpoints are not signed") {
		t.Errorf("the log does not say checkpoints are unsigned:\n%s", srv.stderr.String())
	}
}

// The end of the provisioning path: an operator adds the public key of the host
// application, and a token that host mints with the private half is accepted;
// a token of a key nobody provisioned is not, and neither is one from a key the
// operator revoked, from the next request on and without restarting serve
// (SEC-037). Neither test hook of the composition root is used.
func TestServeAcceptsTokenFromProvisionedIssuerKey(t *testing.T) {
	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	iss := newHostIssuer(t)
	cfg, dataDir := serveConfig(t, f, identityYAML)

	// Nothing is provisioned: nothing is accepted, and serve says why.
	bare := startServe(t, cfg)
	if code, body := bare.call(t, iss, http.MethodGet, "/v1/health", ""); code != http.StatusUnauthorized {
		t.Fatalf("a token before any issuer key is provisioned = %d: %s", code, body)
	}
	if err := bare.stop(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bare.stderr.String(), "no issuer key is provisioned") {
		t.Errorf("serve does not warn that no issuer key is provisioned:\n%s", bare.stderr.String())
	}

	provisionIssuer(t, cfg, iss)
	srv := startServe(t, cfg)
	if code, body := srv.call(t, iss, http.MethodGet, "/v1/health", ""); code != http.StatusOK {
		t.Fatalf("a token of the provisioned key = %d: %s", code, body)
	}
	// A different key under an id nobody provisioned is refused.
	stranger := identitytest.NewIssuer(t, identitytest.IssuerConfig{ApplicationID: hostApp, Issuer: hostIssuer, KeyID: "not-provisioned", Audience: hostAudience, Clock: clock.Real{}})
	if code, _ := srv.call(t, stranger, http.MethodGet, "/v1/health", ""); code != http.StatusUnauthorized {
		t.Fatalf("a token of an unprovisioned key id = %d", code)
	}
	// The same id signed by another private key is refused too: the registered
	// public key decides, not the id.
	forger := identitytest.NewIssuer(t, identitytest.IssuerConfig{ApplicationID: hostApp, Issuer: hostIssuer, KeyID: hostKeyID, Audience: hostAudience, Clock: clock.Real{}})
	if code, _ := srv.call(t, forger, http.MethodGet, "/v1/health", ""); code != http.StatusUnauthorized {
		t.Fatalf("a token signed by another private key under the provisioned id = %d", code)
	}
	// A token for another application, signed by this host's key, names a scope the key does not speak for.
	other := iss.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimApplication, "app_other") })
	if code, _ := srv.callToken(t, other, http.MethodGet, "/v1/health", ""); code != http.StatusUnauthorized {
		t.Fatalf("a token that claims another application = %d", code)
	}

	// The operator lists and revokes the key from another process, while serve runs.
	code, out, errOut := runCLI(t, "identity", "list", "-config", cfg)
	if code != ExitOK || !strings.Contains(out, hostKeyID) || !strings.Contains(out, "active") {
		t.Fatalf("identity list = %d\n%s\n%s", code, out, errOut)
	}
	code, out, errOut = runCLI(t, "identity", "revoke-issuer-key", "-config", cfg, "-app", hostApp, "-key-id", hostKeyID, "-reason", "test rotation")
	if code != ExitOK || !strings.Contains(out, "revoked") {
		t.Fatalf("identity revoke-issuer-key = %d\n%s\n%s", code, out, errOut)
	}
	if code, body := srv.call(t, iss, http.MethodGet, "/v1/health", ""); code != http.StatusUnauthorized {
		t.Fatalf("a token of a revoked key = %d: %s", code, body)
	}
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}

	// Both changes are in the audit log, on the application's chain, with no key bytes.
	db := openData(t, dataDir)
	rows, err := db.QueryContext(context.Background(), `SELECT event_type, data FROM audit_events WHERE application_id = ? AND tenant_id = '' ORDER BY chain_seq`, hostApp)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var typ, data string
		if err := rows.Scan(&typ, &data); err != nil {
			t.Fatal(err)
		}
		types = append(types, typ)
		if strings.Contains(data, trust.EncodePublicKey(iss.Public)) || strings.Contains(data, "BEGIN") {
			t.Errorf("an audit event of %s carries key material: %s", typ, data)
		}
		if typ == "identity.issuer_key_revoked" && !strings.Contains(data, "test rotation") {
			t.Errorf("the revocation event lacks the reason: %s", data)
		}
	}
	if strings.Join(types, ",") != "identity.issuer_key_added,identity.issuer_key_revoked" {
		t.Errorf("audit events of the application = %v", types)
	}
}

// Signed chain-head checkpoints are produced by serve with the provisioned key,
// and the audit log verifies them: the checkpoint is anchored in the trust store
// `keys init` registered the key in (SEC-086).
func TestServeSignsAuditCheckpoints(t *testing.T) {
	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	iss := newHostIssuer(t)
	cfg, dataDir := serveConfig(t, f, identityYAML+"keys:\n  checkpoint_interval: 100ms\n")
	if code, out, errOut := runCLI(t, "keys", "init", "-config", cfg); code != ExitOK {
		t.Fatalf("keys init = %d\n%s\n%s", code, out, errOut)
	}
	// One event, appended by the CLI (which holds no signing key): the head of the chain.
	provisionIssuer(t, cfg, iss)

	srv := startServe(t, cfg)
	chain := audit.ChainKey{ApplicationID: hostApp}
	db := openData(t, dataDir)
	alg, err := audit.New(db, audit.Options{Verifier: trust.NewVerifier(trust.NewStore(db, clock.Real{}), clock.Real{})})
	if err != nil {
		t.Fatal(err)
	}
	pollUntil(t, 20*time.Second, "the periodic sweep to sign the chain head", func() bool {
		rep, err := alg.VerifyChain(context.Background(), chain, audit.VerifyOptions{})
		return err == nil && rep.Anchored
	})

	// A second event while serve runs: the next sweep, or the one at shutdown, anchors it.
	code, out, errOut := runCLI(t, "identity", "revoke-issuer-key", "-config", cfg, "-app", hostApp, "-key-id", hostKeyID)
	if code != ExitOK {
		t.Fatalf("identity revoke-issuer-key = %d\n%s\n%s", code, out, errOut)
	}
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	rep, err := alg.VerifyChain(context.Background(), chain, audit.VerifyOptions{RequireAnchor: true})
	if err != nil || !rep.OK() || !rep.Anchored || rep.Events != 2 || rep.AnchorSeq != 2 || rep.EventsAfterAnchor != 0 {
		t.Fatalf("after shutdown the chain is %+v (%v), want two events anchored by a verified checkpoint at 2", rep, err)
	}
	if !strings.Contains(srv.stderr.String(), `"audit checkpoints are signed"`) {
		t.Errorf("the startup log does not say checkpoints are signed:\n%s", srv.stderr.String())
	}
}

// The checkpoint at shutdown is signed once the jobs and services have stopped:
// what one appends while it winds down is anchored too, not left unsigned.
func TestServeFinalCheckpointFollowsTheServicesStopping(t *testing.T) {
	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	iss := newHostIssuer(t)
	cfg, dataDir := serveConfig(t, f, identityYAML+"keys:\n  checkpoint_interval: 100ms\n")
	if code, out, errOut := runCLI(t, "keys", "init", "-config", cfg); code != ExitOK {
		t.Fatalf("keys init = %d\n%s\n%s", code, out, errOut)
	}
	provisionIssuer(t, cfg, iss)
	registerServeProvider("zz_late_writer", func(_ context.Context, d serveDeps) error {
		d.AddService(func(context.Context) func() {
			return func() {
				loc, err := audit.ForApplication(hostApp, "late-writer")
				if err == nil {
					_, err = d.Audit.AppendAt(context.Background(), loc, audit.NewEvent{Type: audit.EventSettingsChanged, Data: map[string]any{"setting": "test.late"}})
				}
				if err != nil {
					t.Errorf("the late append: %v", err)
				}
			}
		})
		return nil
	})
	t.Cleanup(func() {
		serveProvidersMu.Lock()
		delete(serveProviders, "zz_late_writer")
		serveProvidersMu.Unlock()
	})

	srv := startServe(t, cfg)
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	db := openData(t, dataDir)
	alg, err := audit.New(db, audit.Options{Verifier: trust.NewVerifier(trust.NewStore(db, clock.Real{}), clock.Real{})})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := alg.VerifyChain(context.Background(), audit.ChainKey{ApplicationID: hostApp}, audit.VerifyOptions{RequireAnchor: true})
	if err != nil || !rep.OK() || rep.Events != 2 || rep.AnchorSeq != 2 || rep.EventsAfterAnchor != 0 {
		t.Fatalf("after shutdown the chain is %+v (%v), want both events anchored", rep, err)
	}
}

// The identity section is registered on the registry serve runs with, not only on
// config.Default: an identity block in the file of a test, a tool or a second
// registry is read, and serve reads it through identity.CurrentConfig.
func TestServeRegistersTheIdentitySectionOnOtherRegistries(t *testing.T) {
	env := serveEnv(t, io.Discard, io.Discard)
	have := map[string]bool{}
	for _, name := range env.Config.Sections() {
		have[name] = true
	}
	for _, want := range []string{identity.SectionName, keysSectionName, localaiSectionName, inferenceSectionName, serveProfileSectionName, gateway.SectionName} {
		if !have[want] {
			t.Errorf("section %q is not registered on the registry serve runs with (have %v)", want, env.Config.Sections())
		}
	}
	cfg := writeConfig(t, identityYAML)
	if err := env.Config.LoadFrom(cfg, env.LookupEnv); err != nil {
		t.Fatalf("an identity block was refused: %v", err)
	}
	if got := identity.CurrentConfig(); got.DeploymentID != "dep_cli" || got.Audience != hostAudience {
		t.Errorf("serve would read identity = %+v", got)
	}
}

// A route area's services come from a provider that registers itself, and one
// that fails stops serve from starting: nothing mounts half wired.
func TestServeProvidersAddServicesAndCanRefuseTheStart(t *testing.T) {
	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	cfg, _ := serveConfig(t, f, "")

	var saw serveDeps
	register := func(name string, p serveProvider) {
		registerServeProvider(name, p)
		t.Cleanup(func() {
			serveProvidersMu.Lock()
			delete(serveProviders, name)
			serveProvidersMu.Unlock()
		})
	}
	register("zz_probe", func(_ context.Context, d serveDeps) error {
		saw = d
		return gateway.Provide[probeService](d.Services, probeImpl{})
	})
	srv := startServe(t, cfg)
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	if saw.DB == nil || saw.Clock == nil || saw.Audit == nil || saw.Policy == nil || saw.Providers == nil || saw.Log == nil || saw.DataDir == "" {
		t.Fatalf("the provider was not given the process infrastructure: %+v", saw)
	}
	if saw.Warm == nil {
		t.Error("the provider was not given the tracker that localSelector budgets user turns by")
	}
	if _, ok := gateway.Lookup[probeService](saw.Services); !ok {
		t.Error("what the provider provided is not in the registry")
	}
	if _, ok := gateway.Lookup[gateway.ActionService](saw.Services); !ok {
		t.Error("the services of the public API are not in the registry the provider sees")
	}

	register("zz_failing", func(context.Context, serveDeps) error { return errors.New("the probe store is down") })
	err, stdout, _ := refusedServe(t, cfg)
	if err == nil || !strings.Contains(err.Error(), "zz_failing") || !strings.Contains(err.Error(), "the probe store is down") || strings.Contains(stdout, "listening on") {
		t.Fatalf("serve with a failing provider = %v\n%s", err, stdout)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("a provider registered twice did not panic")
			}
		}()
		registerServeProvider("zz_failing", func(context.Context, serveDeps) error { return nil })
	}()
}

type probeService interface{ Probe() string }

type probeImpl struct{}

func (probeImpl) Probe() string { return "ok" }

func TestDescribeUnmounted(t *testing.T) {
	got := describeUnmounted(map[string][]string{"v1_rag": {"rag.Service"}, "v1_files": {"files.Store", "gateway.Scanner"}})
	if got != "v1_files (needs files.Store, gateway.Scanner); v1_rag (needs rag.Service)" {
		t.Errorf("describeUnmounted = %q", got)
	}
}

// The published TEST-ONLY keys that internal/identity refuses as issuer keys are
// exactly the ones internal/trust refuses as publisher keys. identity may not
// import trust (the module map), so it keeps its own list; this composition-root
// test is where the two are compared. The trust side is read from the source of
// internal/trust/testonly.go (every base64 public key literal in it), not from
// two constants named here, so a key added to either list and not the other
// fails this test.
func TestPublishedTestOnlyKeysMatchTrust(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "trust", "testonly.go"))
	if err != nil {
		t.Fatal(err)
	}
	literal := regexp.MustCompile(`"([A-Za-z0-9+/]{43}=)"`)
	var trustKeys []string
	for _, m := range literal.FindAllStringSubmatch(string(src), -1) {
		trustKeys = append(trustKeys, m[1])
	}
	if len(trustKeys) < 2 {
		t.Fatalf("found %d public key literals in internal/trust/testonly.go; the extraction is broken", len(trustKeys))
	}
	sort.Strings(trustKeys)
	identityKeys := identity.PublishedTestOnlyKeys()
	sort.Strings(identityKeys)
	if strings.Join(identityKeys, ",") != strings.Join(trustKeys, ",") {
		t.Errorf("identity lists %v, trust lists %v", identityKeys, trustKeys)
	}
	for _, b64 := range trustKeys {
		pub, err := trust.DecodePublicKey(b64)
		if err != nil {
			t.Fatal(err)
		}
		if !trust.IsTestOnlyKey(pub) || !identity.IsPublishedTestOnlyKey(pub) {
			t.Errorf("trust key %s is not refused by both packages", b64)
		}
	}
	// And the key the fixtures and reference apps sign with is on the list.
	if !identity.IsPublishedTestOnlyKey(devkeys.PublicKey()) {
		t.Error("the tools/devkeys key is not refused as an issuer key")
	}
}
