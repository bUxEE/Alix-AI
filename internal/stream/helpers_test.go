package stream

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

const streamExamples = "../../schemas/conversation/examples/turn-event/valid"

// streamTurnID mints a well-formed turn id (trn_ + 26 Crockford characters).
func streamTurnID(n int) string { return fmt.Sprintf("trn_01J0000000000000000000%04d", n) }

// streamScope is the verified scope of the test principal p in tenant t.
func streamScope(tenant, principal string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep1", ApplicationID: "app1", TenantID: tenant, WorkspaceID: "ws1",
		PrincipalID: principal, Roles: []string{"user"}, CapabilityScopes: []string{"appointments.book"},
		SessionID: "ses1",
	}
}

var (
	streamExampleMu    sync.Mutex
	streamExampleCache = map[string][]byte{}
)

// streamExample loads a published turn-event example as a generic document (a
// fresh copy: callers change it).
func streamExample(t testing.TB, name string) map[string]any {
	t.Helper()
	streamExampleMu.Lock()
	raw, ok := streamExampleCache[name]
	if !ok {
		var err error
		if raw, err = os.ReadFile(filepath.Join(streamExamples, name)); err != nil {
			streamExampleMu.Unlock()
			t.Fatal(err)
		}
		streamExampleCache[name] = raw
	}
	streamExampleMu.Unlock()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// streamEvent builds the event of a kind for a turn at a sequence number from
// the published examples, so what the tests stream is what the schema allows.
func streamEvent(t testing.TB, turnID string, seq int64, kind string) conversation.TurnEvent {
	t.Helper()
	var doc map[string]any
	switch kind {
	case "token":
		doc = streamExample(t, "token.json")
		doc["text"] = "word" + strconv.FormatInt(seq, 10) + " "
	case "progress":
		doc = streamExample(t, "progress-queued.json")
	case "directive":
		doc = streamExample(t, "directive.json")
		// The facts the chart of a later event may show.
		d := doc["directive"].(map[string]any)
		d["turn_id"] = turnID
		facts := d["facts"].([]any)
		for i, n := range []int64{12, 18, 9} {
			facts = append(facts, map[string]any{
				"field": "booked_" + strconv.Itoa(i),
				"value": map[string]any{"kind": "integer", "value": n},
			})
		}
		d["facts"] = facts
	case "markdown":
		doc = streamExample(t, "block-markdown.json")
	case "chart":
		doc = streamExample(t, "block-chart.json")
	case "chart-invented":
		doc = streamExample(t, "block-chart.json")
		data := doc["block"].(map[string]any)["data"].([]any)
		data[0].(map[string]any)["booked"] = 99 // not a fact
	case "done":
		doc = streamExample(t, "done-completed.json")
	case "prepared":
		doc = streamExample(t, "prepared-action.json")
	default:
		t.Fatalf("unknown event kind %q", kind)
	}
	doc["turn_id"] = turnID
	doc["seq"] = seq
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var ev conversation.TurnEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("example %s does not decode: %v", kind, err)
	}
	return ev
}

// streamKinds is the shape of a standard turn: n events, the last one done.
func streamKinds(n int) []string {
	base := []string{"progress", "directive", "markdown", "chart"}
	out := make([]string, 0, n)
	for i := 0; i < n-1; i++ {
		if i < len(base) {
			out = append(out, base[i])
		} else {
			out = append(out, "token")
		}
	}
	return append(out, "done")
}

// streamPublish publishes events [from, to) of the standard turn.
func streamPublish(t testing.TB, turn *Turn, turnID string, kinds []string, from, to int) {
	t.Helper()
	for i := from; i < to; i++ {
		if err := turn.Publish(streamEvent(t, turnID, int64(i), kinds[i])); err != nil {
			t.Fatalf("publish seq %d (%s): %v", i, kinds[i], err)
		}
	}
}

// streamRig is a hub behind an httptest server (plain HTTP on loopback).
type streamRig struct {
	hub *Hub
	srv *httptest.Server
	opt Options
}

// streamScopeFromHeader is the test host's authentication: the principal and
// tenant come from headers, never from the URL.
func streamScopeFromHeader(r *http.Request) (contracts.Scope, error) {
	p := r.Header.Get("X-Test-Principal")
	if p == "" {
		return contracts.Scope{}, contracts.New(contracts.ErrUnauthenticated, "no credential")
	}
	tenant := r.Header.Get("X-Test-Tenant")
	if tenant == "" {
		tenant = "ten1"
	}
	return streamScope(tenant, p), nil
}

func newStreamRig(t testing.TB, cfg Config, mod func(*Options)) *streamRig {
	t.Helper()
	rig := &streamRig{hub: NewHub(cfg)}
	rig.opt = Options{
		Scope: streamScopeFromHeader, WriteTimeout: 2 * time.Second, Heartbeat: 5 * time.Second, RetryAfter: time.Second,
	}
	if mod != nil {
		mod(&rig.opt)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /sse/{turn_id}", rig.hub.SSEHandler(rig.opt))
	mux.Handle("GET /ws/{turn_id}", rig.hub.WebSocketHandler(rig.opt))
	mux.Handle("POST /sse/{turn_id}", rig.hub.SSEHandler(rig.opt))
	rig.srv = httptest.NewServer(mux)
	t.Cleanup(rig.srv.Close)
	return rig
}

// streamSSEEvent is one parsed server-sent event.
type streamSSEEvent struct {
	ID    string
	Event string
	Data  string
}

// streamGet opens an SSE stream as principal p with extra headers and query.
func (rig *streamRig) get(t testing.TB, ctx context.Context, turnID, principal string, header map[string]string, query string) *http.Response {
	t.Helper()
	u := rig.srv.URL + "/sse/" + turnID
	if query != "" {
		u += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	if principal != "" {
		req.Header.Set("X-Test-Principal", principal)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	return resp
}

// streamReadSSE reads events until max events or the end of the stream.
func streamReadSSE(t testing.TB, body io.Reader, max int) []streamSSEEvent {
	t.Helper()
	var (
		out []streamSSEEvent
		cur streamSSEEvent
		any bool
	)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if any {
				out = append(out, cur)
				if max > 0 && len(out) >= max {
					return out
				}
			}
			cur, any = streamSSEEvent{}, false
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "retry:"):
		case strings.HasPrefix(line, "id: "):
			cur.ID, any = strings.TrimPrefix(line, "id: "), true
		case strings.HasPrefix(line, "event: "):
			cur.Event, any = strings.TrimPrefix(line, "event: "), true
		case strings.HasPrefix(line, "data: "):
			cur.Data, any = strings.TrimPrefix(line, "data: "), true
		default:
			t.Fatalf("unexpected SSE line %q", line)
		}
	}
	return out
}

// streamSeqs is the seq of each event, failing if an event does not decode.
func streamSeqs(t testing.TB, evs []streamSSEEvent) []int64 {
	t.Helper()
	out := make([]int64, 0, len(evs))
	for _, e := range evs {
		var ev conversation.TurnEvent
		if err := json.Unmarshal([]byte(e.Data), &ev); err != nil {
			t.Fatalf("event %q does not decode: %v", e.ID, err)
		}
		var head struct {
			Seq  int64  `json:"seq"`
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(e.Data), &head)
		if strconv.FormatInt(head.Seq, 10) != e.ID {
			t.Fatalf("id %q does not match the event's seq %d", e.ID, head.Seq)
		}
		if head.Type != e.Event {
			t.Fatalf("event name %q does not match the event's type %q", e.Event, head.Type)
		}
		out = append(out, head.Seq)
	}
	return out
}

func streamRange(from, to int64) []int64 {
	var out []int64
	for i := from; i < to; i++ {
		out = append(out, i)
	}
	return out
}

// streamBody reads a whole response body.
func streamBody(t testing.TB, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// stalledWriter is a ResponseWriter whose peer has stopped reading: after okWrites
// writes every Write blocks until the write deadline and then fails, as a TCP
// connection with full buffers does.
type stalledWriter struct {
	hdr       http.Header
	okWrites  int
	writes    atomic.Int64
	deadline  atomic.Int64 // unix nanos
	status    int
	stalledAt atomic.Int64
}

func newStalledWriter(okWrites int) *stalledWriter {
	return &stalledWriter{hdr: http.Header{}, okWrites: okWrites}
}

func (w *stalledWriter) Header() http.Header  { return w.hdr }
func (w *stalledWriter) WriteHeader(code int) { w.status = code }
func (w *stalledWriter) Flush()               {}
func (w *stalledWriter) SetWriteDeadline(t time.Time) error {
	w.deadline.Store(t.UnixNano())
	return nil
}

func (w *stalledWriter) Write(p []byte) (int, error) {
	if int(w.writes.Add(1)) <= w.okWrites {
		return len(p), nil
	}
	w.stalledAt.CompareAndSwap(0, time.Now().UnixNano())
	if d := time.Until(time.Unix(0, w.deadline.Load())); d > 0 {
		time.Sleep(d)
	}
	return 0, os.ErrDeadlineExceeded
}

// streamReq builds a GET request for a turn as a principal, for a direct handler call.
func streamReq(turnID, principal, query string) *http.Request {
	u := "/sse/" + turnID
	if query != "" {
		u += "?" + query
	}
	r := httptest.NewRequest(http.MethodGet, u, nil)
	r.SetPathValue("turn_id", turnID)
	if principal != "" {
		r.Header.Set("X-Test-Principal", principal)
	}
	return r
}

// errorWithDetails is an error body whose open details map holds what a test
// puts in it.
func errorWithDetails(details map[string]any) common.Error {
	return common.Error{Code: common.ErrorCode("unavailable"), Message: "not ready", Retryable: true, Details: details}
}
