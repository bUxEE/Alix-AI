package providers

import (
	"context"
	"io"
	"sync"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Defaults for the optional Budget fields that cover rich requests.
const (
	DefaultMediaPartToks  = 1024
	DefaultMaxMediaParts  = 8
	DefaultMaxOutputBytes = 8 << 20
)

func (b Budget) mediaToks() int {
	if b.MediaPartToks > 0 {
		return b.MediaPartToks
	}
	return DefaultMediaPartToks
}

func (b Budget) maxMediaParts() int {
	if b.MaxMediaParts > 0 {
		return b.MaxMediaParts
	}
	return DefaultMaxMediaParts
}

func (b Budget) maxOutputBytes() int {
	if b.MaxOutputBytes > 0 {
		return b.MaxOutputBytes
	}
	return DefaultMaxOutputBytes
}

func mediaParts(req RichRequest) (images, others int) {
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			switch p.Kind {
			case ModImage:
				images++
			case ModFile, ModAudio:
				others++
			}
		}
	}
	return images, others
}

// EstimateRich returns the estimated prompt size of req: message text and
// framing, a fixed conservative cost per media reference (the bytes are not
// visible here), echoed tool-call arguments, the tool specs (name,
// description and schema: master plan 17.2 caps tool-schema size), the output
// constraint and the stop sequences.
func (b Budget) EstimateRich(req RichRequest) int {
	n := 0
	for _, m := range req.Messages {
		n += perMessageOverhead
		for _, p := range m.Parts {
			if p.Kind == ModText {
				n += b.estimate(p.Text)
			} else {
				n += b.mediaToks()
			}
		}
		for _, c := range m.ToolCalls {
			n += b.estimate(c.Name) + b.estimate(string(c.Arguments))
		}
	}
	for _, t := range req.Tools {
		n += b.estimate(toolSpecText(t))
	}
	n += b.estimate(string(req.Output.JSONSchema)) + b.estimate(req.Output.Grammar)
	for _, s := range req.Stop {
		n += b.estimate(s)
	}
	return n
}

// CheckRich validates req and enforces the media, prompt and output caps. It
// returns a copy with MaxToks defaulted to the output cap. Nothing is sent.
func (b Budget) CheckRich(req RichRequest) (RichRequest, error) {
	if err := b.Validate(); err != nil {
		return RichRequest{}, err
	}
	if err := req.Validate(); err != nil {
		return RichRequest{}, err
	}
	img, other := mediaParts(req)
	if got := img + other; got > b.maxMediaParts() {
		return RichRequest{}, budgetError("max_media_parts", b.maxMediaParts(), got)
	}
	if got := b.EstimateRich(req); got > b.MaxPromptTokens {
		return RichRequest{}, budgetError("max_prompt_tokens", b.MaxPromptTokens, got)
	}
	req = req.Clone()
	switch {
	case req.MaxToks == 0:
		req.MaxToks = b.MaxOutputTokens
	case req.MaxToks > b.MaxOutputTokens:
		return RichRequest{}, budgetError("max_output_tokens", b.MaxOutputTokens, req.MaxToks)
	}
	return req, nil
}

// EstimateRichCost prices the worst case of req against prices: the estimated
// prompt, the full output cap, every image reference and a requested image.
// The figures are integer micro-units rounded up (an upper bound) and
// Estimated is true. A request that uses a unit the table does not price is an
// error: it cannot be budgeted, so it is not sent.
func (b Budget) EstimateRichCost(req RichRequest, prices PriceTable) (CostUsage, error) {
	req, err := b.CheckRich(req)
	if err != nil {
		return CostUsage{}, err
	}
	img, _ := mediaParts(req)
	u := CostUsage{ToksIn: b.EstimateRich(req), ToksOut: req.MaxToks, ImagesIn: img}
	if req.WantsOutput(ModImage) && len(req.OutputModalities) > 0 {
		u.ImagesOut = 1
	}
	return prices.Cost(u)
}

// CheckRichResult checks a reply against the request that produced it and the
// budget: every part is text or an allowed media type the request asked for,
// the media stays under MaxOutputBytes, every tool-call proposal is on the
// request's allowlist with object arguments, usage and rate-limit figures are
// sane, and a JSON Schema constrained reply parses. It returns the reply with
// proposals normalised. A bad reply is schema_invalid, never a success.
func (b Budget) CheckRichResult(req RichRequest, res RichResult) (RichResult, error) {
	total := 0
	for _, p := range res.Parts {
		if err := p.ValidateOutput(); err != nil {
			return RichResult{}, err
		}
		if p.Kind != ModText && !req.WantsOutput(p.Kind) {
			return RichResult{}, schemaInvalid("provider produced a modality that was not requested")
		}
		if total += len(p.Data); total > b.maxOutputBytes() {
			return RichResult{}, schemaInvalid("provider media output exceeds the budget")
		}
	}
	calls, err := ValidateProposals(res.ToolCalls, req.Tools, req.ToolChoice)
	if err != nil {
		return RichResult{}, err
	}
	res.ToolCalls = calls
	if err := res.Usage.Validate(); err != nil {
		return RichResult{}, err
	}
	if res.RateLimit != nil && res.RateLimit.RetryAfter < 0 {
		return RichResult{}, schemaInvalid("negative retry-after")
	}
	if req.Output.Kind() == ConstraintJSONSchema {
		j, err := ParseJSONOutput(res.Text())
		if err != nil {
			return RichResult{}, err
		}
		res.JSON = j
	}
	return res, nil
}

// WithRichBudget wraps inner so every call is checked before the provider is
// called (scope, request shape, media, prompt and output caps, and, when inner
// can describe its model, its capabilities) and every reply after it (see
// CheckRichResult). It applies b.Timeout, maps a deadline to timeout, and
// wraps streams so chunk order, tool proposals and media are enforced on every
// Recv, not only in CollectRich.
func WithRichBudget(inner MultimodalGenerator, b Budget) (MultimodalGenerator, error) {
	if inner == nil {
		return nil, invalidRequest("budget: nil multimodal generator")
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return &budgetedRich{inner: inner, b: b}, nil
}

type budgetedRich struct {
	inner MultimodalGenerator
	b     Budget
}

var _ CapabilityProvider = (*budgetedRich)(nil)

// Capabilities passes through the inner provider's description.
func (g *budgetedRich) Capabilities(ctx context.Context) (ModelCapabilities, error) {
	cp, ok := g.inner.(CapabilityProvider)
	if !ok {
		return ModelCapabilities{}, contracts.New(contracts.ErrUnavailable, "providers: provider cannot describe its model")
	}
	return cp.Capabilities(ctx)
}

func (g *budgetedRich) prepare(ctx context.Context, scope contracts.Scope, req RichRequest, stream bool) (RichRequest, error) {
	if err := scope.Validate(); err != nil {
		return RichRequest{}, err
	}
	req, err := g.b.CheckRich(req)
	if err != nil {
		return RichRequest{}, err
	}
	if cp, ok := g.inner.(CapabilityProvider); ok {
		caps, err := cp.Capabilities(ctx)
		if err != nil {
			return RichRequest{}, ContextError(err)
		}
		if err := caps.Validate(); err != nil {
			return RichRequest{}, err
		}
		if err := caps.Check(req); err != nil {
			return RichRequest{}, err
		}
		if stream && !caps.Streaming {
			return RichRequest{}, UnsupportedError("streaming")
		}
	}
	return req, nil
}

func (g *budgetedRich) Generate(ctx context.Context, scope contracts.Scope, req RichRequest) (RichResult, error) {
	req, err := g.prepare(ctx, scope, req, false)
	if err != nil {
		return RichResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, g.b.Timeout)
	defer cancel()
	res, err := g.inner.Generate(ctx, scope, req)
	if err != nil {
		return RichResult{}, ContextError(err)
	}
	return g.b.CheckRichResult(req, res)
}

func (g *budgetedRich) GenerateStream(ctx context.Context, scope contracts.Scope, req RichRequest) (RichStream, error) {
	req, err := g.prepare(ctx, scope, req, true)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, g.b.Timeout)
	s, err := g.inner.GenerateStream(ctx, scope, req)
	if err != nil {
		cancel()
		return nil, ContextError(err)
	}
	return &budgetedRichStream{
		inner: s, cancel: cancel, b: g.b,
		st: newRichStreamState(req, req.Output.Kind() == ConstraintJSONSchema),
	}, nil
}

// budgetedRichStream keeps the timeout context alive for the stream's
// lifetime, releases it when the stream ends or is closed, and rejects the
// first chunk that breaks the stream rules. After a rejection every later Recv
// returns the same error.
type budgetedRichStream struct {
	inner  RichStream
	cancel context.CancelFunc
	b      Budget
	once   sync.Once

	mu    sync.Mutex
	st    *richStreamState
	media int
	err   error
}

func (s *budgetedRichStream) release() { s.once.Do(s.cancel) }

func (s *budgetedRichStream) fail(err error) (RichChunk, error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
	s.inner.Close()
	s.release()
	return RichChunk{}, err
}

func (s *budgetedRichStream) Recv() (RichChunk, error) {
	s.mu.Lock()
	prev := s.err
	s.mu.Unlock()
	if prev != nil {
		return RichChunk{}, prev
	}
	c, err := s.inner.Recv()
	if err != nil {
		s.release()
		if err == io.EOF {
			s.mu.Lock()
			done := s.st.done
			s.mu.Unlock()
			if !done {
				return RichChunk{}, contracts.New(contracts.ErrUnavailable, "providers: stream ended before completion")
			}
			return c, err
		}
		return RichChunk{}, ContextError(err)
	}
	s.mu.Lock()
	err = s.st.accept(c)
	if err == nil && c.Kind == DeltaPart {
		if s.media += len(c.Part.Data); s.media > s.b.maxOutputBytes() {
			err = schemaInvalid("provider media output exceeds the budget")
		}
	}
	if err == nil && c.Kind == DeltaDone {
		_, err = s.st.finish()
	}
	s.mu.Unlock()
	if err != nil {
		return s.fail(err)
	}
	return c, nil
}

func (s *budgetedRichStream) Close() error {
	err := s.inner.Close()
	s.release()
	return err
}
