package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestHealthz(t *testing.T) {
	h := newHarness(t)

	rec := h.do(http.MethodGet, "/healthz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"ok"}` {
		t.Errorf("body = %q", got)
	}
	if id := rec.Header().Get(HeaderRequestID); !requestIDRE.MatchString(id) {
		t.Errorf("%s = %q, want a minted req_ ID", HeaderRequestID, id)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("hardening headers missing: %v", rec.Header())
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("a CORS header was sent (SEC-092)")
	}

	// Two requests never share an ID.
	if other := h.do(http.MethodGet, "/healthz", nil).Header().Get(HeaderRequestID); other == rec.Header().Get(HeaderRequestID) {
		t.Error("two requests got the same ID")
	}

	// Liveness checks no dependency: a dead runtime does not fail it.
	h.fake.SetHealth(providers.HealthUnavailable, "down")
	h.reg.Refresh(context.Background())
	if rec := h.do(http.MethodGet, "/healthz", nil); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz with a dead runtime = %d, want 200", rec.Code)
	}
	if h.fake.Calls() != 0 {
		t.Error("/healthz reached the model")
	}

	// HEAD is fine; other methods get the 405 envelope and an Allow header.
	if rec := h.do(http.MethodHead, "/healthz", nil); rec.Code != http.StatusOK {
		t.Errorf("HEAD /healthz = %d, want 200", rec.Code)
	}
	rec = h.do(http.MethodPost, "/healthz", "{}")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
		t.Errorf("Allow = %q, want \"GET, HEAD\"", allow)
	}
	mustEnvelope(t, rec)
}

func TestReadyzReflectsProviderHealth(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	ready := func() (int, ReadyReport) {
		t.Helper()
		rec := h.do(http.MethodGet, "/readyz", nil)
		var rep ReadyReport
		if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
			t.Fatalf("/readyz body is not a ReadyReport: %v\n%s", err, rec.Body.String())
		}
		return rec.Code, rep
	}
	// The registry starts every role as ok; the fake's own health is what the
	// refresh path must pick up.
	code, rep := ready()
	if code != http.StatusOK || rep.Status != providers.HealthOK || !rep.Ready {
		t.Fatalf("healthy runtime: %d %+v", code, rep)
	}
	if len(rep.Roles) != 1 || rep.Roles[0].Role != providers.RoleController || !rep.Roles[0].Configured {
		t.Fatalf("roles = %+v, want just the controller", rep.Roles)
	}

	// The runtime goes down. Within the TTL the cached answer stands, so
	// repeated probes by anonymous callers do not become load on the runtime.
	h.fake.SetHealth(providers.HealthUnavailable, "model server is down")
	probes := h.fake.probes.Load()
	if code, rep = ready(); code != http.StatusOK || rep.Status != providers.HealthOK {
		t.Fatalf("within the TTL the cached report must stand: %d %+v", code, rep)
	}
	if h.fake.probes.Load() != probes {
		t.Error("a request inside the TTL probed the runtime")
	}

	// After the TTL the probe runs again and the report follows the runtime.
	h.clk.Advance(DefaultHealthTTL + time.Second)
	code, rep = ready()
	if code != http.StatusServiceUnavailable || rep.Status != providers.HealthUnavailable || rep.Ready {
		t.Fatalf("dead runtime: %d %+v, want 503 unavailable", code, rep)
	}
	if rep.Roles[0].Status != providers.HealthUnavailable || rep.Roles[0].Reason != "model server is down" ||
		rep.Roles[0].Effect != providers.EffectLLMUnavailable {
		t.Errorf("role report = %+v", rep.Roles[0])
	}
	if rec := h.do(http.MethodGet, "/readyz", nil); rec.Header().Get("Retry-After") == "" {
		t.Error("a 503 readiness answer has no Retry-After")
	}

	// Degraded still serves: the service is ready, and says what is reduced.
	h.fake.SetHealth(providers.HealthDegraded, "a model is missing")
	h.clk.Advance(DefaultHealthTTL + time.Second)
	if code, rep = ready(); code != http.StatusOK || rep.Status != providers.HealthDegraded || !rep.Ready {
		t.Fatalf("degraded runtime: %d %+v, want 200 degraded", code, rep)
	}

	// Recovery.
	h.fake.SetHealth(providers.HealthOK, "")
	h.clk.Advance(DefaultHealthTTL + time.Second)
	if code, rep = ready(); code != http.StatusOK || rep.Status != providers.HealthOK {
		t.Fatalf("recovered runtime: %d %+v", code, rep)
	}

	// The report names no provider and no runtime version.
	body := h.do(http.MethodGet, "/readyz", nil).Body.String()
	for _, leak := range []string{"fake", "version", "provider"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Errorf("/readyz body mentions %q: %s", leak, body)
		}
	}
	_ = ctx
}

func TestReadyzWithoutControllerOrWithFailedCheck(t *testing.T) {
	// No controller registered at all: the service cannot answer.
	empty, err := NewServer(Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, srv: empty}
	rec := h.do(http.MethodGet, "/readyz", nil)
	var rep ReadyReport
	_ = json.Unmarshal(rec.Body.Bytes(), &rep)
	if rec.Code != http.StatusServiceUnavailable || rep.Status != providers.HealthUnavailable {
		t.Fatalf("no controller: %d %+v, want 503 unavailable", rec.Code, rep)
	}
	if len(rep.Roles) != 1 || rep.Roles[0].Configured || rep.Roles[0].Effect != providers.EffectLLMUnavailable {
		t.Errorf("roles = %+v, want the controller as not configured", rep.Roles)
	}

	// A healthy runtime with a failing check is not ready, and the caller sees
	// the check's typed message, not its cause.
	failing := newHarness(t, func(o *Options) {
		o.Checks = []Check{
			{Name: "store", Fn: func(context.Context) error { return nil }},
			{Name: "model_verification", Fn: func(context.Context) error {
				return contracts.Wrap(contracts.ErrUnavailable, errors.New("sha256 mismatch /secret/path token=CHECKSECRET"), "model verification failed")
			}},
		}
	})
	rec = failing.do(http.MethodGet, "/readyz", nil)
	rep = ReadyReport{}
	_ = json.Unmarshal(rec.Body.Bytes(), &rep)
	if rec.Code != http.StatusServiceUnavailable || rep.Ready {
		t.Fatalf("failed check: %d %+v", rec.Code, rep)
	}
	if len(rep.Checks) != 2 || !rep.Checks[0].OK || rep.Checks[1].OK || rep.Checks[1].Reason != "model verification failed" {
		t.Errorf("checks = %+v", rep.Checks)
	}
	if strings.Contains(rec.Body.String(), "CHECKSECRET") || strings.Contains(rec.Body.String(), "/secret/path") {
		t.Errorf("the cause of a check leaked: %s", rec.Body.String())
	}
}

// Many simultaneous /readyz callers cost one probe.
func TestReadyzProbesOncePerTTL(t *testing.T) {
	h := newHarness(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rec := h.do(http.MethodGet, "/readyz", nil); rec.Code != http.StatusOK {
				t.Errorf("GET /readyz = %d", rec.Code)
			}
		}()
	}
	wg.Wait()
	if n := h.fake.probes.Load(); n != 1 {
		t.Errorf("20 concurrent /readyz calls probed the runtime %d times, want 1", n)
	}
}
