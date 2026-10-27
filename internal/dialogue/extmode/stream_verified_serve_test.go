package extmode

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/stream"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// delayedRich wraps a RichStream and sleeps before every chunk after the first,
// so a serve-level consumer can observe the first verified segment before the
// provider finishes (OpenAI-compatible stream pacing).
type delayedRich struct {
	inner providers.RichStream
	delay time.Duration
	n     int
}

func (d *delayedRich) Recv() (providers.RichChunk, error) {
	if d.n > 0 && d.delay > 0 {
		time.Sleep(d.delay)
	}
	d.n++
	return d.inner.Recv()
}
func (d *delayedRich) Close() error { return d.inner.Close() }

type delaySender struct {
	inner *recordingSender
	delay time.Duration
}

func (s delaySender) Send(ctx context.Context, scope contracts.Scope, req SendRequest) (providers.RichResult, error) {
	return s.inner.Send(ctx, scope, req)
}
func (s delaySender) SendStream(ctx context.Context, scope contracts.Scope, req SendRequest) (providers.RichStream, error) {
	rs, err := s.inner.SendStream(ctx, scope, req)
	if err != nil {
		return nil, err
	}
	return &delayedRich{inner: rs, delay: s.delay}, nil
}

// openAICompatSSE is a minimal OpenAI chat.completions stream used to assert
// sentence-sized deltas arrive with pacing (fake upstream for serve tests).
func openAICompatSSE(sentences []string, gap time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = io.WriteString(w, s)
			if fl != nil {
				fl.Flush()
			}
		}
		write("data: {\"id\":\"chatcmpl_test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		for i, s := range sentences {
			if i > 0 && gap > 0 {
				time.Sleep(gap)
			}
			esc := strings.ReplaceAll(s, `\`, `\\`)
			esc = strings.ReplaceAll(esc, `"`, `\"`)
			write(`data: {"id":"chatcmpl_test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"` + esc + `"}}]}` + "\n\n")
		}
		write("data: {\"id\":\"chatcmpl_test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":8,\"total_tokens\":11}}\n\n")
		write("data: [DONE]\n\n")
	}
}

func hubPublish(hub *stream.Hub) dialogue.VerifiedSegmentHook {
	return func(_ context.Context, scope contracts.Scope, step dialogue.VerifiedSegment) {
		if hub == nil || step.TurnID == "" {
			return
		}
		turn, err := hub.Lookup(scope, step.TurnID)
		if err != nil {
			turn, err = hub.Begin(scope, step.TurnID)
		}
		if err != nil {
			return
		}
		if step.Replace {
			_ = turn.PublishTextReplaced(step.Text)
			return
		}
		_ = turn.PublishVerifiedSegment(step.Text)
	}
}

func TestVerifiedStreamServeFirstSegmentBudgetAndReplace(t *testing.T) {
	t.Parallel()

	t.Run("first verified segment before stream end and budget settled", func(t *testing.T) {
		var mu sync.Mutex
		var firstAt time.Time
		var segs []dialogue.VerifiedSegment
		hub := stream.NewHub(stream.Config{})
		on := func(ctx context.Context, scope contracts.Scope, step dialogue.VerifiedSegment) {
			hubPublish(hub)(ctx, scope, step)
			mu.Lock()
			defer mu.Unlock()
			if firstAt.IsZero() && step.Text != "" && !step.Replace {
				firstAt = time.Now()
			}
			segs = append(segs, step)
		}
		w := newWorld(t, withModelResponses(), withDirectiveClass("internal"),
			withStreamVerified(on), withStreamDelay(40*time.Millisecond))
		w.toolCall("availability_list", `{"date":"2026-10-14"}`)
		w.respond("Here are the free slots.")
		start := time.Now()
		res := w.say(receptionist(), sayFree)
		endAt := time.Now()
		wantAct(t, res, "present_result")
		mu.Lock()
		defer mu.Unlock()
		if !res.Response.Streamed {
			t.Fatalf("expected Streamed, got %+v segs=%+v", res.Response, segs)
		}
		if firstAt.IsZero() || !firstAt.Before(endAt) {
			t.Fatalf("first=%v end=%v start=%v segs=%+v", firstAt, endAt, start, segs)
		}
		if len(w.budgets.reserved) == 0 || w.budgets.settled < 1 {
			t.Fatalf("reserved=%d settled=%d", len(w.budgets.reserved), w.budgets.settled)
		}
		if turn, err := hub.Lookup(receptionist(), res.TurnID); err != nil || turn.NextSeq() == 0 {
			t.Fatalf("hub mid-turn publish missing: err=%v", err)
		}
	})

	t.Run("bad third segment emits replace then template", func(t *testing.T) {
		var mu sync.Mutex
		var segs []dialogue.VerifiedSegment
		hub := stream.NewHub(stream.Config{})
		on := func(ctx context.Context, scope contracts.Scope, step dialogue.VerifiedSegment) {
			hubPublish(hub)(ctx, scope, step)
			mu.Lock()
			segs = append(segs, step)
			mu.Unlock()
		}
		w := newWorld(t, withModelResponses(), withDirectiveClass("internal"),
			withStreamVerified(on), withStreamDelay(5*time.Millisecond))
		w.toolCall("availability_list", `{"date":"2026-10-14"}`)
		// Two sentences that can pass, then an unverified number.
		w.respond("Here are the free slots. Dr. Anna Verdi has openings. There are 99 left.")
		res := w.say(receptionist(), sayFree)
		wantAct(t, res, "present_result")
		if res.Response.Source != verify.SourceTemplate {
			t.Fatalf("want template after bad segment, got %+v", res.Response)
		}
		mu.Lock()
		defer mu.Unlock()
		var sawReplace, sawGood bool
		for _, s := range segs {
			if strings.Contains(s.Text, "99") && !s.Replace {
				t.Fatalf("unverified number reached the hub: %+v", segs)
			}
			if s.Replace {
				sawReplace = true
			}
			if strings.Contains(s.Text, "free slots") || strings.Contains(s.Text, "Anna Verdi") {
				sawGood = true
			}
		}
		if !sawReplace {
			t.Fatalf("expected text_replaced, got %+v", segs)
		}
		if !sawGood {
			t.Fatalf("expected earlier verified segments before replace, got %+v", segs)
		}
		if turn, err := hub.Lookup(receptionist(), res.TurnID); err != nil || turn.NextSeq() == 0 {
			t.Fatalf("hub mid-turn publish missing: err=%v", err)
		}
	})

	t.Run("egress denial sends nothing", func(t *testing.T) {
		var mu sync.Mutex
		var segs []dialogue.VerifiedSegment
		hub := stream.NewHub(stream.Config{})
		on := func(ctx context.Context, scope contracts.Scope, step dialogue.VerifiedSegment) {
			hubPublish(hub)(ctx, scope, step)
			mu.Lock()
			segs = append(segs, step)
			mu.Unlock()
		}
		w := newWorld(t, withModelResponses(), withoutEgressPolicy(), withStreamVerified(on), withStreamDelay(5*time.Millisecond))
		w.toolCall("appointments_book", `{"patient":"Luca Ferri","slot":"2026-10-14 10:30"}`)
		w.respond("Sure. How many would you like?")
		res := w.say(receptionist(), sayBook)
		if res.Code != contracts.ErrPermissionDenied {
			t.Fatalf("code = %q (act %s), want permission_denied", res.Code, res.Directive.Act)
		}
		if w.ext.Calls() != 0 || len(w.budgets.reserved) != 0 {
			t.Fatal("something was sent or reserved after a denial")
		}
		mu.Lock()
		defer mu.Unlock()
		for _, s := range segs {
			if s.Text != "" && !s.Replace {
				t.Fatalf("segment published despite egress denial: %+v", segs)
			}
		}
	})
}

func TestOpenAICompatSSEPacing(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(openAICompatSSE([]string{"First. ", "Second. ", "Third."}, 30*time.Millisecond))
	t.Cleanup(srv.Close)
	start := time.Now()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Fatalf("SSE finished too fast: %v", time.Since(start))
	}
	if !strings.Contains(string(body), "First. ") || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("body=%s", body)
	}
}
