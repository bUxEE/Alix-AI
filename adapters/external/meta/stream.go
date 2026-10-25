package meta

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// GenerateStream implements providers.MultimodalGenerator. It starts a streaming
// chat completion (stream_options.include_usage is always set) and returns once
// the host has answered 200; a refusal before that (rate limit, authentication,
// bad request) is returned as the error, after the bounded retries. A stream
// that is cut off, malformed, ends without the [DONE] sentinel, or ends without
// a finish reason and usage yields an error from Recv and no success.
func (c *Client) GenerateStream(ctx context.Context, scope contracts.Scope, req providers.RichRequest) (providers.RichStream, error) {
	p, err := c.prepare(scope, req, true)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithTimeout(ctx, c.cfg.StreamTimeout)
	started := c.clk.Now()
	rep, err := c.call(sctx, scope, c.routes.chat, p.body, "text/event-stream")
	if err != nil {
		cancel()
		return nil, err
	}
	return &stream{
		c: c, p: p, rep: rep, started: started,
		ExtStream: external.NewExtStream(sctx, cancel, ProviderName, rep.resp.Body),
		toolIdx:   map[int]int{},
	}, nil
}

// stream is a pull-style providers.RichStream over one SSE response. The
// embedded ExtStream owns the connection, Close and the sticky error.
type stream struct {
	*external.ExtStream
	c       *Client
	p       *prepared
	rep     *reply
	started time.Time

	seq     int
	pending []providers.RichChunk
	// toolIdx maps the host's tool call index to the proposal number; toolGot
	// counts argument bytes per proposal number.
	toolIdx map[int]int
	toolGot []int
	first   time.Duration
	done    bool

	model  string
	msgID  string
	usage  *apiUsage
	finish *string
}

// Recv implements providers.RichStream. Chunks still queued from the event
// that failed are returned before the failure.
func (s *stream) Recv() (providers.RichChunk, error) {
	for {
		if s.Closed() {
			return providers.RichChunk{}, providers.ErrStreamClosed
		}
		if len(s.pending) > 0 {
			ck := s.pending[0]
			s.pending = s.pending[1:]
			s.seq++
			ck.Seq = s.seq
			return ck, nil
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
		if err := s.handle(ev); err != nil {
			return providers.RichChunk{}, s.Fail(err)
		}
	}
}

func unexpectedEnd() error {
	return apiErr(contracts.ErrUnavailable, pv.ProviderErrorKindConnectionLost, true, "the stream ended before it completed")
}

// streamChunk holds the members of a chat.completion.chunk that are read. An
// error event has only Error.
type streamChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int             `json:"index"`
		Delta        json.RawMessage `json:"delta"`
		FinishReason *string         `json:"finish_reason"`
	} `json:"choices"`
	Usage *apiUsage `json:"usage"`
	Error *struct {
		Type string `json:"type"`
		Code any    `json:"code"`
	} `json:"error"`
}

func (s *stream) push(ck providers.RichChunk) { s.pending = append(s.pending, ck) }

func (s *stream) handle(ev external.SSEEvent) error {
	data := strings.TrimSpace(ev.Data)
	if data == "" {
		return nil
	}
	if data == "[DONE]" {
		return s.complete()
	}
	var e streamChunk
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		return malformed("a stream event is not valid JSON")
	}
	if e.Error != nil {
		code := ""
		if cs, ok := e.Error.Code.(string); ok {
			code = cs
		} else if safeCodeRE.MatchString(e.Error.Type) {
			code = e.Error.Type
		}
		return mapFailureCode(code)
	}
	if e.Model != "" {
		s.model = e.Model
	}
	if s.msgID == "" {
		s.msgID = e.ID
	}
	if e.Usage != nil {
		s.usage = e.Usage
	}
	if len(e.Choices) > 1 {
		return malformed("the stream holds more than one choice")
	}
	for _, ch := range e.Choices {
		var delta map[string]json.RawMessage
		if len(ch.Delta) > 0 && json.Unmarshal(ch.Delta, &delta) != nil {
			return malformed("a stream delta is not valid JSON")
		}
		for _, k := range hostRunKeys {
			if v, ok := delta[k]; ok && !isEmptyJSON(v) {
				return malformed("the stream holds output that was not requested")
			}
		}
		text, err := contentText(delta["content"])
		if err != nil {
			return err
		}
		var tcalls []apiToolCall
		if v, ok := delta["tool_calls"]; ok && !isEmptyJSON(v) {
			if json.Unmarshal(v, &tcalls) != nil {
				return malformed("a stream delta holds malformed tool calls")
			}
		}
		if text != "" {
			s.markFirst()
			s.push(providers.RichChunk{Kind: providers.DeltaText, Text: text})
		}
		for _, tc := range tcalls {
			if tc.Type != "" && tc.Type != "function" {
				return malformed("the stream holds a tool call that was not requested")
			}
			hi := 0
			if tc.Index != nil {
				hi = *tc.Index
			}
			n, known := s.toolIdx[hi]
			if !known {
				n = len(s.toolIdx)
				s.toolIdx[hi] = n
				s.toolGot = append(s.toolGot, 0)
				s.markFirst()
				s.push(providers.RichChunk{Kind: providers.DeltaToolArgs, ToolIndex: n, ToolID: tc.ID, ToolName: tc.Function.Name})
			}
			if tc.Function.Arguments != "" {
				s.toolGot[n] += len(tc.Function.Arguments)
				s.markFirst()
				s.push(providers.RichChunk{Kind: providers.DeltaToolArgs, ToolIndex: n, ArgsDelta: tc.Function.Arguments})
			}
		}
		if ch.FinishReason != nil {
			s.finish = ch.FinishReason
		}
	}
	return nil
}

func (s *stream) markFirst() {
	if s.first == 0 {
		s.first = s.c.clk.Now().Sub(s.started)
		if s.first <= 0 {
			s.first = 1
		}
	}
}

// complete builds the final chunk at the [DONE] sentinel. A stream that carries
// no finish reason or no usage is not a success: the first would hide a cut-off
// answer, the second would leave the call unmetered.
func (s *stream) complete() error {
	finish, err := finishFor(s.finish)
	if err != nil {
		return unexpectedEnd()
	}
	tools := len(s.toolIdx)
	switch {
	case finish == providers.FinishFiltered:
	case tools > 0:
		finish = providers.FinishToolCalls
	case finish == providers.FinishToolCalls:
		return malformed("the response stopped for a tool call but holds none")
	}
	usage, err := s.c.price(s.p.model, s.usage)
	if err != nil {
		return err
	}
	// A call with no arguments streams none: its input is {}.
	for n, got := range s.toolGot {
		if got == 0 {
			s.push(providers.RichChunk{Kind: providers.DeltaToolArgs, ToolIndex: n, ArgsDelta: "{}"})
		}
	}
	s.done = true
	trace := s.rep.traceID
	if trace == "" && external.ExtTraceID(s.msgID) {
		trace = s.msgID
	}
	s.push(providers.RichChunk{
		Kind: providers.DeltaDone, FinishReason: finish, Usage: &usage, RateLimit: s.rep.rate,
		Timings: providers.Timings{TimeToFirstToken: s.first, Total: s.c.clk.Now().Sub(s.started)},
		TraceID: trace, Model: s.c.replyModel(s.model),
	})
	return nil
}
