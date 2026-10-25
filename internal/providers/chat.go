package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// MessageRole is the author of a chat message.
type MessageRole string

const (
	MsgSystem    MessageRole = "system"
	MsgUser      MessageRole = "user"
	MsgAssistant MessageRole = "assistant"
	// MsgTool carries data returned by a tool or document. It is data,
	// not instructions, and is untrusted unless the builder says otherwise.
	MsgTool MessageRole = "tool"
)

// Message is one chat turn.
type Message struct {
	Role    MessageRole `json:"role"`
	Content string      `json:"content"`
	// Trusted marks content authored by AlixAI itself (templates, validated
	// state). The zero value is false: content is untrusted unless the prompt
	// builder asserts otherwise, so a forgotten marker fails closed. Untrusted
	// content (user text, tool results, retrieved documents, model output) is
	// fenced by the prompt builder; adapters never promote it, and a system
	// message must be Trusted.
	Trusted bool `json:"trusted"`
}

// OutputConstraint restricts what the model may emit. At most one of the two
// fields is set; the zero value means unconstrained.
type OutputConstraint struct {
	// JSONSchema is a JSON Schema document (a JSON object).
	JSONSchema json.RawMessage `json:"json_schema,omitempty"`
	// Grammar is a GBNF grammar.
	Grammar string `json:"grammar,omitempty"`
}

// ConstraintKind names the active constraint.
type ConstraintKind string

const (
	ConstraintNone       ConstraintKind = ""
	ConstraintJSONSchema ConstraintKind = "json_schema"
	ConstraintGrammar    ConstraintKind = "grammar"
)

// SchemaConstraint returns a constraint that forces output matching doc.
func SchemaConstraint(doc json.RawMessage) OutputConstraint {
	return OutputConstraint{JSONSchema: doc}
}

// GrammarConstraint returns a constraint that forces output matching a GBNF
// grammar.
func GrammarConstraint(gbnf string) OutputConstraint {
	return OutputConstraint{Grammar: gbnf}
}

// Kind reports which constraint is set. A (forbidden) both-set value reports
// ConstraintJSONSchema; Validate rejects it.
func (c OutputConstraint) Kind() ConstraintKind {
	switch {
	case len(bytes.TrimSpace(c.JSONSchema)) > 0:
		return ConstraintJSONSchema
	case strings.TrimSpace(c.Grammar) != "":
		return ConstraintGrammar
	}
	return ConstraintNone
}

// Validate rejects a constraint with both fields set, a schema that is not a
// JSON object, and a grammar that is only whitespace.
func (c OutputConstraint) Validate() error {
	hasSchema := len(c.JSONSchema) > 0
	hasGrammar := c.Grammar != ""
	switch {
	case hasSchema && hasGrammar:
		return invalidRequest("output constraint sets both json_schema and grammar")
	case hasSchema:
		trimmed := bytes.TrimSpace(c.JSONSchema)
		if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
			return invalidRequest("json_schema constraint is not a JSON object")
		}
	case hasGrammar && strings.TrimSpace(c.Grammar) == "":
		return invalidRequest("grammar constraint is empty")
	}
	return nil
}

// Limits on a request's shape, independent of any budget.
const (
	maxStopSequences = 4
	maxTemperature   = 2.0
)

// ChatRequest is one text-generation call.
type ChatRequest struct {
	// Role selects the model job: RoleController (the default when empty) or
	// RoleRouter.
	Role Role `json:"role,omitempty"`
	// Profile is an optional hint naming a model profile (for example "lite"
	// or a model registry id). The adapter's configuration decides what it
	// maps to; callers never choose arbitrary model files.
	Profile  string           `json:"profile,omitempty"`
	Messages []Message        `json:"messages"`
	Output   OutputConstraint `json:"output"`
	// MaxTokens caps the generated tokens. Zero asks for the budget's cap.
	MaxTokens int `json:"max_toks,omitempty"`
	// Temperature is the sampling temperature in [0, 2]. The zero value is
	// greedy and deterministic, the right default for typed output.
	Temperature float64  `json:"temperature,omitempty"`
	Stop        []string `json:"stop,omitempty"`
}

func invalidRequest(msg string) error {
	return contracts.New(contracts.ErrValidationFailed, "providers: "+msg)
}

// EffectiveRole returns the role the request addresses.
func (r ChatRequest) EffectiveRole() Role {
	if r.Role == "" {
		return RoleController
	}
	return r.Role
}

// Validate checks the request's shape. It does not check budgets (see
// Budget.CheckChat) or the scope.
func (r ChatRequest) Validate() error {
	if role := r.EffectiveRole(); !role.IsText() {
		return invalidRequest(fmt.Sprintf("role %q is not served by a text generator", string(role)))
	}
	if len(r.Messages) == 0 {
		return invalidRequest("chat request has no messages")
	}
	for i, m := range r.Messages {
		switch m.Role {
		case MsgSystem:
			if !m.Trusted {
				return invalidRequest(fmt.Sprintf("message %d: a system message must be marked trusted", i))
			}
		case MsgUser, MsgAssistant, MsgTool:
		default:
			return invalidRequest(fmt.Sprintf("message %d: unknown role %q", i, string(m.Role)))
		}
	}
	if err := r.Output.Validate(); err != nil {
		return err
	}
	if r.MaxTokens < 0 {
		return invalidRequest("max_toks is negative")
	}
	if !(r.Temperature >= 0 && r.Temperature <= maxTemperature) { // also rejects NaN
		return invalidRequest("temperature must be within [0, 2]")
	}
	if len(r.Stop) > maxStopSequences {
		return invalidRequest(fmt.Sprintf("more than %d stop sequences", maxStopSequences))
	}
	for _, s := range r.Stop {
		if s == "" {
			return invalidRequest("empty stop sequence")
		}
	}
	return nil
}

// Clone returns a deep copy.
func (r ChatRequest) Clone() ChatRequest {
	r.Messages = append([]Message(nil), r.Messages...)
	r.Stop = append([]string(nil), r.Stop...)
	r.Output.JSONSchema = append(json.RawMessage(nil), r.Output.JSONSchema...)
	return r
}

// Usage counts tokens. The JSON keys avoid the word "token" so the logging
// redactor does not mask them.
type Usage struct {
	InputTokens  int `json:"tok_in"`
	OutputTokens int `json:"tok_out"`
}

// Total returns input plus output.
func (u Usage) Total() int { return u.InputTokens + u.OutputTokens }

// Timings are the latencies the runtime (or adapter) observed.
type Timings struct {
	// TimeToFirstToken is zero when unknown.
	TimeToFirstToken time.Duration `json:"ttft_ns"`
	Total            time.Duration `json:"total_ns"`
}

// FinishReason says why generation stopped.
type FinishReason string

const (
	// FinishStop: the model finished or hit a stop sequence.
	FinishStop FinishReason = "stop"
	// FinishLength: generation hit MaxTokens and the output is truncated.
	FinishLength FinishReason = "length"
)

// ChatResult is a complete, successful generation.
type ChatResult struct {
	Text string `json:"text"`
	// JSON is the parsed output when the request carried a JSON Schema
	// constraint: syntactically valid JSON, not schema-validated.
	JSON         json.RawMessage `json:"json,omitempty"`
	FinishReason FinishReason    `json:"finish_reason,omitempty"`
	Usage        Usage           `json:"usage"`
	Timings      Timings         `json:"timings"`
	// TraceID is the runtime's identifier for the call, for correlation.
	TraceID string `json:"trace_id,omitempty"`
	// Model is the model the runtime reports having used.
	Model string `json:"model,omitempty"`
}

// ChatChunk is one piece of a streamed generation.
type ChatChunk struct {
	Delta string `json:"delta,omitempty"`
	// Done marks the final chunk; FinishReason, Usage, Timings, TraceID and
	// Model are only meaningful on it.
	Done         bool         `json:"done,omitempty"`
	FinishReason FinishReason `json:"finish_reason,omitempty"`
	Usage        Usage        `json:"usage"`
	Timings      Timings      `json:"timings"`
	TraceID      string       `json:"trace_id,omitempty"`
	Model        string       `json:"model,omitempty"`
}

// ErrStreamClosed is returned by Recv after Close.
var ErrStreamClosed = errors.New("providers: stream closed")

// ChatStream is a pull-style stream of chunks.
//
// Recv returns the next chunk. After the chunk with Done set it returns
// io.EOF. Any other error ends the stream: output received so far is partial
// and must not be acted on. Recv honours the context given to ChatStream: once
// it is cancelled or past its deadline Recv returns promptly with an error
// wrapping the context's error and no further chunk is produced. Close
// releases the runtime connection, is idempotent and safe to call
// concurrently with a blocked Recv; callers must always Close.
type ChatStream interface {
	Recv() (ChatChunk, error)
	Close() error
}

// TextGenerator serves the controller and router roles.
//
// Implementations validate scope and request (fail closed), honour ctx, and
// return *contracts.Error values. A constrained request with output that does
// not parse is schema_invalid, never a result.
type TextGenerator interface {
	// Chat runs one generation to completion.
	Chat(ctx context.Context, scope contracts.Scope, req ChatRequest) (ChatResult, error)
	// ChatStream starts a generation and returns before it completes.
	ChatStream(ctx context.Context, scope contracts.Scope, req ChatRequest) (ChatStream, error)
}

// ParseJSONOutput checks that text is exactly one valid JSON document and
// returns it trimmed. Adapters use it for constrained requests; failure is
// schema_invalid. The error never echoes the text (it may hold tenant data).
func ParseJSONOutput(text string) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || !json.Valid([]byte(trimmed)) {
		return nil, contracts.New(contracts.ErrSchemaInvalid, "providers: constrained model output is not valid JSON").
			WithDetails("output_len", len(text))
	}
	return json.RawMessage(trimmed), nil
}

// Collect drains a stream into a ChatResult and closes it. The result is
// returned only when the stream completed (a Done chunk, then io.EOF):
//
//   - an interrupted stream yields its error and no text (never act on
//     partial output);
//   - a stream that ends with io.EOF before Done is unavailable;
//   - for a JSON Schema constraint the text must parse (schema_invalid).
func Collect(s ChatStream, constraint OutputConstraint) (ChatResult, error) {
	defer s.Close()
	var (
		text strings.Builder
		res  ChatResult
		done bool
	)
	for {
		c, err := s.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ChatResult{}, annotatePartial(err, text.Len())
		}
		if done {
			return ChatResult{}, contracts.New(contracts.ErrInternal, "providers: runtime sent data after the final chunk")
		}
		text.WriteString(c.Delta)
		if c.Done {
			done = true
			res.FinishReason, res.Usage, res.Timings = c.FinishReason, c.Usage, c.Timings
			res.TraceID, res.Model = c.TraceID, c.Model
		}
	}
	if !done {
		return ChatResult{}, contracts.New(contracts.ErrUnavailable, "providers: stream ended before completion").
			WithDetails("partial_len", text.Len())
	}
	res.Text = text.String()
	if constraint.Kind() == ConstraintJSONSchema {
		j, err := ParseJSONOutput(res.Text)
		if err != nil {
			return ChatResult{}, err
		}
		res.JSON = j
	}
	return res, nil
}

// annotatePartial records how much text was received before err, without
// returning that text, and without mutating the adapter's error value.
func annotatePartial(err error, partialLen int) error {
	if partialLen == 0 || errors.Is(err, context.Canceled) {
		return err
	}
	var ce *contracts.Error
	if errors.As(err, &ce) {
		cp := *ce
		cp.Details = make(map[string]any, len(ce.Details)+1)
		for k, v := range ce.Details {
			cp.Details[k] = v
		}
		cp.Details["partial_len"] = partialLen
		return &cp
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, "providers: stream interrupted").
		WithDetails("partial_len", partialLen)
}

// ContextError converts a context error into the package's error shape: a
// deadline becomes timeout (retaining context.DeadlineExceeded for errors.Is),
// cancellation stays a plain error wrapping context.Canceled. Other errors
// are returned unchanged.
func ContextError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.DeadlineExceeded):
		if contracts.HasCode(err, contracts.ErrTimeout) {
			return err
		}
		return contracts.Wrap(contracts.ErrTimeout, err, "providers: deadline exceeded")
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("providers: canceled: %w", err)
	}
	return err
}
