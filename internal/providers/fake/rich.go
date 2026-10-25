package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Rich scripting. A rule that scripts parts or tool calls answers
// MultimodalGenerator calls with them; a rule that only scripts text answers a
// rich call with that text as one text part, so existing rules keep working.
//
//	f.On(fake.HasPart(providers.ModImage)).Reply("a cat")
//	f.On(fake.OffersTool("book")).ProposeToolCall("call_1", "book", `{"slot":"10:00"}`)
//	f.On(fake.Any()).Fail(fake.FailRateLimited).RetryAfter(3 * time.Second)
//
// The fake never runs a proposed tool: there is nothing in the contract to run
// it with.

// DefaultCapabilities is what a fake reports unless WithCapabilities says
// otherwise: every modality in, text, image and audio out, streaming, tool
// calls, strict structured output, generous limits and no price table.
func DefaultCapabilities(provider, model string) providers.ModelCapabilities {
	return providers.ModelCapabilities{
		Provider:         provider,
		Model:            model,
		Version:          "fake",
		InputModalities:  []providers.Modality{providers.ModText, providers.ModImage, providers.ModFile, providers.ModAudio},
		OutputModalities: []providers.Modality{providers.ModText, providers.ModImage, providers.ModAudio},
		Streaming:        true,
		StructuredOutput: providers.StructuredStrict,
		Grammar:          true,
		Tools:            providers.ToolCapability{Supported: true, StrictArguments: true, ParallelCalls: true, MaxTools: 32},
		MaxContextToks:   128000,
		MaxOutputToks:    16384,
		ImageGeneration:  true,
		SpeechToText:     true,
		TextToSpeech:     true,
		Metering:         providers.MeteringPerResponse,
	}
}

// WithCapabilities sets what Capabilities reports and what rich calls are
// checked against. It panics on an incoherent record: that is a bug in the
// test.
func WithCapabilities(c providers.ModelCapabilities) Option {
	if err := c.Validate(); err != nil {
		panic(fmt.Sprintf("fake: WithCapabilities: %v", err))
	}
	return func(p *Provider) { p.caps = c.Clone() }
}

// Capabilities implements providers.CapabilityProvider.
func (p *Provider) Capabilities(ctx context.Context) (providers.ModelCapabilities, error) {
	if err := ctx.Err(); err != nil {
		return providers.ModelCapabilities{}, providers.ContextError(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.caps.Clone(), nil
}

// ReplyParts sets the reply parts (text, or media bytes) of a rich call.
func (r *Rule) ReplyParts(parts ...providers.ContentPart) *Rule {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	r.parts = nil
	for _, p := range parts {
		r.parts = append(r.parts, p.Clone())
	}
	return r
}

// ProposeToolCall adds a tool-call proposal to the reply. argsJSON is used as
// is, valid or not.
func (r *Rule) ProposeToolCall(id, name, argsJSON string) *Rule {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	r.toolCalls = append(r.toolCalls, providers.ToolCallProposal{ID: id, Name: name, Arguments: json.RawMessage(argsJSON)})
	return r
}

// Cost makes the reply report a provider-stated cost (Estimated false).
func (r *Rule) Cost(micros int64, currency string) *Rule {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	r.cost = &providers.CostUsage{CostMicros: micros, Currency: currency}
	return r
}

// RateInfo makes a successful reply carry rate-limit information.
func (r *Rule) RateInfo(info providers.RateLimitInfo) *Rule {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	r.rate = &info
	return r
}

// RetryAfter sets the retry-after FailRateLimited reports for this rule.
func (r *Rule) RetryAfter(d time.Duration) *Rule {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	r.retryAfter = d
	return r
}

// richReply is what a rich call resolved to.
type richReply struct {
	parts []providers.ContentPart
	calls []providers.ToolCallProposal
}

func (p *Provider) resolveRich(pl plan, call Call) (richReply, error) {
	switch pl.fail {
	case FailRateLimited:
		d := pl.retryAfter
		if d <= 0 {
			d = DefaultRetryAfter
		}
		return richReply{}, providers.RateLimitError(providers.RateLimitInfo{RetryAfter: d, Scope: "requests"})
	case FailMalformedToolCall:
		return richReply{calls: []providers.ToolCallProposal{
			{ID: "call_bad", Name: "unlisted_tool", Arguments: json.RawMessage(`{"truncated": [1, 2,`)},
		}}, nil
	}
	if !pl.noRule && (len(pl.parts) > 0 || len(pl.toolCalls) > 0) && (pl.fail == "" || pl.fail == FailPartialStream) {
		return richReply{parts: cloneParts(pl.parts), calls: cloneCalls(pl.toolCalls)}, nil
	}
	text, err := p.resolveText(pl, call)
	if err != nil {
		return richReply{}, err
	}
	var parts []providers.ContentPart
	if text != "" {
		parts = []providers.ContentPart{providers.TextPart(text)}
	}
	return richReply{parts: parts}, nil
}

func cloneParts(in []providers.ContentPart) []providers.ContentPart {
	out := make([]providers.ContentPart, len(in))
	for i, p := range in {
		out[i] = p.Clone()
	}
	return out
}

func cloneCalls(in []providers.ToolCallProposal) []providers.ToolCallProposal {
	out := make([]providers.ToolCallProposal, len(in))
	for i, c := range in {
		out[i] = c.Clone()
	}
	return out
}

func (p *Provider) richUsage(pl plan, req providers.RichRequest, rr richReply) providers.CostUsage {
	u := providers.CostUsage{ToksIn: providers.Budget{}.EstimateRich(req)}
	for _, part := range rr.parts {
		switch part.Kind {
		case providers.ModText:
			u.ToksOut += providers.EstimateTokens(part.Text)
		case providers.ModImage:
			u.ImagesOut++
		}
	}
	for _, c := range rr.calls {
		u.ToksOut += providers.EstimateTokens(c.Name) + providers.EstimateTokens(string(c.Arguments))
	}
	for _, m := range req.Messages {
		for _, part := range m.Parts {
			if part.Kind == providers.ModImage {
				u.ImagesIn++
			}
		}
	}
	if pl.usage != nil {
		u.ToksIn, u.ToksOut = pl.usage.InputTokens, pl.usage.OutputTokens
	}
	if pl.cost != nil {
		u.CostMicros, u.Currency, u.Estimated = pl.cost.CostMicros, pl.cost.Currency, false
		return u
	}
	p.mu.Lock()
	prices := p.caps.Pricing
	p.mu.Unlock()
	if prices != nil {
		if priced, err := prices.Cost(u); err == nil {
			return priced
		}
	}
	u.Estimated = true
	return u
}

func (p *Provider) finishFor(rr richReply) providers.FinishReason {
	if len(rr.calls) > 0 {
		return providers.FinishToolCalls
	}
	return providers.FinishStop
}

// begin for rich calls: scope, request and capabilities are checked before
// anything is recorded, so a rejected call never reaches the "model".
func (p *Provider) beginRich(op Op, scope contracts.Scope, req providers.RichRequest, stream bool) (plan, error) {
	if err := scope.Validate(); err != nil {
		return plan{}, err
	}
	if err := req.Validate(); err != nil {
		return plan{}, err
	}
	p.mu.Lock()
	caps := p.caps
	p.mu.Unlock()
	if err := caps.Check(req); err != nil {
		return plan{}, err
	}
	if stream && !caps.Streaming {
		return plan{}, providers.UnsupportedError("streaming")
	}
	return p.begin(op, scope, req.TextView(), &req)
}

// Generate implements providers.MultimodalGenerator.
func (p *Provider) Generate(ctx context.Context, scope contracts.Scope, req providers.RichRequest) (providers.RichResult, error) {
	if err := ctx.Err(); err != nil {
		return providers.RichResult{}, providers.ContextError(err)
	}
	req = req.Clone()
	pl, err := p.beginRich(OpGenerate, scope, req, false)
	if err != nil {
		return providers.RichResult{}, err
	}
	rr, err := p.resolveRich(pl, Call{Scope: scope, Request: req.TextView(), Rich: &req})
	if err != nil {
		return providers.RichResult{}, err
	}
	if pl.fail == FailPartialStream {
		return providers.RichResult{}, injected(FailPartialStream)
	}
	if pl.stall >= 0 {
		<-ctx.Done()
		return providers.RichResult{}, providers.ContextError(ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return providers.RichResult{}, providers.ContextError(err)
	}
	res := providers.RichResult{
		Parts: rr.parts, ToolCalls: rr.calls, FinishReason: p.finishFor(rr),
		Usage: p.richUsage(pl, req, rr), RateLimit: pl.rate,
		Timings: pl.timings, TraceID: p.traceID(pl.seq), Model: p.model,
	}
	if req.Output.Kind() == providers.ConstraintJSONSchema {
		j, err := providers.ParseJSONOutput(res.Text())
		if err != nil {
			return providers.RichResult{}, err
		}
		res.JSON = j
	}
	p.advance(pl.timings.Total)
	return res, nil
}

// GenerateStream implements providers.MultimodalGenerator. Text arrives as
// DeltaText chunks of the configured size, each media part whole, each tool
// call as DeltaToolArgs chunks, then a usage chunk and the final chunk.
func (p *Provider) GenerateStream(ctx context.Context, scope contracts.Scope, req providers.RichRequest) (providers.RichStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, providers.ContextError(err)
	}
	req = req.Clone()
	pl, err := p.beginRich(OpGenerateStream, scope, req, true)
	if err != nil {
		return nil, err
	}
	rr, err := p.resolveRich(pl, Call{Scope: scope, Request: req.TextView(), Rich: &req})
	if err != nil {
		return nil, err
	}
	var chunks []providers.RichChunk
	add := func(c providers.RichChunk) {
		c.Seq = len(chunks) + 1
		chunks = append(chunks, c)
	}
	for _, part := range rr.parts {
		if part.Kind == providers.ModText {
			for _, d := range splitRunes(part.Text, p.chunkSize) {
				add(providers.RichChunk{Kind: providers.DeltaText, Text: d})
			}
			continue
		}
		cp := part.Clone()
		add(providers.RichChunk{Kind: providers.DeltaPart, Part: &cp})
	}
	for i, c := range rr.calls {
		pieces := splitRunes(string(c.Arguments), p.chunkSize)
		if len(pieces) == 0 {
			pieces = []string{""}
		}
		for j, piece := range pieces {
			ch := providers.RichChunk{Kind: providers.DeltaToolArgs, ToolIndex: i, ArgsDelta: piece}
			if j == 0 {
				ch.ToolID, ch.ToolName = c.ID, c.Name
			}
			add(ch)
		}
	}
	content := len(chunks)
	usage := p.richUsage(pl, req, rr)
	add(providers.RichChunk{Kind: providers.DeltaUsage, Usage: &usage})
	add(providers.RichChunk{
		Kind: providers.DeltaDone, Usage: &usage, FinishReason: p.finishFor(rr), RateLimit: pl.rate,
		Timings: pl.timings, TraceID: p.traceID(pl.seq), Model: p.model,
	})
	s := &richStream{p: p, ctx: ctx, chunks: chunks, failAfter: -1, stallAfter: pl.stall, done: make(chan struct{}), total: pl.timings.Total}
	if pl.fail == FailPartialStream {
		s.failAfter = content / 2
		if s.failAfter < 1 {
			s.failAfter = 1
		}
		if s.failAfter > content {
			s.failAfter = content
		}
		s.failErr = injected(FailPartialStream)
	}
	p.mu.Lock()
	p.openStreams++
	p.mu.Unlock()
	return s, nil
}

// richStream is the fake's providers.RichStream. Like stream it owns no
// goroutine. Lock order: richStream.mu, then Provider.mu.
type richStream struct {
	p      *Provider
	ctx    context.Context
	chunks []providers.RichChunk
	total  time.Duration

	failAfter  int
	failErr    error
	stallAfter int

	done chan struct{}

	mu     sync.Mutex
	i      int
	closed bool
	term   error
	closeO sync.Once
}

func (s *richStream) endLocked(err error) error {
	if s.term == nil {
		s.term = err
		s.p.mu.Lock()
		s.p.openStreams--
		s.p.mu.Unlock()
	}
	return err
}

// Recv implements providers.RichStream.
func (s *richStream) Recv() (providers.RichChunk, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return providers.RichChunk{}, providers.ErrStreamClosed
	}
	if s.term != nil {
		err := s.term
		s.mu.Unlock()
		return providers.RichChunk{}, err
	}
	if err := s.ctx.Err(); err != nil {
		err = s.endLocked(providers.ContextError(err))
		s.mu.Unlock()
		return providers.RichChunk{}, err
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
			return providers.RichChunk{}, providers.ErrStreamClosed
		}
		return providers.RichChunk{}, s.endLocked(providers.ContextError(s.ctx.Err()))
	}
	if s.failAfter >= 0 && s.i >= s.failAfter {
		err := s.endLocked(s.failErr)
		s.mu.Unlock()
		return providers.RichChunk{}, err
	}
	if s.i >= len(s.chunks) {
		err := s.endLocked(io.EOF)
		s.mu.Unlock()
		return providers.RichChunk{}, err
	}
	c := s.chunks[s.i]
	s.i++
	last := c.Kind == providers.DeltaDone
	s.mu.Unlock()

	s.p.mu.Lock()
	s.p.chunksSent++
	s.p.mu.Unlock()
	if last {
		s.p.advance(s.total)
	}
	return c, nil
}

// Close implements providers.RichStream. It is idempotent and unblocks a
// concurrent Recv.
func (s *richStream) Close() error {
	s.mu.Lock()
	s.closed = true
	s.endLocked(providers.ErrStreamClosed)
	s.mu.Unlock()
	s.closeO.Do(func() { close(s.done) })
	return nil
}
