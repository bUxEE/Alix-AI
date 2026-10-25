// Package fake is the deterministic AI runtime used by almost every AlixAI
// test. One *Provider implements providers.TextGenerator, providers.Embedder,
// providers.Reranker and providers.AIRuntimeProvider, so a single value can
// be registered for every role.
//
//	f := fake.New()
//	f.On(fake.Contains("book")).Reply(`{"capability_id":"book"}`)
//	f.FailNext(fake.FailUnavailable)
//	... exercise the code under test ...
//	for _, p := range f.Prompts() { ... } // everything that reached the model
//
// It behaves like a real adapter where that matters: it validates the scope
// and the request (fail closed), returns schema_invalid when a constrained
// request gets non-JSON, never returns partial output as a success, and
// honours context cancellation mid-stream. A call that no rule matches is an
// error, never a default reply, so a test cannot pass by accident.
//
// Rules are checked newest first: a later On overrides an earlier one, and a
// rule with Times(n) steps aside once it has matched n times.
package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// FailKind is an injectable failure.
type FailKind string

const (
	// FailTimeout fails at once with timeout (wrapping
	// context.DeadlineExceeded). It does not sleep; use Rule.StallAfter to
	// make a call hang until its context ends.
	FailTimeout FailKind = "timeout"
	// FailMalformedJSON makes the runtime answer with truncated JSON
	// (MalformedJSON). A JSON-Schema-constrained request fails with
	// schema_invalid; an unconstrained one receives the text as is.
	FailMalformedJSON FailKind = "malformed_json"
	// FailPartialStream cuts a stream after some chunks with unavailable. On
	// Chat, Embed and Rerank it fails with unavailable straight away.
	FailPartialStream FailKind = "partial_stream"
	// FailUnavailable fails at once with unavailable (connection refused).
	FailUnavailable FailKind = "unavailable"
	// FailResourceExhausted fails at once with resource_exhausted (the model
	// could not be loaded).
	FailResourceExhausted FailKind = "resource_exhausted"
	// FailRateLimited fails at once with rate_limited carrying a retry-after:
	// Rule.RetryAfter on a scripted rule, DefaultRetryAfter on a queued
	// failure.
	FailRateLimited FailKind = "rate_limited"
	// FailMalformedToolCall makes a rich call deliver a tool-call proposal
	// that names a tool nobody offered and has unparseable arguments, as a
	// misbehaving provider would. Callers (and the budget decorator) must
	// reject it. On a plain call it fails with schema_invalid.
	FailMalformedToolCall FailKind = "malformed_tool_call"
)

// DefaultRetryAfter is the retry-after FailRateLimited reports when no rule
// supplies one.
const DefaultRetryAfter = 2 * time.Second

func (k FailKind) valid() bool {
	switch k {
	case FailTimeout, FailMalformedJSON, FailPartialStream, FailUnavailable, FailResourceExhausted, FailRateLimited, FailMalformedToolCall:
		return true
	}
	return false
}

// MalformedJSON is the text FailMalformedJSON delivers.
const MalformedJSON = `{"truncated": [1, 2,`

// partialFiller is streamed by FailPartialStream when no rule supplies text.
const partialFiller = "partial output that must never be acted on"

// Op is the kind of call a Prompt records.
type Op string

const (
	OpChat       Op = "chat"
	OpChatStream Op = "chat_stream"
	OpEmbed      Op = "embed"
	OpRerank     Op = "rerank"
	// OpGenerate and OpGenerateStream are MultimodalGenerator calls.
	OpGenerate       Op = "generate"
	OpGenerateStream Op = "generate_stream"
)

// Prompt is one call as it reached the "model": the scope it ran under and
// everything sent. Only calls that passed scope and request validation are
// recorded; a rejected call never reached the runtime.
type Prompt struct {
	Seq    int
	Op     Op
	Scope  contracts.Scope
	Chat   providers.ChatRequest   // OpChat, OpChatStream
	Embed  providers.EmbedRequest  // OpEmbed
	Rerank providers.RerankRequest // OpRerank
	// Rich is the request as sent (OpGenerate, OpGenerateStream); Chat then
	// holds its text view (media shown as "[kind ref]" placeholders).
	Rich providers.RichRequest
}

// Texts returns every string the call sent to the model: message contents,
// the output schema or grammar, stop sequences, embedding inputs, and the
// rerank query and documents.
func (p Prompt) Texts() []string {
	var out []string
	for _, m := range p.Chat.Messages {
		out = append(out, m.Content)
	}
	if len(p.Chat.Output.JSONSchema) > 0 {
		out = append(out, string(p.Chat.Output.JSONSchema))
	}
	if p.Chat.Output.Grammar != "" {
		out = append(out, p.Chat.Output.Grammar)
	}
	out = append(out, p.Chat.Stop...)
	out = append(out, p.Embed.Inputs...)
	if p.Rerank.Query != "" {
		out = append(out, p.Rerank.Query)
	}
	out = append(out, p.Rerank.Documents...)
	for _, t := range p.Rich.Tools {
		out = append(out, t.Name, t.Description, string(t.Parameters))
	}
	return out
}

// Contains reports whether any text sent contains sub.
func (p Prompt) Contains(sub string) bool {
	for _, t := range p.Texts() {
		if strings.Contains(t, sub) {
			return true
		}
	}
	return false
}

func (p Prompt) clone() Prompt {
	p.Scope = p.Scope.Clone()
	p.Chat = p.Chat.Clone()
	p.Embed.Inputs = append([]string(nil), p.Embed.Inputs...)
	p.Rerank.Documents = append([]string(nil), p.Rerank.Documents...)
	p.Rich = p.Rich.Clone()
	return p
}

// Option configures New.
type Option func(*Provider)

// WithName sets the name reported through AIRuntimeProvider (default "fake").
func WithName(name string) Option { return func(p *Provider) { p.name = name } }

// WithModel sets the model id reported in results (default "fake-model").
func WithModel(model string) Option { return func(p *Provider) { p.model = model } }

// WithChunkSize sets how many runes each streamed delta holds (default 12).
func WithChunkSize(runes int) Option {
	return func(p *Provider) {
		if runes > 0 {
			p.chunkSize = runes
		}
	}
}

// WithEmbedDim sets the dimensionality of default embeddings.
func WithEmbedDim(dim int) Option {
	return func(p *Provider) {
		if dim > 0 {
			p.embedDim = dim
		}
	}
}

// WithClock makes every call advance clk by the call's scripted total
// latency, so tests that drive a clock.Fake see time pass without sleeping.
func WithClock(clk *clock.Fake) Option { return func(p *Provider) { p.clk = clk } }

// WithEmbedFunc replaces the default embeddings.
func WithEmbedFunc(f func(providers.EmbedRequest) ([][]float32, error)) Option {
	return func(p *Provider) { p.embedFn = f }
}

// WithRerankFunc replaces the default scoring. The hits it returns are
// normalised like any runtime's (ordered best first, TopN applied).
func WithRerankFunc(f func(providers.RerankRequest) ([]providers.RerankHit, error)) Option {
	return func(p *Provider) { p.rerankFn = f }
}

// Provider is the deterministic fake runtime. It is safe for concurrent use.
type Provider struct {
	name      string
	model     string
	chunkSize int
	embedDim  int
	clk       *clock.Fake
	embedFn   func(providers.EmbedRequest) ([][]float32, error)
	rerankFn  func(providers.RerankRequest) ([]providers.RerankHit, error)
	caps      providers.ModelCapabilities

	mu          sync.Mutex
	rules       []*Rule
	fails       []FailKind
	prompts     []Prompt
	seq         int
	health      providers.HealthReport
	openStreams int
	chunksSent  int
}

var (
	_ providers.TextGenerator     = (*Provider)(nil)
	_ providers.Embedder          = (*Provider)(nil)
	_ providers.Reranker          = (*Provider)(nil)
	_ providers.AIRuntimeProvider = (*Provider)(nil)

	_ providers.MultimodalGenerator = (*Provider)(nil)
	_ providers.CapabilityProvider  = (*Provider)(nil)
)

// New returns a fake with no rules.
func New(opts ...Option) *Provider {
	p := &Provider{
		name: "fake", model: "fake-model", chunkSize: 12, embedDim: DefaultEmbedDim,
		health: providers.HealthReport{Status: providers.HealthOK, Version: "fake"},
	}
	p.caps = DefaultCapabilities(p.name, p.model)
	for _, o := range opts {
		o(p)
	}
	return p
}

// Rule is one scripted behaviour, created by Provider.On. Its methods return
// the rule so calls chain; configure it before the calls it should answer.
type Rule struct {
	p *Provider
	m Matcher

	replies  []string
	replyFn  func(Call) (string, error)
	failKind FailKind
	times    int
	hits     int
	usage    *providers.Usage
	timings  providers.Timings
	stall    int
	hasReply bool

	// rich scripting (see rich.go)
	parts      []providers.ContentPart
	toolCalls  []providers.ToolCallProposal
	cost       *providers.CostUsage
	rate       *providers.RateLimitInfo
	retryAfter time.Duration
}

// On scripts a rule for calls the matcher accepts (nil matches everything).
// Rules are checked newest first.
func (p *Provider) On(m Matcher) *Rule {
	if m == nil {
		m = Any()
	}
	r := &Rule{p: p, m: m, stall: -1}
	p.mu.Lock()
	p.rules = append(p.rules, r)
	p.mu.Unlock()
	return r
}

// Reply sets the text the runtime answers with. For a JSON-constrained request
// pass the JSON document.
func (r *Rule) Reply(text string) *Rule {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	r.replies, r.replyFn, r.hasReply = []string{text}, nil, true
	return r
}

// ReplyJSON is Reply with v marshalled to JSON. It panics if v cannot be
// marshalled: that is a bug in the test.
func (r *Rule) ReplyJSON(v any) *Rule {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("fake: ReplyJSON: %v", err))
	}
	return r.Reply(string(b))
}

// ReplySeq answers successive matches with successive texts; the last repeats.
func (r *Rule) ReplySeq(texts ...string) *Rule {
	if len(texts) == 0 {
		panic("fake: ReplySeq needs at least one reply")
	}
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	r.replies, r.replyFn, r.hasReply = append([]string(nil), texts...), nil, true
	return r
}

// ReplyFunc computes the reply from the call. Its error is returned as is.
func (r *Rule) ReplyFunc(f func(Call) (string, error)) *Rule {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	r.replies, r.replyFn, r.hasReply = nil, f, true
	return r
}

// Fail makes every match fail with kind.
func (r *Rule) Fail(kind FailKind) *Rule {
	if !kind.valid() {
		panic(fmt.Sprintf("fake: unknown FailKind %q", string(kind)))
	}
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	r.failKind = kind
	return r
}

// Times limits the rule to n matches, after which it steps aside (n <= 0
// removes the limit).
func (r *Rule) Times(n int) *Rule {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	r.times = n
	return r
}

// Usage sets the reported token usage (default: estimated from the text).
func (r *Rule) Usage(in, out int) *Rule {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	r.usage = &providers.Usage{InputTokens: in, OutputTokens: out}
	return r
}

// Timings sets the reported latencies (default zero). With WithClock the
// provider's clock advances by total on each match.
func (r *Rule) Timings(timeToFirstToken, total time.Duration) *Rule {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	r.timings = providers.Timings{TimeToFirstToken: timeToFirstToken, Total: total}
	return r
}

// StallAfter simulates a hung runtime: a stream delivers n chunks and then
// blocks until its context ends or it is closed; Chat blocks until its
// context ends.
func (r *Rule) StallAfter(n int) *Rule {
	if n < 0 {
		panic("fake: StallAfter needs n >= 0")
	}
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	r.stall = n
	return r
}

// Hits returns how many calls the rule has answered.
func (r *Rule) Hits() int {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	return r.hits
}

// FailNext makes the next call (of any kind) fail with kind. Calls queue: two
// FailNext calls fail the next two calls, in order.
func (p *Provider) FailNext(kind FailKind) { p.FailTimes(kind, 1) }

// FailTimes queues n failures of kind.
func (p *Provider) FailTimes(kind FailKind, n int) {
	if !kind.valid() {
		panic(fmt.Sprintf("fake: unknown FailKind %q", string(kind)))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := 0; i < n; i++ {
		p.fails = append(p.fails, kind)
	}
}

// Prompts returns every call that reached the runtime, oldest first, as deep
// copies: nothing the caller does to them affects the record.
func (p *Provider) Prompts() []Prompt {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Prompt, len(p.prompts))
	for i, pr := range p.prompts {
		out[i] = pr.clone()
	}
	return out
}

// Containing returns the calls that sent sub in any text.
func (p *Provider) Containing(sub string) []Prompt {
	var out []Prompt
	for _, pr := range p.Prompts() {
		if pr.Contains(sub) {
			out = append(out, pr)
		}
	}
	return out
}

// Calls returns the number of calls that reached the runtime.
func (p *Provider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.prompts)
}

// ClearPrompts forgets the recorded calls (rules and queued failures stay).
func (p *Provider) ClearPrompts() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prompts = nil
}

// Reset removes rules, queued failures and recorded calls.
func (p *Provider) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules, p.fails, p.prompts = nil, nil, nil
}

// OpenStreams returns the number of streams started and not yet finished
// (completed, failed, cancelled or closed). A test that leaves it above zero
// leaked a stream.
func (p *Provider) OpenStreams() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.openStreams
}

// ChunksDelivered returns the number of stream chunks handed to callers.
func (p *Provider) ChunksDelivered() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.chunksSent
}

// Name implements providers.AIRuntimeProvider.
func (p *Provider) Name() string { return p.name }

// Health implements providers.AIRuntimeProvider; it returns what SetHealth
// last set (ok by default).
func (p *Provider) Health(ctx context.Context) (providers.HealthReport, error) {
	if err := ctx.Err(); err != nil {
		return providers.HealthReport{}, providers.ContextError(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.health, nil
}

// SetHealth sets what Health reports.
func (p *Provider) SetHealth(status providers.Health, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.health = providers.HealthReport{Status: status, Reason: reason, Version: "fake"}
}

// plan is what one call resolved to.
type plan struct {
	seq     int
	fail    FailKind
	text    string
	hasText bool
	replyFn func(Call) (string, error)
	usage   *providers.Usage
	timings providers.Timings
	stall   int
	noRule  bool

	parts      []providers.ContentPart
	toolCalls  []providers.ToolCallProposal
	cost       *providers.CostUsage
	rate       *providers.RateLimitInfo
	retryAfter time.Duration
}

// begin validates, records and resolves a chat call.
func (p *Provider) begin(op Op, scope contracts.Scope, req providers.ChatRequest, rich *providers.RichRequest) (plan, error) {
	if err := scope.Validate(); err != nil {
		return plan{}, err
	}
	if err := req.Validate(); err != nil {
		return plan{}, err
	}
	call := Call{Scope: scope, Request: req}
	var rec Prompt
	if rich != nil {
		rc := rich.Clone()
		call.Rich = &rc
		rec.Rich = rc
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	pl := plan{seq: p.seq, stall: -1}
	rec.Seq, rec.Op, rec.Scope, rec.Chat = p.seq, op, scope, req
	p.prompts = append(p.prompts, rec.clone())

	queued := len(p.fails) > 0
	if queued {
		pl.fail, p.fails = p.fails[0], p.fails[1:]
	}
	// A queued failure does not consume a rule's matches, but the kinds that
	// stream or return text still need the text a rule would have supplied.
	needRule := !queued || pl.fail == FailMalformedJSON || pl.fail == FailPartialStream
	var rule *Rule
	if needRule {
		for i := len(p.rules) - 1; i >= 0; i-- {
			r := p.rules[i]
			if r.times > 0 && r.hits >= r.times {
				continue
			}
			if r.m(call) {
				rule = r
				break
			}
		}
	}
	if rule == nil {
		pl.noRule = true
		return pl, nil
	}
	if !queued {
		rule.hits++
		pl.fail = rule.failKind
	}
	pl.usage, pl.timings, pl.stall = rule.usage, rule.timings, rule.stall
	pl.parts, pl.toolCalls, pl.cost, pl.rate, pl.retryAfter = rule.parts, rule.toolCalls, rule.cost, rule.rate, rule.retryAfter
	switch {
	case rule.replyFn != nil:
		pl.replyFn = rule.replyFn
	case len(rule.replies) > 0:
		i := rule.hits - 1
		if queued || i < 0 {
			i = 0
		}
		if i >= len(rule.replies) {
			i = len(rule.replies) - 1
		}
		pl.text, pl.hasText = rule.replies[i], true
	}
	return pl, nil
}

func injected(kind FailKind) error {
	switch kind {
	case FailTimeout:
		return contracts.Wrap(contracts.ErrTimeout, context.DeadlineExceeded, "fake: injected timeout")
	case FailUnavailable:
		return contracts.New(contracts.ErrUnavailable, "fake: injected runtime unavailable")
	case FailResourceExhausted:
		return contracts.New(contracts.ErrResourceExhausted, "fake: injected resource exhaustion")
	case FailRateLimited:
		return providers.RateLimitError(providers.RateLimitInfo{RetryAfter: DefaultRetryAfter, Scope: "requests"})
	case FailMalformedToolCall:
		return contracts.New(contracts.ErrSchemaInvalid, "fake: injected malformed tool call")
	case FailPartialStream:
		return contracts.New(contracts.ErrUnavailable, "fake: injected connection drop mid-response")
	case FailMalformedJSON:
		return contracts.New(contracts.ErrSchemaInvalid, "fake: injected malformed runtime response")
	}
	return nil
}

func (p *Provider) noScript(req providers.ChatRequest) error {
	last := ""
	if n := len(req.Messages); n > 0 {
		last = req.Messages[n-1].Content
	}
	if len(last) > 60 {
		last = last[:60] + "..."
	}
	return contracts.Newf(contracts.ErrInternal, "fake: no scripted reply matches this request (last message %q); add a rule with On(...)", last)
}

// resolveText returns the text the runtime answers with and the plan's
// failure (if the failure should preempt any answer).
func (p *Provider) resolveText(pl plan, call Call) (string, error) {
	switch pl.fail {
	case FailTimeout, FailUnavailable, FailResourceExhausted, FailRateLimited, FailMalformedToolCall:
		return "", injected(pl.fail)
	case FailMalformedJSON:
		return MalformedJSON, nil
	}
	if pl.noRule && pl.fail != FailPartialStream {
		return "", p.noScript(call.Request)
	}
	switch {
	case pl.replyFn != nil:
		return pl.replyFn(call)
	case pl.hasText:
		return pl.text, nil
	case pl.fail == FailPartialStream:
		return partialFiller, nil
	case pl.stall >= 0:
		return "", nil // a rule that only simulates a hung runtime
	}
	return "", contracts.New(contracts.ErrInternal, "fake: the matching rule has no reply or failure; call Reply, ReplyJSON, ReplySeq, ReplyFunc or Fail")
}

func (p *Provider) usageFor(pl plan, req providers.ChatRequest, text string) providers.Usage {
	if pl.usage != nil {
		return *pl.usage
	}
	return providers.Usage{
		InputTokens:  providers.Budget{}.EstimateChat(req),
		OutputTokens: providers.EstimateTokens(text),
	}
}

func (p *Provider) traceID(seq int) string { return fmt.Sprintf("fake-%06d", seq) }

func (p *Provider) advance(d time.Duration) {
	if p.clk != nil && d > 0 {
		p.clk.Advance(d)
	}
}

// Chat implements providers.TextGenerator.
func (p *Provider) Chat(ctx context.Context, scope contracts.Scope, req providers.ChatRequest) (providers.ChatResult, error) {
	if err := ctx.Err(); err != nil {
		return providers.ChatResult{}, providers.ContextError(err)
	}
	pl, err := p.begin(OpChat, scope, req, nil)
	if err != nil {
		return providers.ChatResult{}, err
	}
	text, err := p.resolveText(pl, Call{Scope: scope, Request: req.Clone()})
	if err != nil {
		return providers.ChatResult{}, err
	}
	if pl.fail == FailPartialStream {
		return providers.ChatResult{}, injected(FailPartialStream)
	}
	if pl.stall >= 0 {
		<-ctx.Done()
		return providers.ChatResult{}, providers.ContextError(ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return providers.ChatResult{}, providers.ContextError(err)
	}
	res := providers.ChatResult{
		Text:         text,
		FinishReason: providers.FinishStop,
		Usage:        p.usageFor(pl, req, text),
		Timings:      pl.timings,
		TraceID:      p.traceID(pl.seq),
		Model:        p.model,
	}
	if req.Output.Kind() == providers.ConstraintJSONSchema {
		j, err := providers.ParseJSONOutput(text)
		if err != nil {
			return providers.ChatResult{}, err
		}
		res.JSON = j
	}
	p.advance(pl.timings.Total)
	return res, nil
}

// ChatStream implements providers.TextGenerator.
func (p *Provider) ChatStream(ctx context.Context, scope contracts.Scope, req providers.ChatRequest) (providers.ChatStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, providers.ContextError(err)
	}
	pl, err := p.begin(OpChatStream, scope, req, nil)
	if err != nil {
		return nil, err
	}
	text, err := p.resolveText(pl, Call{Scope: scope, Request: req.Clone()})
	if err != nil {
		return nil, err
	}
	deltas := splitRunes(text, p.chunkSize)
	chunks := make([]providers.ChatChunk, 0, len(deltas)+1)
	for _, d := range deltas {
		chunks = append(chunks, providers.ChatChunk{Delta: d})
	}
	chunks = append(chunks, providers.ChatChunk{
		Done:         true,
		FinishReason: providers.FinishStop,
		Usage:        p.usageFor(pl, req, text),
		Timings:      pl.timings,
		TraceID:      p.traceID(pl.seq),
		Model:        p.model,
	})
	s := &stream{p: p, ctx: ctx, chunks: chunks, failAfter: -1, stallAfter: pl.stall, done: make(chan struct{}), total: pl.timings.Total}
	if pl.fail == FailPartialStream {
		s.failAfter = len(deltas) / 2
		if s.failAfter < 1 {
			s.failAfter = 1
		}
		if s.failAfter > len(deltas) {
			s.failAfter = len(deltas)
		}
		s.failErr = injected(FailPartialStream)
	}
	p.mu.Lock()
	p.openStreams++
	p.mu.Unlock()
	return s, nil
}

func splitRunes(s string, n int) []string {
	if s == "" {
		return nil
	}
	r := []rune(s)
	var out []string
	for len(r) > 0 {
		k := n
		if k > len(r) {
			k = len(r)
		}
		out = append(out, string(r[:k]))
		r = r[k:]
	}
	return out
}

// beginAux validates, records and resolves an embed or rerank call: only the
// queued failure applies (rules script chat).
func (p *Provider) beginAux(op Op, scope contracts.Scope, pr Prompt) (FailKind, int, error) {
	if err := scope.Validate(); err != nil {
		return "", 0, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	pr.Seq, pr.Op, pr.Scope = p.seq, op, scope
	p.prompts = append(p.prompts, pr.clone())
	var kind FailKind
	if len(p.fails) > 0 {
		kind, p.fails = p.fails[0], p.fails[1:]
	}
	return kind, p.seq, nil
}

// Embed implements providers.Embedder. The default vectors are EmbedVector;
// FailNext applies; scripted chat rules do not.
func (p *Provider) Embed(ctx context.Context, scope contracts.Scope, req providers.EmbedRequest) (providers.EmbedResult, error) {
	if err := ctx.Err(); err != nil {
		return providers.EmbedResult{}, providers.ContextError(err)
	}
	if err := scope.Validate(); err != nil {
		return providers.EmbedResult{}, err
	}
	if err := req.Validate(); err != nil {
		return providers.EmbedResult{}, err
	}
	kind, seq, err := p.beginAux(OpEmbed, scope, Prompt{Embed: req})
	if err != nil {
		return providers.EmbedResult{}, err
	}
	if kind != "" {
		return providers.EmbedResult{}, injected(kind)
	}
	var vecs [][]float32
	if p.embedFn != nil {
		if vecs, err = p.embedFn(req); err != nil {
			return providers.EmbedResult{}, err
		}
	} else {
		for _, in := range req.Inputs {
			vecs = append(vecs, EmbedVector(in, p.embedDim))
		}
	}
	tok := 0
	for _, in := range req.Inputs {
		tok += providers.EstimateTokens(in)
	}
	res := providers.EmbedResult{
		Vectors: vecs, Usage: providers.Usage{InputTokens: tok},
		TraceID: p.traceID(seq), Model: p.model,
	}
	if len(vecs) > 0 {
		res.Dimensions = len(vecs[0])
	}
	return res, nil
}

// Rerank implements providers.Reranker. The default score is RerankScore;
// FailNext applies; scripted chat rules do not.
func (p *Provider) Rerank(ctx context.Context, scope contracts.Scope, req providers.RerankRequest) (providers.RerankResult, error) {
	if err := ctx.Err(); err != nil {
		return providers.RerankResult{}, providers.ContextError(err)
	}
	if err := scope.Validate(); err != nil {
		return providers.RerankResult{}, err
	}
	if err := req.Validate(); err != nil {
		return providers.RerankResult{}, err
	}
	kind, seq, err := p.beginAux(OpRerank, scope, Prompt{Rerank: req})
	if err != nil {
		return providers.RerankResult{}, err
	}
	if kind != "" {
		return providers.RerankResult{}, injected(kind)
	}
	var hits []providers.RerankHit
	if p.rerankFn != nil {
		if hits, err = p.rerankFn(req); err != nil {
			return providers.RerankResult{}, err
		}
	} else {
		for i, d := range req.Documents {
			hits = append(hits, providers.RerankHit{Index: i, Score: RerankScore(req.Query, d)})
		}
		sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	}
	tok := providers.EstimateTokens(req.Query)
	for _, d := range req.Documents {
		tok += providers.EstimateTokens(d)
	}
	res := providers.RerankResult{Hits: hits, Usage: providers.Usage{InputTokens: tok}, TraceID: p.traceID(seq), Model: p.model}
	return res.Normalize(req)
}
