package anthropic

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

func (f *fakeKeys) Key(_ context.Context, _ contracts.Scope, _ string) (credentials.Secret, error) {
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
	sleeps *sleeps
	logs   *bytes.Buffer
}

func testConfig(base string) Config {
	return Config{BaseURL: base, CredentialRef: "cred_01TESTREFERENCE", Model: "claude-haiku-4-5"} // TEST-ONLY opaque reference id, not a key
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

func textResponseWith(text string, in, out int) string {
	return render("text.json", map[string]string{"TEXT": jstr(text), "IN": itoa(in), "OUT": itoa(out)})
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func toolResponse(id, name, args string, in, out int) string {
	return render("tool_call.json", map[string]string{"CALL_ID": jstr(id), "NAME": jstr(name), "ARGS_RAW": args, "IN": itoa(in), "OUT": itoa(out)})
}

// streamOf renders a whole text stream: start, one text block, end.
func streamOf(in, out int, stop string, deltas ...string) []string {
	kv := map[string]string{"IN": itoa(in), "OUT": itoa(out), "STOP": jstr(stop)}
	parts := []string{render("stream_start.sse", kv), render("stream_text_open.sse", kv)}
	for _, d := range deltas {
		parts = append(parts, render("stream_text_delta.sse", map[string]string{"TEXT": jstr(d)}))
	}
	return append(parts, render("stream_block_stop.sse", kv), render("stream_end.sse", kv))
}
