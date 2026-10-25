package realtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/buxee/alix-ai/adapters/localai"
	pv "github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// suiteLog records every request any test server received, so TestMain can
// assert that the whole suite stayed on the Realtime allowlist.
var suiteLog struct {
	mu   sync.Mutex
	seen map[string]int
}

func recordSuiteRequest(r *http.Request) {
	suiteLog.mu.Lock()
	defer suiteLog.mu.Unlock()
	if suiteLog.seen == nil {
		suiteLog.seen = map[string]int{}
	}
	suiteLog.seen[r.Method+" "+r.URL.Path]++
}

func TestMain(m *testing.M) {
	code := m.Run()
	allowed := map[string]bool{}
	for _, e := range AllowedEndpoints() {
		allowed[e] = true
	}
	suiteLog.mu.Lock()
	var bad []string
	for k := range suiteLog.seen {
		if !allowed[k] {
			bad = append(bad, k)
		}
	}
	suiteLog.mu.Unlock()
	sort.Strings(bad)
	if code == 0 && len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: the suite sent requests outside the realtime allowlist: %v\n", bad)
		code = 1
	}
	os.Exit(code)
}

func testScope() contracts.Scope {
	return contracts.Scope{
		DeploymentID:     "dep_test",
		ApplicationID:    "app_test",
		TenantID:         "ten_test",
		WorkspaceID:      "wsp_test",
		PrincipalID:      "usr_test",
		Roles:            []string{"member"},
		CapabilityScopes: []string{"cap.read"},
		SessionID:        "ses_test",
	}
}

var bg = context.Background()

func wantCode(t testing.TB, err error, code contracts.ErrCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil error, want code %s", code)
	}
	if !contracts.HasCode(err, code) {
		t.Fatalf("error code = %s (%v), want %s", contracts.CodeOf(err), err, code)
	}
}

// pcm16k is n of non-silent 16 kHz mono PCM.
var fmt16k = pv.PCM16Mono(16000)

func pcm(d time.Duration) []byte {
	out := make([]byte, fmt16k.BytesFor(d))
	for i := 0; i+1 < len(out); i += 2 {
		out[i], out[i+1] = byte(i/2), byte(i/512)
	}
	return out
}

// rtServer is a loopback Realtime stub: it accepts the upgrade, records what
// the client sent and hands the connection to a scripted handler.
type rtServer struct {
	*httptest.Server
	mu      sync.Mutex
	queries []string
	auths   []string
	events  []map[string]any // every client event, in order
}

func newRT(t testing.TB, h func(rc *rtConn)) *rtServer {
	t.Helper()
	s := &rtServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recordSuiteRequest(r)
		s.mu.Lock()
		s.queries = append(s.queries, r.URL.RawQuery)
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		s.mu.Unlock()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.SetReadLimit(1 << 24)
		h(&rtConn{t: t, c: c, s: s})
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *rtServer) clientEvents() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.events...)
}

func (s *rtServer) clientTypes() []string {
	var out []string
	for _, e := range s.clientEvents() {
		out = append(out, fmt.Sprint(e["type"]))
	}
	return out
}

func (s *rtServer) connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queries)
}

// rtConn is the server side of one connection.
type rtConn struct {
	t testing.TB
	c *websocket.Conn
	s *rtServer
}

// recv reads the next client event; ok is false when the connection ended.
func (rc *rtConn) recv() (map[string]any, bool) {
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	_, data, err := rc.c.Read(ctx)
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		rc.t.Errorf("client sent a malformed event: %v", err)
		return nil, false
	}
	rc.s.mu.Lock()
	rc.s.events = append(rc.s.events, m)
	rc.s.mu.Unlock()
	return m, true
}

// until reads client events until one of type typ arrives; it returns the
// number of events of type audio append that preceded it.
func (rc *rtConn) until(typ string) (appends int, ok bool) {
	for {
		m, ok := rc.recv()
		if !ok {
			return appends, false
		}
		switch m["type"] {
		case typ:
			return appends, true
		case evAudioAppend:
			appends++
		}
	}
}

func (rc *rtConn) send(m map[string]any) {
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	b, _ := json.Marshal(m)
	_ = rc.c.Write(ctx, websocket.MessageText, b)
}

// handshake plays the runtime's side of the session setup: session.created,
// the client's session.update, then session.updated reporting sessionType.
func (rc *rtConn) handshake(sessionType string) (update map[string]any, ok bool) {
	rc.send(map[string]any{"type": evSessionCreated, "session": map[string]any{"type": "realtime"}})
	m, ok := rc.recv()
	if !ok || m["type"] != evSessionUpdate {
		rc.t.Errorf("first client event = %v, want session.update", m)
		return nil, false
	}
	rc.send(map[string]any{"type": evSessionUpdated, "session": map[string]any{"type": sessionType}})
	return m, true
}

// drain reads until the connection ends and reports whether it did within d.
func (rc *rtConn) drain(d time.Duration) bool {
	ctx, cancel := context.WithTimeout(bg, d)
	defer cancel()
	for {
		if _, _, err := rc.c.Read(ctx); err != nil {
			return ctx.Err() == nil
		}
	}
}

func transCompleted(item, text string) map[string]any {
	return map[string]any{"type": evTransCompleted, "item_id": item, "transcript": text}
}

func transDelta(item, delta string) map[string]any {
	return map[string]any{"type": evTransDelta, "item_id": item, "delta": delta}
}

func testRealtimeConfig() Config {
	return Config{Model: "rt-pipeline", Profiles: map[string]string{"it": "rt-it"}}
}

func (s *rtServer) client(t testing.TB, mutate ...func(*localai.Config, *Config)) *Client {
	t.Helper()
	rt, cfg := localai.Config{Endpoint: s.URL, RequestTimeout: 5 * time.Second}, testRealtimeConfig()
	for _, m := range mutate {
		m(&rt, &cfg)
	}
	c, err := New(rt, cfg, WithAPIKey("TESTONLY-realtime-key"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func openStream(t testing.TB, c *Client) pv.STTStream {
	t.Helper()
	s, err := c.TranscribeStream(bg, testScope(), pv.STTStreamRequest{Format: fmt16k, Language: "it-IT"})
	if err != nil {
		t.Fatalf("TranscribeStream: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// recvAll drains a stream's events up to the first error (io.EOF included).
func recvAll(s pv.STTStream) (evs []pv.TranscriptEvent, err error) {
	for {
		ev, err := s.Recv()
		if err != nil {
			return evs, err
		}
		evs = append(evs, ev)
	}
}
