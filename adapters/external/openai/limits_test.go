package openai

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
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// The size caps and the end of a stream, through the client's surface only: a
// reply or a stream over its cap is a malformed response, never a truncated
// success; the connection is released; and Close and Recv behave the same once
// a stream has ended, however it ended.

// sseDelta is one response.output_text.delta event.
func sseDelta(text string) string {
	b, _ := json.Marshal(text)
	return "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":" + string(b) + "}\n\n"
}

// sseOpening and sseCompleted are the events of sseText before its first delta
// and from response.completed on.
var (
	sseOpening   = sseText[:strings.Index(sseText, "event: response.output_text.delta")]
	sseCompleted = sseText[strings.Index(sseText, "event: response.completed"):]
)

// heldOpen builds a rig whose provider answers with what write sends and then
// keeps the connection open, as a provider still streaming would. The channel
// is closed once the client has released the connection.
func heldOpen(t *testing.T, contentType string, write func(w io.Writer), mutate func(*Config)) (*rig, <-chan struct{}) {
	t.Helper()
	block, released := make(chan struct{}), make(chan struct{})
	r := newRig(t, func(_ int, w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", contentType)
		write(w)
		w.(http.Flusher).Flush()
		select {
		case <-req.Context().Done():
			close(released)
		case <-block:
		}
	}, mutate)
	t.Cleanup(func() {
		// A client that keeps the connection fails the test instead of hanging it.
		r.api.srv.CloseClientConnections()
		close(block)
	})
	return r, released
}

func waitReleased(t *testing.T, released <-chan struct{}) {
	t.Helper()
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("the connection to the provider stayed open")
	}
}

// wantMalformed checks a refusal over a size cap: schema_invalid, a malformed
// response after bytes were sent, and a fixed message.
func wantMalformed(t *testing.T, err error, msg string) {
	t.Helper()
	ce := wantCode(t, err, contracts.ErrSchemaInvalid)
	if ce.Message != ProviderName+": "+msg || ce.Details["provider_error_kind"] != string(pv.ProviderErrorKindMalformedResponse) || ce.Details["sent"] != true {
		t.Fatalf("error %v, details %v", ce, ce.Details)
	}
}

// drain reads text chunks until Recv fails and returns how much text came first.
func drain(t *testing.T, st providers.RichStream) (int, error) {
	t.Helper()
	n := 0
	for {
		c, err := st.Recv()
		if err != nil {
			return n, err
		}
		if c.Kind != providers.DeltaText {
			t.Fatalf("a %v chunk before the failure", c.Kind)
		}
		n += len(c.Text)
	}
}

// afterFailure checks a stream that failed: the failure is sticky, Close
// releases the connection, and Recv then reports the stream closed.
func afterFailure(t *testing.T, st providers.RichStream, failed error, released <-chan struct{}) {
	t.Helper()
	for range 2 {
		if c, err := st.Recv(); err != failed || c != (providers.RichChunk{}) {
			t.Fatalf("Recv after the failure: %+v %v", c, err)
		}
	}
	_ = st.Close()
	if released != nil {
		waitReleased(t, released)
	}
	for range 2 {
		if c, err := st.Recv(); !errors.Is(err, providers.ErrStreamClosed) || c != (providers.RichChunk{}) {
			t.Fatalf("Recv after Close: %+v %v", c, err)
		}
	}
	_ = st.Close()
}

func TestReplyOverTheSizeCap(t *testing.T) {
	body := textResponse
	r := newRig(t, replyJSON(body), func(c *Config) { c.MaxResponseBytes = int64(len(body)) })
	if res, err := r.client.Generate(context.Background(), testScope(), userReq("hi")); err != nil || res.Text() != "Hello there." {
		t.Fatalf("a reply at the cap: %+v %v", res, err)
	}
	// One byte over is malformed, and the connection is released although the
	// provider keeps it open.
	r, released := heldOpen(t, "application/json", func(w io.Writer) { _, _ = io.WriteString(w, body) },
		func(c *Config) { c.MaxResponseBytes = int64(len(body)) - 1 })
	_, err := r.client.Generate(context.Background(), testScope(), userReq("hi"))
	wantMalformed(t, err, "the response exceeds the size limit")
	waitReleased(t, released)
}

func TestStreamOverTheSizeCap(t *testing.T) {
	// 64 KiB deltas, far under the event cap, until the stream passes 32 MiB.
	delta := sseDelta(strings.Repeat("a", 64<<10))
	r, released := heldOpen(t, "text/event-stream", func(w io.Writer) {
		_, _ = io.WriteString(w, sseOpening)
		for n := 0; n <= 33<<20; n += len(delta) {
			if _, err := io.WriteString(w, delta); err != nil {
				return
			}
		}
		_, _ = io.WriteString(w, sseCompleted)
	}, nil)
	st, err := r.client.GenerateStream(context.Background(), testScope(), userReq("hi"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := drain(t, st)
	wantMalformed(t, err, "the stream exceeds the size limit")
	if got < 31<<20 || got >= 32<<20 {
		t.Fatalf("%d bytes of text came before the 32 MiB cap", got)
	}
	afterFailure(t, st, err, released)
}

func TestStreamEventOverTheSizeCap(t *testing.T) {
	under := strings.Repeat("u", 1<<20-4<<10) // an event just under the 1 MiB cap is delivered whole
	over := strings.Repeat("o", 1<<20)
	r, released := heldOpen(t, "text/event-stream", func(w io.Writer) {
		_, _ = io.WriteString(w, sseOpening+sseDelta(under)+sseDelta(over)+sseCompleted)
	}, nil)
	st, err := r.client.GenerateStream(context.Background(), testScope(), userReq("hi"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := drain(t, st)
	wantMalformed(t, err, "a stream event exceeds the size limit")
	if got != len(under) {
		t.Fatalf("%d bytes of text before the oversized event, want %d", got, len(under))
	}
	afterFailure(t, st, err, released)
}

func TestStreamAfterItsEnd(t *testing.T) {
	ctx := context.Background()
	t.Run("a completed stream reports its end until Close", func(t *testing.T) {
		r := newRig(t, sseHandler(sseText), nil)
		st, err := r.client.GenerateStream(ctx, testScope(), userReq("hi"))
		if err != nil {
			t.Fatal(err)
		}
		var kinds []providers.DeltaKind
		for {
			c, err := st.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			kinds = append(kinds, c.Kind)
		}
		if fmt.Sprint(kinds) != "[text text done]" {
			t.Fatalf("kinds %v", kinds)
		}
		for range 2 {
			if c, err := st.Recv(); err != io.EOF || c != (providers.RichChunk{}) {
				t.Fatalf("Recv after the end: %+v %v", c, err)
			}
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close after the end: %v", err)
		}
		for range 2 {
			if c, err := st.Recv(); !errors.Is(err, providers.ErrStreamClosed) || c != (providers.RichChunk{}) {
				t.Fatalf("Recv after Close: %+v %v", c, err)
			}
		}
		if err := st.Close(); err != nil {
			t.Fatalf("a second Close: %v", err)
		}
	})
	t.Run("a stream cut off before its end fails for good", func(t *testing.T) {
		r := newRig(t, sseHandler(sseText[:strings.Index(sseText, "event: response.completed")]), nil)
		st, err := r.client.GenerateStream(ctx, testScope(), userReq("hi"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := drain(t, st)
		ce := wantCode(t, err, contracts.ErrUnavailable)
		if ce.Message != ProviderName+": the stream ended before it completed" || got != len("Hello.") ||
			ce.Details["provider_error_kind"] != string(pv.ProviderErrorKindConnectionLost) || ce.Details["sent"] != true {
			t.Fatalf("error %v, details %v, %d bytes of text", ce, ce.Details, got)
		}
		afterFailure(t, st, err, nil)
	})
	t.Run("Close ends a blocked Recv and releases the connection", func(t *testing.T) {
		r, released := heldOpen(t, "text/event-stream", func(w io.Writer) { _, _ = io.WriteString(w, sseOpening) }, nil)
		st, err := r.client.GenerateStream(ctx, testScope(), userReq("hi"))
		if err != nil {
			t.Fatal(err)
		}
		out := make(chan error, 1)
		go func() { _, err := st.Recv(); out <- err }()
		time.Sleep(50 * time.Millisecond)
		_ = st.Close()
		select {
		case err := <-out:
			if !errors.Is(err, providers.ErrStreamClosed) {
				t.Fatalf("the blocked Recv returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Close did not end the blocked Recv")
		}
		waitReleased(t, released)
		if c, err := st.Recv(); !errors.Is(err, providers.ErrStreamClosed) || c != (providers.RichChunk{}) {
			t.Fatalf("Recv after Close: %+v %v", c, err)
		}
	})
}
