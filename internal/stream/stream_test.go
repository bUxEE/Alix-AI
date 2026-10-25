package stream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// TestSSEResumeFromCursor: a client that loses its connection resumes from the
// last event it received, by the Last-Event-ID header or the cursor parameter,
// and gets every later event exactly once, in order, byte for byte what a client
// that stayed connected got. The turn is run once: a resume reads from the hub
// and never reaches the producer.
func TestSSEResumeFromCursor(t *testing.T) {
	rig := newStreamRig(t, Config{}, nil)
	turnID := streamTurnID(1)
	kinds := streamKinds(12)
	scope := streamScope("ten1", "alice")

	var runs atomic.Int64
	gate := make(chan struct{})
	turn, err := rig.hub.Start(context.Background(), scope, turnID, func(ctx context.Context, tn *Turn) error {
		runs.Add(1)
		streamPublish(t, tn, turnID, kinds, 0, 4)
		<-gate // the client drops here, mid-turn
		streamPublish(t, tn, turnID, kinds, 4, len(kinds))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Client 1: reads four events and goes away.
	c1ctx, c1cancel := context.WithCancel(ctx)
	resp := rig.get(t, c1ctx, turnID, "alice", nil, "")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d, content type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	first := streamReadSSE(t, resp.Body, 4)
	c1cancel()
	resp.Body.Close()
	if got := streamSeqs(t, first); !reflect.DeepEqual(got, streamRange(0, 4)) {
		t.Fatalf("client 1 read seqs %v", got)
	}

	// Client 2 resumes while the producer is still blocked: it gets what is
	// retained after its cursor at once, then the rest live.
	resp2 := rig.get(t, ctx, turnID, "alice", map[string]string{"Last-Event-ID": "1"}, "")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("resume status %d", resp2.StatusCode)
	}
	defer resp2.Body.Close()
	head := streamReadSSE(t, resp2.Body, 2)
	if got := streamSeqs(t, head); !reflect.DeepEqual(got, []int64{2, 3}) {
		t.Fatalf("resume from 1 started with seqs %v, want [2 3]", got)
	}
	resume := make(chan []streamSSEEvent, 1)
	go func() { resume <- streamReadSSE(t, resp2.Body, 0) }()
	select {
	case <-resume:
		t.Fatal("the resumed stream ended before the turn did")
	case <-time.After(100 * time.Millisecond):
	}
	close(gate)
	var tail []streamSSEEvent
	select {
	case tail = <-resume:
	case <-ctx.Done():
		t.Fatal("the resumed stream did not finish")
	}
	if got := streamSeqs(t, tail); !reflect.DeepEqual(got, streamRange(4, int64(len(kinds)))) {
		t.Fatalf("live part of the resume carried seqs %v", got)
	}
	if last := tail[len(tail)-1]; last.Event != "done" {
		t.Fatalf("the stream does not end with done: %q", last.Event)
	}
	if !turn.Finished() {
		t.Fatal("turn not finished")
	}

	// A client that stayed connected for the whole turn gets the same bytes.
	whole := streamReadSSE(t, mustOK(t, rig.get(t, ctx, turnID, "alice", nil, "")).Body, 0)
	if got := streamSeqs(t, whole); !reflect.DeepEqual(got, streamRange(0, int64(len(kinds)))) {
		t.Fatalf("a full read carried seqs %v", got)
	}
	for _, e := range append(append([]streamSSEEvent(nil), head...), tail...) {
		var found bool
		for _, w := range whole {
			if w.ID == e.ID {
				found = true
				if w != e {
					t.Errorf("seq %s differs between a resumed and a full read:\n %v\n %v", e.ID, e, w)
				}
			}
		}
		if !found {
			t.Errorf("seq %s missing from the full read", e.ID)
		}
	}

	t.Run("cursor parameter and header", func(t *testing.T) {
		n := int64(len(kinds))
		for name, tc := range map[string]struct {
			header map[string]string
			query  string
			want   []int64
		}{
			"cursor parameter":           {nil, "cursor=5", streamRange(6, n)},
			"header":                     {map[string]string{"Last-Event-ID": "7"}, "", streamRange(8, n)},
			"the later of the two wins":  {map[string]string{"Last-Event-ID": "8"}, "cursor=3", streamRange(9, n)},
			"cursor 0 is event 0 seen":   {nil, "cursor=0", streamRange(1, n)},
			"empty cursor is the start":  {nil, "cursor=", streamRange(0, n)},
			"no cursor is the start":     {nil, "", streamRange(0, n)},
			"cursor at the last but one": {nil, "cursor=" + itoa(n-2), []int64{n - 1}},
		} {
			resp := mustOK(t, rig.get(t, ctx, turnID, "alice", tc.header, tc.query))
			got := streamSeqs(t, streamReadSSE(t, resp.Body, 0))
			resp.Body.Close()
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s: seqs %v, want %v", name, got, tc.want)
			}
		}
	})

	t.Run("a cursor at the end of a finished turn is 204", func(t *testing.T) {
		resp := rig.get(t, ctx, turnID, "alice", nil, "cursor="+itoa(int64(len(kinds)-1)))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status %d, want 204", resp.StatusCode)
		}
		if body := streamBody(t, resp); body != "" {
			t.Errorf("a 204 has a body: %q", body)
		}
	})

	t.Run("a bad cursor is refused, never an empty stream", func(t *testing.T) {
		for name, tc := range map[string]struct {
			header map[string]string
			query  string
			status int
			reason Reason
		}{
			"not a number":      {nil, "cursor=abc", 400, ReasonCursorMalformed},
			"negative":          {nil, "cursor=-1", 400, ReasonCursorMalformed},
			"a fraction":        {nil, "cursor=1.5", 400, ReasonCursorMalformed},
			"a sign":            {nil, "cursor=%2B1", 400, ReasonCursorMalformed},
			"too large":         {nil, "cursor=4294967296", 400, ReasonCursorMalformed},
			"twice":             {nil, "cursor=1&cursor=2", 400, ReasonCursorMalformed},
			"bad header":        {map[string]string{"Last-Event-ID": "x"}, "", 400, ReasonCursorMalformed},
			"ahead of the turn": {nil, "cursor=500", 400, ReasonCursorAhead},
			"one past the end":  {nil, "cursor=" + itoa(int64(len(kinds))), 400, ReasonCursorAhead},
		} {
			resp := rig.get(t, ctx, turnID, "alice", tc.header, tc.query)
			body := streamBody(t, resp)
			if resp.StatusCode != tc.status {
				t.Errorf("%s: status %d, want %d (%s)", name, resp.StatusCode, tc.status, body)
				continue
			}
			var e contracts.Error
			if err := json.Unmarshal([]byte(body), &e); err != nil || e.Code != contracts.ErrValidationFailed {
				t.Errorf("%s: body %q is not a typed error", name, body)
			}
			if e.Details["reason"] != string(tc.reason) {
				t.Errorf("%s: reason %v, want %s", name, e.Details["reason"], tc.reason)
			}
		}
	})

	t.Run("the producer ran once", func(t *testing.T) {
		if n := runs.Load(); n != 1 {
			t.Fatalf("the turn was run %d times; a resume must only read", n)
		}
	})

	t.Run("a resume carries no confirmation secret header", func(t *testing.T) {
		resp := mustOK(t, rig.get(t, ctx, turnID, "alice", nil, "cursor=2"))
		defer resp.Body.Close()
		for k := range resp.Header {
			if strings.Contains(strings.ToLower(k), "confirmation") || strings.Contains(strings.ToLower(k), "token") {
				t.Errorf("response header %s on a stream", k)
			}
		}
	})
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func mustOK(t testing.TB, resp *http.Response) *http.Response {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	return resp
}

// A cursor behind the retained window is refused with 410; the stream is never
// resumed with a gap.
func TestSSEResumeBehindTheWindowIsExpired(t *testing.T) {
	rig := newStreamRig(t, Config{MaxEvents: 8, MaxLag: 4}, nil)
	turnID := streamTurnID(2)
	turn, err := rig.hub.Begin(streamScope("ten1", "alice"), turnID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := streamKinds(30)
	streamPublish(t, turn, turnID, kinds, 0, 30)
	if st := turn.Stats(); st.Retained > 8 || st.FirstSeq != int64(30-st.Retained) {
		t.Fatalf("ring holds %d events from %d", st.Retained, st.FirstSeq)
	}
	ctx := context.Background()
	resp := rig.get(t, ctx, turnID, "alice", nil, "cursor=3")
	body := streamBody(t, resp)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("status %d, want 410: %s", resp.StatusCode, body)
	}
	var e contracts.Error
	_ = json.Unmarshal([]byte(body), &e)
	if e.Details["reason"] != string(ReasonCursorExpired) {
		t.Errorf("body %s", body)
	}
	// From the oldest retained event onward is still possible.
	st := turn.Stats()
	resp = mustOK(t, rig.get(t, ctx, turnID, "alice", nil, "cursor="+itoa(st.FirstSeq-1)))
	defer resp.Body.Close()
	if got := streamSeqs(t, streamReadSSE(t, resp.Body, 0)); !reflect.DeepEqual(got, streamRange(st.FirstSeq, 30)) {
		t.Errorf("window read %v", got)
	}
}

// A turn is visible to the scope that opened it and to nobody else. Another
// principal, another tenant and an unknown turn all get the same refusal.
func TestSSEScopeIsolation(t *testing.T) {
	rig := newStreamRig(t, Config{}, nil)
	turnID := streamTurnID(3)
	turn, _ := rig.hub.Begin(streamScope("ten1", "alice"), turnID)
	streamPublish(t, turn, turnID, streamKinds(5), 0, 5)
	ctx := context.Background()

	unknown := rig.get(t, ctx, streamTurnID(99), "alice", nil, "")
	unknownBody := streamBody(t, unknown)
	if unknown.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown turn: %d %s", unknown.StatusCode, unknownBody)
	}
	for name, tc := range map[string]struct {
		principal string
		tenant    string
	}{
		"another principal": {"bob", ""},
		"another tenant":    {"alice", "ten2"},
	} {
		resp := rig.get(t, ctx, turnID, tc.principal, map[string]string{"X-Test-Tenant": tc.tenant}, "")
		body := streamBody(t, resp)
		if resp.StatusCode != unknown.StatusCode || body != unknownBody {
			t.Errorf("%s: %d %q differs from an unknown turn's %d %q", name, resp.StatusCode, body, unknown.StatusCode, unknownBody)
		}
	}
	// No credential at all.
	resp := rig.get(t, ctx, turnID, "", nil, "")
	body := streamBody(t, resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no credential: %d %s", resp.StatusCode, body)
	}
	// A principal in the query string is not a principal.
	resp = rig.get(t, ctx, turnID, "", nil, "principal=alice&tenant=ten1&cursor=0")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a principal in the query was believed: %d", resp.StatusCode)
	}
	resp.Body.Close()
	// Only GET reads a stream; a POST must not run or restart anything.
	req, _ := http.NewRequest(http.MethodPost, rig.srv.URL+"/sse/"+turnID, strings.NewReader("{}"))
	req.Header.Set("X-Test-Principal", "alice")
	pr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	pr.Body.Close()
	if pr.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", pr.StatusCode)
	}
	// The same turn id can be used by another scope without touching this one.
	other, err := rig.hub.Begin(streamScope("ten2", "alice"), turnID)
	if err != nil {
		t.Fatalf("a turn id is per scope: %v", err)
	}
	_ = other
	if _, err := rig.hub.Begin(streamScope("ten1", "alice"), turnID); err == nil || contracts.CodeOf(err) != contracts.ErrConflict {
		t.Errorf("a turn id is used twice in one scope: %v", err)
	}
}

// TestWebSocketStream: the WebSocket transport carries the same events, one per
// text message, resumes from a cursor, refuses a foreign Origin and any message
// from the client, and ends with a normal closure after done.
func TestWebSocketStream(t *testing.T) {
	rig := newStreamRig(t, Config{}, func(o *Options) { o.AllowedOrigins = []string{"app.example.test"} })
	turnID := streamTurnID(4)
	kinds := streamKinds(10)
	scope := streamScope("ten1", "alice")
	gate := make(chan struct{})
	_, err := rig.hub.Start(context.Background(), scope, turnID, func(ctx context.Context, tn *Turn) error {
		streamPublish(t, tn, turnID, kinds, 0, 5)
		<-gate
		streamPublish(t, tn, turnID, kinds, 5, len(kinds))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(rig.srv.URL, "http") + "/ws/" + turnID

	dial := func(query string, hdr http.Header) (*websocket.Conn, *http.Response, error) {
		if hdr == nil {
			hdr = http.Header{}
		}
		hdr.Set("X-Test-Principal", "alice")
		u := wsURL
		if query != "" {
			u += "?" + query
		}
		return websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: hdr, Subprotocols: []string{WebSocketSubprotocol}})
	}
	readEvent := func(c *websocket.Conn) (conversation.TurnEvent, string) {
		t.Helper()
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if typ != websocket.MessageText {
			t.Fatalf("message type %v, want text", typ)
		}
		var ev conversation.TurnEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			t.Fatalf("message does not decode as a TurnEvent: %v", err)
		}
		var head struct {
			Seq int64 `json:"seq"`
		}
		_ = json.Unmarshal(data, &head)
		return ev, string(data) + "#" + itoa(head.Seq)
	}

	c, resp, err := dial("", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if got := c.Subprotocol(); got != WebSocketSubprotocol {
		t.Errorf("subprotocol %q", got)
	}
	_ = resp
	seen := map[int64]string{}
	for want := int64(0); want < 5; want++ {
		_, raw := readEvent(c)
		seq := seqOfRaw(raw)
		if seq != want {
			t.Fatalf("got seq %d, want %d", seq, want)
		}
		seen[seq] = raw
	}
	close(gate) // the rest of the turn is delivered live
	for want := int64(5); want < int64(len(kinds)); want++ {
		_, raw := readEvent(c)
		if seq := seqOfRaw(raw); seq != want {
			t.Fatalf("live: got seq %d, want %d", seq, want)
		}
		seen[seqOfRaw(raw)] = raw
	}
	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("after done the connection ends with %v, want a normal closure", err)
	}

	t.Run("resume from a cursor", func(t *testing.T) {
		c, _, err := dial("cursor=6", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer c.CloseNow()
		for want := int64(7); want < int64(len(kinds)); want++ {
			_, raw := readEvent(c)
			if seq := seqOfRaw(raw); seq != want {
				t.Fatalf("got seq %d, want %d", seq, want)
			}
			if raw != seen[want] {
				t.Errorf("seq %d differs between the live and the resumed message", want)
			}
		}
		if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
			t.Errorf("end: %v", err)
		}
	})

	t.Run("the same bytes as the SSE transport", func(t *testing.T) {
		resp := mustOK(t, rig.get(t, ctx, turnID, "alice", nil, ""))
		defer resp.Body.Close()
		for _, e := range streamReadSSE(t, resp.Body, 0) {
			if got := seen[seqOfRaw(e.Data+"#"+e.ID)]; got != e.Data+"#"+e.ID {
				t.Errorf("seq %s: the transports disagree", e.ID)
			}
		}
	})

	t.Run("a cursor at the end closes at once", func(t *testing.T) {
		c, _, err := dial("cursor="+itoa(int64(len(kinds)-1)), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer c.CloseNow()
		if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
			t.Errorf("end: %v", err)
		}
	})

	t.Run("refusals are HTTP statuses before the upgrade", func(t *testing.T) {
		for name, tc := range map[string]struct {
			query  string
			turn   string
			status int
		}{
			"malformed cursor": {"cursor=zzz", turnID, 400},
			"cursor ahead":     {"cursor=999", turnID, 400},
			"unknown turn":     {"", streamTurnID(77), 403},
		} {
			u := "ws" + strings.TrimPrefix(rig.srv.URL, "http") + "/ws/" + tc.turn
			if tc.query != "" {
				u += "?" + tc.query
			}
			_, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: http.Header{"X-Test-Principal": {"alice"}}})
			if err == nil {
				t.Errorf("%s: upgraded", name)
				continue
			}
			if resp == nil || resp.StatusCode != tc.status {
				t.Errorf("%s: response %v, want %d", name, resp, tc.status)
			}
		}
		_, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{})
		if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("no credential: %v %v", resp, err)
		}
	})

	t.Run("a foreign origin is refused, an allowed one is not", func(t *testing.T) {
		_, resp, err := dial("", http.Header{"Origin": {"https://evil.example.test"}})
		if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Errorf("foreign origin: %v %v", resp, err)
		}
		c, _, err := dial("cursor=8", http.Header{"Origin": {"https://app.example.test"}})
		if err != nil {
			t.Fatalf("allowed origin: %v", err)
		}
		c.CloseNow()
	})

	t.Run("the client may not send", func(t *testing.T) {
		gate2 := make(chan struct{})
		id2 := streamTurnID(5)
		_, err := rig.hub.Start(context.Background(), scope, id2, func(ctx context.Context, tn *Turn) error {
			streamPublish(t, tn, id2, kinds, 0, 2)
			<-gate2
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		defer close(gate2)
		u := "ws" + strings.TrimPrefix(rig.srv.URL, "http") + "/ws/" + id2
		c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: http.Header{"X-Test-Principal": {"alice"}}})
		if err != nil {
			t.Fatal(err)
		}
		defer c.CloseNow()
		readEvent(c)
		readEvent(c)
		if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"turn","text":"do it"}`)); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, _, err = c.Read(ctx)
		if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
			t.Errorf("a message from the client ends with %v, want policy violation", err)
		}
	})
}

func seqOfRaw(raw string) int64 {
	data, _, _ := strings.Cut(raw, "#")
	var head struct {
		Seq int64 `json:"seq"`
	}
	_ = json.Unmarshal([]byte(data), &head)
	return head.Seq
}

// TestBackpressureBounded: a reader that does not keep up costs the service a
// bounded amount and never stalls the producer. The ring is bounded in events
// and bytes, a live subscriber's lag is bounded and past it the subscriber is cut
// off, a stalled connection is dropped at its write deadline, and a cut-off
// reader resumes without a gap or a repeat while its events are retained.
//
// SEC-077
func TestBackpressureBounded(t *testing.T) {
	t.Run("the producer is never blocked and memory stays within the bounds", func(t *testing.T) {
		hub := NewHub(Config{MaxEvents: 16, MaxLag: 4, MaxBytes: 8 << 10})
		turnID := streamTurnID(10)
		turn, err := hub.Begin(streamScope("ten1", "alice"), turnID)
		if err != nil {
			t.Fatal(err)
		}
		sub, err := turn.Subscribe(FromStart)
		if err != nil {
			t.Fatal(err)
		}
		defer sub.Close()
		const total = 5000
		kinds := make([]string, total)
		for i := range kinds {
			kinds[i] = "token"
		}
		kinds[total-1] = "done"
		start := time.Now()
		done := make(chan struct{})
		go func() {
			defer close(done)
			streamPublish(t, turn, turnID, kinds, 0, total)
		}()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("the producer was blocked by a reader that does not read")
		}
		t.Logf("published %d events past a stalled reader in %v", total, time.Since(start))

		st := turn.Stats()
		if st.Retained > 16 {
			t.Errorf("the ring holds %d events, bound 16", st.Retained)
		}
		if st.Bytes > 8<<10 {
			t.Errorf("the ring holds %d bytes, bound %d", st.Bytes, 8<<10)
		}
		if hs := hub.Stats(); hs.Bytes != int64(st.Bytes) {
			t.Errorf("the hub accounts %d bytes, the turn holds %d", hs.Bytes, st.Bytes)
		}
		if st.FirstSeq != int64(total-st.Retained) {
			t.Errorf("window starts at %d with %d events of %d", st.FirstSeq, st.Retained, total)
		}
		// The stalled reader was cut off the moment its queue was full: it never
		// holds a lag above the bound, and its next read says so.
		if _, err := sub.Next(context.Background()); !errors.Is(err, ErrSlowConsumer) {
			t.Fatalf("a reader 5000 events behind got %v, want ErrSlowConsumer", err)
		}
		if contracts.CodeOf(ErrSlowConsumer) != contracts.ErrResourceExhausted {
			t.Errorf("the slow consumer error is not typed: %q", contracts.CodeOf(ErrSlowConsumer))
		}
		if peak := sub.PeakLag(); peak > 4 {
			t.Errorf("peak lag %d exceeds the bound 4", peak)
		}
		if st.Subscribers != 0 {
			t.Errorf("%d subscribers remain after the only one was cut off", st.Subscribers)
		}
		// Resuming a long way behind is refused, not served with a gap.
		if _, err := turn.Subscribe(10); !errors.Is(err, ErrCursorExpired) {
			t.Errorf("a cursor behind the window: %v", err)
		}
	})

	t.Run("a cut-off reader resumes without a gap or a repeat", func(t *testing.T) {
		hub := NewHub(Config{MaxEvents: 64, MaxLag: 8})
		turnID := streamTurnID(11)
		turn, _ := hub.Begin(streamScope("ten1", "alice"), turnID)
		const total = 40
		kinds := make([]string, total)
		for i := range kinds {
			kinds[i] = "token"
		}
		kinds[total-1] = "done"
		streamPublish(t, turn, turnID, kinds, 0, 3)

		sub, err := turn.Subscribe(FromStart)
		if err != nil {
			t.Fatal(err)
		}
		var got []int64
		for i := 0; i < 3; i++ {
			f, err := sub.Next(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, f.Seq)
		}
		// The reader stops reading while the producer carries on: 20 more
		// events is more than its queue of 8.
		streamPublish(t, turn, turnID, kinds, 3, 23)
		if _, err := sub.Next(context.Background()); !errors.Is(err, ErrSlowConsumer) {
			t.Fatalf("cut-off: %v", err)
		}
		sub.Close()
		streamPublish(t, turn, turnID, kinds, 23, total)

		// It resumes from the last event it received.
		resumed, err := turn.Subscribe(got[len(got)-1])
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		defer resumed.Close()
		for {
			f, err := resumed.Next(context.Background())
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, f.Seq)
		}
		if !reflect.DeepEqual(got, streamRange(0, total)) {
			t.Fatalf("seqs across the cut and the resume: %v", got)
		}
	})

	t.Run("a connection that stops reading is dropped at its write deadline", func(t *testing.T) {
		hub := NewHub(Config{})
		turnID := streamTurnID(12)
		turn, _ := hub.Begin(streamScope("ten1", "alice"), turnID)
		kinds := streamKinds(8)
		streamPublish(t, turn, turnID, kinds, 0, 4)

		h := hub.SSEHandler(Options{Scope: streamScopeFromHeader, WriteTimeout: 150 * time.Millisecond, Heartbeat: time.Minute})
		w := newStalledWriter(3) // the retry hint and two events fit; then the peer stops
		ended := make(chan struct{})
		go func() {
			defer close(ended)
			h.ServeHTTP(w, streamReq(turnID, "alice", ""))
		}()
		// The producer carries on and is not held up by the stalled connection.
		pubDone := make(chan struct{})
		go func() {
			defer close(pubDone)
			streamPublish(t, turn, turnID, kinds, 4, 8)
		}()
		select {
		case <-pubDone:
		case <-time.After(5 * time.Second):
			t.Fatal("the producer waited for a stalled connection")
		}
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
			t.Fatal("the handler is still waiting on a connection that stopped reading")
		}
		if w.stalledAt.Load() == 0 {
			t.Fatal("the writer never stalled; the test did not exercise the deadline")
		}
		if n := turn.Stats().Subscribers; n != 0 {
			t.Errorf("%d subscribers remain after the connection was dropped", n)
		}
	})

	t.Run("one event and one turn are bounded", func(t *testing.T) {
		hub := NewHub(Config{MaxEventBytes: 600, MaxBytes: 2000})
		turnID := streamTurnID(13)
		turn, _ := hub.Begin(streamScope("ten1", "alice"), turnID)
		big := streamEvent(t, turnID, 0, "token")
		tok := big.Value.(conversation.TokenEvent)
		tok.Text = strings.Repeat("x", 1000)
		err := turn.Publish(conversation.TurnEvent{Value: tok})
		if !errors.Is(err, ErrEventRefused) {
			t.Fatalf("an oversize event: %v", err)
		}
		// The refusal did not use up the sequence number.
		if err := turn.Publish(streamEvent(t, turnID, 0, "token")); err != nil {
			t.Fatalf("the same seq after a refusal: %v", err)
		}
		for i := 1; i < 40; i++ {
			if err := turn.Publish(streamEvent(t, turnID, int64(i), "token")); err != nil {
				t.Fatal(err)
			}
		}
		if st := turn.Stats(); st.Bytes > 2000 {
			t.Errorf("turn holds %d bytes, bound 2000", st.Bytes)
		}
	})

	t.Run("the hub is bounded across turns and forgets finished turns first", func(t *testing.T) {
		fake := clock.NewFake(time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC))
		hub := NewHub(Config{MaxTurns: 3, MaxTurnsPerTenant: 3, MaxTotalBytes: 6 << 10, Clock: fake})
		mk := func(n int, finish bool) *Turn {
			id := streamTurnID(100 + n)
			turn, err := hub.Begin(streamScope("ten1", "alice"), id)
			if err != nil {
				t.Fatalf("begin %d: %v", n, err)
			}
			streamPublish(t, turn, id, []string{"token", "done"}, 0, 1)
			if finish {
				if err := turn.Publish(streamEvent(t, id, 1, "done")); err != nil {
					t.Fatal(err)
				}
			}
			fake.Advance(time.Second)
			return turn
		}
		a := mk(1, true)
		_ = mk(2, true)
		_ = mk(3, false)
		// The hub is at its turn limit; the oldest finished turn makes way.
		d := mk(4, false)
		if _, err := hub.Lookup(streamScope("ten1", "alice"), a.ID()); !errors.Is(err, ErrNotVisible) {
			t.Errorf("the oldest finished turn was not forgotten: %v", err)
		}
		if hs := hub.Stats(); hs.Turns != 3 {
			t.Errorf("hub holds %d turns, bound 3", hs.Turns)
		}
		_ = d
		// With the finished ones gone, a fifth open turn does not fit: refused,
		// not an unbounded hub, and the open turns are untouched.
		fifth := streamTurnID(200)
		_, err := hub.Begin(streamScope("ten1", "alice"), fifth)
		if err != nil {
			// one finished turn (2) is still there, so this admits and evicts it
			t.Fatalf("begin with a finished turn to forget: %v", err)
		}
		if _, err := hub.Begin(streamScope("ten1", "alice"), streamTurnID(201)); !errors.Is(err, ErrCapacity) {
			t.Errorf("a hub full of open turns admitted another: %v", err)
		}
		if contracts.CodeOf(ErrCapacity) != contracts.ErrResourceExhausted {
			t.Errorf("capacity is not a typed resource_exhausted")
		}
	})

	t.Run("one tenant at its share does not starve another", func(t *testing.T) {
		hub := NewHub(Config{MaxTurns: 8, MaxTurnsPerTenant: 2, MaxTotalBytes: 64 << 10, MaxBytesPerTenant: 1500})
		a, b := streamScope("tenA", "alice"), streamScope("tenB", "bob")
		idA1, idA2, idA3, idB1 := streamTurnID(300), streamTurnID(301), streamTurnID(302), streamTurnID(303)
		ta1, err := hub.Begin(a, idA1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := hub.Begin(a, idA2); err != nil {
			t.Fatal(err)
		}
		_, err = hub.Begin(a, idA3)
		if !errors.Is(err, ErrCapacity) || contracts.CodeOf(err) != contracts.ErrResourceExhausted {
			t.Fatalf("a tenant over its turn share: %v", err)
		}
		tb, err := hub.Begin(b, idB1)
		if err != nil {
			t.Fatalf("another tenant is refused because tenant A is at its limit: %v", err)
		}
		// Tenant A uses up its byte share; B's stream is unaffected.
		var aEvents int
		for i := 0; ; i++ {
			if i > 200 {
				t.Fatal("tenant A is never refused")
			}
			err := ta1.Publish(streamEvent(t, idA1, int64(i), "token"))
			if err != nil {
				if !errors.Is(err, ErrCapacity) {
					t.Fatalf("tenant A at its byte share: %v", err)
				}
				break
			}
			aEvents++
		}
		if aEvents < 5 {
			t.Fatalf("tenant A got only %d events in before its share", aEvents)
		}
		if ta1.NextSeq() != int64(aEvents) {
			t.Errorf("a refused event used up a sequence number")
		}
		for i := 0; i < aEvents/2; i++ {
			if err := tb.Publish(streamEvent(t, idB1, int64(i), "token")); err != nil {
				t.Fatalf("tenant B is held up by tenant A at event %d: %v", i, err)
			}
		}
		hub.mu.Lock()
		held := hub.tenants[tenantKey{application: "app1", tenant: "tenA"}].bytes.Load()
		hub.mu.Unlock()
		if held > 1500 {
			t.Errorf("tenant A holds %d bytes, share 1500", held)
		}

		// A tenant's own finished turns make room for its next turn; another
		// tenant's finished turns are left alone.
		hub2 := NewHub(Config{MaxTurns: 8, MaxTurnsPerTenant: 2})
		finish := func(sc contracts.Scope, n int) {
			id := streamTurnID(n)
			tn, err := hub2.Begin(sc, id)
			if err != nil {
				t.Fatalf("begin %d: %v", n, err)
			}
			if err := tn.Publish(streamEvent(t, id, 0, "done")); err != nil {
				t.Fatal(err)
			}
		}
		finish(a, 400)
		finish(b, 401)
		finish(a, 402)
		if _, err := hub2.Begin(a, streamTurnID(403)); err != nil {
			t.Fatalf("a tenant with finished turns at its share: %v", err)
		}
		if _, err := hub2.Lookup(a, streamTurnID(400)); !errors.Is(err, ErrNotVisible) {
			t.Errorf("tenant A's oldest finished turn was kept: %v", err)
		}
		if _, err := hub2.Lookup(b, streamTurnID(401)); err != nil {
			t.Errorf("tenant B's finished turn was forgotten for tenant A: %v", err)
		}
		if n := len(hub2.tenants); n != 2 {
			t.Errorf("%d tenant accounts, want 2", n)
		}
	})

	t.Run("a turn serves a bounded number of subscribers", func(t *testing.T) {
		hub := NewHub(Config{MaxSubscribers: 2})
		turnID := streamTurnID(14)
		turn, _ := hub.Begin(streamScope("ten1", "alice"), turnID)
		streamPublish(t, turn, turnID, streamKinds(3), 0, 1)
		var subs []*Subscription
		for i := 0; i < 2; i++ {
			s, err := turn.Subscribe(FromStart)
			if err != nil {
				t.Fatal(err)
			}
			subs = append(subs, s)
		}
		if _, err := turn.Subscribe(FromStart); !errors.Is(err, ErrSubscribers) {
			t.Fatalf("third subscriber: %v", err)
		}
		subs[0].Close()
		if s, err := turn.Subscribe(FromStart); err != nil {
			t.Fatalf("a place was freed: %v", err)
		} else {
			s.Close()
		}
	})
}

// A rich block is validated and sanitised before it is buffered, so a live stream
// and a resumed one carry the same, clean bytes; a chart may show only numbers
// that are in the turn's facts.
func TestStreamValidatesBeforeBuffering(t *testing.T) {
	hub := NewHub(Config{})
	turnID := streamTurnID(20)
	turn, _ := hub.Begin(streamScope("ten1", "alice"), turnID)
	kinds := streamKinds(8)
	streamPublish(t, turn, turnID, kinds, 0, 2) // progress, directive (with the facts 12, 18, 9)

	// A chart with an invented number is refused, and the seq is free again.
	err := turn.Publish(streamEvent(t, turnID, 2, "chart-invented"))
	if !errors.Is(err, ErrEventRefused) {
		t.Fatalf("a chart with a number that is not a fact: %v", err)
	}
	if contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
		t.Errorf("code %q", contracts.CodeOf(err))
	}
	if turn.NextSeq() != 2 {
		t.Fatalf("a refused event used up its sequence number: next is %d", turn.NextSeq())
	}
	// A chart of the facts passes.
	if err := turn.Publish(streamEvent(t, turnID, 2, "chart")); err != nil {
		t.Fatalf("a chart of facts: %v", err)
	}

	// Markdown is sanitised on the way in: what is buffered is what is sent.
	md := streamEvent(t, turnID, 3, "markdown")
	be := md.Value.(conversation.BlockEvent)
	be.Block = conversation.RichBlock{Value: conversation.MarkdownBlock{Text: "Booked <script>alert(1)</script> [here](javascript:alert(1))"}}
	if err := turn.Publish(conversation.TurnEvent{Value: be}); err != nil {
		t.Fatalf("markdown block: %v", err)
	}
	sub, _ := turn.Subscribe(2)
	f, err := sub.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(f.Data), "<script") || strings.Contains(string(f.Data), "javascript:") {
		t.Errorf("an unsanitised block was buffered: %s", f.Data)
	}

	// A chart before any directive has no facts at all, so any number is refused.
	id2 := streamTurnID(21)
	t2, _ := hub.Begin(streamScope("ten1", "alice"), id2)
	if err := t2.Publish(streamEvent(t, id2, 0, "chart")); !errors.Is(err, ErrEventRefused) {
		t.Errorf("a chart before the directive's facts: %v", err)
	}

	// Sequence and identity are enforced.
	if err := turn.Publish(streamEvent(t, turnID, 9, "token")); !errors.Is(err, ErrOutOfOrder) {
		t.Errorf("a gap in the sequence: %v", err)
	}
	if err := turn.Publish(streamEvent(t, streamTurnID(55), 4, "token")); !errors.Is(err, ErrOutOfOrder) {
		t.Errorf("another turn's event: %v", err)
	}
	if err := turn.Publish(conversation.TurnEvent{}); err == nil {
		t.Errorf("an empty event was published")
	}
	// Finished: nothing more.
	streamPublish(t, turn, turnID, kinds, 4, 8)
	if err := turn.Publish(streamEvent(t, turnID, 8, "token")); !errors.Is(err, ErrTurnClosed) {
		t.Errorf("an event after done: %v", err)
	}
}

// The confirmation secret is never in a TurnEvent: an event with a member that
// may hold a credential is refused rather than sent, and a stream's response
// never carries the confirmation header.
func TestStreamNeverCarriesConfirmationSecret(t *testing.T) {
	const secret = "cnf_s3cr3t-value-0123456789"
	hub := NewHub(Config{})
	turnID := streamTurnID(30)
	turn, _ := hub.Begin(streamScope("ten1", "alice"), turnID)

	for name, details := range map[string]map[string]any{
		"token":              {"token": secret},
		"confirmation_token": {"confirmation_token": secret},
		"nested":             {"context": map[string]any{"Authorization": "Bearer " + secret}},
		"password":           {"password": secret},
	} {
		ev := conversation.TurnEvent{Value: conversation.ErrorEvent{
			TurnID: "trn_01J0000000000000000000" + "0030", Seq: 0,
			Error: errorWithDetails(details),
		}}
		err := turn.Publish(ev)
		if !errors.Is(err, ErrEventRefused) {
			t.Errorf("%s: an error event carrying a credential-shaped member was published: %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the refusal repeats the secret: %v", name, err)
		}
	}
	// A member that merely mentions a word is not a credential.
	ok := conversation.TurnEvent{Value: conversation.ErrorEvent{
		TurnID: "trn_01J0000000000000000000" + "0030", Seq: 0,
		Error: errorWithDetails(map[string]any{"token_count": 3, "levels": []any{"tenant"}}),
	}}
	if err := turn.Publish(ok); err != nil {
		t.Errorf("a harmless detail was refused: %v", err)
	}
	if st := turn.Stats(); st.Retained != 1 {
		t.Fatalf("retained %d", st.Retained)
	}
	sub, _ := turn.Subscribe(FromStart)
	f, _ := sub.Next(context.Background())
	if strings.Contains(string(f.Data), secret) {
		t.Errorf("the secret is in a buffered frame: %s", f.Data)
	}

	// Through the endpoints: no header named like a secret, no secret in a body.
	rig := newStreamRig(t, Config{}, nil)
	id2 := streamTurnID(31)
	t2, _ := rig.hub.Begin(streamScope("ten1", "alice"), id2)
	kinds := []string{"progress", "directive", "prepared", "done"}
	streamPublish(t, t2, id2, kinds, 0, 4)
	resp := mustOK(t, rig.get(t, context.Background(), id2, "alice", nil, "cursor=0"))
	defer resp.Body.Close()
	for k, v := range resp.Header {
		if strings.EqualFold(k, "X-AlixAI-Confirmation-Token") || strings.Contains(strings.Join(v, ","), secret) {
			t.Errorf("header %s on a stream", k)
		}
	}
	if body, _ := io.ReadAll(resp.Body); strings.Contains(string(body), secret) {
		t.Errorf("secret in a resumed stream")
	}
}

// TestStreamRetention: a finished turn stays resumable for its retention and is
// then forgotten; an open turn that goes quiet is abandoned and says so.
func TestStreamRetention(t *testing.T) {
	fake := clock.NewFake(time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC))
	hub := NewHub(Config{Clock: fake, Retention: time.Minute, IdleTimeout: 5 * time.Minute})
	scope := streamScope("ten1", "alice")

	doneID, openID := streamTurnID(40), streamTurnID(41)
	finished, _ := hub.Begin(scope, doneID)
	streamPublish(t, finished, doneID, streamKinds(3), 0, 3)
	open, _ := hub.Begin(scope, openID)
	streamPublish(t, open, openID, streamKinds(3), 0, 1)
	sub, err := open.Subscribe(0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	fake.Advance(30 * time.Second)
	if _, err := hub.Lookup(scope, doneID); err != nil {
		t.Fatalf("a finished turn within its retention: %v", err)
	}
	fake.Advance(31 * time.Second)
	if _, err := hub.Lookup(scope, doneID); !errors.Is(err, ErrNotVisible) {
		t.Fatalf("a finished turn past its retention: %v", err)
	}
	fake.Advance(5 * time.Minute)
	hub.Sweep()
	if _, err := sub.Next(context.Background()); !errors.Is(err, ErrAborted) {
		t.Fatalf("an idle open turn: %v", err)
	}
	if contracts.CodeOf(ErrAborted) != contracts.ErrUnavailable {
		t.Errorf("an abandoned turn is not typed unavailable")
	}
	if err := open.Publish(streamEvent(t, openID, 1, "token")); !errors.Is(err, ErrAborted) {
		t.Errorf("publishing to an abandoned turn: %v", err)
	}
	if _, err := open.Subscribe(0); !errors.Is(err, ErrAborted) {
		t.Errorf("subscribing to an abandoned turn: %v", err)
	}
	fake.Advance(2 * time.Minute)
	hub.Sweep()
	if st := hub.Stats(); st.Turns != 0 || st.Bytes != 0 {
		t.Errorf("hub still holds %+v after everything expired", st)
	}
}

// TestStartFailsTurnCleanly: a producer that returns an error, panics or ends
// without a done event leaves the stream well formed: an error event, then a
// failed done event.
func TestStartFailsTurnCleanly(t *testing.T) {
	for name, produce := range map[string]func(ctx context.Context, tn *Turn) error{
		"error": func(ctx context.Context, tn *Turn) error {
			return contracts.New(contracts.ErrUnavailable, "the model is not ready")
		},
		"panic":   func(ctx context.Context, tn *Turn) error { panic("boom") },
		"no done": func(ctx context.Context, tn *Turn) error { return nil },
		"internal error text is not leaked": func(ctx context.Context, tn *Turn) error {
			return errors.New("connect to 10.0.0.5:5432 failed")
		},
	} {
		t.Run(name, func(t *testing.T) {
			hub := NewHub(Config{})
			id := streamTurnID(50)
			turn, err := hub.Start(context.Background(), streamScope("ten1", "alice"), id, produce)
			if err != nil {
				t.Fatal(err)
			}
			sub, err := turn.Subscribe(FromStart)
			if err != nil {
				t.Fatal(err)
			}
			defer sub.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var types []string
			var raw []string
			for {
				f, err := sub.Next(ctx)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				types = append(types, f.Type)
				raw = append(raw, string(f.Data))
			}
			if !reflect.DeepEqual(types, []string{"error", "done"}) {
				t.Fatalf("events %v, want [error done]", types)
			}
			if !strings.Contains(raw[1], `"failed"`) {
				t.Errorf("done: %s", raw[1])
			}
			for _, r := range raw {
				if strings.Contains(r, "10.0.0.5") || strings.Contains(r, "boom") {
					t.Errorf("internal text leaked into the stream: %s", r)
				}
			}
			if name == "error" && !strings.Contains(raw[0], "unavailable") {
				t.Errorf("typed error lost: %s", raw[0])
			}
		})
	}
}

// ParseCursor reads Last-Event-ID and the cursor parameter strictly.
func TestParseCursor(t *testing.T) {
	for name, tc := range map[string]struct {
		header []string
		query  string
		want   int64
		err    bool
	}{
		"none":        {nil, "", FromStart, false},
		"query":       {nil, "cursor=7", 7, false},
		"header":      {[]string{"9"}, "", 9, false},
		"larger wins": {[]string{"2"}, "cursor=5", 5, false},
		"zero":        {[]string{"0"}, "", 0, false},
		"max":         {nil, "cursor=4294967295", 4294967295, false},
		"over max":    {nil, "cursor=4294967296", 0, true},
		"hex":         {nil, "cursor=0x10", 0, true},
		"space":       {[]string{" 3"}, "", 0, true},
		"two headers": {[]string{"1", "2"}, "", 0, true},
		"two params":  {nil, "cursor=1&cursor=1", 0, true},
		"negative":    {nil, "cursor=-3", 0, true},
		"huge":        {nil, "cursor=99999999999999999999", 0, true},
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/x?"+tc.query, nil)
			for _, h := range tc.header {
				r.Header.Add("Last-Event-ID", h)
			}
			got, err := ParseCursor(r)
			if tc.err {
				if !errors.Is(err, ErrCursorMalformed) {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

// Both transports can carry many readers of one turn at once; each gets the
// whole stream, exactly once.
func TestStreamManyReaders(t *testing.T) {
	rig := newStreamRig(t, Config{}, nil)
	turnID := streamTurnID(60)
	kinds := streamKinds(30)
	gate := make(chan struct{})
	_, err := rig.hub.Start(context.Background(), streamScope("ten1", "alice"), turnID, func(ctx context.Context, tn *Turn) error {
		streamPublish(t, tn, turnID, kinds, 0, 10)
		<-gate
		streamPublish(t, tn, turnID, kinds, 10, len(kinds))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	results := make([][]int64, 6)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := rig.get(t, ctx, turnID, "alice", nil, "")
			defer resp.Body.Close()
			results[i] = streamSeqs(t, streamReadSSE(t, resp.Body, 0))
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(gate)
	wg.Wait()
	for i, got := range results {
		if !reflect.DeepEqual(got, streamRange(0, int64(len(kinds)))) {
			t.Errorf("reader %d got %v", i, got)
		}
	}
}

// TestPublishResponseSanitisesAsAWhole: model Markdown reaches a stream only
// through PublishResponse, which sanitises the whole text and cuts the result
// into token events afterwards. A hostile construct placed across a fragment
// boundary is gone from the joined text, every fragment fits the schema, and the
// fragments concatenate to exactly the sanitised text.
//
// SEC-019
func TestPublishResponseSanitisesAsAWhole(t *testing.T) {
	rig := newStreamRig(t, Config{}, nil)
	turnID := streamTurnID(70)
	// The tag is split exactly where a fragment would end if the text were cut
	// first and sanitised after: neither half looks like a tag on its own.
	filler := strings.Repeat("word ", 818) // 4090 characters
	hostile := "Intro **paragraph**.\n\n" + filler + "<scr" + "ipt>alert(1)</scr" + "ipt> more [x](javascript:alert(1)) " +
		strings.Repeat("perch\u00e9 \U0001F600 tail words. ", 400) + strings.Repeat("\u00e9", 5000)
	want, _, err := rig.hub.Config().Validator.SanitizeResponse(hostile)
	if err != nil {
		t.Fatal(err)
	}
	scope := streamScope("ten1", "alice")
	turn, err := rig.hub.Begin(scope, turnID)
	if err != nil {
		t.Fatal(err)
	}
	if err := turn.Publish(streamEvent(t, turnID, 0, "progress")); err != nil {
		t.Fatal(err)
	}
	if err := turn.PublishResponse(hostile); err != nil {
		t.Fatalf("PublishResponse: %v", err)
	}
	if err := turn.Publish(streamEvent(t, turnID, turn.NextSeq(), "done")); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	resp := mustOK(t, rig.get(t, ctx, turnID, "alice", nil, ""))
	defer resp.Body.Close()
	evs := streamReadSSE(t, resp.Body, 0)
	var joined strings.Builder
	var tokens int
	for i, e := range evs {
		if e.ID != itoa(int64(i)) {
			t.Fatalf("event %d has id %s: sequence not contiguous", i, e.ID)
		}
		if e.Event != "token" {
			continue
		}
		var ev conversation.TurnEvent
		if err := json.Unmarshal([]byte(e.Data), &ev); err != nil {
			t.Fatal(err)
		}
		text := ev.Value.(conversation.TokenEvent).Text
		if n := utf8.RuneCountInString(text); n < 1 || n > MaxTokenRunes {
			t.Errorf("token %d has %d runes; the schema allows 1..%d", i, n, MaxTokenRunes)
		}
		if !utf8.ValidString(text) {
			t.Errorf("token %d is not valid UTF-8", i)
		}
		joined.WriteString(text)
		tokens++
	}
	if tokens < 3 {
		t.Fatalf("only %d token events; the test text should need several", tokens)
	}
	got := joined.String()
	if got != want {
		t.Errorf("the fragments do not concatenate to the sanitised text (%d vs %d bytes)", len(got), len(want))
	}
	for _, bad := range []string{"<script", "</script", "javascript:", "<scr"} {
		if strings.Contains(strings.ToLower(got), bad) {
			t.Errorf("%q survived in the streamed text", bad)
		}
	}
	if !strings.Contains(got, "**paragraph**") {
		t.Errorf("harmless formatting was lost: %.60q", got)
	}

	t.Run("a refused response publishes nothing", func(t *testing.T) {
		id := streamTurnID(71)
		tn, _ := rig.hub.Begin(scope, id)
		err := tn.PublishResponse("pay now" + string(rune(0x200b)) + " please")
		if !errors.Is(err, ErrEventRefused) {
			t.Fatalf("a response with an invisible character: %v", err)
		}
		if tn.NextSeq() != 0 {
			t.Errorf("a refused response used sequence numbers")
		}
		if err := tn.PublishResponse("  \n\t "); err != nil || tn.NextSeq() != 0 {
			t.Errorf("a response with nothing to show: %v, next %d", err, tn.NextSeq())
		}
	})
}
