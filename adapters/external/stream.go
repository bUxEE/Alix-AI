package external

// The stream scaffolding every hosted-provider adapter shares: the size caps of
// a streamed answer, the server-sent-events reader over the connection, Close,
// and the read step of Recv with its error mapping. Each adapter keeps its own
// event handling, its order of checks in Recv and its rule for when a stream
// whose connection ends is complete.

import (
	"context"
	"errors"
	"io"
	"sync/atomic"

	"github.com/buxee/alix-ai/internal/providers"
)

const (
	extMaxStreamEvent = 1 << 20  // one server-sent event
	extMaxStreamBytes = 32 << 20 // a whole stream
)

// extCapReader fails with err once more than left bytes have been read.
type extCapReader struct {
	r    io.Reader
	left int64
	err  error
}

func (c *extCapReader) Read(b []byte) (int, error) {
	if c.left <= 0 {
		return 0, c.err
	}
	if int64(len(b)) > c.left {
		b = b[:c.left]
	}
	n, err := c.r.Read(b)
	c.left -= int64(n)
	return n, err
}

// ExtStream is the part of a pull-style providers.RichStream over one SSE
// response that does not depend on the provider: the connection, the capped
// event reader over it, the closed flag and the sticky error. An adapter embeds
// it and builds its Recv around Next.
type ExtStream struct {
	provider string
	ctx      context.Context
	cancel   context.CancelFunc
	body     io.ReadCloser
	sse      *SSEReader
	tooLarge error

	closed atomic.Bool
	err    error // sticky
}

// NewExtStream reads the events of body, at most 1 MiB each and 32 MiB in all.
// ctx is the context of the stream and cancel ends it.
func NewExtStream(ctx context.Context, cancel context.CancelFunc, provider string, body io.ReadCloser) *ExtStream {
	s := &ExtStream{
		provider: provider, ctx: ctx, cancel: cancel, body: body,
		tooLarge: ExtMalformed(provider, "the stream exceeds the size limit"),
	}
	s.sse = NewSSEReader(&extCapReader{r: body, left: extMaxStreamBytes, err: s.tooLarge}, extMaxStreamEvent)
	return s
}

// Close releases the connection. It is idempotent and safe to call while Recv
// is blocked: cancelling the context closes the connection under the reader.
func (s *ExtStream) Close() error {
	s.closed.Store(true)
	s.cancel()
	return s.body.Close()
}

// Closed reports whether Close was called.
func (s *ExtStream) Closed() bool { return s.closed.Load() }

// Err is the sticky error: nil until the stream failed.
func (s *ExtStream) Err() error { return s.err }

// Fail records err as the sticky error and returns it.
func (s *ExtStream) Fail(err error) error {
	s.err = err
	return err
}

// Next reads the next event. A failure is recorded as the sticky error and
// returned: an ended context or a broken connection mapped as a transport
// error, an event or a stream over its cap as a malformed response. A read cut
// short by Close returns providers.ErrStreamClosed, and the clean end of the
// connection io.EOF, neither of them recorded: whether the stream was complete
// when its connection ended is the adapter's call.
func (s *ExtStream) Next() (SSEEvent, error) {
	if err := s.ctx.Err(); err != nil {
		return SSEEvent{}, s.Fail(ExtMapTransportError(s.ctx, s.provider, err))
	}
	ev, err := s.sse.Next()
	if err == nil {
		return ev, nil
	}
	if s.closed.Load() {
		return SSEEvent{}, providers.ErrStreamClosed
	}
	switch {
	case errors.Is(err, io.EOF):
		return SSEEvent{}, io.EOF
	case errors.Is(err, ErrEventTooLarge):
		err = ExtMalformed(s.provider, "a stream event exceeds the size limit")
	case errors.Is(err, s.tooLarge):
	default:
		err = ExtMapTransportError(s.ctx, s.provider, err)
	}
	return SSEEvent{}, s.Fail(err)
}
