package localai

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// jsonSchemaName is the "name" of the OpenAI json_schema response format.
const jsonSchemaName = "alixai_output"

// Wire types. They are the adapter's own: providers.Message is never
// marshalled, because its Trusted flag is an AlixAI-internal marker that must
// not reach the runtime.

type messageWire struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormatWire struct {
	Type       string         `json:"type"`
	JSONSchema jsonSchemaWire `json:"json_schema"`
}

type jsonSchemaWire struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type streamOptionsWire struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatRequestWire struct {
	Model         string             `json:"model"`
	Messages      []messageWire      `json:"messages"`
	Stream        bool               `json:"stream"`
	StreamOptions *streamOptionsWire `json:"stream_options,omitempty"`
	MaxTokens     int                `json:"max_tokens"`
	// Temperature is a pointer without omitempty so that 0 (greedy) is sent:
	// LocalAI treats an absent temperature as "use the model's default".
	Temperature    *float64            `json:"temperature"`
	Stop           []string            `json:"stop,omitempty"`
	ResponseFormat *responseFormatWire `json:"response_format,omitempty"`
	Grammar        string              `json:"grammar,omitempty"`
}

type usageWire struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func (u *usageWire) usage() (providers.Usage, error) {
	if u == nil {
		return providers.Usage{}, nil
	}
	if u.PromptTokens < 0 || u.CompletionTokens < 0 {
		return providers.Usage{}, schemaInvalid("runtime reported a negative token count")
	}
	return providers.Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}, nil
}

type messageRespWire struct {
	Content      *string         `json:"content"`
	ToolCalls    json.RawMessage `json:"tool_calls"`
	FunctionCall json.RawMessage `json:"function_call"`
}

// hasToolCall reports whether the runtime attached a tool or function call.
// The adapter never offers tools, so any is a protocol violation.
func (m *messageRespWire) hasToolCall() bool {
	present := func(raw json.RawMessage) bool {
		t := strings.TrimSpace(string(raw))
		return t != "" && t != "null" && t != "[]" && t != "{}"
	}
	return present(m.ToolCalls) || present(m.FunctionCall)
}

type choiceWire struct {
	FinishReason *string          `json:"finish_reason"`
	Message      *messageRespWire `json:"message"`
	Delta        *messageRespWire `json:"delta"`
}

type chatResponseWire struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []choiceWire `json:"choices"`
	Usage   *usageWire   `json:"usage"`
	Error   *apiError    `json:"error"`
}

// mapFinish converts a finish reason. Anything but "stop" and "length"
// (tool_calls, function_call, content_filter, unknown) fails closed: the
// adapter requested none of them.
func mapFinish(reason *string) (providers.FinishReason, error) {
	if reason == nil {
		return "", schemaInvalid("response has no finish reason")
	}
	switch *reason {
	case "stop":
		return providers.FinishStop, nil
	case "length":
		return providers.FinishLength, nil
	}
	return "", schemaInvalid("response has an unexpected finish reason")
}

// buildChat validates a request and turns it into the wire form. Scope and
// request are checked before anything touches the network, so an invalid call
// never reaches the runtime (fail closed). The scope is validated but never
// sent: LocalAI knows nothing about tenants.
func (c *Client) buildChat(scope contracts.Scope, req providers.ChatRequest, stream bool) (chatRequestWire, error) {
	if err := scope.Validate(); err != nil {
		return chatRequestWire{}, err
	}
	if err := req.Validate(); err != nil {
		return chatRequestWire{}, err
	}
	model, err := c.model(req.EffectiveRole(), req.Profile)
	if err != nil {
		return chatRequestWire{}, err
	}
	maxToks := req.MaxTokens
	if maxToks == 0 {
		maxToks = c.cfg.DefaultMaxOutputToks
	}
	temp := req.Temperature
	wire := chatRequestWire{
		Model:       model,
		Messages:    make([]messageWire, len(req.Messages)),
		Stream:      stream,
		MaxTokens:   maxToks,
		Temperature: &temp,
		Stop:        append([]string(nil), req.Stop...),
	}
	for i, m := range req.Messages {
		wire.Messages[i] = messageWire{Role: string(m.Role), Content: m.Content}
	}
	if stream {
		wire.StreamOptions = &streamOptionsWire{IncludeUsage: true}
	}
	switch req.Output.Kind() {
	case providers.ConstraintJSONSchema:
		wire.ResponseFormat = &responseFormatWire{
			Type:       "json_schema",
			JSONSchema: jsonSchemaWire{Name: jsonSchemaName, Strict: true, Schema: req.Output.JSONSchema},
		}
	case providers.ConstraintGrammar:
		wire.Grammar = req.Output.Grammar
	}
	return wire, nil
}

// Chat implements providers.TextGenerator.
func (c *Client) Chat(ctx context.Context, scope contracts.Scope, req providers.ChatRequest) (providers.ChatResult, error) {
	wire, err := c.buildChat(scope, req, false)
	if err != nil {
		return providers.ChatResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer cancel()
	started := c.clk.Now()
	resp, err := c.send(ctx, "POST", pathChat, wire, "application/json")
	if err != nil {
		return providers.ChatResult{}, err
	}
	defer resp.Body.Close()
	var out chatResponseWire
	if err := c.decodeBody(ctx, resp.Body, &out); err != nil {
		return providers.ChatResult{}, err
	}
	if out.Error != nil {
		return providers.ChatResult{}, mapEventError(*out.Error)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message == nil {
		return providers.ChatResult{}, schemaInvalid("response must hold exactly one message")
	}
	choice := out.Choices[0]
	if choice.Message.hasToolCall() {
		return providers.ChatResult{}, schemaInvalid("runtime returned a tool call, which the adapter never requests")
	}
	finish, err := mapFinish(choice.FinishReason)
	if err != nil {
		return providers.ChatResult{}, err
	}
	usage, err := out.Usage.usage()
	if err != nil {
		return providers.ChatResult{}, err
	}
	res := providers.ChatResult{
		FinishReason: finish,
		Usage:        usage,
		Timings:      providers.Timings{Total: c.clk.Now().Sub(started)},
		TraceID:      traceID(out.ID),
		Model:        reportedModel(out.Model, wire.Model),
	}
	if choice.Message.Content != nil {
		res.Text = *choice.Message.Content
	}
	if req.Output.Kind() == providers.ConstraintJSONSchema {
		// LocalAI's json_schema constraint is best effort (see the package
		// documentation), so the output is always checked.
		j, err := providers.ParseJSONOutput(res.Text)
		if err != nil {
			return providers.ChatResult{}, err
		}
		res.JSON = j
	}
	c.logDone(ctx, "chat", wire.Model, usage, started)
	return res, nil
}
