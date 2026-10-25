package openai

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

// GenerateStream implements providers.MultimodalGenerator. It starts a
// streaming Responses call and returns once the provider has answered 200; a
// refusal before that (rate limit, authentication, bad request) is returned as
// the error, after the bounded retries. A stream that is cut off, malformed or
// ends without response.completed yields an error from Recv and no success.
//
// A request whose structured-output schema or tool schemas needed a shape
// conversion (open maps, optional properties) is refused here: the deltas cannot
// be restored piece by piece, so such a request uses Generate.
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
	}, nil
}

type streamTool struct {
	itemID string
	got    int
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
	tools   []streamTool
	first   time.Duration
	refused bool
	done    bool
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
	Type      string          `json:"type"`
	Delta     string          `json:"delta"`
	ItemID    string          `json:"item_id"`
	Arguments string          `json:"arguments"`
	Code      string          `json:"code"`
	Item      *apiItem        `json:"item"`
	Response  json.RawMessage `json:"response"`
}

// hostedToolEvents are event families of provider-run tools. None was
// requested, so seeing one rejects the stream.
var hostedToolEvents = []string{
	"response.web_search_call.", "response.file_search_call.", "response.code_interpreter_call",
	"response.mcp_", "response.computer_call", "response.image_generation_call.", "response.custom_tool_call",
}

func (s *stream) handle(ev external.SSEEvent) (*providers.RichChunk, error) {
	if ev.Data == "" || ev.Data == "[DONE]" {
		return nil, nil
	}
	var e streamEvent
	if err := json.Unmarshal([]byte(ev.Data), &e); err != nil {
		return nil, malformed("a stream event is not valid JSON")
	}
	for _, pfx := range hostedToolEvents {
		if strings.HasPrefix(e.Type, pfx) {
			return nil, malformed("the stream holds an event of a tool that was not requested")
		}
	}
	switch e.Type {
	case "response.output_item.added":
		if e.Item == nil {
			return nil, malformed("an item event carries no item")
		}
		switch e.Item.Type {
		case "message", "reasoning":
			return nil, nil
		case "function_call":
			s.tools = append(s.tools, streamTool{itemID: e.Item.ID})
			return &providers.RichChunk{
				Kind: providers.DeltaToolArgs, ToolIndex: len(s.tools) - 1, ToolID: e.Item.CallID, ToolName: e.Item.Name,
			}, nil
		}
		return nil, malformed("the stream holds an item that was not requested")
	case "response.output_text.delta":
		if e.Delta == "" {
			return nil, nil
		}
		s.markFirst()
		return &providers.RichChunk{Kind: providers.DeltaText, Text: e.Delta}, nil
	case "response.refusal.delta":
		s.refused = true
		return nil, nil
	case "response.function_call_arguments.delta":
		i := s.toolIndex(e.ItemID)
		if i < 0 {
			return nil, malformed("an argument delta names an unknown item")
		}
		if e.Delta == "" {
			return nil, nil
		}
		s.tools[i].got += len(e.Delta)
		s.markFirst()
		return &providers.RichChunk{Kind: providers.DeltaToolArgs, ToolIndex: i, ArgsDelta: e.Delta}, nil
	case "response.function_call_arguments.done":
		i := s.toolIndex(e.ItemID)
		if i < 0 {
			return nil, malformed("an argument event names an unknown item")
		}
		if s.tools[i].got == 0 && e.Arguments != "" {
			s.tools[i].got = len(e.Arguments)
			return &providers.RichChunk{Kind: providers.DeltaToolArgs, ToolIndex: i, ArgsDelta: e.Arguments}, nil
		}
		return nil, nil
	case "response.completed", "response.incomplete":
		return s.finish(e)
	case "response.failed":
		var ar apiResponse
		_ = json.Unmarshal(e.Response, &ar)
		code := ""
		if ar.Error != nil {
			code = ar.Error.Code
		}
		return nil, mapFailureCode(code)
	case "error":
		return nil, mapFailureCode(e.Code)
	}
	return nil, nil // created, in_progress, *.done, content_part.*, reasoning summaries, ...
}

func (s *stream) toolIndex(itemID string) int {
	for i, t := range s.tools {
		if t.itemID != "" && t.itemID == itemID {
			return i
		}
	}
	return -1
}

func (s *stream) markFirst() {
	if s.first == 0 {
		s.first = s.c.clk.Now().Sub(s.started)
		if s.first <= 0 {
			s.first = 1
		}
	}
}

// finish builds the final chunk from response.completed or response.incomplete.
func (s *stream) finish(e streamEvent) (*providers.RichChunk, error) {
	var ar apiResponse
	if err := json.Unmarshal(e.Response, &ar); err != nil {
		return nil, malformed("the final stream event is not a response object")
	}
	finish := providers.FinishStop
	if e.Type == "response.incomplete" {
		reason := ""
		if ar.IncompleteDetails != nil {
			reason = ar.IncompleteDetails.Reason
		}
		switch reason {
		case "max_output_tokens":
			finish = providers.FinishLength
		case "content_filter":
			finish = providers.FinishFiltered
		default:
			return nil, malformed("the response is incomplete for an unknown reason")
		}
	}
	switch {
	case s.refused:
		finish = providers.FinishFiltered
	case len(s.tools) > 0:
		finish = providers.FinishToolCalls
	}
	usage, err := s.c.price(s.p.model, ar.Usage)
	if err != nil {
		return nil, err
	}
	s.done = true
	trace := s.rep.traceID
	if trace == "" && external.ExtTraceID(ar.ID) {
		trace = ar.ID
	}
	return &providers.RichChunk{
		Kind: providers.DeltaDone, FinishReason: finish, Usage: &usage, RateLimit: s.rep.rate,
		Timings: providers.Timings{TimeToFirstToken: s.first, Total: s.c.clk.Now().Sub(s.started)},
		TraceID: trace, Model: ar.Model,
	}, nil
}
