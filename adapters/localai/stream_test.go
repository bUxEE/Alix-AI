package localai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// streamChunk is one LocalAI v4.10.0 chat.completion.chunk event.
func streamChunk(delta, finish string) string {
	d := "{}"
	if delta != "" {
		b, _ := json.Marshal(delta)
		d = `{"content":` + string(b) + `}`
	}
	f := "null"
	if finish != "" {
		f = fmt.Sprintf("%q", finish)
	}
	return fmt.Sprintf(`{"created":1790000000,"object":"chat.completion.chunk","id":"chatcmpl-s1","model":"ctl-model",`+
		`"choices":[{"index":0,"finish_reason":%s,"delta":%s}]}`, f, d)
}

const (
	roleChunk    = `{"object":"chat.completion.chunk","id":"chatcmpl-s1","model":"ctl-model","choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","content":""}}]}`
	usageTrailer = `{"object":"chat.completion.chunk","id":"chatcmpl-s1","model":"ctl-model","choices":[],"usage":{"prompt_tokens":31,"completion_tokens":7,"total_tokens":38}}`
)

// sseHandler answers with the given events, each as "data: <event>", then
// ends the response.
func sseHandler(events ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, e := range events {
			fmt.Fprintf(w, "data: %s\n\n", e)
			w.(http.Flusher).Flush()
		}
	}
}

func rawSSEHandler(raw string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, raw)
	}
}

// drain reads a stream to its end and returns the text, the Done chunk and
// the terminating error (io.EOF for a complete stream).
func drain(t *testing.T, s providers.ChatStream) (text string, done *providers.ChatChunk, err error) {
	t.Helper()
	defer s.Close()
	var sb strings.Builder
	for i := 0; i < 1000; i++ {
		var c providers.ChatChunk
		c, err = s.Recv()
		if err != nil {
			return sb.String(), done, err
		}
		sb.WriteString(c.Delta)
		if c.Done {
			cc := c
			done = &cc
		}
	}
	t.Fatal("stream did not end")
	return
}

var happyStream = []string{
	roleChunk,
	streamChunk(`{"intent"`, ""),
	streamChunk(`:"book"}`, ""),
	streamChunk("", "stop"),
	usageTrailer,
	"[DONE]",
}

func TestChatStreamParsesSSE(t *testing.T) {
	t.Run("complete stream", func(t *testing.T) {
		rt := newRuntime(t, sseHandler(happyStream...))
		c, _ := rt.client(t)
		req := chatReq("book a room")
		req.Output = providers.SchemaConstraint(json.RawMessage(`{"type":"object"}`))
		s, err := c.ChatStream(bg, testScope(), req)
		if err != nil {
			t.Fatal(err)
		}
		got := rt.only(t)
		body := decodeObject(t, got.Body)
		if body["stream"] != true {
			t.Errorf("stream = %v, want true", body["stream"])
		}
		if diff := fmt.Sprint(body["stream_options"]); diff != "map[include_usage:true]" {
			t.Errorf("stream_options = %v, want include_usage so the usage trailer is sent", body["stream_options"])
		}
		if got.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("Accept = %q", got.Header.Get("Accept"))
		}

		var deltas []string
		var done providers.ChatChunk
		var sawDone bool
		for {
			ch, err := s.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("Recv: %v", err)
			}
			if sawDone {
				t.Fatal("chunk after the Done chunk")
			}
			if ch.Done {
				done, sawDone = ch, true
				continue
			}
			deltas = append(deltas, ch.Delta)
		}
		if !sawDone {
			t.Fatal("no Done chunk before io.EOF")
		}
		if diff := strings.Join(deltas, "|"); diff != `{"intent"|:"book"}` {
			t.Errorf("deltas = %q (role-only and empty deltas must not surface)", diff)
		}
		if done.FinishReason != providers.FinishStop || done.Usage != (providers.Usage{InputTokens: 31, OutputTokens: 7}) {
			t.Errorf("done = %+v", done)
		}
		if done.TraceID != "chatcmpl-s1" || done.Model != "ctl-model" {
			t.Errorf("done trace/model = %q / %q", done.TraceID, done.Model)
		}
		if done.Timings.Total < 0 || done.Timings.TimeToFirstToken < 0 || done.Timings.TimeToFirstToken > done.Timings.Total {
			t.Errorf("timings = %+v", done.Timings)
		}
		if _, err := s.Recv(); err != io.EOF {
			t.Errorf("Recv after EOF = %v, want io.EOF", err)
		}
		if err := s.Close(); err != nil || s.Close() != nil {
			t.Error("Close must be idempotent")
		}
		if _, err := s.Recv(); !errors.Is(err, providers.ErrStreamClosed) {
			t.Errorf("Recv after Close = %v, want ErrStreamClosed", err)
		}
	})

	t.Run("collected result is parsed as JSON", func(t *testing.T) {
		rt := newRuntime(t, sseHandler(happyStream...))
		c, _ := rt.client(t)
		req := chatReq("book a room")
		req.Output = providers.SchemaConstraint(json.RawMessage(`{"type":"object"}`))
		s, err := c.ChatStream(bg, testScope(), req)
		if err != nil {
			t.Fatal(err)
		}
		res, err := providers.Collect(s, req.Output)
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if string(res.JSON) != `{"intent":"book"}` || res.Usage.OutputTokens != 7 {
			t.Errorf("res = %+v", res)
		}
	})

	t.Run("CRLF, comments, other fields and no space after the colon", func(t *testing.T) {
		raw := ": keep-alive\r\n\r\n" +
			"event: message\r\nid: 1\r\ndata:" + streamChunk("he", "") + "\r\n\r\n" +
			"data: " + streamChunk("llo", "") + "\r\n\r\n" +
			"data: " + streamChunk("", "stop") + "\r\n\r\n" +
			"data: [DONE]\r\n\r\n"
		rt := newRuntime(t, rawSSEHandler(raw))
		c, _ := rt.client(t)
		s, err := c.ChatStream(bg, testScope(), chatReq("hi"))
		if err != nil {
			t.Fatal(err)
		}
		text, done, err := drain(t, s)
		if err != io.EOF || done == nil || text != "hello" {
			t.Fatalf("text = %q, done = %v, err = %v", text, done, err)
		}
		if done.Usage != (providers.Usage{}) {
			t.Errorf("usage without a trailer = %+v, want zero", done.Usage)
		}
	})

	t.Run("finish reason and text in one chunk", func(t *testing.T) {
		ev := `{"id":"x","choices":[{"index":0,"finish_reason":"length","delta":{"content":"cut"}}]}`
		rt := newRuntime(t, sseHandler(ev, "[DONE]"))
		c, _ := rt.client(t)
		s, _ := c.ChatStream(bg, testScope(), chatReq("hi"))
		text, done, err := drain(t, s)
		if err != io.EOF || done == nil || text != "cut" || done.FinishReason != providers.FinishLength {
			t.Fatalf("text = %q, done = %+v, err = %v", text, done, err)
		}
	})

	failures := []struct {
		name string
		raw  string
		code contracts.ErrCode
	}{
		{"error event then [DONE]",
			"data: " + streamChunk("par", "") + "\n\n" +
				`data: {"error":{"message":"could not load model: out of memory","type":"server_error","code":"server_error"}}` + "\n\ndata: [DONE]\n\n",
			contracts.ErrResourceExhausted},
		{"error event with no known phrase",
			`data: {"error":{"message":"boom","type":"server_error"}}` + "\n\ndata: [DONE]\n\n",
			contracts.ErrInternal},
		{"[DONE] with no finish reason",
			"data: " + streamChunk("text", "") + "\n\ndata: [DONE]\n\n",
			contracts.ErrSchemaInvalid},
		{"[DONE] alone", "data: [DONE]\n\n", contracts.ErrSchemaInvalid},
		{"body ends without [DONE]",
			"data: " + streamChunk("text", "") + "\n\ndata: " + streamChunk("", "stop") + "\n\n",
			contracts.ErrUnavailable},
		{"body ends in the middle of an event",
			"data: " + streamChunk("text", "") + "\n\ndata: {\"choices\":[{\"del",
			contracts.ErrUnavailable},
		{"empty body", "", contracts.ErrUnavailable},
		{"text after the finish reason",
			"data: " + streamChunk("", "stop") + "\n\ndata: " + streamChunk("late", "") + "\n\ndata: [DONE]\n\n",
			contracts.ErrSchemaInvalid},
		{"second finish reason",
			"data: " + streamChunk("", "stop") + "\n\ndata: " + streamChunk("", "stop") + "\n\ndata: [DONE]\n\n",
			contracts.ErrSchemaInvalid},
		{"unknown finish reason",
			"data: " + streamChunk("", "tool_calls") + "\n\ndata: [DONE]\n\n",
			contracts.ErrSchemaInvalid},
		{"tool call delta",
			`data: {"choices":[{"index":0,"finish_reason":null,"delta":{"tool_calls":[{"id":"c1","type":"function"}]}}]}` + "\n\ndata: [DONE]\n\n",
			contracts.ErrSchemaInvalid},
		{"event that is not JSON", "data: not json\n\ndata: [DONE]\n\n", contracts.ErrSchemaInvalid},
		{"two choices in one event",
			`data: {"choices":[{"delta":{"content":"a"}},{"delta":{"content":"b"}}]}` + "\n\n",
			contracts.ErrSchemaInvalid},
	}
	for _, tt := range failures {
		t.Run(tt.name, func(t *testing.T) {
			rt := newRuntime(t, rawSSEHandler(tt.raw))
			c, _ := rt.client(t)
			req := chatReq("hi")
			s, err := c.ChatStream(bg, testScope(), req)
			if err != nil {
				t.Fatal(err)
			}
			_, done, err := drain(t, s)
			wantCode(t, err, tt.code)
			if done != nil {
				t.Fatal("a failed stream delivered a Done chunk")
			}

			// Through Collect: never a result, never partial text.
			s2, _ := c.ChatStream(bg, testScope(), req)
			res, err := providers.Collect(s2, providers.OutputConstraint{})
			wantCode(t, err, tt.code)
			if res.Text != "" {
				t.Errorf("Collect returned partial text %q", res.Text)
			}
		})
	}

	t.Run("sticky error", func(t *testing.T) {
		rt := newRuntime(t, rawSSEHandler("data: not json\n\n"))
		c, _ := rt.client(t)
		s, _ := c.ChatStream(bg, testScope(), chatReq("hi"))
		defer s.Close()
		_, err1 := s.Recv()
		_, err2 := s.Recv()
		wantCode(t, err1, contracts.ErrSchemaInvalid)
		if err1 != err2 {
			t.Errorf("a failed stream must keep returning its error, got %v then %v", err1, err2)
		}
	})

	t.Run("response that is not an event stream", func(t *testing.T) {
		rt := newRuntime(t, serveJSON(200, chatBody("x", "stop")))
		c, _ := rt.client(t)
		_, err := c.ChatStream(bg, testScope(), chatReq("hi"))
		wantCode(t, err, contracts.ErrSchemaInvalid)
	})

	t.Run("HTTP failure surfaces from ChatStream", func(t *testing.T) {
		rt := newRuntime(t, serveJSON(503, `{"error":{"message":"model is still loading","code":503}}`))
		c, _ := rt.client(t)
		_, err := c.ChatStream(bg, testScope(), chatReq("hi"))
		wantCode(t, err, contracts.ErrUnavailable)
	})

	t.Run("oversized stream", func(t *testing.T) {
		var events []string
		for i := 0; i < 50; i++ {
			events = append(events, streamChunk(strings.Repeat("x", 100), ""))
		}
		rt := newRuntime(t, sseHandler(append(events, streamChunk("", "stop"), "[DONE]")...))
		c, _ := rt.client(t, func(cfg *Config) { cfg.MaxResponseBytes = 2048 })
		s, _ := c.ChatStream(bg, testScope(), chatReq("hi"))
		_, _, err := drain(t, s)
		wantCode(t, err, contracts.ErrSchemaInvalid)
	})
}

// blockingStream sends one delta and then holds the response open until the
// client goes away or release is closed.
func blockingStream(release <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprintf(w, "data: %s\n\n", streamChunk("first", ""))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}
}

func TestChatStreamHonoursContext(t *testing.T) {
	recvWithin := func(t *testing.T, s providers.ChatStream) error {
		t.Helper()
		errc := make(chan error, 1)
		go func() { _, err := s.Recv(); errc <- err }()
		select {
		case err := <-errc:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("Recv did not return")
			return nil
		}
	}

	t.Run("cancel mid-stream", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		rt := newRuntime(t, blockingStream(release))
		c, _ := rt.client(t)
		ctx, cancel := context.WithCancel(bg)
		defer cancel()
		s, err := c.ChatStream(ctx, testScope(), chatReq("hi"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if ch, err := s.Recv(); err != nil || ch.Delta != "first" {
			t.Fatalf("first Recv = %+v, %v", ch, err)
		}
		time.AfterFunc(50*time.Millisecond, cancel)
		err = recvWithin(t, s)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Recv after cancel = %v, want an error wrapping context.Canceled", err)
		}
		if contracts.HasCode(err, contracts.ErrTimeout) {
			t.Error("a cancellation is not a timeout")
		}
		if _, err := s.Recv(); err == nil || err == io.EOF {
			t.Errorf("no further chunk may follow a cancellation, got %v", err)
		}
	})

	t.Run("deadline is a timeout", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		rt := newRuntime(t, blockingStream(release))
		c, _ := rt.client(t, func(cfg *Config) { cfg.RequestTimeout = 600 * time.Millisecond })
		s, err := c.ChatStream(bg, testScope(), chatReq("hi"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if _, err := s.Recv(); err != nil {
			t.Fatal(err)
		}
		err = recvWithin(t, s)
		wantCode(t, err, contracts.ErrTimeout)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("timeout must still match context.DeadlineExceeded: %v", err)
		}
	})

	t.Run("Close unblocks a pending Recv", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		rt := newRuntime(t, blockingStream(release))
		c, _ := rt.client(t)
		s, err := c.ChatStream(bg, testScope(), chatReq("hi"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Recv(); err != nil {
			t.Fatal(err)
		}
		time.AfterFunc(50*time.Millisecond, func() { _ = s.Close() })
		err = recvWithin(t, s)
		if !errors.Is(err, providers.ErrStreamClosed) {
			t.Fatalf("Recv after Close = %v, want ErrStreamClosed", err)
		}
	})

	t.Run("already-canceled context never reaches the runtime", func(t *testing.T) {
		rt := newRuntime(t, sseHandler(happyStream...))
		c, _ := rt.client(t)
		ctx, cancel := context.WithCancel(bg)
		cancel()
		_, err := c.ChatStream(ctx, testScope(), chatReq("hi"))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if n := len(rt.requests()); n != 0 {
			t.Fatalf("%d requests sent on a canceled context", n)
		}
	})
}
