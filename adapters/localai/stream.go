package localai

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// maxSSELine bounds one server-sent-events line.
const maxSSELine = 1 << 20

// ChatStream implements providers.TextGenerator. The returned stream is
// complete only when Recv has delivered a chunk with Done set (followed by
// io.EOF); every other ending is an error and the text received so far is
// partial.
func (c *Client) ChatStream(ctx context.Context, scope contracts.Scope, req providers.ChatRequest) (providers.ChatStream, error) {
	wire, err := c.buildChat(scope, req, true)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	started := c.clk.Now()
	resp, err := c.send(ctx, "POST", pathChat, wire, "text/event-stream")
	if err != nil {
		cancel()
		return nil, err
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "text/event-stream" {
		_ = resp.Body.Close()
		cancel()
		return nil, schemaInvalid("runtime did not answer with a server-sent event stream")
	}
	sc := bufio.NewScanner(newCapReader(resp.Body, c.cfg.MaxResponseBytes))
	sc.Buffer(make([]byte, 0, 4096), maxSSELine)
	return &chatStream{
		c:       c,
		ctx:     ctx,
		cancel:  cancel,
		body:    resp.Body,
		sc:      sc,
		started: started,
		model:   wire.Model,
	}, nil
}

// chatStream reads a LocalAI chat stream (see the package documentation for
// the event order). Recv must not be called concurrently with itself; Close
// may be called at any time.
type chatStream struct {
	c       *Client
	ctx     context.Context
	cancel  context.CancelFunc
	body    io.ReadCloser
	sc      *bufio.Scanner
	started time.Time
	model   string

	traceID    string
	firstDelta time.Time
	sawFinish  bool
	finish     providers.FinishReason
	usage      providers.Usage
	emitted    bool // the Done chunk has been returned
	err        error

	closed  atomic.Bool
	release sync.Once
}

func (s *chatStream) free() {
	s.release.Do(func() {
		s.cancel()
		_ = s.body.Close()
	})
}

// fail ends the stream with err. Later Recv calls return the same error.
func (s *chatStream) fail(err error) (providers.ChatChunk, error) {
	if s.closed.Load() {
		return providers.ChatChunk{}, providers.ErrStreamClosed
	}
	s.err = err
	s.free()
	return providers.ChatChunk{}, err
}

// Close releases the connection. It is idempotent and unblocks a pending Recv.
func (s *chatStream) Close() error {
	s.closed.Store(true)
	s.free()
	return nil
}

// event is one decoded stream chunk.
type chunkWire struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []choiceWire `json:"choices"`
	Usage   *usageWire   `json:"usage"`
	Error   *apiError    `json:"error"`
}

// nextPayload returns the data of the next server-sent event: the "data"
// lines of one event joined by newlines. Comments, other fields and empty
// events are skipped, and an event cut off by the end of the body is dropped.
func (s *chatStream) nextPayload() (string, error) {
	var data []string
	for s.sc.Scan() {
		line := strings.TrimSuffix(s.sc.Text(), "\r")
		switch {
		case line == "":
			if len(data) > 0 {
				return strings.Join(data, "\n"), nil
			}
		case line[0] == ':':
		default:
			field, value, _ := strings.Cut(line, ":")
			if field == "data" {
				data = append(data, strings.TrimPrefix(value, " "))
			}
		}
	}
	if err := s.sc.Err(); err != nil {
		return "", err
	}
	return "", io.EOF
}

// Recv implements providers.ChatStream.
func (s *chatStream) Recv() (providers.ChatChunk, error) {
	if s.closed.Load() {
		return providers.ChatChunk{}, providers.ErrStreamClosed
	}
	if s.err != nil {
		return providers.ChatChunk{}, s.err
	}
	if s.emitted {
		return providers.ChatChunk{}, io.EOF
	}
	if err := s.ctx.Err(); err != nil {
		return s.fail(providers.ContextError(err))
	}
	for {
		payload, err := s.nextPayload()
		switch {
		case errors.Is(err, io.EOF):
			// The body ended without the [DONE] marker: interrupted.
			return s.fail(contracts.New(contracts.ErrUnavailable, "localai: stream ended before completion"))
		case err != nil:
			return s.fail(mapTransportError(s.ctx, err))
		}
		if payload == "[DONE]" {
			if !s.sawFinish {
				return s.fail(schemaInvalid("stream ended without a finish reason"))
			}
			return s.done()
		}
		var ev chunkWire
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return s.fail(schemaInvalid("stream event is not valid JSON"))
		}
		if ev.Error != nil {
			return s.fail(mapEventError(*ev.Error))
		}
		if s.traceID == "" {
			s.traceID = traceID(ev.ID)
		}
		if ev.Usage != nil {
			u, uerr := ev.Usage.usage()
			if uerr != nil {
				return s.fail(uerr)
			}
			s.usage = u
		}
		if len(ev.Choices) == 0 {
			continue // the usage trailer
		}
		if len(ev.Choices) > 1 {
			return s.fail(schemaInvalid("stream event holds more than one choice"))
		}
		ch := ev.Choices[0]
		var content string
		if ch.Delta != nil {
			if ch.Delta.hasToolCall() {
				return s.fail(schemaInvalid("runtime streamed a tool call, which the adapter never requests"))
			}
			if ch.Delta.Content != nil {
				content = *ch.Delta.Content
			}
		}
		if content != "" && s.sawFinish {
			return s.fail(schemaInvalid("stream carried text after its finish reason"))
		}
		if ch.FinishReason != nil {
			if s.sawFinish {
				return s.fail(schemaInvalid("stream carried a second finish reason"))
			}
			finish, ferr := mapFinish(ch.FinishReason)
			if ferr != nil {
				return s.fail(ferr)
			}
			s.sawFinish, s.finish = true, finish
		}
		if content != "" {
			if s.firstDelta.IsZero() {
				s.firstDelta = s.c.clk.Now()
			}
			return providers.ChatChunk{Delta: content}, nil
		}
	}
}

// done builds the final chunk after a complete stream.
func (s *chatStream) done() (providers.ChatChunk, error) {
	now := s.c.clk.Now()
	t := providers.Timings{Total: now.Sub(s.started)}
	if !s.firstDelta.IsZero() {
		t.TimeToFirstToken = s.firstDelta.Sub(s.started)
	}
	s.emitted = true
	s.free()
	s.c.logDone(s.ctx, "chat_stream", s.model, s.usage, s.started)
	return providers.ChatChunk{
		Done:         true,
		FinishReason: s.finish,
		Usage:        s.usage,
		Timings:      t,
		TraceID:      s.traceID,
		Model:        s.model,
	}, nil
}
