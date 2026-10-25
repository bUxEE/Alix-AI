package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// GenerateStream implements providers.MultimodalGenerator. It starts a
// streaming Messages call and returns once the provider has answered 200; a
// refusal before that (rate limit, authentication, bad request) is returned as
// the error, after the bounded retries. A stream that is cut off, malformed or
// ends without message_stop yields an error from Recv and no success.
//
// A converted schema keeps the shape of the original, so streamed structured
// output and streamed tool arguments are allowed; the caller validates the
// assembled document or arguments as it does for Generate.
func (c *Client) GenerateStream(ctx context.Context, scope contracts.Scope, req providers.RichRequest) (providers.RichStream, error) {
	p, err := c.prepare(scope, req, true)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithTimeout(ctx, c.cfg.StreamTimeout)
	started := c.clk.Now()
	rep, err := c.call(sctx, scope, routes[0], p.body, "text/event-stream")
	if err != nil {
		cancel()
		return nil, err
	}
	return &stream{
		c: c, p: p, rep: rep, started: started,
		ExtStream: external.NewExtStream(sctx, cancel, ProviderName, rep.resp.Body),
		blocks:    map[int]*streamBlock{},
	}, nil
}

// streamBlock is one content block of the message being streamed.
type streamBlock struct {
	kind    string // text, tool_use or thinking
	toolIdx int
	got     int
}

// stream is a pull-style providers.RichStream over one SSE response. The
// embedded ExtStream owns the connection, Close and the sticky error.
type stream struct {
	*external.ExtStream
	c       *Client
	p       *prepared
	rep     *reply
	started time.Time

	seq    int
	blocks map[int]*streamBlock
	tools  int
	first  time.Duration
	done   bool

	// from message_start / message_delta
	model string
	msgID string
	usage apiUsage
	stop  *string
}

// Recv implements providers.RichStream.
func (s *stream) Recv() (providers.RichChunk, error) {
	for {
		if s.Closed() {
			return providers.RichChunk{}, providers.ErrStreamClosed
		}
		if err := s.Err(); err != nil {
			return providers.RichChunk{}, err
		}
		if s.done {
			return providers.RichChunk{}, io.EOF
		}
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			err = s.Fail(unexpectedEnd())
		}
		if err != nil {
			return providers.RichChunk{}, err
		}
		chunk, err := s.handle(ev)
		if err != nil {
			return providers.RichChunk{}, s.Fail(err)
		}
		if chunk != nil {
			s.seq++
			chunk.Seq = s.seq
			return *chunk, nil
		}
	}
}

func unexpectedEnd() error {
	return apiErr(contracts.ErrUnavailable, pv.ProviderErrorKindConnectionLost, true, "the stream ended before it completed")
}

// streamEvent holds the members of a stream event that are read.
type streamEvent struct {
	Type    string          `json:"type"`
	Index   int             `json:"index"`
	Message *apiMessage     `json:"message"`
	Block   *apiBlock       `json:"content_block"`
	Usage   *apiUsage       `json:"usage"`
	Delta   json.RawMessage `json:"delta"`
	Error   *struct {
		Type string `json:"type"`
	} `json:"error"`
}

type streamDelta struct {
	Type        string  `json:"type"`
	Text        string  `json:"text"`
	PartialJSON string  `json:"partial_json"`
	StopReason  *string `json:"stop_reason"`
}

// merge copies the members a later usage object states over the earlier ones
// (message_delta usage is cumulative).
func (u *apiUsage) merge(o *apiUsage) {
	if o == nil {
		return
	}
	if o.InputTokens != nil {
		u.InputTokens = o.InputTokens
	}
	if o.OutputTokens != nil {
		u.OutputTokens = o.OutputTokens
	}
	if o.CacheCreationTokens != nil {
		u.CacheCreationTokens = o.CacheCreationTokens
	}
	if o.CacheReadTokens != nil {
		u.CacheReadTokens = o.CacheReadTokens
	}
}

func (s *stream) handle(ev external.SSEEvent) (*providers.RichChunk, error) {
	if ev.Data == "" {
		return nil, nil
	}
	var e streamEvent
	if err := json.Unmarshal([]byte(ev.Data), &e); err != nil {
		return nil, malformed("a stream event is not valid JSON")
	}
	switch e.Type {
	case "message_start":
		if e.Message == nil {
			return nil, malformed("the first event carries no message")
		}
		s.model, s.msgID = e.Message.Model, e.Message.ID
		s.usage.merge(e.Message.Usage)
		return nil, nil
	case "content_block_start":
		if e.Block == nil {
			return nil, malformed("a block event carries no block")
		}
		switch e.Block.Type {
		case "text":
			s.blocks[e.Index] = &streamBlock{kind: "text"}
			if e.Block.Text != "" {
				s.markFirst()
				return &providers.RichChunk{Kind: providers.DeltaText, Text: e.Block.Text}, nil
			}
			return nil, nil
		case "thinking", "redacted_thinking":
			s.blocks[e.Index] = &streamBlock{kind: "thinking"}
			return nil, nil
		case "tool_use":
			s.blocks[e.Index] = &streamBlock{kind: "tool_use", toolIdx: s.tools}
			s.tools++
			return &providers.RichChunk{
				Kind: providers.DeltaToolArgs, ToolIndex: s.tools - 1, ToolID: e.Block.ID, ToolName: e.Block.Name,
			}, nil
		}
		// A provider-run tool block or anything unknown was never requested.
		return nil, malformed("the stream holds a block that was not requested")
	case "content_block_delta":
		b := s.blocks[e.Index]
		if b == nil {
			return nil, malformed("a delta names an unknown block")
		}
		var d streamDelta
		if err := json.Unmarshal(e.Delta, &d); err != nil {
			return nil, malformed("a delta is not valid JSON")
		}
		switch d.Type {
		case "text_delta":
			if b.kind != "text" {
				return nil, malformed("a text delta in a block that is not text")
			}
			if d.Text == "" {
				return nil, nil
			}
			s.markFirst()
			return &providers.RichChunk{Kind: providers.DeltaText, Text: d.Text}, nil
		case "input_json_delta":
			if b.kind != "tool_use" {
				return nil, malformed("an argument delta in a block that is not a tool call")
			}
			if d.PartialJSON == "" {
				return nil, nil
			}
			b.got += len(d.PartialJSON)
			s.markFirst()
			return &providers.RichChunk{Kind: providers.DeltaToolArgs, ToolIndex: b.toolIdx, ArgsDelta: d.PartialJSON}, nil
		}
		return nil, nil // thinking_delta, signature_delta, citations_delta
	case "content_block_stop":
		b := s.blocks[e.Index]
		if b != nil && b.kind == "tool_use" && b.got == 0 {
			// A call with no arguments streams no partial_json: its input is {}.
			b.got = 2
			return &providers.RichChunk{Kind: providers.DeltaToolArgs, ToolIndex: b.toolIdx, ArgsDelta: "{}"}, nil
		}
		return nil, nil
	case "message_delta":
		var d streamDelta
		_ = json.Unmarshal(e.Delta, &d)
		if d.StopReason != nil {
			s.stop = d.StopReason
		}
		s.usage.merge(e.Usage)
		return nil, nil
	case "message_stop":
		return s.finish()
	case "error":
		typ := ""
		if e.Error != nil {
			typ = e.Error.Type
		}
		return nil, mapErrorType(typ)
	}
	return nil, nil // ping and event types added later
}

func (s *stream) markFirst() {
	if s.first == 0 {
		s.first = s.c.clk.Now().Sub(s.started)
		if s.first <= 0 {
			s.first = 1
		}
	}
}

// finish builds the final chunk at message_stop.
func (s *stream) finish() (*providers.RichChunk, error) {
	finish, err := finishFor(s.stop)
	if err != nil {
		return nil, unexpectedEnd() // message_stop without a stop reason
	}
	switch {
	case finish == providers.FinishFiltered:
	case s.tools > 0:
		finish = providers.FinishToolCalls
	case finish == providers.FinishToolCalls:
		return nil, malformed("the response stopped for a tool call but holds none")
	}
	usage, err := s.c.price(s.p.model, &s.usage)
	if err != nil {
		return nil, err
	}
	s.done = true
	trace := s.rep.traceID
	if trace == "" && external.ExtTraceID(s.msgID) {
		trace = s.msgID
	}
	return &providers.RichChunk{
		Kind: providers.DeltaDone, FinishReason: finish, Usage: &usage, RateLimit: s.rep.rate,
		Timings: providers.Timings{TimeToFirstToken: s.first, Total: s.c.clk.Now().Sub(s.started)},
		TraceID: trace, Model: s.model,
	}, nil
}
