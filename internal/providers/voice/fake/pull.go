package fake

import (
	"context"
	"io"
	"sync"

	"github.com/buxee/alix-ai/internal/providers"
)

// pull is the pull-style stream both fake streams are built on. It owns no
// goroutine: recv does its waiting in the caller's goroutine, so a stream that
// is cancelled or closed has nothing left running.
//
// Items are queued by the producer (push) and handed out in order. The stream
// ends with io.EOF once finish was called and the queue is drained, with an
// error when the context ends, the stream is closed, or the scripted failure
// point is reached, and it can simulate a hung runtime (stallAfter).
//
// Lock order: pull.mu, then Provider.mu. Provider code never takes pull.mu.
type pull[T any] struct {
	p   *Provider
	ctx context.Context

	failAfter  int // deliver this many items, then fail with failErr; -1 never
	failErr    error
	stallAfter int // deliver this many items, then block; -1 never

	wake chan struct{} // capacity 1: a producer made progress
	done chan struct{} // closed by Close, unblocks a waiting recv

	mu        sync.Mutex
	queue     []T
	finished  bool // no more items will be queued
	delivered int
	closed    bool
	term      error // terminal error (io.EOF included); set once the stream ended
	closeO    sync.Once
}

func newPull[T any](ctx context.Context, p *Provider, failAfter int, failErr error, stallAfter int) *pull[T] {
	p.mu.Lock()
	p.openStreams++
	p.mu.Unlock()
	return &pull[T]{
		p: p, ctx: ctx, failAfter: failAfter, failErr: failErr, stallAfter: stallAfter,
		wake: make(chan struct{}, 1), done: make(chan struct{}),
	}
}

// endLocked records the terminal error and releases the provider's open-stream
// count exactly once. The caller holds s.mu.
func (s *pull[T]) endLocked(err error) error {
	if s.term == nil {
		s.term = err
		s.p.mu.Lock()
		s.p.openStreams--
		s.p.mu.Unlock()
	}
	return s.term
}

// push queues items and wakes a waiting recv. finish marks the end of input.
func (s *pull[T]) push(finish bool, items ...T) {
	s.mu.Lock()
	s.queue = append(s.queue, items...)
	if finish {
		s.finished = true
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// liveErrLocked reports why the stream can no longer accept or deliver anything, or nil
// while it is live. The caller holds s.mu.
func (s *pull[T]) liveErrLocked() error {
	if s.closed {
		return providers.ErrStreamClosed
	}
	if s.term != nil {
		return s.term
	}
	if err := s.ctx.Err(); err != nil {
		return s.endLocked(providers.ContextError(err))
	}
	return nil
}

// recv returns the next item, blocking until one is available.
func (s *pull[T]) recv() (T, error) {
	var zero T
	for {
		s.mu.Lock()
		if err := s.liveErrLocked(); err != nil {
			s.mu.Unlock()
			return zero, err
		}
		if s.stallAfter >= 0 && s.delivered >= s.stallAfter {
			s.mu.Unlock()
			select { // a hung runtime: only the context or Close ends it
			case <-s.ctx.Done():
			case <-s.done:
			}
			continue
		}
		if s.failAfter >= 0 && s.delivered >= s.failAfter {
			err := s.endLocked(s.failErr)
			s.mu.Unlock()
			return zero, err
		}
		if len(s.queue) > 0 {
			item := s.queue[0]
			s.queue = s.queue[1:]
			s.delivered++
			s.mu.Unlock()
			s.p.mu.Lock()
			s.p.chunksSent++
			s.p.mu.Unlock()
			return item, nil
		}
		if s.finished {
			err := s.endLocked(io.EOF)
			s.mu.Unlock()
			return zero, err
		}
		s.mu.Unlock()
		select {
		case <-s.wake:
		case <-s.ctx.Done():
		case <-s.done:
		}
	}
}

// Close implements the Close of both stream interfaces. It is idempotent and
// unblocks a concurrent recv.
func (s *pull[T]) Close() error {
	s.mu.Lock()
	s.closed = true
	s.endLocked(providers.ErrStreamClosed)
	s.mu.Unlock()
	s.closeO.Do(func() { close(s.done) })
	return nil
}
