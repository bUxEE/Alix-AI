package extmode

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
)

type slowRichStream struct {
	chunks []providers.RichChunk
	i      int
	delay  time.Duration
}

func (s *slowRichStream) Recv() (providers.RichChunk, error) {
	if s.i >= len(s.chunks) {
		return providers.RichChunk{}, io.EOF
	}
	if s.delay > 0 && s.i > 0 {
		time.Sleep(s.delay)
	}
	c := s.chunks[s.i]
	s.i++
	return c, nil
}
func (s *slowRichStream) Close() error { return nil }

func TestLeasedChatStreamFirstChunkBeforeLaterDelay(t *testing.T) {
	inner := &slowRichStream{
		delay: 50 * time.Millisecond,
		chunks: []providers.RichChunk{
			{Kind: providers.DeltaText, Text: "First. "},
			{Kind: providers.DeltaText, Text: "Second. "},
			{Kind: providers.DeltaDone, FinishReason: providers.FinishStop, Model: "m1",
				Usage: &providers.CostUsage{ToksIn: 1, ToksOut: 2}},
		},
	}
	cs := &leasedChatStream{ctx: context.Background(), inner: inner, st: &staged{}}
	start := time.Now()
	c1, err := cs.Recv()
	if err != nil || c1.Delta != "First. " {
		t.Fatalf("first: %v %#v", err, c1)
	}
	if time.Since(start) > 25*time.Millisecond {
		t.Fatalf("first chunk arrived too late: %v", time.Since(start))
	}
	c2, err := cs.Recv()
	if err != nil || c2.Delta != "Second. " {
		t.Fatalf("second: %v %#v", err, c2)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatalf("second chunk should wait on delay; elapsed %v", time.Since(start))
	}
	done, err := cs.Recv()
	if err != nil || !done.Done {
		t.Fatalf("done: %v %#v", err, done)
	}
	if _, err := cs.Recv(); err != io.EOF {
		t.Fatalf("eof: %v", err)
	}
	_ = cs.Close()
}
