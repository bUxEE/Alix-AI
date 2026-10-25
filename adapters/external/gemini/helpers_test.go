package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// fixtureKey and fixtureToken are placeholders that are deliberately not shaped
// like a real key or token.
const (
	fixtureKey   = "fixture-credential-value-0001"
	fixtureToken = "fixture-access-value-0001"
)

const testModel = "gemini-3.5-flash-lite"

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

func (f *fakeKeys) Key(_ context.Context, _ contracts.Scope, _ string) (credentials.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return credentials.Secret(f.key), nil
}

// fakeTokens plays the OAuth engine: it serves an access value, a fresher one
// after a forced refresh, and records how it was asked.
type fakeTokens struct {
	mu      sync.Mutex
	current string
	fresh   string
	scopes  [][]string
	forced  []bool
	err     error
}

func (f *fakeTokens) Bearer(_ context.Context, _ contracts.Scope, _ string, scopes []string, force bool) (credentials.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scopes = append(f.scopes, append([]string(nil), scopes...))
	f.forced = append(f.forced, force)
	if f.err != nil {
		return "", f.err
	}
	if force && f.fresh != "" {
		f.current = f.fresh
	}
	return credentials.Secret(f.current), nil
}

// captured is one request the fake server received.
type captured struct {
	Method string
	Path   string
	Query  string
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
		f.got = append(f.got, captured{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: body})
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

// replyStatus writes a fixture body with the given status.
func replyStatus(status int, body string) func(int, http.ResponseWriter, *http.Request) {
	return func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// replySSE writes the named SSE fixtures as one 200 event stream.
func replySSE(bodies ...string) func(int, http.ResponseWriter, *http.Request) {
	return func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, strings.Join(bodies, ""))
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
	tokens *fakeTokens
	sleeps *sleeps
	logs   *bytes.Buffer
}

func testConfig(base string) Config {
	return Config{BaseURL: base, CredentialRef: "cred_01TESTREFERENCE", Model: testModel} // TEST-ONLY opaque reference id, not a key
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
	r := &rig{api: api, keys: &fakeKeys{key: fixtureKey}, tokens: &fakeTokens{current: fixtureToken, fresh: fixtureToken + "-fresh"}, sleeps: &sleeps{}, logs: &bytes.Buffer{}}
	log := slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	opts := []Option{WithSleeper(r.sleeps.sleep), WithLogger(log)}
	var keys KeySource
	if cfg.Auth == AuthOAuth {
		opts = append(opts, WithTokenSource(r.tokens))
	} else {
		keys = r.keys
	}
	r.client, err = New(cfg, reg.Dialer(ProviderName), keys, opts...)
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

const bookSchema = `{"type":"object","additionalProperties":false,"required":["patient","slot"],"properties":{"patient":{"type":"string"},"slot":{"type":"string"}}}`

var bookTool = providers.ToolSpec{Name: "book", Description: "Book an appointment.", Parameters: json.RawMessage(bookSchema)}

func wantCode(t testing.TB, err error, code contracts.ErrCode) *contracts.Error {
	t.Helper()
	var ce *contracts.Error
	if err == nil {
		t.Fatalf("no error, want %s", code)
	}
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("error %v, want code %s", err, code)
	}
	return ce
}

// Fixtures ---------------------------------------------------------------

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func usageKV(in, out int) map[string]string {
	return map[string]string{"IN": itoa(in), "OUT": itoa(out), "TOTAL": itoa(in + out), "RID": "FixtureResp0001"}
}

func textResponseWith(text string, in, out int) string {
	kv := usageKV(in, out)
	kv["TEXT"] = jstr(text)
	return render("text.json", kv)
}

func toolResponse(id, name, args string, in, out int) string {
	kv := usageKV(in, out)
	kv["CALL_ID"], kv["NAME"], kv["ARGS_RAW"] = jstr(id), jstr(name), args
	return render("tool_call.json", kv)
}

// streamOf renders a whole text stream: one event per delta, then the final
// event with the finish reason and the usage. The stream has no terminator.
func streamOf(in, out int, finish string, deltas ...string) []string {
	var parts []string
	for _, d := range deltas {
		kv := usageKV(in, 0)
		kv["TEXT"] = jstr(d)
		parts = append(parts, render("stream_text.sse", kv))
	}
	kv := usageKV(in, out)
	kv["FINISH"] = finish
	return append(parts, render("stream_final.sse", kv))
}
