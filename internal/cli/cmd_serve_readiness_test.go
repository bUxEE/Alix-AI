package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/adapters/localai"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/models/registry"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// serveWarmFreeTextModel is a controller name that is not a registry id. The rule
// that localai.models.controller is a verified registry id is on in every build
// and every fixture; the tests that configure this name do it to see it refused.
const serveWarmFreeTextModel = "my-local-model"

// serveReadinessGen is a controller the tests drive: it records the time left on
// the deadline each call arrived with, and its first call can be slow (a cold
// model load).
type serveReadinessGen struct {
	first time.Duration
	err   error

	mu        sync.Mutex
	calls     int
	remaining []time.Duration
	scopes    []contracts.Scope
}

func (g *serveReadinessGen) Chat(ctx context.Context, scope contracts.Scope, req providers.ChatRequest) (providers.ChatResult, error) {
	g.mu.Lock()
	g.calls++
	n := g.calls
	left := time.Duration(-1)
	if dl, ok := ctx.Deadline(); ok {
		left = time.Until(dl)
	}
	g.remaining = append(g.remaining, left)
	g.scopes = append(g.scopes, scope)
	g.mu.Unlock()
	if g.err != nil {
		return providers.ChatResult{}, g.err
	}
	if n == 1 && g.first > 0 {
		select {
		case <-time.After(g.first):
		case <-ctx.Done():
			return providers.ChatResult{}, ctx.Err()
		}
	}
	return providers.ChatResult{Text: "ready", FinishReason: providers.FinishStop}, nil
}

func (g *serveReadinessGen) ChatStream(context.Context, contracts.Scope, providers.ChatRequest) (providers.ChatStream, error) {
	return nil, errors.New("serveReadinessGen: streaming is not used")
}

func (g *serveReadinessGen) snapshot() (calls int, remaining []time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls, append([]time.Duration(nil), g.remaining...)
}

func serveReadinessShippedRegistry(t *testing.T) *models.Registry {
	t.Helper()
	mr, err := models.LoadFS(registry.FS)
	if err != nil {
		t.Fatal(err)
	}
	return mr
}

// serveReadinessWiring is serveReadinessInput over a registry whose controller
// is gen, and what the provider handed to its callbacks.
type serveReadinessWiring struct {
	in       serveReadinessInput
	checks   []gateway.Check
	services []func(ctx context.Context) (stop func())
}

func newServeReadinessWiring(t *testing.T, gen providers.TextGenerator, controller string, env map[string]string) *serveReadinessWiring {
	t.Helper()
	reg := providers.NewRegistry(clock.Real{})
	if gen != nil {
		if err := reg.Register(providers.RoleController, gen); err != nil {
			t.Fatal(err)
		}
	}
	w := &serveReadinessWiring{}
	w.in = serveReadinessInput{
		log: discardLog(), clk: clock.Real{}, reg: reg,
		localai:      localai.Config{Models: localai.ModelSet{Controller: controller}},
		inference:    inferenceConfig{ModelsDir: t.TempDir(), Models: []string{serveModelID}},
		loadRegistry: func() (*models.Registry, error) { return serveReadinessShippedRegistry(t), nil },
		lookupEnv:    func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		addCheck:     func(c gateway.Check) { w.checks = append(w.checks, c) },
		addService:   func(s func(ctx context.Context) (stop func())) { w.services = append(w.services, s) },
	}
	return w
}

// F4 of the M0-13 spike: the first call to a cold model took 8 to 50 s and the
// gateway's budget is 60 s, so the first end-to-end attempt timed out. The call
// that may have to load the model gets a longer budget than a user turn, and the
// service is not ready until that call has answered.
func TestFirstCallBudgetCoversColdModelLoad(t *testing.T) {
	t.Run("the budget covers the longest cold load the spike measured", func(t *testing.T) {
		base := providers.DefaultBudget()
		first := serveReadinessFirstCallBudget(base)
		if first.Timeout <= 60*time.Second {
			t.Errorf("first-call timeout = %s, want more than the 60 s of a user turn", first.Timeout)
		}
		if first.Timeout < serveReadinessColdLoadMeasured+base.Timeout {
			t.Errorf("first-call timeout = %s, want at least the measured cold load (%s) plus a user-turn budget (%s)",
				first.Timeout, serveReadinessColdLoadMeasured, base.Timeout)
		}
		if first.Timeout > localai.DefaultRequestTimeout {
			t.Errorf("first-call timeout = %s is longer than the adapter's own request timeout (%s), which would cut it short",
				first.Timeout, localai.DefaultRequestTimeout)
		}
		if first.MaxPromptTokens != base.MaxPromptTokens || first.MaxOutputTokens != base.MaxOutputTokens || first.MaxBatchItems != base.MaxBatchItems {
			t.Errorf("only the timeout may change: %+v against %+v", first, base)
		}
	})

	t.Run("a load longer than a user turn fails the user budget and passes the first-call budget", func(t *testing.T) {
		base := providers.DefaultBudget()
		base.Timeout = 60 * time.Millisecond // scaled down: the same shape as 60 s against a 50 s load plus an answer
		gen := func(g *serveReadinessGen) func() (providers.TextGenerator, error) {
			return func() (providers.TextGenerator, error) { return g, nil }
		}

		slow := &serveReadinessGen{first: 150 * time.Millisecond}
		err := serveReadinessWarmUp(gen(slow), base)(context.Background())
		if contracts.CodeOf(err) != contracts.ErrTimeout {
			t.Fatalf("a cold load that outlasts the user-turn budget: %v, want a timeout (the premise of the test)", err)
		}

		slow = &serveReadinessGen{first: 150 * time.Millisecond}
		first := serveReadinessFirstCallBudgetFor(base, 400*time.Millisecond)
		if err := serveReadinessWarmUp(gen(slow), first)(context.Background()); err != nil {
			t.Fatalf("the same load under the first-call budget: %v", err)
		}
		if _, left := slow.snapshot(); len(left) != 1 || left[0] <= base.Timeout {
			t.Errorf("the call arrived with %v left, want more than the user-turn timeout %s", left, base.Timeout)
		}
	})

	t.Run("the service sends the first call with that budget, once, and only when readiness is asked for", func(t *testing.T) {
		gen := &serveReadinessGen{first: 80 * time.Millisecond}
		w := newServeReadinessWiring(t, gen, serveModelID, nil)
		if err := serveReadinessWire(context.Background(), w.in); err != nil {
			t.Fatal(err)
		}
		if len(w.checks) != 1 || w.checks[0].Name != serveReadinessCheckName || len(w.services) != 1 {
			t.Fatalf("wired %d checks (%v) and %d services, want the %q check and one service", len(w.checks), w.checks, len(w.services), serveReadinessCheckName)
		}
		if calls, _ := gen.snapshot(); calls != 0 {
			t.Fatalf("the model was called while wiring (%d): the warm-up is driven by /readyz, not by start", calls)
		}

		ctx, cancel := context.WithCancel(context.Background())
		stop := w.services[0](ctx)
		defer func() { cancel(); stop() }()

		// The check waits for the model (here it loads in 80 ms), then says ready.
		if err := w.checks[0].Fn(ctx); err != nil {
			t.Fatalf("check after a quick load = %v", err)
		}
		for i := 0; i < 3; i++ {
			if err := w.checks[0].Fn(ctx); err != nil {
				t.Fatalf("check %d = %v", i, err)
			}
		}
		calls, left := gen.snapshot()
		if calls != 1 {
			t.Fatalf("model calls = %d, want exactly 1 however often the check ran", calls)
		}
		if left[0] < 2*time.Minute {
			t.Errorf("the warm-up call arrived with %s left, want the first-call budget (more than 2 minutes, a user turn gets 60 s)", left[0])
		}
		if sc := gen.scopes[0]; sc.Validate() != nil {
			t.Errorf("the warm-up scope %+v is not a valid scope: %v", sc, sc.Validate())
		}
	})

	t.Run("a runtime that went away and came back is warmed again", func(t *testing.T) {
		gen := &serveReadinessGen{}
		w := newServeReadinessWiring(t, gen, serveModelID, nil)
		if err := serveReadinessWire(context.Background(), w.in); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		stop := w.services[0](ctx)
		defer func() { cancel(); stop() }()

		if err := w.checks[0].Fn(ctx); err != nil {
			t.Fatal(err)
		}
		// The registry marks the runtime unavailable (a restart), then ok again.
		if err := w.in.reg.SetHealth(providers.RoleController, providers.HealthUnavailable, "restarting"); err != nil {
			t.Fatal(err)
		}
		if err := w.checks[0].Fn(ctx); err == nil {
			t.Fatal("a check reported ready while the runtime was unavailable")
		}
		if err := w.in.reg.SetHealth(providers.RoleController, providers.HealthOK, ""); err != nil {
			t.Fatal(err)
		}
		if err := w.checks[0].Fn(ctx); err != nil {
			t.Fatalf("after the runtime came back: %v", err)
		}
		if calls, _ := gen.snapshot(); calls != 2 {
			t.Errorf("model calls = %d, want 2: the restarted runtime has unloaded the model, so it is warmed again", calls)
		}
	})
}

// F6 of the M0-13 spike: nothing tied localai.models.controller to a verified
// registry id, so a configuration could verify one model file and chat with
// another model's name. The name must be (or be derived from) a verified id.
func TestControllerModelNameDerivesFromVerifiedRegistryID(t *testing.T) {
	mr := serveReadinessShippedRegistry(t)
	const (
		embedder = "granite-embedding-97m-multilingual-r2-q4_k_m"
		other    = "qwen3.5-2b-q4_k_m"
	)
	for _, id := range []string{serveModelID, embedder, other} {
		if _, ok := mr.Get(id); !ok {
			t.Fatalf("the shipped registry has no %q: the test's premise changed", id)
		}
	}

	cases := []struct {
		name       string
		verified   []string
		configured string
		want       string
		wantErr    string
	}{
		{name: "derived when empty and exactly one verified entry is a controller", verified: []string{serveModelID}, want: serveModelID},
		{name: "an embedding model next to it does not make the controller ambiguous", verified: []string{embedder, serveModelID}, want: serveModelID},
		{name: "the verified id itself", verified: []string{serveModelID}, configured: serveModelID, want: serveModelID},
		{name: "free text is refused", verified: []string{serveModelID}, configured: serveWarmFreeTextModel, wantErr: "not a verified registry id"},
		{name: "another registry entry that was not verified is refused", verified: []string{serveModelID}, configured: other, wantErr: "not a verified registry id"},
		{name: "a verified embedding model is not a controller", verified: []string{embedder, serveModelID}, configured: embedder, wantErr: "not a verified registry id"},
		{name: "a name that merely contains the id is refused", verified: []string{serveModelID}, configured: serveModelID + "-v2", wantErr: "not a verified registry id"},
		{name: "case is not folded", verified: []string{serveModelID}, configured: strings.ToUpper(serveModelID), wantErr: "not a verified registry id"},
		{name: "nothing verified", configured: serveModelID, wantErr: "no entry of inference.models is a controller"},
		{name: "empty with no controller to derive from", verified: []string{embedder}, wantErr: "nothing to derive from"},
		{name: "empty with two controllers is ambiguous", verified: []string{serveModelID, other}, wantErr: "several controller models"},
		{name: "an id that is not in the registry at all", verified: []string{"no-such-model"}, configured: "no-such-model", wantErr: "not a verified registry id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := serveReadinessControllerModel(mr, tc.verified, tc.configured)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("error = %v", err)
			case tc.wantErr == "" && got != tc.want:
				t.Fatalf("model = %q, want %q", got, tc.want)
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one containing %q (model %q)", err, tc.wantErr, got)
				}
				if contracts.CodeOf(err) != contracts.ErrValidationFailed {
					t.Errorf("code = %s, want validation_failed", contracts.CodeOf(err))
				}
			}
		})
	}

	t.Run("serve refuses a free-text controller name and accepts the verified id", func(t *testing.T) {
		w := newServeReadinessWiring(t, &serveReadinessGen{}, serveWarmFreeTextModel, nil)
		err := serveReadinessWire(context.Background(), w.in)
		if err == nil || !strings.Contains(err.Error(), "localai.models.controller") || contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Fatalf("free-text controller: %v, want a validation_failed refusal naming localai.models.controller", err)
		}
		if len(w.checks) != 0 || len(w.services) != 0 {
			t.Error("a refused start still wired a check or a service")
		}

		w = newServeReadinessWiring(t, &serveReadinessGen{}, serveModelID, nil)
		if err := serveReadinessWire(context.Background(), w.in); err != nil {
			t.Fatalf("the verified id: %v", err)
		}

		// With the inference path off nothing is registered and nothing is named.
		w = newServeReadinessWiring(t, nil, serveWarmFreeTextModel, nil)
		if err := serveReadinessWire(context.Background(), w.in); err != nil || len(w.checks) != 0 {
			t.Errorf("inference off: %v with %d checks, want no error and no check", err, len(w.checks))
		}
	})

	t.Run("through alixai serve", func(t *testing.T) {
		rt := newServeReadinessRuntime(t, func(w http.ResponseWriter, _ int) { serveReadinessAnswer(w) })
		f := newFixture(t)
		f.rt = &stubRuntime{Server: rt.Server}
		t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
		cfg, _ := serveConfig(t, f, "")
		serveWarmSetController(t, cfg, serveWarmFreeTextModel)

		err, stdout := serveReadinessRefused(t, cfg)
		if err == nil || !strings.Contains(err.Error(), "localai.models.controller") || strings.Contains(stdout, "listening on") {
			t.Fatalf("serve with a free-text controller = %v\n%s", err, stdout)
		}
		if got := rt.chats.Load(); got != 0 {
			t.Errorf("the runtime was asked to generate %d times for a configuration that was refused", got)
		}

		serveWarmSetController(t, cfg, serveModelID)
		srv := startServe(t, cfg)
		// The warm-up may need more than one poll on a loaded host; it must get there.
		pollUntil(t, 20*time.Second, "/readyz with the verified id to turn ready", func() bool {
			code, _, _ := srv.doOnce(t, http.MethodGet, "/readyz", "")
			return code == http.StatusOK
		})
		if err := srv.stop(); err != nil {
			t.Fatal(err)
		}
	})
}

// Resource budgets R3 (M0-15): with fewer active backends than resident models
// every call reloads one (15.2 s for a chat after an embedding, about 70 page
// faults a second). A configuration that keeps more models resident than
// LOCALAI_MAX_ACTIVE_BACKENDS allows is refused at start, and the shipped
// profiles set the value their resident roles need.
func TestMaxActiveBackendsCoversResidentRoles(t *testing.T) {
	ctl := localai.ModelSet{Controller: "granite-4.0-1b-q4_k_m"}
	ctlEmb := localai.ModelSet{Controller: "granite-4.0-1b-q4_k_m", Embedder: "granite-embedding-97m-multilingual-r2-q4_k_m"}
	sharedRouter := localai.ModelSet{Controller: "granite-4.0-1b-q4_k_m", Router: "granite-4.0-1b-q4_k_m", Embedder: "granite-embedding-97m-multilingual-r2-q4_k_m"}
	three := localai.ModelSet{Controller: "a", Router: "b", Embedder: "c"}

	cases := []struct {
		name     string
		set      localai.ModelSet
		raw      string
		present  bool
		wantErr  string
		wantNote string
	}{
		{name: "one model, one backend", set: ctl, raw: "1", present: true},
		{name: "controller and embedder with one backend", set: ctlEmb, raw: "1", present: true, wantErr: "at least 2"},
		{name: "controller and embedder with two", set: ctlEmb, raw: "2", present: true},
		{name: "more than needed is fine", set: ctlEmb, raw: "4", present: true},
		{name: "a router that shares the controller's model needs no slot", set: sharedRouter, raw: "2", present: true},
		{name: "three distinct models with two backends", set: three, raw: "2", present: true, wantErr: "at least 3"},
		{name: "three distinct models with three", set: three, raw: "3", present: true},
		{name: "zero is unlimited and passes with a note", set: ctlEmb, raw: "0", present: true, wantNote: "unlimited"},
		{name: "negative is refused", set: ctlEmb, raw: "-1", present: true, wantErr: "non-negative integer"},
		{name: "not a number is refused", set: ctlEmb, raw: "two", present: true, wantErr: "non-negative integer"},
		{name: "a fraction is refused", set: ctlEmb, raw: "1.5", present: true, wantErr: "non-negative integer"},
		{name: "a signed number is refused", set: ctlEmb, raw: "+2", present: true, wantErr: "non-negative integer"},
		{name: "an absurd number is refused", set: ctlEmb, raw: "99999999999", present: true, wantErr: "non-negative integer"},
		{name: "unset with two models says it cannot check", set: ctlEmb, wantNote: "not set in AlixAI's environment"},
		{name: "set but empty counts as unset", set: ctlEmb, raw: "  ", present: true, wantNote: "not set in AlixAI's environment"},
		{name: "unset with one model is silent", set: ctl},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			note, err := serveReadinessCheckActiveBackends(tc.set, tc.raw, tc.present)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || contracts.CodeOf(err) != contracts.ErrValidationFailed {
					t.Fatalf("error = %v, want a validation_failed containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v", err)
			}
			if (tc.wantNote == "") != (note == "") || !strings.Contains(note, tc.wantNote) {
				t.Errorf("note = %q, want one containing %q", note, tc.wantNote)
			}
		})
	}

	t.Run("serve refuses to start with too few backends, whether or not inference is on", func(t *testing.T) {
		env := map[string]string{serveReadinessActiveBackendsEnv: "1"}
		for _, gen := range []providers.TextGenerator{&serveReadinessGen{}, nil} {
			w := newServeReadinessWiring(t, gen, serveModelID, env)
			w.in.localai.Models.Embedder = "granite-embedding-97m-multilingual-r2-q4_k_m"
			err := serveReadinessWire(context.Background(), w.in)
			if err == nil || !strings.Contains(err.Error(), serveReadinessActiveBackendsEnv) {
				t.Fatalf("inference on=%v: %v, want a refusal naming the setting", gen != nil, err)
			}
			if len(w.checks) != 0 {
				t.Error("a refused start still wired a check")
			}
		}
		w := newServeReadinessWiring(t, &serveReadinessGen{}, serveModelID, map[string]string{serveReadinessActiveBackendsEnv: "2"})
		w.in.localai.Models.Embedder = "granite-embedding-97m-multilingual-r2-q4_k_m"
		if err := serveReadinessWire(context.Background(), w.in); err != nil {
			t.Fatalf("two backends for two models: %v", err)
		}
	})

	t.Run("through alixai serve", func(t *testing.T) {
		f := newFixture(t)
		t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
		cfg, _ := serveConfig(t, f, "")
		raw, err := os.ReadFile(cfg)
		if err != nil {
			t.Fatal(err)
		}
		controllerLine := []byte("    controller: " + serveModelID + "\n")
		if !bytes.Contains(raw, controllerLine) {
			t.Fatalf("the fixture config has no controller line:\n%s", raw)
		}
		raw = bytes.Replace(raw, controllerLine, append(append([]byte(nil), controllerLine...), "    embedder: emb-model\n"...), 1)
		if err := os.WriteFile(cfg, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		old := serveReadinessLookupEnv
		t.Cleanup(func() { serveReadinessLookupEnv = old })
		serveReadinessLookupEnv = func(k string) (string, bool) {
			if k == serveReadinessActiveBackendsEnv {
				return "1", true
			}
			return "", false
		}
		err, stdout := serveReadinessRefused(t, cfg)
		if err == nil || !strings.Contains(err.Error(), serveReadinessActiveBackendsEnv) || strings.Contains(stdout, "listening on") {
			t.Fatalf("serve with 2 resident models and 1 backend = %v\n%s", err, stdout)
		}
	})

	// The shipped profile files (resource-budgets R2: lite keeps the controller
	// and the embedder resident, balanced those and speech to text and to speech,
	// the experimental 4 GB profile the controller only) set a value that covers
	// them, and nothing in .env.example silently overrides it back to 1.
	t.Run("the shipped compose profiles cover their resident roles", func(t *testing.T) {
		dir := filepath.Join("..", "..", "deploy", "container", "localai")
		setting := regexp.MustCompile(`(?m)^\s*LOCALAI_MAX_ACTIVE_BACKENDS:\s*\$\{LOCALAI_MAX_ACTIVE_BACKENDS:-(\d+)\}\s*$`)
		for _, p := range []struct {
			file     string
			resident int // the roles the profile declares resident
		}{
			{"compose.yaml", 2},
			{"compose.lite.yaml", 2},
			{"compose.balanced.yaml", 4},
			{"compose.4g.yaml", 1},
		} {
			raw, err := os.ReadFile(filepath.Join(dir, p.file))
			if err != nil {
				t.Fatal(err)
			}
			m := setting.FindSubmatch(raw)
			if m == nil {
				t.Errorf("%s does not default LOCALAI_MAX_ACTIVE_BACKENDS (want `${LOCALAI_MAX_ACTIVE_BACKENDS:-N}`)", p.file)
				continue
			}
			got, _ := strconv.Atoi(string(m[1]))
			if got < p.resident {
				t.Errorf("%s defaults LOCALAI_MAX_ACTIVE_BACKENDS to %d, below its %d resident roles", p.file, got, p.resident)
			}
			if p.file == "compose.4g.yaml" && got != 1 {
				t.Errorf("the 4g profile keeps only the controller resident: want 1, have %d", got)
			}
		}
		env, err := os.ReadFile(filepath.Join(dir, ".env.example"))
		if err != nil {
			t.Fatal(err)
		}
		if regexp.MustCompile(`(?m)^\s*LOCALAI_MAX_ACTIVE_BACKENDS\s*=`).Match(env) {
			t.Error(".env.example sets LOCALAI_MAX_ACTIVE_BACKENDS: a copied .env would override every profile's default")
		}
	})
}

// serveReadinessRuntime is a LocalAI stand-in whose chat answer the test shapes
// (slow, failing) and counts. Everything else answers as the shared stub does.
type serveReadinessRuntime struct {
	*httptest.Server
	chats atomic.Int32
}

func newServeReadinessRuntime(t *testing.T, chat func(w http.ResponseWriter, n int)) *serveReadinessRuntime {
	t.Helper()
	rt := &serveReadinessRuntime{}
	rt.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/readyz":
			w.WriteHeader(http.StatusOK)
		case "/version":
			_, _ = fmt.Fprintf(w, `{"version":%q}`, serveVersion+" (0123456)")
		case "/v1/models":
			_, _ = fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model"}]}`, serveModelID)
		case "/v1/chat/completions":
			chat(w, int(rt.chats.Add(1)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(rt.Close)
	return rt
}

func serveReadinessAnswer(w http.ResponseWriter) {
	body, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-1", "model": serveModelID,
		"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "ready"}}},
		"usage":   map[string]any{"prompt_tokens": 12, "completion_tokens": 1},
	})
	_, _ = w.Write(body)
}

// serveReadinessRefused runs serve and returns why it refused to start; like refusedServe
// with a longer bound, so a loaded host does not turn a refusal into a timeout.
func serveReadinessRefused(t *testing.T, cfgPath string) (err error, stdout string) {
	t.Helper()
	var out, errOut syncBuf
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = runServe(ctx, serveEnv(t, &out, &errOut), []string{"-config", cfgPath})
	return err, out.String()
}

func serveReadinessReport(t *testing.T, srv *running) (int, gateway.ReadyReport, string) {
	t.Helper()
	code, _, body := srv.doOnce(t, http.MethodGet, "/readyz", "")
	var rep gateway.ReadyReport
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatalf("/readyz = %d, not a ReadyReport: %s", code, body)
	}
	return code, rep, string(body)
}

func serveReadinessCheck(t *testing.T, rep gateway.ReadyReport) gateway.CheckReport {
	t.Helper()
	for _, c := range rep.Checks {
		if c.Name == serveReadinessCheckName {
			return c
		}
	}
	t.Fatalf("/readyz has no %q check: %+v", serveReadinessCheckName, rep.Checks)
	return gateway.CheckReport{}
}

// The whole path through `alixai serve`: nothing is generated until readiness is
// asked for, /readyz then waits a moment for the model, says warming up (503)
// while a cold load is under way, reports a failed model by its code, and never
// calls the model again once it answered.
func TestServeReadyzDrivesTheModelWarmUp(t *testing.T) {
	startWith := func(t *testing.T, rt *serveReadinessRuntime, extra string) *running {
		t.Helper()
		f := newFixture(t)
		f.rt = &stubRuntime{Server: rt.Server}
		t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
		cfg, _ := serveConfig(t, f, extra)
		return startServe(t, cfg)
	}

	t.Run("a fast runtime: ready, with one warm-up call that is never repeated", func(t *testing.T) {
		rt := newServeReadinessRuntime(t, func(w http.ResponseWriter, _ int) { serveReadinessAnswer(w) })
		srv := startWith(t, rt, "gateway:\n  health_ttl: 30ms\n")
		if got := rt.chats.Load(); got != 0 {
			t.Fatalf("the model was called %d times before anyone asked for readiness", got)
		}
		// Normally the first poll is already ready; a starved host may need another.
		var code int
		var rep gateway.ReadyReport
		var body string
		pollUntil(t, 20*time.Second, "/readyz to turn ready", func() bool {
			code, rep, body = serveReadinessReport(t, srv)
			return code == http.StatusOK
		})
		if check := serveReadinessCheck(t, rep); !rep.Ready || !check.OK || check.Reason != "" {
			t.Fatalf("/readyz = %d, check %+v: %s", code, check, body)
		}
		for i := 0; i < 4; i++ {
			time.Sleep(40 * time.Millisecond) // past the report cache, so each poll probes afresh
			if code, _, body := serveReadinessReport(t, srv); code != http.StatusOK {
				t.Fatalf("poll %d = %d: %s", i, code, body)
			}
		}
		if got := rt.chats.Load(); got != 1 {
			t.Errorf("warm-up calls = %d, want 1 however often /readyz is polled", got)
		}
		if err := srv.stop(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a cold load is warming up, then ready, with one call", func(t *testing.T) {
		rt := newServeReadinessRuntime(t, func(w http.ResponseWriter, _ int) {
			time.Sleep(gateway.ReadinessWarmthDefaultWaitFor + 600*time.Millisecond) // longer than one poll waits
			serveReadinessAnswer(w)
		})
		srv := startWith(t, rt, "gateway:\n  health_ttl: 50ms\n")
		code, rep, body := serveReadinessReport(t, srv)
		check := serveReadinessCheck(t, rep)
		if code != http.StatusServiceUnavailable || rep.Ready || check.OK || !strings.Contains(check.Reason, "warming up") {
			t.Fatalf("while the model loads: /readyz = %d, check %+v: %s", code, check, body)
		}
		pollUntil(t, 10*time.Second, "/readyz to turn ready", func() bool {
			c, _, _ := srv.doOnce(t, http.MethodGet, "/readyz", "")
			return c == http.StatusOK
		})
		if got := rt.chats.Load(); got != 1 {
			t.Errorf("warm-up calls = %d, want 1: a poll during the load must join it", got)
		}
		if err := srv.stop(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a model that cannot answer keeps the service not ready and names only the code", func(t *testing.T) {
		rt := newServeReadinessRuntime(t, func(w http.ResponseWriter, _ int) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"failed to load model /srv/SECRET-DIR/x.gguf: invalid magic characters"}}`)
		})
		srv := startWith(t, rt, "gateway:\n  health_ttl: 30ms\n")
		// The failure shows on the first poll, or on the next if the host was slow to run the call.
		var code int
		var rep gateway.ReadyReport
		var body string
		var check gateway.CheckReport
		pollUntil(t, 20*time.Second, "/readyz to report the failed warm-up", func() bool {
			code, rep, body = serveReadinessReport(t, srv)
			check = serveReadinessCheck(t, rep)
			return strings.Contains(check.Reason, "did not answer")
		})
		if code != http.StatusServiceUnavailable || rep.Ready || check.OK {
			t.Fatalf("/readyz = %d, check %+v: %s", code, check, body)
		}
		if strings.Contains(body, "SECRET-DIR") || strings.Contains(body, "invalid magic") {
			t.Errorf("/readyz exposes the runtime's error text: %s", body)
		}
		if err := srv.stop(); err != nil {
			t.Fatal(err)
		}
	})
}

// serveWarmSetController rewrites the controller line of a serve config written
// by serveConfig: to name, or away when name is empty. A config without the line
// is not what the fixture writes, so that is a failure, not a silent no-op.
func serveWarmSetController(t *testing.T, cfgPath, name string) {
	t.Helper()
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	line := regexp.MustCompile(`(?m)^    controller: \S+\n`)
	if len(line.FindAll(raw, -1)) != 1 {
		t.Fatalf("the fixture config has no single controller line:\n%s", raw)
	}
	repl := ""
	if name != "" {
		repl = "    controller: " + name + "\n"
	}
	if err := os.WriteFile(cfgPath, line.ReplaceAll(raw, []byte(repl)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// serveWarmUserTurn runs one user turn the way the dialogue engine does: the
// selector hands out the generator, the generator makes one Chat call. It returns
// the time the model call had left on its deadline (the budget it was given, less
// the microseconds the call took to arrive) and the call's error.
func serveWarmUserTurn(t *testing.T, sel localSelector, gen *serveReadinessGen) (time.Duration, error) {
	t.Helper()
	p, err := sel.Select(context.Background(), serveReadinessScope(), providers.RoleController)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	before, _ := gen.snapshot()
	_, cerr := p.Generator.Chat(context.Background(), serveReadinessScope(), providers.ChatRequest{
		Role:      providers.RoleController,
		Messages:  []providers.Message{{Role: providers.MsgUser, Content: "Make 3 demos"}},
		MaxTokens: 16,
	})
	calls, left := gen.snapshot()
	if calls != before+1 {
		t.Fatalf("the turn made %d model calls, want 1", calls-before)
	}
	return left[len(left)-1], cerr
}

// serveWarmNearBudget says the call arrived with about the whole of budget left.
func serveWarmNearBudget(left, budget time.Duration) bool {
	return left <= budget && left > budget-10*time.Second
}

// LocalAI's idle watchdog is global, so after 15 minutes without a call the
// controller is unloaded and the next user turn loads it again: 8 to 50 s in the
// spike (M0-13 F4), against a user-turn budget of 60 s. That turn gets the same
// budget as the readiness warm-up, and only that turn.
func TestUserTurnAfterIdleUnloadGetsTheColdLoadBudget(t *testing.T) {
	ordinary := providers.DefaultBudget().Timeout
	coldLoad := serveReadinessFirstCallBudget(providers.DefaultBudget()).Timeout
	if coldLoad <= ordinary+serveReadinessColdLoadMeasured {
		t.Fatalf("the cold-load budget %s does not cover a user turn plus the longest load measured (%s): the premise of the test", coldLoad, ordinary+serveReadinessColdLoadMeasured)
	}
	type rig struct {
		sel localSelector
		gen *serveReadinessGen
		clk *clock.Fake
		tr  *serveWarmTracker
		reg *providers.Registry
	}
	newRig := func(t *testing.T) *rig {
		t.Helper()
		r := &rig{gen: &serveReadinessGen{}, clk: clock.NewFake(time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC))}
		r.tr = serveWarmNewTracker(r.clk, nil)
		r.reg = providers.NewRegistry(r.clk)
		if err := r.reg.Register(providers.RoleController, r.gen); err != nil {
			t.Fatal(err)
		}
		r.sel = localSelector{reg: r.reg, budget: providers.DefaultBudget(), warm: r.tr}
		return r
	}
	// turn runs one user turn that must succeed and checks the budget it got.
	turn := func(t *testing.T, r *rig, when string, wantCold bool) {
		t.Helper()
		left, err := serveWarmUserTurn(t, r.sel, r.gen)
		if err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		want, what := ordinary, "the ordinary user-turn budget"
		if wantCold {
			want, what = coldLoad, "the cold-load budget"
		}
		if !serveWarmNearBudget(left, want) {
			t.Errorf("%s: the model call had %s left, want about %s (%s)", when, left, want, what)
		}
	}

	t.Run("the first turn after the start may have to load the model", func(t *testing.T) {
		turn(t, newRig(t), "first turn", true)
	})

	t.Run("a turn after an idle unload gets the cold-load budget, the next one the ordinary budget", func(t *testing.T) {
		r := newRig(t)
		turn(t, r, "first turn", true)
		turn(t, r, "second turn, straight after", false)

		r.clk.Advance(16 * time.Minute) // past LocalAI's 15 minute idle unload
		turn(t, r, "turn after 16 idle minutes", true)
		turn(t, r, "turn right after the model answered again", false)
	})

	t.Run("the window closes before LocalAI's own idle timeout does", func(t *testing.T) {
		r := newRig(t)
		if r.tr.window >= serveWarmDefaultIdleTimeout {
			t.Fatalf("window = %s, not shorter than LocalAI's idle timeout %s: a turn at the edge would get 60 s for a model that was just unloaded",
				r.tr.window, serveWarmDefaultIdleTimeout)
		}
		turn(t, r, "first turn", true)
		r.clk.Advance(r.tr.window - time.Second)
		turn(t, r, "turn just inside the window", false)
		r.clk.Advance(r.tr.window) // exactly the window since the last answer: cold
		turn(t, r, "turn exactly one window after the last answer", true)
	})

	t.Run("a cold turn that failed leaves the next one cold", func(t *testing.T) {
		r := newRig(t)
		r.gen.err = contracts.New(contracts.ErrTimeout, "the model did not load in time")
		left, err := serveWarmUserTurn(t, r.sel, r.gen)
		if err == nil {
			t.Fatal("the failing runtime answered")
		}
		if !serveWarmNearBudget(left, coldLoad) {
			t.Errorf("failing turn had %s left, want about %s", left, coldLoad)
		}
		r.gen.err = nil
		turn(t, r, "turn after the failure", true)
		turn(t, r, "turn after the answer", false)
	})

	t.Run("the warm-up call counts, and a runtime that restarted is cold again", func(t *testing.T) {
		r := newRig(t)
		w := newServeReadinessWiring(t, r.gen, serveModelID, nil)
		w.in.reg, w.in.clk, w.in.warm = r.reg, r.clk, r.tr
		if err := serveReadinessWire(context.Background(), w.in); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		stop := w.services[0](ctx)
		defer func() { cancel(); stop() }()
		if err := w.checks[0].Fn(ctx); err != nil { // /readyz: the warm-up call loads the model and it answers
			t.Fatal(err)
		}
		turn(t, r, "first user turn after a warm-up that answered", false)

		if err := r.reg.SetHealth(providers.RoleController, providers.HealthUnavailable, "restarting"); err != nil {
			t.Fatal(err)
		}
		if err := w.checks[0].Fn(ctx); err == nil {
			t.Fatal("ready while the runtime is unavailable")
		}
		if err := r.reg.SetHealth(providers.RoleController, providers.HealthOK, ""); err != nil {
			t.Fatal(err)
		}
		// Not through /readyz this time: the turn finds the runtime back, with its model unloaded.
		turn(t, r, "turn after the runtime restarted", true)
	})

	t.Run("a selector without a tracker keeps the ordinary budget", func(t *testing.T) {
		r := newRig(t)
		r.sel.warm = nil
		turn(t, r, "turn", false)
	})

	t.Run("LocalAI's idle timeout, when AlixAI can see it, sets the window", func(t *testing.T) {
		clk := clock.NewFake(time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC))
		env := func(v string) func(string) (string, bool) {
			return func(k string) (string, bool) {
				if k != serveWarmIdleEnv || v == "" {
					return "", false
				}
				return v, true
			}
		}
		for _, tc := range []struct {
			name, value string
			want        time.Duration
		}{
			{"not visible: LocalAI's default of 15 minutes", "", 10 * time.Minute},
			{"3 minutes", "3m", 2 * time.Minute},
			{"one hour", "1h", 40 * time.Minute},
			{"not a duration: the default", "soon", 10 * time.Minute},
			{"zero: the default", "0s", 10 * time.Minute},
			{"negative: the default", "-5m", 10 * time.Minute},
		} {
			if got := serveWarmNewTracker(clk, env(tc.value)).window; got != tc.want {
				t.Errorf("%s: window = %s, want %s", tc.name, got, tc.want)
			}
		}
		tr := serveWarmNewTracker(clk, env("3m"))
		tr.touch()
		clk.Advance(2*time.Minute - time.Nanosecond)
		if tr.cold() {
			t.Error("cold inside a 2 minute window")
		}
		clk.Advance(time.Nanosecond)
		if !tr.cold() {
			t.Error("not cold after the window")
		}
	})

	t.Run("a stream counts from its first chunk, not from its start", func(t *testing.T) {
		clk := clock.NewFake(time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC))
		req := providers.ChatRequest{
			Role: providers.RoleController, Messages: []providers.Message{{Role: providers.MsgUser, Content: "hi"}}, MaxTokens: 8,
		}
		tr := serveWarmNewTracker(clk, nil)
		g, err := tr.budgeted(&serveWarmStreamGen{chunks: 2}, providers.DefaultBudget())
		if err != nil {
			t.Fatal(err)
		}
		s, err := g.ChatStream(context.Background(), serveReadinessScope(), req)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if !tr.cold() {
			t.Error("warm before the model produced anything")
		}
		if _, err := s.Recv(); err != nil {
			t.Fatal(err)
		}
		if tr.cold() {
			t.Error("still cold after a chunk arrived")
		}

		failing := serveWarmNewTracker(clk, nil)
		fg, err := failing.budgeted(&serveWarmStreamGen{startErr: errors.New("no model")}, providers.DefaultBudget())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fg.ChatStream(context.Background(), serveReadinessScope(), req); err == nil {
			t.Fatal("the stream that could not start started")
		}
		if !failing.cold() {
			t.Error("a stream that failed to start counted as an answer")
		}
	})
}

// serveWarmStreamGen is a controller that only streams: chunks of text, or an
// error when the stream is opened.
type serveWarmStreamGen struct {
	chunks   int
	startErr error
}

func (g *serveWarmStreamGen) Chat(context.Context, contracts.Scope, providers.ChatRequest) (providers.ChatResult, error) {
	return providers.ChatResult{}, errors.New("serveWarmStreamGen: Chat is not used")
}

func (g *serveWarmStreamGen) ChatStream(context.Context, contracts.Scope, providers.ChatRequest) (providers.ChatStream, error) {
	if g.startErr != nil {
		return nil, g.startErr
	}
	return &serveWarmChunks{left: g.chunks}, nil
}

type serveWarmChunks struct{ left int }

func (s *serveWarmChunks) Recv() (providers.ChatChunk, error) {
	if s.left == 0 {
		return providers.ChatChunk{}, io.EOF
	}
	s.left--
	return providers.ChatChunk{Delta: "x", Done: s.left == 0, FinishReason: providers.FinishStop}, nil
}

func (s *serveWarmChunks) Close() error { return nil }

// M0-18 derived the controller name from the verified registry id when
// localai.models.controller is empty, but startInference refused an empty name
// before that code ran. It derives the name now, builds the client with it, and
// counts it as a resident model; a name that cannot be derived keeps the
// inference path off.
func TestEmptyControllerNameIsDerivedFromTheVerifiedRegistryID(t *testing.T) {
	t.Run("startInference builds the client with the derived name", func(t *testing.T) {
		f := newFixture(t)
		lcfg := f.localai()
		lcfg.Models.Controller = ""
		reg, off := startInference(context.Background(), discardLog(), clock.Real{}, lcfg, f.inference())
		if off != nil {
			t.Fatalf("startInference refused an empty controller name that can be derived: %s: %v", off.reason, off.err)
		}
		// The runtime lists the registry id and nothing else: the controller is usable
		// only if the client looks for the derived name.
		if !reg.Usable(providers.RoleController) {
			t.Fatalf("controller status = %+v, want usable", reg.Status(providers.RoleController))
		}
		gen, err := reg.TextGenerator(providers.RoleController)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := gen.Chat(context.Background(), serveReadinessScope(), providers.ChatRequest{
			Role: providers.RoleController, Messages: []providers.Message{{Role: providers.MsgUser, Content: "hi"}}, MaxTokens: 4,
		}); err != nil {
			t.Fatal(err)
		}
		if got := f.rt.chatModels(); len(got) != 1 || got[0] != serveModelID {
			t.Errorf("the chat named %q, want [%s]", got, serveModelID)
		}
	})

	t.Run("through alixai serve, with no controller line in the configuration", func(t *testing.T) {
		f := newFixture(t)
		t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
		cfg, _ := serveConfig(t, f, "")
		serveWarmSetController(t, cfg, "")
		srv := startServe(t, cfg)
		pollUntil(t, 20*time.Second, "/readyz to turn ready", func() bool {
			code, _, _ := srv.doOnce(t, http.MethodGet, "/readyz", "")
			return code == http.StatusOK
		})
		got := f.rt.chatModels()
		if len(got) == 0 {
			t.Fatal("the model was never called: /readyz is ready without a warm-up")
		}
		for _, m := range got {
			if m != serveModelID {
				t.Errorf("a chat named %q, want %q", m, serveModelID)
			}
		}
		if err := srv.stop(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(srv.stderr.String(), "derived from the verified registry id") {
			t.Errorf("the log does not say that the controller name was derived:\n%s", srv.stderr.String())
		}
	})

	t.Run("the derived controller takes its slot in the LOCALAI_MAX_ACTIVE_BACKENDS guard", func(t *testing.T) {
		const embedder = "granite-embedding-97m-multilingual-r2-q4_k_m"
		w := newServeReadinessWiring(t, &serveReadinessGen{}, "", map[string]string{serveReadinessActiveBackendsEnv: "1"})
		w.in.localai.Models.Embedder = embedder
		err := serveReadinessWire(context.Background(), w.in)
		if err == nil || !strings.Contains(err.Error(), serveReadinessActiveBackendsEnv) || !strings.Contains(err.Error(), "at least 2") {
			t.Fatalf("an empty controller and an embedder with one backend: %v, want a refusal asking for 2 (the controller is resident too)", err)
		}
		if len(w.checks) != 0 {
			t.Error("a refused start still wired a check")
		}
		w = newServeReadinessWiring(t, &serveReadinessGen{}, "", map[string]string{serveReadinessActiveBackendsEnv: "2"})
		w.in.localai.Models.Embedder = embedder
		if err := serveReadinessWire(context.Background(), w.in); err != nil {
			t.Fatalf("an empty controller and an embedder with two backends: %v", err)
		}
	})

	t.Run("the readiness provider still refuses a name that cannot be derived", func(t *testing.T) {
		w := newServeReadinessWiring(t, &serveReadinessGen{}, "", nil)
		w.in.inference.Models = []string{serveModelID, "qwen3.5-2b-q4_k_m"}
		err := serveReadinessWire(context.Background(), w.in)
		if err == nil || !strings.Contains(err.Error(), "several controller models") || contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Fatalf("two verified controllers and no name: %v, want a validation_failed refusal", err)
		}
		if len(w.checks) != 0 || len(w.services) != 0 {
			t.Error("a refused start still wired a check or a service")
		}
	})
}
