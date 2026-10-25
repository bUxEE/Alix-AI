package agui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	sdkgo "github.com/buxee/alix-ai/pkg/sdk-go"
)

const goldenDir = "../../../tests/agui/testdata"

// testSecret has the shape of a real confirmation secret (cfm_ and 43 characters of base64url).
const testSecret = "cfm_Zq9-Zq9-Zq9-Zq9-Zq9-Zq9-Zq9-Zq9-Zq9-Zq9-Zq9"

var testNow = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC) // the clock of the golden streams

// ---- the golden streams ----

type golden struct {
	Name    string `json:"name"`
	Request struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		Body    json.RawMessage   `json:"body"`
	} `json:"request"`
	Response struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		SSEFile string            `json:"sse_file"`
	} `json:"response"`
	sse []byte
}

func (g golden) body(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, g.Request.Body); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func loadGoldens(t *testing.T) []golden {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(goldenDir, "*.json"))
	if err != nil || len(files) < 10 {
		t.Fatalf("the golden streams are not where they should be: %d files (%v)", len(files), err)
	}
	var out []golden
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var g golden
		if err := json.Unmarshal(raw, &g); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if g.sse, err = os.ReadFile(filepath.Join(goldenDir, g.Response.SSEFile)); err != nil {
			t.Fatal(err)
		}
		out = append(out, g)
	}
	return out
}

func loadGolden(t *testing.T, name string) golden {
	t.Helper()
	for _, g := range loadGoldens(t) {
		if g.Name == name {
			return g
		}
	}
	t.Fatalf("no golden stream %q", name)
	return golden{}
}

// ---- a recording fake gateway ----

type gwRequest struct {
	Method, Path, Query string
	Header              http.Header
	Body                []byte
}

type gateway struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	requests []gwRequest
	handle   func(g *gateway, w http.ResponseWriter, r *http.Request, req gwRequest)
}

func newGateway(t *testing.T, handle func(g *gateway, w http.ResponseWriter, r *http.Request, req gwRequest)) *gateway {
	g := &gateway{t: t, handle: handle}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := gwRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: body}
		g.mu.Lock()
		g.requests = append(g.requests, req)
		g.mu.Unlock()
		g.handle(g, w, r, req)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *gateway) last() gwRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.requests) == 0 {
		g.t.Fatal("the gateway received no request")
	}
	return g.requests[len(g.requests)-1]
}

func (g *gateway) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.requests)
}

func writeSSE(w http.ResponseWriter, sse []byte, secret string) {
	w.Header().Set("Content-Type", "text/event-stream")
	if secret != "" {
		w.Header().Set(HeaderConfirmationToken, secret)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(sse)
}

func writeErr(w http.ResponseWriter, status int, code contracts.ErrCode, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(contracts.New(code, message))
	_, _ = w.Write(b)
}

// ---- a proxy under test ----

type rig struct {
	t      *testing.T
	proxy  *Proxy
	store  *MemorySecretStore
	front  *httptest.Server // what the browser talks to
	logs   *bytes.Buffer
	mu     sync.Mutex
	tokens []sdkgo.TokenRequest
	minted []string
	tokErr error
}

func newRig(t *testing.T, gw *gateway) *rig {
	t.Helper()
	r := &rig{t: t, logs: &bytes.Buffer{}, store: NewMemorySecretStore(func() time.Time { return testNow }, 0)}
	p, err := NewProxy(Options{
		BaseURL: gw.srv.URL,
		GetContextToken: func(ctx context.Context, req sdkgo.TokenRequest) (string, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.tokens = append(r.tokens, req)
			if r.tokErr != nil {
				return "", r.tokErr
			}
			tok := fmt.Sprintf("ctx_tok_%d", len(r.tokens))
			r.minted = append(r.minted, tok)
			return tok, nil
		},
		Secrets: r.store,
		Now:     func() time.Time { return testNow },
		Logger:  slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	r.proxy = p
	r.front = httptest.NewServer(http.StripPrefix("/agui", p.Handler()))
	t.Cleanup(r.front.Close)
	return r
}

func (r *rig) tokenCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.tokens)
}

func (r *rig) token(i int) sdkgo.TokenRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tokens[i]
}

type answer struct {
	status int
	header http.Header
	body   []byte
}

func (r *rig) do(method, path string, body []byte, header map[string]string) answer {
	r.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, r.front.URL+"/agui"+path, rd)
	if err != nil {
		r.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return answer{status: resp.StatusCode, header: resp.Header, body: b}
}

func (r *rig) run(body []byte, header map[string]string) answer {
	r.t.Helper()
	return r.do(http.MethodPost, "/", body, header)
}

func runBody(thread, runID string, extra string) []byte {
	return []byte(fmt.Sprintf(`{"threadId":%q,"runId":%q,"messages":[],"state":{},"tools":[],"context":[],"forwardedProps":{}%s}`, thread, runID, extra))
}

func resumeBody(thread, runID, prep, status, payload string) []byte {
	return runBody(thread, runID, fmt.Sprintf(`,"resume":[{"interruptId":%q,"status":%q,"payload":%s}]`, prep, status, payload))
}

func assertNoSecret(t *testing.T, what string, a answer) {
	t.Helper()
	if a.header.Get(HeaderConfirmationToken) != "" {
		t.Fatalf("%s: the browser received the %s header", what, HeaderConfirmationToken)
	}
	for k, vs := range a.header {
		for _, v := range vs {
			if strings.Contains(v, testSecret) || strings.Contains(strings.ToLower(k), "confirmation") {
				t.Fatalf("%s: response header %s carries the secret or names it", what, k)
			}
		}
	}
	if bytes.Contains(a.body, []byte(testSecret)) {
		t.Fatalf("%s: the response body carries the secret", what)
	}
}

// ---- TestProxyStreamsGoldenStreamsUnchanged ----

func TestProxyStreamsGoldenStreamsUnchanged(t *testing.T) {
	for _, g := range loadGoldens(t) {
		t.Run(g.Name, func(t *testing.T) {
			wantSecretIn := g.Request.Headers[HeaderConfirmationToken] != ""
			var resume struct {
				ThreadID string `json:"threadId"`
				Resume   []struct {
					InterruptID string `json:"interruptId"`
				} `json:"resume"`
			}
			isRun := g.Request.Method == http.MethodPost
			upstreamPath := g.Request.Path
			thread := ""
			if isRun {
				if err := json.Unmarshal(g.Request.Body, &resume); err != nil {
					t.Fatal(err)
				}
				thread = resume.ThreadID
			} else { // a stream the events route replays: /v1/agui/threads/{thread}/runs/{run}/events
				thread = strings.Split(upstreamPath, "/")[4]
			}
			gw := newGateway(t, func(_ *gateway, w http.ResponseWriter, r *http.Request, req gwRequest) {
				if req.Path != upstreamPath || req.Method != g.Request.Method {
					t.Errorf("unexpected upstream request %s %s, want %s %s", req.Method, req.Path, g.Request.Method, upstreamPath)
				}
				if isRun && !bytes.Equal(req.Body, g.body(t)) {
					t.Errorf("the body was not forwarded unchanged: got %s, want %s", req.Body, g.body(t))
				}
				if got := req.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ctx_tok_") {
					t.Errorf("Authorization = %q, want the minted token", got)
				}
				if got := req.Header.Get(HeaderConfirmationToken); wantSecretIn && got != testSecret || !wantSecretIn && got != "" {
					t.Errorf("secret header at the gateway = %q (the golden request expects one: %v)", got, wantSecretIn)
				}
				secret := ""
				if g.Response.Headers[HeaderConfirmationToken] != "" {
					secret = testSecret
				}
				writeSSE(w, g.sse, secret)
			})
			rg := newRig(t, gw)
			if wantSecretIn { // the run that prepared the action stored it
				if err := rg.store.Put(context.Background(), SecretKey{resume.ThreadID, resume.Resume[0].InterruptID}, testSecret, testNow.Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			var a answer
			if isRun {
				a = rg.run(g.body(t), nil)
			} else {
				a = rg.do(http.MethodGet, strings.TrimPrefix(upstreamPath, "/v1/agui"), nil, nil)
			}
			if a.status != g.Response.Status {
				t.Fatalf("status %d, want %d: %s", a.status, g.Response.Status, a.body)
			}
			if ct := a.header.Get("Content-Type"); ct != "text/event-stream" {
				t.Errorf("Content-Type = %q", ct)
			}
			if cc := a.header.Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store: a run is never cached", cc)
			}
			assertNoSecret(t, g.Name, a)
			if !bytes.Equal(a.body, g.sse) {
				t.Fatalf("the proxied stream differs from %s\n got %q\nwant %q", g.Response.SSEFile, a.body, g.sse)
			}
			wantOp := "runAgui"
			if !isRun {
				wantOp = "streamAguiRunEvents"
			}
			if rg.tokenCount() != 1 || rg.token(0).Operation != wantOp || rg.token(0).SessionID != thread {
				t.Errorf("token requests = %+v, want one %s for the thread", rg.tokens, wantOp)
			}
			if g.Response.Headers[HeaderConfirmationToken] != "" { // an interrupt run: the secret is now held server-side
				if rg.store.Len() != 1 {
					t.Errorf("the secret of the interrupt run was not kept server-side (store holds %d)", rg.store.Len())
				}
			}
		})
	}
}

// ---- TestProxyKeepsTheConfirmationSecretServerSide ----

// secretGateway is the part of the gateway the secret rules depend on (docs/integration/agui.md sections 6 and 9): a run that
// prepares an action returns the secret in a header; an approval needs it (428 without), is single use (409 after), and a
// run id seen before replays its recorded stream and never sends the secret again; a reject ends with the cancelled outcome.
type secretGateway struct {
	prepare, approve, reject golden
	mu                       sync.Mutex
	runs                     map[string][]byte
	confirmed                bool
}

func (s *secretGateway) handle(_ *gateway, w http.ResponseWriter, _ *http.Request, req gwRequest) {
	var in struct {
		RunID  string `json:"runId"`
		Resume []struct {
			Status  string `json:"status"`
			Payload struct {
				Approved bool `json:"approved"`
			} `json:"payload"`
		} `json:"resume"`
	}
	_ = json.Unmarshal(req.Body, &in)
	s.mu.Lock()
	defer s.mu.Unlock()
	if recorded, ok := s.runs[in.RunID]; ok { // a retry: the recorded run, never the secret again
		writeSSE(w, recorded, "")
		return
	}
	switch {
	case len(in.Resume) == 0:
		s.runs[in.RunID] = s.prepare.sse
		writeSSE(w, s.prepare.sse, testSecret)
	case in.Resume[0].Status == "resolved" && in.Resume[0].Payload.Approved:
		switch got := req.Header.Get(HeaderConfirmationToken); {
		case got == "":
			writeErr(w, http.StatusPreconditionRequired, contracts.ErrConfirmationRequired, "the approval needs the confirmation secret")
		case got != testSecret:
			writeErr(w, http.StatusForbidden, contracts.ErrScopeViolation, "the thread or the interrupt does not resolve for this caller")
		case s.confirmed:
			writeErr(w, http.StatusConflict, contracts.ErrConflict, "the preparation is no longer awaiting a decision")
		default:
			s.confirmed = true
			s.runs[in.RunID] = s.approve.sse
			writeSSE(w, s.approve.sse, "")
		}
	default:
		s.runs[in.RunID] = s.reject.sse
		writeSSE(w, s.reject.sse, "")
	}
}

func TestProxyKeepsTheConfirmationSecretServerSide(t *testing.T) {
	sg := &secretGateway{
		prepare: loadGolden(t, "prepare_interrupt"), approve: loadGolden(t, "resume_approve"), reject: loadGolden(t, "resume_reject"),
		runs: map[string][]byte{},
	}
	gw := newGateway(t, sg.handle)
	rg := newRig(t, gw)
	const thread, prep = "ses_test_01", "prep_01J00000000000000000000001"
	key := SecretKey{thread, prep}
	forged := "cfm_FORGED-FORGED-FORGED-FORGED-FORGED-FORGED-FOR"
	var seen []answer
	check := func(what string, a answer) answer {
		t.Helper()
		assertNoSecret(t, what, a)
		seen = append(seen, a)
		return a
	}

	// 1. The run that prepares the action: the browser's own secret header, cookie and Authorization are not forwarded, the
	// gateway's secret header is not returned, and the secret is kept.
	prepareBody := sg.prepare.body(t)
	a := check("prepare", rg.run(prepareBody, map[string]string{
		HeaderConfirmationToken: forged, "Cookie": "session=browser-cookie", "Authorization": "Bearer browser-credential",
	}))
	if a.status != 200 || !bytes.Equal(a.body, sg.prepare.sse) {
		t.Fatalf("prepare: status %d, body differs from the golden stream", a.status)
	}
	up := gw.last()
	if v := up.Header.Get(HeaderConfirmationToken); v != "" {
		t.Fatalf("a secret header of the browser reached the gateway: %q", v)
	}
	if up.Header.Get("Cookie") != "" || strings.Contains(up.Header.Get("Authorization"), "browser-credential") {
		t.Fatalf("the browser's cookie or credential reached the gateway: %v", up.Header)
	}
	if got, ok, _ := rg.store.Get(context.Background(), key); !ok || got != testSecret {
		t.Fatal("the secret of the prepared action was not kept under (thread, preparation)")
	}

	// 2. The same run id again: the gateway replays it without the header. The stored secret is neither overwritten nor deleted.
	check("prepare replay", rg.run(prepareBody, nil))
	if got, ok, _ := rg.store.Get(context.Background(), key); !ok || got != testSecret {
		t.Fatal("a replay that came back without the secret changed the stored one")
	}

	// 3. Another thread cannot use it, and a forged header does not stand in for it: the approval goes without a header.
	other := check("other thread", rg.run(resumeBody("ses_other", "run_other", prep, "resolved", `{"approved":true}`), map[string]string{HeaderConfirmationToken: forged}))
	if other.status != http.StatusPreconditionRequired {
		t.Fatalf("approval on another thread: status %d, want the gateway's 428 relayed", other.status)
	}
	if v := gw.last().Header.Get(HeaderConfirmationToken); v != "" {
		t.Fatalf("a secret reached the gateway for another thread: %q", v)
	}
	if tr := rg.token(rg.tokenCount() - 1); tr.SessionID != "ses_other" {
		t.Fatalf("the token was minted for session %q, want ses_other", tr.SessionID)
	}

	// 4. The approval on the right thread attaches the stored secret (not the forged one), and the secret is spent.
	approveBody := resumeBody(thread, "run_approve", prep, "resolved", `{"approved":true}`)
	a = check("approve", rg.run(approveBody, map[string]string{HeaderConfirmationToken: forged}))
	if a.status != 200 || !bytes.Equal(a.body, sg.approve.sse) {
		t.Fatalf("approve: status %d: %s", a.status, a.body)
	}
	if v := gw.last().Header.Get(HeaderConfirmationToken); v != testSecret {
		t.Fatalf("the approval carried %q, want the stored secret", v)
	}
	if _, ok, _ := rg.store.Get(context.Background(), key); ok {
		t.Fatal("the secret was not deleted after it was used")
	}

	// 5. After use: a second approval under a new run id reaches the gateway without a header and is refused there. The proxy
	// relays the status and the document and never wraps it in a stream.
	again := check("second approval", rg.run(resumeBody(thread, "run_approve_2", prep, "resolved", `{"approved":true}`), nil))
	if again.status != http.StatusPreconditionRequired || !json.Valid(again.body) || again.header.Get("Content-Type") != "application/json" {
		t.Fatalf("second approval: status %d, content type %q, body %s", again.status, again.header.Get("Content-Type"), again.body)
	}
	if v := gw.last().Header.Get(HeaderConfirmationToken); v != "" {
		t.Fatalf("a spent secret was sent again: %q", v)
	}
	// ... and one that does carry the secret (a host's own store handing it out again) is the gateway's 409, relayed as such.
	_ = rg.store.Put(context.Background(), key, testSecret, testNow.Add(time.Minute))
	conflict := check("conflict", rg.run(resumeBody(thread, "run_approve_3", prep, "resolved", `{"approved":true}`), nil))
	if conflict.status != http.StatusConflict || !bytes.Contains(conflict.body, []byte(`"conflict"`)) {
		t.Fatalf("a second approval with the secret: status %d, body %s; want 409 conflict", conflict.status, conflict.body)
	}
	// The same run id of the approval replays the recorded run and never sends the secret again.
	replay := check("approve replay", rg.run(approveBody, nil))
	if replay.status != 200 || !bytes.Equal(replay.body, sg.approve.sse) {
		t.Fatalf("approve replay: status %d", replay.status)
	}

	// 6. A reject ends with the cancelled outcome and carries no secret.
	_ = rg.store.Put(context.Background(), key, testSecret, testNow.Add(time.Minute))
	rej := check("reject", rg.run(resumeBody(thread, "run_reject", prep, "resolved", `{"approved":false}`), nil))
	if rej.status != 200 || !bytes.Contains(rej.body, []byte(`"outcome":{"type":"cancelled"}`)) {
		t.Fatalf("reject: status %d: %s", rej.status, rej.body)
	}
	if v := gw.last().Header.Get(HeaderConfirmationToken); v != "" {
		t.Fatalf("a reject carried the secret: %q", v)
	}

	// 7. A gateway that put the secret into an event is not relayed: the run ends with RUN_ERROR and the secret stays here.
	leaky := newGateway(t, func(_ *gateway, w http.ResponseWriter, _ *http.Request, _ gwRequest) {
		writeSSE(w, []byte("data: {\"type\":\"RUN_STARTED\",\"threadId\":\"ses_test_01\",\"runId\":\"run_leak\"}\n\n"+
			"data: {\"type\":\"TEXT_MESSAGE_CONTENT\",\"messageId\":\"m\",\"delta\":\"your code is "+testSecret+"\"}\n\n"), testSecret)
	})
	leak := check("leaky gateway", newRig(t, leaky).run(runBody(thread, "run_leak", ""), nil))
	if !bytes.Contains(leak.body, []byte(`"type":"RUN_ERROR"`)) || bytes.Contains(leak.body, []byte("your code")) {
		t.Fatalf("a frame carrying the secret was relayed: %s", leak.body)
	}

	// Nothing the browser saw, and nothing in the logs, holds the secret, a context token or an event's text.
	logs := rg.logs.String()
	for _, banned := range append([]string{testSecret, forged, "Done. The item", "widget", "browser-credential"}, rg.minted...) {
		if strings.Contains(logs, banned) {
			t.Errorf("the log output contains %q:\n%s", banned, logs)
		}
	}
	if logs == "" {
		t.Error("the proxy logged nothing at debug level: the log assertions above prove nothing")
	}
	for i, s := range seen {
		assertNoSecret(t, fmt.Sprintf("answer %d", i), s)
	}
}

// An approval the gateway refused without deciding it (a bad request, an expired context token, a foreign caller, a server
// error) leaves the secret for the user's retry; only an answer that used it up (2xx, 409 already decided, 410 expired) spends it.
func TestProxyKeepsTheSecretOfAnApprovalThatWasNotDecided(t *testing.T) {
	var status int
	gw := newGateway(t, func(_ *gateway, w http.ResponseWriter, _ *http.Request, _ gwRequest) {
		writeErr(w, status, contracts.ErrInternal, "refused")
	})
	rg := newRig(t, gw)
	ctx := context.Background()
	const thread, prep = "ses_test_01", "prep_01J00000000000000000000001"
	key := SecretKey{thread, prep}
	for _, c := range []struct {
		status int
		spent  bool
	}{
		{http.StatusBadRequest, false}, {http.StatusUnauthorized, false}, {http.StatusForbidden, false},
		{http.StatusUnsupportedMediaType, false}, {http.StatusTooManyRequests, false}, {http.StatusServiceUnavailable, false},
		{http.StatusConflict, true}, {http.StatusGone, true},
	} {
		status = c.status
		_ = rg.store.Put(ctx, key, testSecret, testNow.Add(time.Minute))
		if a := rg.run(resumeBody(thread, fmt.Sprintf("run_%d", c.status), prep, "resolved", `{"approved":true}`), nil); a.status != c.status {
			t.Fatalf("%d: relayed as %d", c.status, a.status)
		}
		if gw.last().Header.Get(HeaderConfirmationToken) != testSecret {
			t.Fatalf("%d: the approval went without the stored secret", c.status)
		}
		if _, held, _ := rg.store.Get(ctx, key); held == c.spent {
			t.Errorf("%d: secret held = %v, want %v", c.status, held, !c.spent)
		}
	}
}

// ---- TestProxyForwardsResumeAndCancel ----

func TestProxyForwardsResumeAndCancel(t *testing.T) {
	approve := loadGolden(t, "resume_approve")
	reject := loadGolden(t, "resume_reject")
	gw := newGateway(t, func(_ *gateway, w http.ResponseWriter, _ *http.Request, req gwRequest) {
		switch {
		case req.Method == http.MethodPost && req.Path == "/v1/agui":
			if strings.Contains(string(req.Body), `"approved":false`) || strings.Contains(string(req.Body), `"cancelled"`) {
				writeSSE(w, reject.sse, "")
				return
			}
			writeSSE(w, approve.sse, "")
		case req.Method == http.MethodPost && strings.HasSuffix(req.Path, "/cancel"):
			if strings.Contains(req.Path, "run_unknown") {
				writeErr(w, http.StatusForbidden, contracts.ErrScopeViolation, "the run does not resolve for this caller")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"thread_id":"ses_test_01","run_id":"run_live","status":"cancelling"}`))
		case req.Method == http.MethodGet && strings.HasSuffix(req.Path, "/events"):
			switch {
			case strings.Contains(req.Path, "run_done"):
				w.WriteHeader(http.StatusNoContent)
			case strings.Contains(req.Path, "run_old"):
				writeErr(w, http.StatusGone, contracts.ErrExpired, "the cursor is behind the retained window")
			default:
				writeSSE(w, approve.sse, "")
			}
		default:
			t.Errorf("unexpected upstream request %s %s", req.Method, req.Path)
		}
	})
	rg := newRig(t, gw)
	ctx := context.Background()
	const thread, prep = "ses_test_01", "prep_01J00000000000000000000001"
	key := SecretKey{thread, prep}
	seed := func() { _ = rg.store.Put(ctx, key, testSecret, testNow.Add(time.Minute)) }
	held := func() bool { _, ok, _ := rg.store.Get(ctx, key); return ok }

	// An approval carries the secret, and the body is forwarded exactly as it came (members this proxy does not read, spacing).
	seed()
	odd := []byte(`{ "threadId": "ses_test_01",  "runId": "run_a", "messages": [], "newerProtocolMember": {"x": [1, 2]},
	  "resume": [ {"interruptId": "prep_01J00000000000000000000001", "status": "resolved", "payload": {"approved": true}} ] }`)
	if a := rg.run(odd, nil); a.status != 200 || !bytes.Equal(a.body, approve.sse) {
		t.Fatalf("approve: status %d", a.status)
	}
	if up := gw.last(); up.Header.Get(HeaderConfirmationToken) != testSecret || !bytes.Equal(up.Body, odd) {
		t.Fatalf("approve: secret %q, body unchanged: %v", up.Header.Get(HeaderConfirmationToken), bytes.Equal(up.Body, odd))
	}
	if held() {
		t.Error("an approval that was answered left its secret in the store")
	}

	// The answer in the form of a tool message for the confirmation call is the same approval.
	seed()
	toolForm := []byte(fmt.Sprintf(`{"threadId":%q,"runId":"run_tool","messages":[{"id":"m9","role":"tool","toolCallId":"alixai_confirm_%s","content":"{\"approved\":true}"}]}`, thread, prep))
	if a := rg.run(toolForm, nil); a.status != 200 {
		t.Fatalf("tool-message approval: status %d", a.status)
	}
	if gw.last().Header.Get(HeaderConfirmationToken) != testSecret {
		t.Error("an approval in the tool-message form was sent without the secret")
	}

	// A reject, and a dismissed interrupt (cancelled), carry no secret and discard the stored one.
	for name, body := range map[string][]byte{
		"reject":    resumeBody(thread, "run_rej", prep, "resolved", `{"approved":false}`),
		"cancelled": resumeBody(thread, "run_can", prep, "cancelled", `null`),
	} {
		seed()
		a := rg.run(body, nil)
		if a.status != 200 || !bytes.Contains(a.body, []byte(`"outcome":{"type":"cancelled"}`)) {
			t.Fatalf("%s: status %d: %s", name, a.status, a.body)
		}
		if up := gw.last(); up.Header.Get(HeaderConfirmationToken) != "" || !bytes.Equal(up.Body, body) {
			t.Fatalf("%s: the gateway got a secret or a changed body", name)
		}
		if held() {
			t.Errorf("%s: the secret of a declined preparation was not discarded", name)
		}
	}

	// Anything that is not exactly {"approved": true} is never an approval: no secret leaves, and nothing is discarded.
	for name, payload := range map[string]string{
		"true": `true`, "string": `"yes"`, "stringly": `{"approved":"true"}`, "extra member": `{"approved":true,"token":"x"}`,
		"null": `null`, "empty": `{}`,
	} {
		seed()
		rg.run(resumeBody(thread, "run_bad_"+strings.ReplaceAll(name, " ", "_"), prep, "resolved", payload), nil)
		if v := gw.last().Header.Get(HeaderConfirmationToken); v != "" {
			t.Errorf("payload %s: a secret was attached to something that is not an explicit approval", name)
		}
		if !held() {
			t.Errorf("payload %s: the secret was discarded by an answer that decided nothing", name)
		}
	}
	two := runBody(thread, "run_two", fmt.Sprintf(`,"resume":[{"interruptId":%q,"status":"resolved","payload":{"approved":true}},{"interruptId":"prep_other","status":"resolved","payload":{"approved":true}}]`, prep))
	rg.run(two, nil)
	if gw.last().Header.Get(HeaderConfirmationToken) != "" {
		t.Error("a secret was attached to a resume with two entries")
	}

	// Cancel: a fresh token minted for the thread, the JSON answer relayed with its status, an unknown run is the gateway's 403.
	before := rg.tokenCount()
	c := rg.do(http.MethodPost, "/threads/ses_test_01/runs/run_live/cancel", nil, map[string]string{HeaderConfirmationToken: testSecret})
	if c.status != 200 || c.header.Get("Content-Type") != "application/json" || !strings.Contains(string(c.body), `"cancelling"`) {
		t.Fatalf("cancel: status %d, body %s", c.status, c.body)
	}
	up := gw.last()
	if up.Method != http.MethodPost || up.Path != "/v1/agui/threads/ses_test_01/runs/run_live/cancel" || len(up.Body) != 0 || up.Header.Get(HeaderConfirmationToken) != "" {
		t.Fatalf("cancel reached the gateway as %s %s with %d body bytes", up.Method, up.Path, len(up.Body))
	}
	if tr := rg.token(before); tr.Operation != "cancelAguiRun" || tr.SessionID != thread || rg.tokenCount() != before+1 {
		t.Fatalf("cancel token request = %+v", tr)
	}
	if c := rg.do(http.MethodPost, "/threads/ses_test_01/runs/run_unknown/cancel", nil, nil); c.status != http.StatusForbidden || !bytes.Contains(c.body, []byte("scope_violation")) {
		t.Fatalf("cancel of an unknown run: status %d, body %s", c.status, c.body)
	}

	// Reconnect: the cursor of Last-Event-ID or ?cursor= is forwarded after a check, 204 and 410 pass through, each call has its own token.
	before = rg.tokenCount()
	e := rg.do(http.MethodGet, "/threads/ses_test_01/runs/run_live/events?cursor=4", nil, map[string]string{"Last-Event-ID": "5"})
	if e.status != 200 || !bytes.Equal(e.body, approve.sse) {
		t.Fatalf("events: status %d", e.status)
	}
	up = gw.last()
	if up.Path != "/v1/agui/threads/ses_test_01/runs/run_live/events" || up.Query != "cursor=4" || up.Header.Get("Last-Event-ID") != "5" {
		t.Fatalf("events reached the gateway as %s ?%s, Last-Event-ID %q", up.Path, up.Query, up.Header.Get("Last-Event-ID"))
	}
	if tr := rg.token(before); tr.Operation != "streamAguiRunEvents" || tr.SessionID != thread {
		t.Fatalf("events token request = %+v", tr)
	}
	if d := rg.do(http.MethodGet, "/threads/ses_test_01/runs/run_done/events", nil, map[string]string{"Last-Event-ID": "9"}); d.status != http.StatusNoContent || len(d.body) != 0 {
		t.Fatalf("a finished run's cursor: status %d, want 204", d.status)
	}
	if g := rg.do(http.MethodGet, "/threads/ses_test_01/runs/run_old/events", nil, nil); g.status != http.StatusGone || !bytes.Contains(g.body, []byte("expired")) {
		t.Fatalf("a cursor behind the window: status %d, body %s", g.status, g.body)
	}
	n, tokens := gw.count(), rg.tokenCount()
	for _, bad := range []string{"/threads/ses_test_01/runs/run_live/events?cursor=-1", "/threads/ses_test_01/runs/run_live/events?cursor=12345678901", "/threads/ses_test_01/runs/run_live/events?cursor=1%0d%0aX:y"} {
		if b := rg.do(http.MethodGet, bad, nil, nil); b.status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", bad, b.status)
		}
	}
	if b := rg.do(http.MethodGet, "/threads/ses_test_01/runs/run_live/events", nil, map[string]string{"Last-Event-ID": "abc"}); b.status != http.StatusBadRequest {
		t.Errorf("a malformed Last-Event-ID: status %d, want 400", b.status)
	}
	if gw.count() != n || rg.tokenCount() != tokens {
		t.Error("a malformed cursor cost a token or reached the gateway")
	}

	// Capabilities needs a token too; the callback is told the operation and no thread.
	gw2 := newGateway(t, func(_ *gateway, w http.ResponseWriter, _ *http.Request, _ gwRequest) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"protocol":"1.0"}`))
	})
	rg2 := newRig(t, gw2)
	if c := rg2.do(http.MethodGet, "/capabilities", nil, nil); c.status != 200 || string(c.body) != `{"protocol":"1.0"}` {
		t.Fatalf("capabilities: status %d, body %s", c.status, c.body)
	}
	if tr := rg2.token(0); tr.Operation != "getAguiCapabilities" || tr.SessionID != "" {
		t.Fatalf("capabilities token request = %+v", tr)
	}
}

// A run id is what the run request and the OpenAPI accept (1 to 128 characters, not only the thread id's): a run with a ':' in
// its id can be read again and cancelled, and the id is escaped into the gateway's path, never pasted.
func TestProxyAcceptsEveryRunIDTheRunRequestAccepts(t *testing.T) {
	var paths []string
	gw := newGateway(t, func(_ *gateway, w http.ResponseWriter, r *http.Request, _ gwRequest) {
		paths = append(paths, r.URL.EscapedPath())
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"finished"}`))
	})
	rg := newRig(t, gw)
	const thread = "ses_test_01"
	for _, run := range []string{"run:2026-10-10:1", "a/b c?d#e%f", strings.Repeat("r", 128)} {
		events := httptest.NewRecorder()
		rg.proxy.Events(events, httptest.NewRequest(http.MethodGet, "/", nil), thread, run)
		cancel := httptest.NewRecorder()
		rg.proxy.Cancel(cancel, httptest.NewRequest(http.MethodPost, "/", nil), thread, run)
		if events.Code != http.StatusNoContent || cancel.Code != http.StatusOK {
			t.Fatalf("run %q: events %d, cancel %d", run, events.Code, cancel.Code)
		}
		base := "/v1/agui/threads/" + thread + "/runs/" + url.PathEscape(run)
		if got := paths[len(paths)-2:]; got[0] != base+"/events" || got[1] != base+"/cancel" {
			t.Fatalf("run %q reached the gateway as %v", run, got)
		}
	}
	// Through the handler, a ':' in the path is the run id as it is.
	if a := rg.do(http.MethodGet, "/threads/"+thread+"/runs/run:2026-10-10:2/events", nil, nil); a.status != http.StatusNoContent {
		t.Fatalf("events of run:2026-10-10:2 through the handler: status %d, body %s", a.status, a.body)
	}

	n, tokens := gw.count(), rg.tokenCount()
	for _, run := range []string{"", ".", "..", strings.Repeat("r", 129), "a\nb", "a\x00b"} {
		events := httptest.NewRecorder()
		rg.proxy.Events(events, httptest.NewRequest(http.MethodGet, "/", nil), thread, run)
		cancel := httptest.NewRecorder()
		rg.proxy.Cancel(cancel, httptest.NewRequest(http.MethodPost, "/", nil), thread, run)
		if events.Code != http.StatusBadRequest || cancel.Code != http.StatusBadRequest {
			t.Errorf("run %q: events %d, cancel %d, want 400", run, events.Code, cancel.Code)
		}
	}
	if gw.count() != n || rg.tokenCount() != tokens {
		t.Error("a refused run id cost a token or reached the gateway")
	}
}

// ---- refusals before anything is sent ----

func TestProxyRefusesBeforeSpendingAToken(t *testing.T) {
	gw := newGateway(t, func(_ *gateway, w http.ResponseWriter, _ *http.Request, _ gwRequest) { writeSSE(w, nil, "") })
	rg := newRig(t, gw)
	huge := runBody("ses_test_01", "run_big", `,"pad":"`+strings.Repeat("x", MaxRunRequestBytes)+`"`)
	cases := map[string]struct {
		body   []byte
		ctype  string
		status int
	}{
		"not json content type": {runBody("ses_test_01", "r1", ""), "text/plain", http.StatusUnsupportedMediaType},
		"too large":             {huge, "application/json", http.StatusRequestEntityTooLarge},
		"not json":              {[]byte(`nope`), "application/json", http.StatusBadRequest},
		"no thread":             {[]byte(`{"runId":"r1"}`), "application/json", http.StatusBadRequest},
		"path thread":           {runBody("../ses", "r1", ""), "application/json", http.StatusBadRequest},
		"dot thread":            {runBody("..", "r1", ""), "application/json", http.StatusBadRequest},
		"no run":                {[]byte(`{"threadId":"ses_test_01"}`), "application/json", http.StatusBadRequest},
		"resume is not a list":  {runBody("ses_test_01", "r1", `,"resume":"yes"`), "application/json", http.StatusBadRequest},
	}
	for name, c := range cases {
		req, _ := http.NewRequest(http.MethodPost, rg.front.URL+"/agui/", bytes.NewReader(c.body))
		req.Header.Set("Content-Type", c.ctype)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.status || !json.Valid(b) {
			t.Errorf("%s: status %d (%s), want %d with an error document", name, resp.StatusCode, b, c.status)
		}
	}
	if rg.tokenCount() != 0 || gw.count() != 0 {
		t.Errorf("a refused request cost %d token(s) and reached the gateway %d time(s)", rg.tokenCount(), gw.count())
	}

	// The host's callback decides who may use a thread: its refusal is answered, and nothing is sent.
	rg.tokErr = contracts.New(contracts.ErrPermissionDenied, "that is not your thread")
	if a := rg.run(runBody("ses_test_01", "r2", ""), nil); a.status != http.StatusForbidden || bytes.Contains(a.body, []byte("your thread")) {
		t.Errorf("a refusing callback: status %d, body %s (the callback's text must not be echoed)", a.status, a.body)
	}
	rg.tokErr = errors.New("database password is hunter2")
	if a := rg.run(runBody("ses_test_01", "r3", ""), nil); a.status != http.StatusUnauthorized || bytes.Contains(a.body, []byte("hunter2")) {
		t.Errorf("a failing callback: status %d, body %s", a.status, a.body)
	}
	if gw.count() != 0 {
		t.Error("a request without a token reached the gateway")
	}
}

func TestProxyEndsATruncatedStreamWithRunError(t *testing.T) {
	gw := newGateway(t, func(_ *gateway, w http.ResponseWriter, _ *http.Request, _ gwRequest) {
		writeSSE(w, []byte("data: {\"type\":\"RUN_STARTED\",\"threadId\":\"ses_test_01\",\"runId\":\"run_cut\"}\n\ndata: {\"type\":\"STEP_STA"), "")
	})
	a := newRig(t, gw).run(runBody("ses_test_01", "run_cut", ""), nil)
	want := "data: {\"type\":\"RUN_STARTED\",\"threadId\":\"ses_test_01\",\"runId\":\"run_cut\"}\n\n" +
		"data: {\"type\":\"RUN_ERROR\",\"message\":\"the event stream ended before the run did\",\"code\":\"upstream_ended\"}\n\n"
	if string(a.body) != want {
		t.Fatalf("got %q\nwant %q", a.body, want)
	}
}

func TestProxyNeverSendsAnythingAfterTheEndOfARun(t *testing.T) {
	gw := newGateway(t, func(_ *gateway, w http.ResponseWriter, _ *http.Request, _ gwRequest) {
		writeSSE(w, []byte("data: {\"type\":\"RUN_ERROR\",\"message\":\"x\"}\n\ndata: {\"type\":\"RUN_STARTED\",\"threadId\":\"t\",\"runId\":\"r\"}\n\n"), "")
	})
	a := newRig(t, gw).run(runBody("ses_test_01", "run_x", ""), nil)
	if string(a.body) != "data: {\"type\":\"RUN_ERROR\",\"message\":\"x\"}\n\n" {
		t.Fatalf("got %q", a.body)
	}
}

func TestProxyDoesNotForwardResponseHeadersByDefault(t *testing.T) {
	gw := newGateway(t, func(_ *gateway, w http.ResponseWriter, _ *http.Request, _ gwRequest) {
		w.Header().Set("Set-Cookie", "gw=1")
		w.Header().Set("X-Internal", "1")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("X-Request-ID", "req_1")
		writeSSE(w, []byte("data: {\"type\":\"RUN_FINISHED\",\"threadId\":\"ses_test_01\",\"runId\":\"r\"}\n\n"), testSecret)
	})
	a := newRig(t, gw).run(runBody("ses_test_01", "r", ""), nil)
	for _, h := range []string{"Set-Cookie", "X-Internal", "Access-Control-Allow-Origin", HeaderConfirmationToken} {
		if a.header.Get(h) != "" {
			t.Errorf("the upstream's %s header reached the browser", h)
		}
	}
	if a.header.Get("X-Request-ID") != "req_1" {
		t.Error("the request id (for support) was not relayed")
	}
}

func TestNewProxyRefusesWhatTheSDKClientRefuses(t *testing.T) {
	tok := func(context.Context, sdkgo.TokenRequest) (string, error) { return "t", nil }
	for name, o := range map[string]Options{
		"no base url":   {GetContextToken: tok},
		"no callback":   {BaseURL: "http://127.0.0.1:1"},
		"plain http":    {BaseURL: "http://alixai.example.com", GetContextToken: tok},
		"credentials":   {BaseURL: "https://user:pw@alixai.example.com", GetContextToken: tok},
		"query in base": {BaseURL: "https://alixai.example.com?a=b", GetContextToken: tok},
	} {
		if _, err := NewProxy(o); err == nil {
			t.Errorf("%s: NewProxy accepted it", name)
		}
	}
	if _, err := NewProxy(Options{BaseURL: "https://alixai.example.com/prefix/", GetContextToken: tok}); err != nil {
		t.Errorf("a https base with a path prefix: %v", err)
	}
}
