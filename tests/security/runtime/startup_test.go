package runtimesec

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/models/registry"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The startup half of SEC-072: a runtime whose release is not the one the model registry qualified is
// reported unavailable, the provider registry marks the controller unusable, /readyz answers 503 and the
// model route refuses to call the runtime. The adapter's own comparison of LocalAI's /version with its
// qualified set is covered where the adapter lives (adapters/localai: TestHealthReadiness and the
// version tests), and the M0 gate keeps this suite from importing LocalAI code; the runtime here is a
// stand-in provider whose release can be changed while the test runs (which is what an upgrade behind
// the service's back is) and whose Health decides qualified or not with the production model
// qualification, models.Verify(WithRuntime), against an entry of the shipped model registry.

const (
	qualified = "v4.10.0"
	// qualifiedModel is the shipped registry entry that lists v4.10.0 in allowed_runtime_versions.
	qualifiedModel = "granite-embedding-97m-multilingual-r2-q4_k_m"
)

// standIn is a providers.AIRuntimeProvider and TextGenerator: the fake provider with the Health of a
// runtime that reports its release.
type standIn struct {
	*pfake.Provider
	entry      models.Entry
	version    atomic.Value // the release the runtime reports now
	unreadable atomic.Bool  // the runtime cannot say which release it is
	chatCalls  atomic.Int32
}

func newStandIn(t *testing.T, version string) *standIn {
	t.Helper()
	reg, err := models.LoadFS(registry.FS)
	if err != nil {
		t.Fatalf("models.LoadFS: %v", err)
	}
	entry, ok := reg.Get(qualifiedModel)
	if !ok {
		t.Fatalf("the shipped registry has no entry %s", qualifiedModel)
	}
	p := pfake.New()
	p.On(pfake.Any()).ReplyFunc(func(pfake.Call) (string, error) { return "{}", nil })
	s := &standIn{Provider: p, entry: entry}
	s.version.Store(version)
	return s
}

func (s *standIn) setVersion(v string) { s.version.Store(v) }

func (s *standIn) Chat(ctx context.Context, sc contracts.Scope, req providers.ChatRequest) (providers.ChatResult, error) {
	s.chatCalls.Add(1)
	return s.Provider.Chat(ctx, sc, req)
}

// Health is what a runtime that reports its release says about itself: unavailable unless the release
// is one the registry entry was qualified with. The comparison is models.Verify, which checks the
// runtime before it reads the model file; an error that is not the runtime refusal therefore means
// the release passed.
func (s *standIn) Health(context.Context) (providers.HealthReport, error) {
	v := s.version.Load().(string)
	if s.unreadable.Load() {
		return providers.HealthReport{Status: providers.HealthUnavailable, Reason: "runtime version unreadable"}, nil
	}
	err := models.Verify(s.entry, "", models.WithRuntime(models.RuntimeInfo{
		Runtime: s.entry.Backend.Runtime, Backend: s.entry.Backend.Name, Version: v,
	}))
	if errors.Is(err, models.ErrRuntimeNotAllowed) {
		return providers.HealthReport{Status: providers.HealthUnavailable, Reason: "unqualified runtime version", Version: v}, nil
	}
	return providers.HealthReport{Status: providers.HealthOK, Version: v}, nil
}

// stack is the part of `alixai serve` that decides whether the controller model is served: the
// provider registry the runtime is registered in and the gateway whose /readyz and dev route read it.
type stack struct {
	t   *testing.T
	rt  *standIn
	reg *providers.Registry
	srv *gateway.Server
	clk *clock.Fake
}

func newStack(t *testing.T, rt *standIn) *stack {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))
	reg := providers.NewRegistry(clk)
	if err := reg.Register(providers.RoleController, rt); err != nil {
		t.Fatal(err)
	}
	srv, err := gateway.NewServer(gateway.Options{
		Clock: clk, Providers: reg,
		Config: gateway.Config{HealthTTL: time.Second},
		Dev:    gateway.DevConfig{SpikeEndpoint: true},
	})
	if err != nil {
		t.Fatalf("gateway.NewServer: %v", err)
	}
	return &stack{t: t, rt: rt, reg: reg, srv: srv, clk: clk}
}

// ready asks /readyz as an anonymous caller (after the readiness cache has expired) and returns
// the status code and the report.
func (s *stack) ready() (int, gateway.ReadyReport) {
	s.t.Helper()
	s.clk.Advance(2 * time.Second) // past HealthTTL: the probe runs again
	rec := httptest.NewRecorder()
	s.srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var rep gateway.ReadyReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		s.t.Fatalf("/readyz body: %v\n%s", err, rec.Body.String())
	}
	return rec.Code, rep
}

func (s *stack) controllerReport(rep gateway.ReadyReport) gateway.RoleReport {
	s.t.Helper()
	for _, r := range rep.Roles {
		if r.Role == providers.RoleController {
			return r
		}
	}
	s.t.Fatalf("/readyz has no controller role: %+v", rep)
	return gateway.RoleReport{}
}

// propose calls the gateway's model route (the dev endpoint, which calls the controller model
// when the registry says it is usable) and returns the status, the runtime's chat count after and
// the error code.
func (s *stack) propose() (int, int32, contracts.ErrCode) {
	s.t.Helper()
	body := `{"text":"book a visit","capabilities":[{"id":"visits.book","description":"Book a visit","fields":[{"name":"when","type":"string","required":true}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v0/dev/propose", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.srv.Handler().ServeHTTP(rec, req)
	var env struct {
		Error struct {
			Code contracts.ErrCode `json:"code"`
		} `json:"error"`
		Code contracts.ErrCode `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	code := env.Error.Code
	if code == "" {
		code = env.Code
	}
	return rec.Code, s.rt.chatCalls.Load(), code
}

// startupHalf is the part of SEC-072 about the running runtime.
func startupHalf(t *testing.T) {
	ctx := context.Background()

	t.Run("control: the qualified release is healthy, usable and served", func(t *testing.T) {
		rt := newStandIn(t, qualified)
		s := newStack(t, rt)
		rep, err := rt.Health(ctx)
		if err != nil || rep.Status != providers.HealthOK || rep.Version != qualified {
			t.Fatalf("Health = %+v, %v", rep, err)
		}
		s.reg.Refresh(ctx)
		if !s.reg.Usable(providers.RoleController) {
			t.Fatalf("the controller is not usable: %+v", s.reg.Status(providers.RoleController))
		}
		code, rr := s.ready()
		if code != http.StatusOK || !rr.Ready || s.controllerReport(rr).Status != providers.HealthOK {
			t.Fatalf("/readyz = %d %+v", code, rr)
		}
		status, chats, _ := s.propose()
		if chats != 1 {
			t.Fatalf("the model route did not call the runtime (status %d, chat calls %d): the stack does not serve, so the refusals below would prove nothing", status, chats)
		}
	})

	unqualified := map[string]string{
		"a newer release":                  "v4.11.0",
		"an older release":                 "v4.9.0",
		"a different major release":        "v5.0.0",
		"no version at all":                "",
		"the qualified version plus a tag": "v4.10.0-rc1",
	}
	for name, raw := range unqualified {
		t.Run("an unqualified runtime is unavailable and does not serve: "+name, func(t *testing.T) {
			rt := newStandIn(t, raw)
			s := newStack(t, rt)

			rep, err := rt.Health(ctx)
			if err != nil || rep.Status != providers.HealthUnavailable {
				t.Fatalf("Health = %+v, %v, want unavailable", rep, err)
			}
			s.reg.Refresh(ctx)
			if s.reg.Usable(providers.RoleController) {
				t.Fatal("the registry calls the controller usable")
			}
			if got := s.reg.Overall(); got != providers.HealthUnavailable {
				t.Errorf("overall = %s", got)
			}
			code, rr := s.ready()
			if code != http.StatusServiceUnavailable || rr.Ready || s.controllerReport(rr).Status != providers.HealthUnavailable {
				t.Errorf("/readyz = %d %+v, want 503 and unavailable", code, rr)
			}
			// It does not serve: the model route is refused and the runtime is never asked.
			status, chats, ec := s.propose()
			if status != http.StatusServiceUnavailable || ec != contracts.ErrUnavailable || chats != 0 {
				t.Errorf("model route = %d %q with %d chat calls, want 503 unavailable and none", status, ec, chats)
			}
		})
	}

	t.Run("a runtime that cannot say which release it is is unavailable", func(t *testing.T) {
		rt := newStandIn(t, qualified)
		rt.unreadable.Store(true)
		s := newStack(t, rt)
		s.reg.Refresh(ctx)
		if s.reg.Usable(providers.RoleController) {
			t.Error("the controller is usable although its release is unknown")
		}
		if status, chats, _ := s.propose(); status != http.StatusServiceUnavailable || chats != 0 {
			t.Errorf("model route = %d with %d chat calls, want 503 and none", status, chats)
		}
	})

	t.Run("a runtime upgraded behind the service's back is caught at the next probe, and a rollback of the runtime restores service", func(t *testing.T) {
		rt := newStandIn(t, qualified)
		s := newStack(t, rt)
		s.reg.Refresh(ctx)
		if !s.reg.Usable(providers.RoleController) {
			t.Fatal("the qualified runtime is not usable at start")
		}
		if code, _ := s.ready(); code != http.StatusOK {
			t.Fatalf("/readyz = %d before the upgrade", code)
		}

		rt.setVersion("v4.11.0") // somebody upgrades LocalAI
		code, rr := s.ready()
		if code != http.StatusServiceUnavailable || s.controllerReport(rr).Status != providers.HealthUnavailable {
			t.Fatalf("/readyz after the upgrade = %d %+v, want 503 and unavailable", code, rr)
		}
		if status, chats, _ := s.propose(); status != http.StatusServiceUnavailable || chats != 0 {
			t.Errorf("model route after the upgrade = %d with %d chat calls, want 503 and none", status, chats)
		}

		rt.setVersion(qualified) // rolled back to the previous qualified release
		code, rr = s.ready()
		if code != http.StatusOK || s.controllerReport(rr).Status != providers.HealthOK {
			t.Fatalf("/readyz after the rollback = %d %+v, want 200 and ok", code, rr)
		}
		if _, chats, _ := s.propose(); chats != 1 {
			t.Errorf("after the rollback the model route made %d chat calls, want 1: service did not resume", chats)
		}
	})
}
