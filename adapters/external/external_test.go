package external

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func TestSSEReaderParsesEvents(t *testing.T) {
	in := ": comment\r\nevent: a\r\ndata: one\r\ndata: two\r\n\r\n\n\ndata:three\n\nevent: cut\ndata: never dispatched"
	r := NewSSEReader(strings.NewReader(in), 1024)
	want := []SSEEvent{{Event: "a", Data: "one\ntwo"}, {Data: "three"}}
	for i, w := range want {
		got, err := r.Next()
		if err != nil || got != w {
			t.Fatalf("event %d: %+v, %v; want %+v", i, got, err, w)
		}
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("the truncated event was delivered or the end was not clean: %v", err)
	}
}

// A bare CR ends a line as LF and CRLF do, and a CRLF split across reads is still one line end.
func TestSSEReaderAcceptsBareCR(t *testing.T) {
	in := ": comment\revent: a\rdata: one\rdata: two\r\rdata: three\r\n\r\ndata: four\n\r"
	r := NewSSEReader(iotest.OneByteReader(strings.NewReader(in)), 1024)
	for i, w := range []SSEEvent{{Event: "a", Data: "one\ntwo"}, {Data: "three"}, {Data: "four"}} {
		got, err := r.Next()
		if err != nil || got != w {
			t.Fatalf("event %d: %+v, %v; want %+v", i, got, err, w)
		}
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("the end was not clean: %v", err)
	}

	// An event that ends in CR is dispatched at once: the reader does not wait for a byte that may never come.
	pr, pw := io.Pipe()
	defer pw.Close()
	go func() { _, _ = pw.Write([]byte("data: live\r\r")) }()
	got := make(chan SSEEvent, 1)
	go func() {
		ev, _ := NewSSEReader(pr, 1024).Next()
		got <- ev
	}()
	select {
	case ev := <-got:
		if ev.Data != "live" {
			t.Fatalf("got %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an event ending in CR was held until more bytes arrived")
	}
}

func TestSSEReaderBoundsEventSize(t *testing.T) {
	r := NewSSEReader(strings.NewReader("data: "+strings.Repeat("x", 5000)+"\n\n"), 1024)
	if _, err := r.Next(); !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("got %v", err)
	}
	r = NewSSEReader(strings.NewReader(strings.Repeat("data: yyyy\n", 400)+"\n"), 1024)
	if _, err := r.Next(); !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("many small lines: %v", err)
	}
}

func TestRetryPolicyDelay(t *testing.T) {
	p := RetryPolicy{MaxRetries: 3, BaseDelay: time.Second, MaxDelay: 3 * time.Second, MaxRetryAfter: 10 * time.Second}
	for attempt, want := range []time.Duration{time.Second, 2 * time.Second, 3 * time.Second} {
		if got, ok := p.Delay(attempt, 0, nil); !ok || got != want {
			t.Errorf("attempt %d: %v %v, want %v", attempt, got, ok, want)
		}
	}
	if _, ok := p.Delay(3, 0, nil); ok {
		t.Error("retried beyond MaxRetries")
	}
	if got, ok := p.Delay(0, 5*time.Second, nil); !ok || got != 5*time.Second {
		t.Errorf("retry-after not honoured as a minimum: %v %v", got, ok)
	}
	if _, ok := p.Delay(0, 11*time.Second, nil); ok {
		t.Error("waited for a retry-after beyond the cap")
	}
	if got, _ := p.Delay(0, 0, func() float64 { return 0.5 }); got != time.Second+125*time.Millisecond {
		t.Errorf("jitter: %v", got)
	}
	if n := (RetryPolicy{MaxRetries: 99}).Normalised().MaxRetries; n != HardMaxRetries {
		t.Errorf("hard cap: %d", n)
	}
}

func TestSleepHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if err := Sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
}
