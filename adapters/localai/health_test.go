package localai

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const (
	versionBody = `{"version":"v4.10.0 (7ad0cbf259f0c7bf9920fe2438fc3630ecd6c672)"}`
	modelsBody  = `{"object":"list","data":[{"id":"ctl-model","object":"model"},{"id":"router-model","object":"model"},{"id":"embed-model","object":"model"},{"id":"ctl-lite","object":"model"}]}`
)

// healthMux answers the three routes Health reads.
func healthMux(ready http.HandlerFunc, version, models http.HandlerFunc) http.HandlerFunc {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", ready)
	mux.HandleFunc("/version", version)
	mux.HandleFunc("/v1/models", models)
	return mux.ServeHTTP
}

func readyOK(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }

// SEC-072
// The adapter exposes the version it observed so that an unqualified
// runtime can be refused; readiness, the version and the model list decide the
// reported status.
func TestHealthReadiness(t *testing.T) {
	tests := []struct {
		name        string
		mux         http.HandlerFunc
		mutate      func(*Config)
		wantStatus  providers.Health
		wantVersion string
		wantReason  string // substring; "" means no reason expected
	}{
		{
			name:        "ready, version read, models listed",
			mux:         healthMux(readyOK, serveJSON(200, versionBody), serveJSON(200, modelsBody)),
			wantStatus:  providers.HealthOK,
			wantVersion: "v4.10.0",
		},
		{
			name:        "ready, version on the qualified list",
			mux:         healthMux(readyOK, serveJSON(200, versionBody), serveJSON(200, modelsBody)),
			mutate:      func(c *Config) { c.QualifiedVersions = []string{"4.9.0", "V4.10.0"} },
			wantStatus:  providers.HealthOK,
			wantVersion: "v4.10.0",
		},
		{
			name:        "version not on the qualified list is unavailable but still reported",
			mux:         healthMux(readyOK, serveJSON(200, `{"version":"v4.11.2 (abcdef1)"}`), serveJSON(200, modelsBody)),
			mutate:      func(c *Config) { c.QualifiedVersions = []string{"v4.10.0"} },
			wantStatus:  providers.HealthUnavailable,
			wantVersion: "v4.11.2",
			wantReason:  "unqualified",
		},
		{
			name:       "starting: readiness 503 from the startup preload",
			mux:        healthMux(serveJSON(503, `{"status":"starting","reason":"startup preload in progress"}`), serveJSON(200, versionBody), serveJSON(200, modelsBody)),
			wantStatus: providers.HealthUnavailable,
			wantReason: "starting",
		},
		{
			name:       "readiness answers something else",
			mux:        healthMux(serveJSON(500, "x"), serveJSON(200, versionBody), serveJSON(200, modelsBody)),
			wantStatus: providers.HealthUnavailable,
			wantReason: "HTTP 500",
		},
		{
			name:       "key rejected on /version",
			mux:        healthMux(readyOK, serveJSON(401, `{"error":{"message":"An authentication key is required"}}`), serveJSON(401, "{}")),
			wantStatus: providers.HealthUnavailable,
			wantReason: "API key",
		},
		{
			name:       "version unreadable without a qualified list is degraded",
			mux:        healthMux(readyOK, serveJSON(500, "{}"), serveJSON(200, modelsBody)),
			wantStatus: providers.HealthDegraded,
			wantReason: "version unreadable",
		},
		{
			name:       "version unreadable with a qualified list is unavailable",
			mux:        healthMux(readyOK, serveJSON(500, "{}"), serveJSON(200, modelsBody)),
			mutate:     func(c *Config) { c.QualifiedVersions = []string{"v4.10.0"} },
			wantStatus: providers.HealthUnavailable,
			wantReason: "version unreadable",
		},
		{
			name:        "a source build reports no version",
			mux:         healthMux(readyOK, serveJSON(200, `{"version":" (7ad0cbf)"}`), serveJSON(200, modelsBody)),
			wantStatus:  providers.HealthDegraded,
			wantVersion: "",
			wantReason:  "no version",
		},
		{
			name:        "a source build is never qualified",
			mux:         healthMux(readyOK, serveJSON(200, `{"version":" (7ad0cbf)"}`), serveJSON(200, modelsBody)),
			mutate:      func(c *Config) { c.QualifiedVersions = []string{"v4.10.0"} },
			wantStatus:  providers.HealthUnavailable,
			wantVersion: "",
			wantReason:  "unqualified",
		},
		{
			name:        "controller model missing: readyz is 200 even after a failed download",
			mux:         healthMux(readyOK, serveJSON(200, versionBody), serveJSON(200, `{"data":[{"id":"embed-model"},{"id":"router-model"},{"id":"ctl-lite"}]}`)),
			wantStatus:  providers.HealthUnavailable,
			wantVersion: "v4.10.0",
			wantReason:  "controller model",
		},
		{
			name:        "embedding model missing only degrades",
			mux:         healthMux(readyOK, serveJSON(200, versionBody), serveJSON(200, `{"data":[{"id":"ctl-model"},{"id":"router-model"},{"id":"ctl-lite"}]}`)),
			wantStatus:  providers.HealthDegraded,
			wantVersion: "v4.10.0",
			wantReason:  "configured model",
		},
		{
			name:        "model list unreadable",
			mux:         healthMux(readyOK, serveJSON(200, versionBody), serveJSON(500, "{}")),
			wantStatus:  providers.HealthUnavailable,
			wantVersion: "v4.10.0",
			wantReason:  "model list",
		},
		{
			name: "no models configured to check",
			mux:  healthMux(readyOK, serveJSON(200, versionBody), serveJSON(200, `{"data":[]}`)),
			mutate: func(c *Config) {
				c.Models, c.Profiles = ModelSet{}, nil
			},
			wantStatus:  providers.HealthOK,
			wantVersion: "v4.10.0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := newRuntime(t, tt.mux)
			var mut []func(*Config)
			if tt.mutate != nil {
				mut = append(mut, tt.mutate)
			}
			c, _ := rt.client(t, mut...)
			rep, err := c.Health(bg)
			if err != nil {
				t.Fatalf("Health returned an error for a probe that ran: %v", err)
			}
			if rep.Status != tt.wantStatus || rep.Version != tt.wantVersion {
				t.Errorf("report = %+v, want status %s version %q", rep, tt.wantStatus, tt.wantVersion)
			}
			if tt.wantReason == "" && rep.Reason != "" {
				t.Errorf("reason = %q, want none", rep.Reason)
			}
			if !strings.Contains(rep.Reason, tt.wantReason) {
				t.Errorf("reason = %q, want it to mention %q", rep.Reason, tt.wantReason)
			}
			if !rep.Status.Valid() {
				t.Errorf("status %q is not one of the three states", rep.Status)
			}
		})
	}

	t.Run("readiness is anonymous and the other routes carry the key", func(t *testing.T) {
		rt := newRuntime(t, healthMux(readyOK, serveJSON(200, versionBody), serveJSON(200, modelsBody)))
		c, key := rt.client(t)
		if _, err := c.Health(bg); err != nil {
			t.Fatal(err)
		}
		reqs := rt.requests()
		if got := rt.paths(); strings.Join(got, ",") != "GET /readyz,GET /version,GET /v1/models" {
			t.Fatalf("probe order = %v", got)
		}
		if a := reqs[0].Header.Get("Authorization"); a != "" {
			t.Error("the key was sent to the anonymous readiness route")
		}
		for _, r := range reqs[1:] {
			if r.Header.Get("Authorization") != "Bearer "+key {
				t.Errorf("%s lacks the bearer key", r.Path)
			}
		}
	})

	t.Run("an unreachable runtime is a successful probe that reports unavailable", func(t *testing.T) {
		rt := newRuntime(t, healthMux(readyOK, serveJSON(200, versionBody), serveJSON(200, modelsBody)))
		c, _ := rt.client(t)
		rt.Close()
		rep, err := c.Health(bg)
		if err != nil || rep.Status != providers.HealthUnavailable || rep.Reason == "" {
			t.Fatalf("report = %+v, err = %v", rep, err)
		}
	})

	t.Run("a slow runtime hits the health timeout", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		rt := newRuntime(t, func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		})
		c, _ := rt.client(t, func(cfg *Config) { cfg.HealthTimeout = 100 * time.Millisecond })
		rep, err := c.Health(bg)
		if err != nil || rep.Status != providers.HealthUnavailable || !strings.Contains(rep.Reason, "timed out") {
			t.Fatalf("report = %+v, err = %v", rep, err)
		}
	})

	t.Run("ready but silent on /version is unavailable, not degraded", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		hang := func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}
		rt := newRuntime(t, healthMux(readyOK, hang, serveJSON(200, modelsBody)))
		c, _ := rt.client(t, func(cfg *Config) { cfg.HealthTimeout = 300 * time.Millisecond })
		rep, err := c.Health(bg)
		if err != nil || rep.Status != providers.HealthUnavailable || !strings.Contains(rep.Reason, "timed out") {
			t.Fatalf("report = %+v, err = %v", rep, err)
		}
	})

	t.Run("ready but cut off on /version is unavailable, not degraded", func(t *testing.T) {
		drop := func(w http.ResponseWriter, _ *http.Request) {
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		}
		rt := newRuntime(t, healthMux(readyOK, drop, serveJSON(200, modelsBody)))
		c, _ := rt.client(t)
		rep, err := c.Health(bg)
		if err != nil || rep.Status != providers.HealthUnavailable {
			t.Fatalf("report = %+v, err = %v", rep, err)
		}
	})

	t.Run("the caller's own cancellation is an error, not a verdict", func(t *testing.T) {
		rt := newRuntime(t, healthMux(readyOK, serveJSON(200, versionBody), serveJSON(200, modelsBody)))
		c, _ := rt.client(t)
		ctx, cancel := context.WithCancel(bg)
		cancel()
		_, err := c.Health(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})

	t.Run("the registry accepts the client as a provider", func(t *testing.T) {
		rt := newRuntime(t, healthMux(readyOK, serveJSON(200, versionBody), serveJSON(200, modelsBody)))
		c, _ := rt.client(t)
		var p providers.AIRuntimeProvider = c
		if p.Name() != "localai" {
			t.Errorf("Name = %q", p.Name())
		}
	})
}

func TestVersionAndModelListing(t *testing.T) {
	rt := newRuntime(t, healthMux(readyOK, serveJSON(200, versionBody), serveJSON(200, modelsBody)))
	c, _ := rt.client(t)
	v, err := c.Version(bg)
	if err != nil || v.Version != "v4.10.0" || v.Commit != "7ad0cbf259f0c7bf9920fe2438fc3630ecd6c672" {
		t.Fatalf("Version = %+v, %v", v, err)
	}
	ids, err := c.ListModels(bg)
	if err != nil || len(ids) != 4 || ids[0] != "ctl-model" {
		t.Fatalf("ListModels = %v, %v", ids, err)
	}

	t.Run("a bad answer is schema_invalid", func(t *testing.T) {
		rt := newRuntime(t, serveJSON(200, `[1,2]`))
		c, _ := rt.client(t)
		_, err := c.Version(bg)
		wantCode(t, err, contracts.ErrSchemaInvalid)
		_, err = c.ListModels(bg)
		wantCode(t, err, contracts.ErrSchemaInvalid)
	})
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		raw          string
		version, rev string
	}{
		{"v4.10.0 (7ad0cbf259f0c7bf9920fe2438fc3630ecd6c672)", "v4.10.0", "7ad0cbf259f0c7bf9920fe2438fc3630ecd6c672"},
		{"v4.10.0", "v4.10.0", ""},
		{"  v4.10.0  (abcdef1)  ", "v4.10.0", "abcdef1"},
		{" (abcdef1)", "", "abcdef1"},
		{"", "", ""},
		{"v4.10.0 (not a commit)", "v4.10.0", ""},
		{"<script>alert(1)</script> (abcdef1)", "", ""},
		{"v4.10.0\nInjected: header", "", ""},
		{strings.Repeat("a", 200), "", ""},
	}
	for _, tt := range tests {
		got := ParseVersion(tt.raw)
		if got.Version != tt.version || got.Commit != tt.rev {
			t.Errorf("ParseVersion(%q) = %+v, want version %q commit %q", tt.raw, got, tt.version, tt.rev)
		}
	}
}
