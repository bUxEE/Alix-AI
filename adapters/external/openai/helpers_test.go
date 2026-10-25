package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// fixtureKey is a placeholder that is deliberately not shaped like a real key.
const fixtureKey = "fixture-credential-value-0001"

func testScope() contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep_test", ApplicationID: "app_test", TenantID: "ten_test", WorkspaceID: "wsp_test",
		PrincipalID: "usr_test", Roles: []string{"member"}, CapabilityScopes: []string{"cap.read"}, SessionID: "ses_test",
	}
}

type fakeKeys struct {
	mu    sync.Mutex
	key   string
	err   error
	calls int
}

func (f *fakeKeys) Key(_ context.Context, _ contracts.Scope, ref string) (credentials.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return credentials.Secret(f.key), nil
}

// captured is one request the fake server received.
type captured struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

func (c captured) json(t testing.TB) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.Body, &m); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	return m
}

// fakeAPI is a loopback HTTP server that plays the provider from fixtures.
type fakeAPI struct {
	t   *testing.T
	srv *httptest.Server
	mu  sync.Mutex
	got []captured
	// handler answers a request; nil answers 500.
	handler func(n int, w http.ResponseWriter, r *http.Request)
}

func newFakeAPI(t *testing.T, h func(n int, w http.ResponseWriter, r *http.Request)) *fakeAPI {
	f := &fakeAPI{t: t, handler: h}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.got = append(f.got, captured{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
		n := len(f.got)
		f.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		if f.handler == nil {
			http.Error(w, "{}", 500)
			return
		}
		f.handler(n, w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) requests() []captured {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]captured(nil), f.got...)
}

// replyJSON writes a fixture body with status 200.
func replyJSON(body string) func(int, http.ResponseWriter, *http.Request) {
	return func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d = append(s.d, d)
	return nil
}

func (s *sleeps) all() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.d...)
}

// rig is a client wired to a fake API through the real egress dialer.
type rig struct {
	api    *fakeAPI
	client *Client
	keys   *fakeKeys
	sleeps *sleeps
	logs   *bytes.Buffer
}

func testConfig(base string) Config {
	return Config{BaseURL: base, CredentialRef: "cred_01TESTREFERENCE", Model: "gpt-5-mini"} // TEST-ONLY opaque reference id, not a key
}

func newRig(t *testing.T, h func(n int, w http.ResponseWriter, r *http.Request), mutate func(*Config)) *rig {
	t.Helper()
	api := newFakeAPI(t, h)
	cfg := testConfig(api.srv.URL)
	if mutate != nil {
		mutate(&cfg)
	}
	return newRigFor(t, api, cfg, api.srv.URL)
}

// newRigFor builds a client for cfg whose egress allowlist lists allowURL.
func newRigFor(t *testing.T, api *fakeAPI, cfg Config, allowURL string) *rig {
	t.Helper()
	dest, err := egress.DestinationFromURL(allowURL)
	if err != nil {
		t.Fatal(err)
	}
	al, err := egress.NewAllowlist(dest)
	if err != nil {
		t.Fatal(err)
	}
	reg := egress.NewProviderRegistry()
	if err := reg.Set(ProviderName, al); err != nil {
		t.Fatal(err)
	}
	r := &rig{api: api, keys: &fakeKeys{key: fixtureKey}, sleeps: &sleeps{}, logs: &bytes.Buffer{}}
	log := slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r.client, err = New(cfg, reg.Dialer(ProviderName), r.keys, WithSleeper(r.sleeps.sleep), WithLogger(log))
	if err != nil {
		t.Fatal(err)
	}
	r.client.jitter = func() float64 { return 0 }
	return r
}

func userReq(text string) providers.RichRequest {
	return providers.RichRequest{Messages: []providers.RichMessage{
		{Role: providers.MsgSystem, Trusted: true, Parts: []providers.ContentPart{providers.TextPart("You propose typed tasks.")}},
		{Role: providers.MsgUser, Parts: []providers.ContentPart{providers.TextPart(text)}},
	}}
}

func wantCode(t testing.TB, err error, code contracts.ErrCode) *contracts.Error {
	t.Helper()
	var ce *contracts.Error
	if err == nil {
		t.Fatalf("no error, want %s", code)
	}
	if !errorsAs(err, &ce) || ce.Code != code {
		t.Fatalf("error %v, want code %s", err, code)
	}
	return ce
}

func errorsAs(err error, t **contracts.Error) bool {
	for err != nil {
		if ce, ok := err.(*contracts.Error); ok {
			*t = ce
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func mustContain(t testing.TB, what, hay, needle string) {
	t.Helper()
	if !strings.Contains(hay, needle) {
		t.Fatalf("%s lacks %q:\n%s", what, needle, hay)
	}
}

// Fixtures ---------------------------------------------------------------

const textResponse = `{
  "id": "resp_0123456789abcdef",
  "object": "response",
  "status": "completed",
  "model": "gpt-5-mini-2025-08-07",
  "output": [
    {"type": "reasoning", "id": "rs_1", "summary": []},
    {"type": "message", "id": "msg_1", "role": "assistant", "content": [{"type": "output_text", "text": "Hello there.", "annotations": []}]}
  ],
  "usage": {"input_tokens": 1000, "input_tokens_details": {"cached_tokens": 0}, "output_tokens": 500, "output_tokens_details": {"reasoning_tokens": 64}, "total_tokens": 1500}
}`

func textResponseWith(text string) string {
	b, _ := json.Marshal(text)
	return strings.Replace(textResponse, `"Hello there."`, string(b), 1)
}
