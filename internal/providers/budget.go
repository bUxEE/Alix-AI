package providers

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Budget caps what a single request may cost the runtime (master plan 17.2:
// "capped prompt, output and tool-schema tokens", "reject or queue work before
// memory exhaustion"). It is enforced by the With*Budget decorators before the
// runtime is called, so an oversize request never reaches it.
//
// Every limit must be positive: there is no "unlimited", and a zero Budget is
// rejected by Validate rather than silently meaning "no cap".
type Budget struct {
	// MaxPromptTokens caps the estimated input of one call: for chat every
	// message plus the output constraint (a large schema or grammar costs the
	// runtime memory and time); for embedding all inputs together; for
	// reranking the query plus all documents.
	MaxPromptTokens int
	// MaxOutputTokens caps generated tokens. A chat request with MaxTokens 0
	// is given this cap; one asking for more is rejected, not silently
	// clamped, because the caller's own budget arithmetic is then wrong.
	MaxOutputTokens int
	// MaxBatchItems caps the inputs of an embed call and the documents of a
	// rerank call.
	MaxBatchItems int
	// Timeout bounds one call (a whole stream, for ChatStream). It is applied
	// on top of any shorter deadline the caller's context already has.
	Timeout time.Duration
	// Estimator counts tokens. Nil uses EstimateTokens.
	Estimator TokenEstimator

	// The fields below apply to MultimodalGenerator requests (rich.go,
	// rich_budget.go). Unlike the limits above a zero value means the package
	// default, never "unlimited", so a Budget written before they existed
	// stays valid.

	// MediaPartToks is the estimated prompt cost of one image, file or audio
	// reference. Zero means DefaultMediaPartToks.
	MediaPartToks int
	// MaxMediaParts caps the media references of one request. Zero means
	// DefaultMaxMediaParts.
	MaxMediaParts int
	// MaxOutputBytes caps the media bytes of one reply. Zero means
	// DefaultMaxOutputBytes.
	MaxOutputBytes int
}

// TokenEstimator returns an estimate of the tokens in s. It should err on the
// high side: an underestimate lets an oversize request through.
type TokenEstimator func(s string) int

// perMessageOverhead is the estimated cost of a message's role and framing.
const perMessageOverhead = 4

// EstimateTokens is the default estimator: one token per three bytes, rounded
// up. English and Italian average about four bytes per token and CJK about
// three (one rune), so this deliberately overestimates, which fails closed.
// Replace it through Budget.Estimator once a real tokenizer exists.
func EstimateTokens(s string) int { return (len(s) + 2) / 3 }

// DefaultBudget returns conservative limits suited to the Lite profile.
func DefaultBudget() Budget {
	return Budget{
		MaxPromptTokens: 4096,
		MaxOutputTokens: 1024,
		MaxBatchItems:   64,
		Timeout:         60 * time.Second,
	}
}

// Validate rejects a budget with a non-positive limit.
func (b Budget) Validate() error {
	switch {
	case b.MaxPromptTokens <= 0:
		return invalidRequest("budget: MaxPromptTokens must be positive")
	case b.MaxOutputTokens <= 0:
		return invalidRequest("budget: MaxOutputTokens must be positive")
	case b.MaxBatchItems <= 0:
		return invalidRequest("budget: MaxBatchItems must be positive")
	case b.Timeout <= 0:
		return invalidRequest("budget: Timeout must be positive")
	}
	return nil
}

func (b Budget) estimate(s string) int {
	if b.Estimator != nil {
		if n := b.Estimator(s); n > 0 {
			return n
		}
		if s == "" {
			return 0
		}
		return 1
	}
	return EstimateTokens(s)
}

// EstimateChat returns the estimated prompt size of req: message contents and
// framing, the schema or grammar, and stop sequences.
func (b Budget) EstimateChat(req ChatRequest) int {
	n := 0
	for _, m := range req.Messages {
		n += b.estimate(m.Content) + perMessageOverhead
	}
	n += b.estimate(string(req.Output.JSONSchema)) + b.estimate(req.Output.Grammar)
	for _, s := range req.Stop {
		n += b.estimate(s)
	}
	return n
}

// budgetError is the typed rejection. validation_failed, not
// resource_exhausted: the latter is retryable, and the identical oversize
// request can never succeed, so a retry loop would spin forever.
func budgetError(which string, limit, got int) error {
	return contracts.Newf(contracts.ErrValidationFailed, "providers: request exceeds budget %s (%d > %d)", which, got, limit).
		WithDetails("budget", which).
		WithDetails("limit", limit).
		WithDetails("requested", got)
}

// CheckChat validates req and enforces the prompt and output caps. It returns
// a copy with MaxTokens defaulted to the output cap. Nothing is sent anywhere.
func (b Budget) CheckChat(req ChatRequest) (ChatRequest, error) {
	if err := b.Validate(); err != nil {
		return ChatRequest{}, err
	}
	if err := req.Validate(); err != nil {
		return ChatRequest{}, err
	}
	if got := b.EstimateChat(req); got > b.MaxPromptTokens {
		return ChatRequest{}, budgetError("max_prompt_tokens", b.MaxPromptTokens, got)
	}
	req = req.Clone()
	switch {
	case req.MaxTokens == 0:
		req.MaxTokens = b.MaxOutputTokens
	case req.MaxTokens > b.MaxOutputTokens:
		return ChatRequest{}, budgetError("max_output_tokens", b.MaxOutputTokens, req.MaxTokens)
	}
	return req, nil
}

// CheckEmbed validates req and enforces the batch and input caps.
func (b Budget) CheckEmbed(req EmbedRequest) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if err := req.Validate(); err != nil {
		return err
	}
	if len(req.Inputs) > b.MaxBatchItems {
		return budgetError("max_batch_items", b.MaxBatchItems, len(req.Inputs))
	}
	n := 0
	for _, in := range req.Inputs {
		n += b.estimate(in)
	}
	if n > b.MaxPromptTokens {
		return budgetError("max_prompt_tokens", b.MaxPromptTokens, n)
	}
	return nil
}

// CheckRerank validates req and enforces the batch and input caps.
func (b Budget) CheckRerank(req RerankRequest) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if err := req.Validate(); err != nil {
		return err
	}
	if len(req.Documents) > b.MaxBatchItems {
		return budgetError("max_batch_items", b.MaxBatchItems, len(req.Documents))
	}
	n := b.estimate(req.Query)
	for _, d := range req.Documents {
		n += b.estimate(d)
	}
	if n > b.MaxPromptTokens {
		return budgetError("max_prompt_tokens", b.MaxPromptTokens, n)
	}
	return nil
}

// WithBudget wraps inner so every call is checked against b before the
// runtime is called: scope, request shape, prompt and output caps. It then
// applies b.Timeout, maps a deadline to timeout, and verifies that a
// JSON-Schema-constrained result carries valid JSON (schema_invalid
// otherwise), so a success-looking result never holds unparseable output.
func WithBudget(inner TextGenerator, b Budget) (TextGenerator, error) {
	if inner == nil {
		return nil, invalidRequest("budget: nil text generator")
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return &budgetedText{inner: inner, b: b}, nil
}

type budgetedText struct {
	inner TextGenerator
	b     Budget
}

func (g *budgetedText) prepare(scope contracts.Scope, req ChatRequest) (ChatRequest, error) {
	if err := scope.Validate(); err != nil {
		return ChatRequest{}, err
	}
	return g.b.CheckChat(req)
}

func (g *budgetedText) Chat(ctx context.Context, scope contracts.Scope, req ChatRequest) (ChatResult, error) {
	req, err := g.prepare(scope, req)
	if err != nil {
		return ChatResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, g.b.Timeout)
	defer cancel()
	res, err := g.inner.Chat(ctx, scope, req)
	if err != nil {
		return ChatResult{}, ContextError(err)
	}
	if req.Output.Kind() == ConstraintJSONSchema {
		j, perr := ParseJSONOutput(res.Text)
		if perr != nil {
			return ChatResult{}, perr
		}
		res.JSON = j
	}
	return res, nil
}

func (g *budgetedText) ChatStream(ctx context.Context, scope contracts.Scope, req ChatRequest) (ChatStream, error) {
	req, err := g.prepare(scope, req)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, g.b.Timeout)
	s, err := g.inner.ChatStream(ctx, scope, req)
	if err != nil {
		cancel()
		return nil, ContextError(err)
	}
	return &budgetedStream{inner: s, cancel: cancel}, nil
}

// budgetedStream keeps the timeout context alive for the stream's lifetime and
// releases it when the stream ends or is closed.
type budgetedStream struct {
	inner  ChatStream
	cancel context.CancelFunc
	once   sync.Once
}

func (s *budgetedStream) release() { s.once.Do(s.cancel) }

func (s *budgetedStream) Recv() (ChatChunk, error) {
	c, err := s.inner.Recv()
	if err != nil {
		s.release()
		if err == io.EOF {
			return c, err
		}
		return ChatChunk{}, ContextError(err)
	}
	return c, nil
}

func (s *budgetedStream) Close() error {
	err := s.inner.Close()
	s.release()
	return err
}

// WithEmbedBudget wraps inner with request checks, the timeout and a result
// check (one finite, equally sized vector per input).
func WithEmbedBudget(inner Embedder, b Budget) (Embedder, error) {
	if inner == nil {
		return nil, invalidRequest("budget: nil embedder")
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return &budgetedEmbedder{inner: inner, b: b}, nil
}

type budgetedEmbedder struct {
	inner Embedder
	b     Budget
}

func (e *budgetedEmbedder) Embed(ctx context.Context, scope contracts.Scope, req EmbedRequest) (EmbedResult, error) {
	if err := scope.Validate(); err != nil {
		return EmbedResult{}, err
	}
	if err := e.b.CheckEmbed(req); err != nil {
		return EmbedResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, e.b.Timeout)
	defer cancel()
	res, err := e.inner.Embed(ctx, scope, req)
	if err != nil {
		return EmbedResult{}, ContextError(err)
	}
	if err := res.CheckResult(req); err != nil {
		return EmbedResult{}, err
	}
	return res, nil
}

// WithRerankBudget wraps inner with request checks, the timeout and result
// normalisation (valid unique indexes, finite scores, best first, TopN).
func WithRerankBudget(inner Reranker, b Budget) (Reranker, error) {
	if inner == nil {
		return nil, invalidRequest("budget: nil reranker")
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return &budgetedReranker{inner: inner, b: b}, nil
}

type budgetedReranker struct {
	inner Reranker
	b     Budget
}

func (r *budgetedReranker) Rerank(ctx context.Context, scope contracts.Scope, req RerankRequest) (RerankResult, error) {
	if err := scope.Validate(); err != nil {
		return RerankResult{}, err
	}
	if err := r.b.CheckRerank(req); err != nil {
		return RerankResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.b.Timeout)
	defer cancel()
	res, err := r.inner.Rerank(ctx, scope, req)
	if err != nil {
		return RerankResult{}, ContextError(err)
	}
	return res.Normalize(req)
}

// IsBudgetViolation reports whether err is a budget rejection and, if so,
// which budget ("max_prompt_tokens", "max_output_tokens", "max_batch_items").
func IsBudgetViolation(err error) (which string, ok bool) {
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce.Code != contracts.ErrValidationFailed {
		return "", false
	}
	s, ok := ce.Details["budget"].(string)
	return s, ok
}
