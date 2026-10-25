package gemini

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
// streamGenerateContent?alt=sse call and returns once the provider has answered
// 200; a refusal before that (rate limit, authentication, bad request) is
// returned as the error, after the bounded retries.
//
// The stream has no terminator event: every server-sent event is a whole
// GenerateContentResponse, the last of them carries the finishReason, and the
// connection then ends. So the stream is complete only when the connection
// ends cleanly AFTER an event with a finishReason (or a blocked prompt); one that
// is cut off, malformed or ends without a reason yields an error from Recv and
// no success. A function call arrives whole in one event and is passed on as one
// tool-argument chunk.
func (c *Client) GenerateStream(ctx context.Context, scope contracts.Scope, req providers.RichRequest) (providers.RichStream, error) {
	p, err := c.prepare(scope, req, true)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithTimeout(ctx, c.cfg.StreamTimeout)
	started := c.clk.Now()
	rep, err := c.call(sctx, scope, p.route(), p.body, "text/event-stream")
	if err != nil {
		cancel()
		return nil, err
	}
	return &stream{
		c: c, p: p, rep: rep, started: started,
		ExtStream: external.NewExtStream(sctx, cancel, ProviderName, rep.resp.Body),
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
	tools   int
	first   time.Duration
	done    bool

	// from the events
	model   string
	respID  string
	usage   *apiUsage
	reason  string // finishReason of the candidate, once seen
	haveEnd bool   // a finishReason or a prompt block was seen
}

// Recv implements providers.RichStream. A failure is returned before any chunk
// still queued from the event that failed.
func (s *stream) Recv() (providers.RichChunk, error) {
	for {
		if s.Closed() {
			return providers.RichChunk{}, providers.ErrStreamClosed
		}
		if err := s.Err(); err != nil {
			return providers.RichChunk{}, err
		}
		if len(s.pending) > 0 {
			ch := s.pending[0]
			s.pending = s.pending[1:]
			s.seq++
			ch.Seq = s.seq
			return ch, nil
		}
		if s.done {
			return providers.RichChunk{}, io.EOF
		}
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			if s.haveEnd {
				if err := s.finish(); err != nil {
					return providers.RichChunk{}, s.Fail(err)
				}
				continue
			}
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

func (s *stream) push(ch providers.RichChunk) { s.pending = append(s.pending, ch) }

func (s *stream) markFirst() {
	if s.first == 0 {
		s.first = s.c.clk.Now().Sub(s.started)
		if s.first <= 0 {
			s.first = 1
		}
	}
}

// handle reads one event: a whole GenerateContentResponse.
func (s *stream) handle(ev external.SSEEvent) error {
	if ev.Data == "" || ev.Data == "[DONE]" {
		return nil
	}
	var ar apiResponse
	if err := json.Unmarshal([]byte(ev.Data), &ar); err != nil {
		return malformed("a stream event is not valid JSON")
	}
	if ar.Error != nil {
		// An error object inside a stream that was answered 200.
		err, _ := mapHTTPError(0, nil, []byte(ev.Data), s.c.clk.Now())
		return err
	}
	if ar.ModelVersion != "" {
		s.model = ar.ModelVersion
	}
	if ar.ResponseID != "" && s.respID == "" {
		s.respID = ar.ResponseID
	}
	if ar.UsageMetadata != nil {
		s.usage = ar.UsageMetadata // cumulative: the last one reported wins
	}
	cand, err := candidate(&ar)
	if err != nil {
		return err
	}
	if cand == nil {
		s.haveEnd, s.reason = true, "SAFETY" // a refused prompt ends the stream
		return nil
	}
	if cand.Content != nil {
		for _, pt := range cand.Content.Parts {
			switch {
			case pt.Unrequested:
				return malformed("the stream holds a part that was not requested")
			case pt.Call != nil:
				id := s.c.ids.callID(pt.Call.ID, s.respID, s.tools)
				s.c.sigs.put(s.p.scope, id, pt.ThoughtSignature)
				args := string(pt.Call.Args)
				if args == "" || args == "null" {
					args = "{}"
				}
				s.markFirst()
				s.push(providers.RichChunk{Kind: providers.DeltaToolArgs, ToolIndex: s.tools, ToolID: id, ToolName: pt.Call.Name, ArgsDelta: args})
				s.tools++
			case pt.Thought:
			case pt.Text != "":
				s.markFirst()
				s.push(providers.RichChunk{Kind: providers.DeltaText, Text: pt.Text})
			}
		}
	}
	if cand.FinishReason != "" && cand.FinishReason != "FINISH_REASON_UNSPECIFIED" {
		s.haveEnd, s.reason = true, cand.FinishReason
	}
	return nil
}

// finish queues the final chunk once the connection has ended cleanly after the
// finishReason.
func (s *stream) finish() error {
	finish, err := finishFor(s.reason, s.tools > 0)
	if err != nil {
		return err
	}
	usage, err := s.c.price(s.p.model, s.usage)
	if err != nil {
		return err
	}
	s.done = true
	trace := ""
	if external.ExtTraceID(s.respID) {
		trace = s.respID
	}
	s.push(providers.RichChunk{
		Kind: providers.DeltaDone, FinishReason: finish, Usage: &usage,
		Timings: providers.Timings{TimeToFirstToken: s.first, Total: s.c.clk.Now().Sub(s.started)},
		TraceID: trace, Model: s.model,
	})
	return nil
}
