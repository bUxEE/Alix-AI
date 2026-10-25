package fake

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
)

// stream is the fake's providers.ChatStream. It owns no goroutine: Recv does
// the work in the caller's goroutine, so a stream that is cancelled or closed
// has nothing left running.
//
// Lock order: stream.mu, then Provider.mu. Provider code never takes stream.mu.
type stream struct {
	p      *Provider
	ctx    context.Context
	chunks []providers.ChatChunk
	total  time.Duration

	failAfter  int // deliver this many chunks, then fail with failErr; -1 never
	failErr    error
	stallAfter int // deliver this many chunks, then block; -1 never

	done chan struct{} // closed by Close, unblocks a stalled Recv

	mu     sync.Mutex
	i      int
	closed bool
	term   error // terminal error (io.EOF included); set once the stream ended
	closeO sync.Once
}

// endLocked records the terminal error and releases the provider's open-stream
// count exactly once. The caller holds s.mu.
func (s *stream) endLocked(err error) error {
	if s.term == nil {
		s.term = err
		s.p.mu.Lock()
		s.p.openStreams--
		s.p.mu.Unlock()
	}
	return err
}

// Recv implements providers.ChatStream.
func (s *stream) Recv() (providers.ChatChunk, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return providers.ChatChunk{}, providers.ErrStreamClosed
	}
	if s.term != nil {
		err := s.term
		s.mu.Unlock()
		return providers.ChatChunk{}, err
	}
	if err := s.ctx.Err(); err != nil {
		err = s.endLocked(providers.ContextError(err))
		s.mu.Unlock()
		return providers.ChatChunk{}, err
	}
	if s.stallAfter >= 0 && s.i >= s.stallAfter {
		s.mu.Unlock()
		select {
		case <-s.ctx.Done():
		case <-s.done:
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return providers.ChatChunk{}, providers.ErrStreamClosed
		}
		return providers.ChatChunk{}, s.endLocked(providers.ContextError(s.ctx.Err()))
	}
	if s.failAfter >= 0 && s.i >= s.failAfter {
		err := s.endLocked(s.failErr)
		s.mu.Unlock()
		return providers.ChatChunk{}, err
	}
	if s.i >= len(s.chunks) {
		err := s.endLocked(io.EOF)
		s.mu.Unlock()
		return providers.ChatChunk{}, err
	}
	c := s.chunks[s.i]
	s.i++
	last := c.Done
	s.mu.Unlock()

	s.p.mu.Lock()
	s.p.chunksSent++
	s.p.mu.Unlock()
	if last {
		s.p.advance(s.total)
	}
	return c, nil
}

// Close implements providers.ChatStream. It is idempotent and unblocks a
// concurrent Recv.
func (s *stream) Close() error {
	s.mu.Lock()
	s.closed = true
	s.endLocked(providers.ErrStreamClosed)
	s.mu.Unlock()
	s.closeO.Do(func() { close(s.done) })
	return nil
}
