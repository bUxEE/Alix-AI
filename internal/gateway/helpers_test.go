package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	alog "github.com/buxee/alix-ai/internal/log"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
)

// syncBuf is a log sink that tests can read while the server writes.
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

// countingProvider counts the health probes that reach the runtime.
type countingProvider struct {
	*fake.Provider
	probes atomic.Int64
}

func (c *countingProvider) Health(ctx context.Context) (providers.HealthReport, error) {
	c.probes.Add(1)
	return c.Provider.Health(ctx)
}

// harness is a gateway server over the deterministic fake runtime.
type harness struct {
	t     *testing.T
	srv   *Server
	fake  *countingProvider
	reg   *providers.Registry
	clk   *clock.Fake
	logs  *syncBuf
	extra map[string]AreaFunc
}

type option func(*Options)

// newHarness builds a server whose controller role is the fake. The spike
// endpoint is on only when a test asks for it.
func newHarness(t *testing.T, mods ...option) *harness {
	t.Helper()
	h := &harness{
		t:    t,
		fake: &countingProvider{Provider: fake.New()},
		clk:  clock.NewFake(time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)),
		logs: &syncBuf{},
	}
	h.reg = providers.NewRegistry(h.clk)
	if err := h.reg.Register(providers.RoleController, h.fake); err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Logger:    alog.NewWithWriter(config.LogConfig{Level: "debug"}, h.logs),
		Clock:     h.clk,
		Providers: h.reg,
	}
	for _, m := range mods {
		m(&opts)
	}
	srv, err := NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	h.srv = srv
	return h
}

func withSpike() option { return func(o *Options) { o.Dev.SpikeEndpoint = true } }

func withConfig(f func(*Config)) option { return func(o *Options) { f(&o.Config) } }

// do sends one request through the full handler.
func (h *harness) do(method, path string, body any, headers ...string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.doRaw(method, path, encodeBody(h.t, body), "application/json", headers...)
}

func (h *harness) doRaw(method, path, body, contentType string, headers ...string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func encodeBody(t *testing.T, body any) string {
	t.Helper()
	switch v := body.(type) {
	case nil:
		return ""
	case string:
		return v
	case []byte:
		return string(v)
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var requestIDRE = regexp.MustCompile(`^req_[0-9A-HJKMNP-TV-Z]{26}$`)

// envelope is the error body, decoded loosely so a test can see stray keys.
type envelope struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details"`
}

// mustEnvelope decodes an error response and checks the invariants every error
// response must have: JSON, the request ID in the header and in the details.
func mustEnvelope(t *testing.T, rec *httptest.ResponseRecorder) envelope {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json (body %q)", ct, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, rec.Body.String())
	}
	for k := range raw {
		switch k {
		case "code", "message", "retryable", "details":
		default:
			t.Errorf("envelope has member %q outside schemas/common/error", k)
		}
	}
	var env envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	id := rec.Header().Get(HeaderRequestID)
	if !requestIDRE.MatchString(id) {
		t.Errorf("%s = %q, want a minted req_ ID", HeaderRequestID, id)
	}
	if got, _ := env.Details["request_id"].(string); got != id {
		t.Errorf("details.request_id = %q, want the header value %q", got, id)
	}
	return env
}

// omit, as an override value, removes the member; nil is a JSON null.
const omit = "<omit>"

// proposalJSON is a valid model answer for the demo catalogue.
func proposalJSON(over map[string]any) map[string]any {
	p := map[string]any{
		"capability_id": "demo.create",
		"fields":        map[string]any{"count": 3, "when": "2026-10-13"},
		"entity_refs":   []any{},
		"intent":        "task",
		"uncertainty":   map[string]any{"overall": 0.9},
	}
	for k, v := range over {
		if v == omit {
			delete(p, k)
			continue
		}
		p[k] = v
	}
	return p
}

// demoRequest is the dev request the spike tests send.
func demoRequest() ProposeRequest {
	return ProposeRequest{
		Text:  "Make 3 demos on 2026-10-13",
		Today: "2026-10-12",
		Capabilities: []ProposeCapability{
			{ID: "demo.create", Description: "Create a demo", Fields: []ProposeField{
				{Name: "count", Type: "integer", Required: true},
				{Name: "when", Type: "date", Required: true},
				{Name: "kind", Type: "enum", Enum: []string{"a", "b"}},
			}},
			{ID: "demo.list", Description: "List demos"},
		},
	}
}

func testNow() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) }
