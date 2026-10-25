package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// The rich contract EXTENDS the text contract; it does not change it.
// ChatRequest, TextGenerator, Embedder and Reranker keep their exact shape,
// and an adapter that only knows text (adapters/localai) never sees a request
// that carries media or tools: it does not implement MultimodalGenerator, so
// the type system keeps such a request away from it. A text-only RichRequest
// converts back with TextOnly; anything richer fails closed.

// More finish reasons, additive to chat.go.
const (
	// FinishToolCalls: the model stopped to propose tool calls.
	FinishToolCalls FinishReason = "tool_calls"
	// FinishFiltered: the provider withheld output (content filter).
	FinishFiltered FinishReason = "content_filter"
)

// RichMessage is one turn whose content is a list of parts.
type RichMessage struct {
	Role  MessageRole   `json:"role"`
	Parts []ContentPart `json:"parts,omitempty"`
	// Trusted has the meaning it has on Message: content authored by AlixAI
	// itself. A system message must be trusted and text-only.
	Trusted bool `json:"trusted"`
	// ToolCallID (tool role only) names the proposal this result answers. The
	// result is data returned by deterministic code, not an instruction.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ToolCalls (assistant role only) echoes the model's earlier proposals.
	ToolCalls []ToolCallProposal `json:"tool_calls,omitempty"`
}

// RichRequest is one generation call with parts, tools and output modalities.
type RichRequest struct {
	Role     Role             `json:"role,omitempty"`
	Profile  string           `json:"profile,omitempty"`
	Messages []RichMessage    `json:"messages"`
	Output   OutputConstraint `json:"output"`
	// Tools is the allowlist of tools the model may PROPOSE. Empty means none.
	Tools      []ToolSpec `json:"tools,omitempty"`
	ToolChoice ToolChoice `json:"tool_choice,omitempty"`
	// OutputModalities asks for more than text (an image, audio). Empty means
	// text.
	OutputModalities []Modality `json:"output_modalities,omitempty"`
	MaxToks          int        `json:"max_toks,omitempty"`
	Temperature      float64    `json:"temperature,omitempty"`
	Stop             []string   `json:"stop,omitempty"`
}

// FromChat lifts a text request into the rich shape.
func FromChat(r ChatRequest) RichRequest {
	out := RichRequest{
		Role: r.Role, Profile: r.Profile, Output: r.Output,
		MaxToks: r.MaxTokens, Temperature: r.Temperature, Stop: r.Stop,
	}
	for _, m := range r.Messages {
		rm := RichMessage{Role: m.Role, Trusted: m.Trusted}
		if m.Content != "" {
			rm.Parts = []ContentPart{TextPart(m.Content)}
		}
		out.Messages = append(out.Messages, rm)
	}
	return out.Clone()
}

// EffectiveRole returns the role the request addresses.
func (r RichRequest) EffectiveRole() Role {
	if r.Role == "" {
		return RoleController
	}
	return r.Role
}

func partPlaceholder(p ContentPart) string {
	return fmt.Sprintf("[%s %s]", p.Kind, p.RefID)
}

// TextView renders the request as a plain ChatRequest: text parts joined, each
// reference replaced by a "[kind ref]" placeholder, tools dropped. It is for
// shape validation, scripted matching and size estimates, never for sending.
func (r RichRequest) TextView() ChatRequest {
	c := ChatRequest{
		Role: r.Role, Profile: r.Profile, Output: r.Output,
		MaxTokens: r.MaxToks, Temperature: r.Temperature, Stop: r.Stop,
	}
	for _, m := range r.Messages {
		var sb strings.Builder
		for i, p := range m.Parts {
			if i > 0 {
				sb.WriteByte('\n')
			}
			if p.Kind == ModText {
				sb.WriteString(p.Text)
			} else {
				sb.WriteString(partPlaceholder(p))
			}
		}
		c.Messages = append(c.Messages, Message{Role: m.Role, Content: sb.String(), Trusted: m.Trusted})
	}
	return c
}

// TextOnly converts a request that uses only text to a ChatRequest, so it can
// go to a text-only generator. A request with media, tools or a non-text
// output modality is rejected (fail closed), never silently downgraded.
func (r RichRequest) TextOnly() (ChatRequest, error) {
	if err := r.Validate(); err != nil {
		return ChatRequest{}, err
	}
	if len(r.Tools) > 0 || r.ToolChoice != ToolChoiceAuto {
		return ChatRequest{}, UnsupportedError("tool_calls")
	}
	for _, m := range r.Messages {
		if len(m.ToolCalls) > 0 || m.ToolCallID != "" {
			return ChatRequest{}, UnsupportedError("tool_calls")
		}
		for _, p := range m.Parts {
			if p.Kind != ModText {
				return ChatRequest{}, UnsupportedError(string(p.Kind) + "_input")
			}
		}
	}
	for _, m := range r.OutputModalities {
		if m != ModText {
			return ChatRequest{}, UnsupportedError(string(m) + "_output")
		}
	}
	return r.TextView(), nil
}

func (m RichMessage) validate(i int) error {
	fail := func(msg string) error { return invalidRequest(fmt.Sprintf("message %d: %s", i, msg)) }
	switch m.Role {
	case MsgSystem, MsgUser, MsgAssistant, MsgTool:
	default:
		return fail("unknown role")
	}
	if len(m.Parts) > maxPartsPerMessage {
		return fail(fmt.Sprintf("more than %d parts", maxPartsPerMessage))
	}
	if len(m.Parts) == 0 && len(m.ToolCalls) == 0 {
		return fail("no content")
	}
	for _, p := range m.Parts {
		if err := p.ValidateInput(); err != nil {
			return fmt.Errorf("message %d: %w", i, err)
		}
		if p.Kind != ModText && m.Role != MsgUser {
			return fail("only user messages carry media references")
		}
	}
	if m.Role == MsgSystem && !m.Trusted {
		return fail("a system message must be marked trusted")
	}
	if m.Role == MsgTool && m.ToolCallID == "" {
		return fail("a tool message must name the call it answers")
	}
	if m.ToolCallID != "" && (m.Role != MsgTool || !callIDPattern.MatchString(m.ToolCallID)) {
		return fail("invalid tool call id")
	}
	if len(m.ToolCalls) > 0 && m.Role != MsgAssistant {
		return fail("only assistant messages carry tool calls")
	}
	return validateMessageCalls(m.ToolCalls)
}

// Validate checks the request's shape. It does not check budgets (see
// Budget.CheckRich), capabilities (see ModelCapabilities.Check) or the scope.
func (r RichRequest) Validate() error {
	for i, m := range r.Messages {
		if err := m.validate(i); err != nil {
			return err
		}
	}
	// The remaining shape rules are those of the text request.
	if err := r.TextView().Validate(); err != nil {
		return err
	}
	if err := ValidateToolSpecs(r.Tools); err != nil {
		return err
	}
	switch r.ToolChoice {
	case ToolChoiceAuto, ToolChoiceNone:
	case ToolChoiceRequired:
		if len(r.Tools) == 0 {
			return invalidRequest("tool choice is required but no tool is offered")
		}
	default:
		return invalidRequest("unknown tool choice")
	}
	seen := map[Modality]bool{}
	for _, m := range r.OutputModalities {
		if !m.Valid() || seen[m] {
			return invalidRequest("output modalities must be unique and known")
		}
		seen[m] = true
	}
	return nil
}

// WantsOutput reports whether the request asks for modality m (text is always
// wanted).
func (r RichRequest) WantsOutput(m Modality) bool {
	if m == ModText {
		return true
	}
	for _, o := range r.OutputModalities {
		if o == m {
			return true
		}
	}
	return false
}

// Clone returns a deep copy.
func (r RichRequest) Clone() RichRequest {
	msgs := make([]RichMessage, len(r.Messages))
	for i, m := range r.Messages {
		parts := make([]ContentPart, len(m.Parts))
		for j, p := range m.Parts {
			parts[j] = p.Clone()
		}
		calls := make([]ToolCallProposal, len(m.ToolCalls))
		for j, c := range m.ToolCalls {
			calls[j] = c.Clone()
		}
		m.Parts, m.ToolCalls = parts, calls
		if len(parts) == 0 {
			m.Parts = nil
		}
		if len(calls) == 0 {
			m.ToolCalls = nil
		}
		msgs[i] = m
	}
	r.Messages = msgs
	tools := make([]ToolSpec, len(r.Tools))
	for i, t := range r.Tools {
		t.Parameters = append(json.RawMessage(nil), t.Parameters...)
		tools[i] = t
	}
	r.Tools = tools
	if len(tools) == 0 {
		r.Tools = nil
	}
	r.Stop = append([]string(nil), r.Stop...)
	r.OutputModalities = append([]Modality(nil), r.OutputModalities...)
	r.Output.JSONSchema = append(json.RawMessage(nil), r.Output.JSONSchema...)
	return r
}

// Check rejects, before anything is sent, a request that needs something this
// model lacks (additive plan section 5). The error is validation_failed with
// IsUnsupported reporting what is missing.
func (c ModelCapabilities) Check(r RichRequest) error {
	in, _ := modalitySet(c.InputModalities)
	out, _ := modalitySet(c.OutputModalities)
	for _, m := range r.Messages {
		for _, p := range m.Parts {
			if !in[p.Kind] {
				return UnsupportedError(string(p.Kind) + "_input")
			}
		}
	}
	for _, m := range r.OutputModalities {
		if !out[m] {
			return UnsupportedError(string(m) + "_output")
		}
		if m == ModImage && !c.ImageGeneration {
			return UnsupportedError("image_output")
		}
	}
	if len(r.Tools) > 0 || r.ToolChoice != ToolChoiceAuto {
		if !c.Tools.Supported {
			return UnsupportedError("tool_calls")
		}
		if c.Tools.MaxTools > 0 && len(r.Tools) > c.Tools.MaxTools {
			return UnsupportedError("that many tools")
		}
	}
	switch r.Output.Kind() {
	case ConstraintJSONSchema:
		if c.StructuredOutput == StructuredNone {
			return UnsupportedError("structured_output")
		}
	case ConstraintGrammar:
		if !c.Grammar {
			return UnsupportedError("grammar_output")
		}
	}
	if r.MaxToks > c.MaxOutputToks {
		return UnsupportedError("that many output toks")
	}
	return nil
}

// RichResult is a complete, successful generation. Everything in it that came
// from the provider is untrusted data.
type RichResult struct {
	// Parts is the reply in order: text and produced media bytes.
	Parts []ContentPart `json:"parts"`
	// ToolCalls are the model's PROPOSALS, validated against the request's
	// allowlist. They have not been run and nothing here can run them.
	ToolCalls []ToolCallProposal `json:"tool_calls,omitempty"`
	// JSON is the parsed output of a JSON Schema constrained request:
	// syntactically valid, not schema-validated.
	JSON         json.RawMessage `json:"json,omitempty"`
	FinishReason FinishReason    `json:"finish_reason,omitempty"`
	Usage        CostUsage       `json:"usage"`
	// RateLimit is what the provider said about its limits, when it said.
	RateLimit *RateLimitInfo `json:"rate_limit,omitempty"`
	Timings   Timings        `json:"timings"`
	TraceID   string         `json:"trace_id,omitempty"`
	Model     string         `json:"model,omitempty"`
}

// Text returns the text parts joined.
func (r RichResult) Text() string {
	var sb strings.Builder
	for _, p := range r.Parts {
		if p.Kind == ModText {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// DeltaKind says what a RichChunk carries.
type DeltaKind string

const (
	DeltaText     DeltaKind = "text"
	DeltaPart     DeltaKind = "part"
	DeltaToolArgs DeltaKind = "tool_args"
	DeltaUsage    DeltaKind = "usage"
	DeltaDone     DeltaKind = "done"
)

// RichChunk is one event of a streamed generation. Seq starts at 1 and rises
// by exactly one, so order is checkable and a lost or repeated chunk is an
// error.
type RichChunk struct {
	Seq  int       `json:"seq"`
	Kind DeltaKind `json:"kind"`
	// Text is the delta of a DeltaText chunk.
	Text string `json:"text,omitempty"`
	// Part is a complete produced media part (DeltaPart).
	Part *ContentPart `json:"part,omitempty"`
	// DeltaToolArgs: ToolIndex numbers the proposal (0, 1, ... in order of
	// appearance); the first chunk of an index carries ToolID and ToolName;
	// ArgsDelta is the next piece of its JSON arguments.
	ToolIndex int    `json:"tool_index,omitempty"`
	ToolID    string `json:"tool_id,omitempty"`
	ToolName  string `json:"tool_name,omitempty"`
	ArgsDelta string `json:"args_delta,omitempty"`
	// Usage is cumulative: the last one reported wins (DeltaUsage, DeltaDone).
	Usage *CostUsage `json:"usage,omitempty"`
	// The rest is meaningful on DeltaDone only.
	FinishReason FinishReason   `json:"finish_reason,omitempty"`
	RateLimit    *RateLimitInfo `json:"rate_limit,omitempty"`
	Timings      Timings        `json:"timings"`
	TraceID      string         `json:"trace_id,omitempty"`
	Model        string         `json:"model,omitempty"`
}

// RichStream is a pull-style stream of chunks with the semantics of
// ChatStream: after the DeltaDone chunk Recv returns io.EOF; any other error
// ends the stream and the output so far is partial and must not be acted on;
// Recv honours the context given to GenerateStream; Close is idempotent, safe
// concurrently with a blocked Recv, and always required.
type RichStream interface {
	Recv() (RichChunk, error)
	Close() error
}

// MultimodalGenerator is the extension of TextGenerator for external
// providers: parts in, parts and tool-call proposals out. Implementations
// validate scope and request (fail closed), honour ctx and return
// *contracts.Error values (rate_limited carries the retry-after, see
// RateLimitFrom). It offers a provider no way to invoke anything.
type MultimodalGenerator interface {
	Generate(ctx context.Context, scope contracts.Scope, req RichRequest) (RichResult, error)
	GenerateStream(ctx context.Context, scope contracts.Scope, req RichRequest) (RichStream, error)
}

const (
	maxStreamArgs  = maxToolArgsSize
	maxStreamParts = 64
)

type toolAcc struct {
	id, name string
	args     strings.Builder
}

// richStreamState checks a stream's order and content and, when keep is set,
// assembles the result. The budget decorator and CollectRich share it, so a
// raw Recv loop and Collect enforce the same rules.
type richStreamState struct {
	req   RichRequest
	keep  bool
	seq   int
	done  bool
	tools []*toolAcc
	parts []ContentPart
	usage *CostUsage
	fin   RichChunk
	nPart int
}

func newRichStreamState(req RichRequest, keep bool) *richStreamState {
	return &richStreamState{req: req, keep: keep}
}

func (s *richStreamState) accept(c RichChunk) error {
	if s.done {
		return schemaInvalid("provider sent data after the final chunk")
	}
	if c.Seq != s.seq+1 {
		return contracts.New(contracts.ErrSchemaInvalid, "providers: provider stream chunks are out of order").
			WithDetails("expected_seq", s.seq+1)
	}
	s.seq = c.Seq
	switch c.Kind {
	case DeltaText:
		if c.Text == "" || c.Part != nil || c.Usage != nil {
			return schemaInvalid("malformed text delta")
		}
		if s.keep {
			if n := len(s.parts); n > 0 && s.parts[n-1].Kind == ModText {
				s.parts[n-1].Text += c.Text
			} else {
				s.parts = append(s.parts, TextPart(c.Text))
			}
		}
	case DeltaPart:
		if c.Part == nil || c.Text != "" {
			return schemaInvalid("malformed part delta")
		}
		if err := c.Part.ValidateOutput(); err != nil {
			return err
		}
		if c.Part.Kind != ModText && !s.req.WantsOutput(c.Part.Kind) {
			return schemaInvalid("provider produced a modality that was not requested")
		}
		if s.nPart++; s.nPart > maxStreamParts {
			return schemaInvalid("provider stream has too many parts")
		}
		if s.keep {
			s.parts = append(s.parts, *c.Part)
		}
	case DeltaToolArgs:
		return s.acceptTool(c)
	case DeltaUsage:
		if c.Usage == nil {
			return schemaInvalid("malformed usage delta")
		}
		return s.setUsage(c.Usage)
	case DeltaDone:
		if c.Usage != nil {
			if err := s.setUsage(c.Usage); err != nil {
				return err
			}
		}
		if c.RateLimit != nil && c.RateLimit.RetryAfter < 0 {
			return schemaInvalid("negative retry-after")
		}
		s.done, s.fin = true, c
	default:
		return schemaInvalid("unknown stream delta kind")
	}
	return nil
}

func (s *richStreamState) setUsage(u *CostUsage) error {
	if err := u.Validate(); err != nil {
		return err
	}
	cp := *u
	s.usage = &cp
	return nil
}

func (s *richStreamState) acceptTool(c RichChunk) error {
	if c.Text != "" || c.Part != nil || c.Usage != nil || c.ToolIndex < 0 {
		return schemaInvalid("malformed tool delta")
	}
	switch {
	case c.ToolIndex == len(s.tools): // a new proposal
		if c.ToolID == "" || c.ToolName == "" {
			return schemaInvalid("first tool delta carries no id or name")
		}
		if s.req.ToolChoice == ToolChoiceNone || len(s.tools) >= maxProposals {
			return schemaInvalid("model proposed a tool call that is not allowed")
		}
		offered := false
		for _, t := range s.req.Tools {
			if t.Name == c.ToolName {
				offered = true
				break
			}
		}
		if !offered {
			return schemaInvalid("model proposed a tool that was not offered")
		}
		s.tools = append(s.tools, &toolAcc{id: c.ToolID, name: c.ToolName})
	case c.ToolIndex < len(s.tools): // more arguments for an earlier one
		t := s.tools[c.ToolIndex]
		if (c.ToolID != "" && c.ToolID != t.id) || (c.ToolName != "" && c.ToolName != t.name) {
			return schemaInvalid("tool delta changes the id or name of a proposal")
		}
	default:
		return schemaInvalid("tool delta skips an index")
	}
	t := s.tools[c.ToolIndex]
	if t.args.Len()+len(c.ArgsDelta) > maxStreamArgs {
		return schemaInvalid("tool call arguments are too large")
	}
	t.args.WriteString(c.ArgsDelta)
	return nil
}

// finish returns the assembled result. It fails unless the stream completed.
func (s *richStreamState) finish() (RichResult, error) {
	if !s.done {
		return RichResult{}, contracts.New(contracts.ErrUnavailable, "providers: stream ended before completion")
	}
	calls := make([]ToolCallProposal, 0, len(s.tools))
	for _, t := range s.tools {
		calls = append(calls, ToolCallProposal{ID: t.id, Name: t.name, Arguments: json.RawMessage(t.args.String())})
	}
	calls, err := ValidateProposals(calls, s.req.Tools, s.req.ToolChoice)
	if err != nil {
		return RichResult{}, err
	}
	res := RichResult{
		Parts: s.parts, ToolCalls: calls, FinishReason: s.fin.FinishReason,
		RateLimit: s.fin.RateLimit, Timings: s.fin.Timings, TraceID: s.fin.TraceID, Model: s.fin.Model,
	}
	if s.usage != nil {
		res.Usage = *s.usage
	}
	if s.req.Output.Kind() == ConstraintJSONSchema {
		j, err := ParseJSONOutput(res.Text())
		if err != nil {
			return RichResult{}, err
		}
		res.JSON = j
	}
	return res, nil
}

// CollectRich drains a stream into a RichResult and closes it, like Collect.
// The result is returned only for a complete stream: chunks in order, every
// tool proposal on the request's allowlist with arguments that form a JSON
// object, and (for a JSON Schema constraint) parseable output. An interrupted
// or malformed stream yields its error and no result.
func CollectRich(s RichStream, req RichRequest) (RichResult, error) {
	defer s.Close()
	st := newRichStreamState(req, true)
	for {
		c, err := s.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return RichResult{}, annotatePartial(err, st.partialLen())
		}
		if err := st.accept(c); err != nil {
			return RichResult{}, err
		}
	}
	return st.finish()
}

func (s *richStreamState) partialLen() int {
	n := 0
	for _, p := range s.parts {
		n += len(p.Text) + len(p.Data)
	}
	return n
}
