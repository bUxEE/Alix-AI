package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// maxStrictTools is the documented number of strict tools one request may carry.
const maxStrictTools = 20

// prepared is a validated request, ready to send, with what is needed to read
// the answer.
type prepared struct {
	body  map[string]any
	req   providers.RichRequest
	model modelProfile
	// structured is true when a JSON schema output constraint was sent.
	structured bool
}

// capabilities returns the capability record of a model profile.
func (c *Client) capabilities(p modelProfile) providers.ModelCapabilities {
	return providers.ModelCapabilities{
		Provider:         ProviderName,
		Model:            p.id,
		InputModalities:  []providers.Modality{providers.ModText},
		OutputModalities: []providers.Modality{providers.ModText},
		Streaming:        true,
		StructuredOutput: providers.StructuredStrict,
		Tools:            providers.ToolCapability{Supported: true, StrictArguments: true, ParallelCalls: true, MaxTools: maxStrictTools},
		MaxContextToks:   p.context,
		MaxOutputToks:    p.output,
		Pricing:          p.prices,
		Metering:         providers.MeteringPerResponse,
	}
}

func (c *Client) modelFor(profile string) (modelProfile, error) {
	if profile == "" {
		return c.model, nil
	}
	id, ok := c.cfg.Profiles[profile]
	if !ok {
		return modelProfile{}, localFail("unknown model profile")
	}
	return c.cfg.profileFor(id), nil
}

// localFail is a request refused here, before anything is sent.
func localFail(msg string) error {
	return contracts.New(contracts.ErrValidationFailed, "anthropic: "+msg).WithDetails("sent", false)
}

func notSent(err error) error {
	var ce *contracts.Error
	if errors.As(err, &ce) {
		cp := *ce
		cp.Details = map[string]any{}
		for k, v := range ce.Details {
			cp.Details[k] = v
		}
		cp.Details["sent"] = false
		return &cp
	}
	return err
}

// wireMessage is one entry of the messages array.
type wireMessage struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
	// toolResults is true while the message holds only tool_result blocks.
	toolResults bool
}

// prepare validates a request against the model's capabilities and builds the
// Messages API body. It sends nothing. Everything the contract does not allow
// is refused here (fail closed), never dropped:
//
//   - non-text parts (ADR-0024 keeps them off in the first release);
//   - a grammar constraint (the API has no grammar for json output);
//   - a temperature the model rejects, or above the API's maximum of 1;
//   - a conversation that does not start with a user turn, a system message
//     after the first turn, or a conversation that ends with an assistant turn
//     (a prefill: newer models answer 400);
//   - a schema outside the provider's subset (see schema.go).
//
// The body never asks for a provider-native feature: no thinking, metadata,
// service tier, container, MCP server, context management, cache control or
// beta header is ever set, and the only tool kind is the custom (function) tool.
func (c *Client) prepare(scope contracts.Scope, req providers.RichRequest, stream bool) (*prepared, error) {
	if err := scope.Validate(); err != nil {
		return nil, notSent(err)
	}
	if err := req.Validate(); err != nil {
		return nil, notSent(err)
	}
	model, err := c.modelFor(req.Profile)
	if err != nil {
		return nil, err
	}
	if err := c.capabilities(model).Check(req); err != nil {
		return nil, notSent(err)
	}
	if req.Temperature > 0 && (!model.sampling || req.Temperature > 1) {
		return nil, notSent(providers.UnsupportedError("temperature"))
	}
	if c.cfg.Effort != "" && !model.effort {
		return nil, localFail("the model does not accept an effort level")
	}

	p := &prepared{req: req.Clone(), model: model}
	maxOut := req.MaxToks
	if maxOut == 0 {
		maxOut = c.cfg.DefaultMaxOutputToks
		if maxOut > model.output {
			maxOut = model.output
		}
	}
	body := map[string]any{"model": model.id, "max_tokens": maxOut}
	if stream {
		body["stream"] = true
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}
	if len(req.Stop) > 0 {
		body["stop_sequences"] = req.Stop
	}

	system, msgs, err := wireMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	if len(system) > 0 {
		body["system"] = system
	}
	body["messages"] = msgs

	var reps []SchemaReport
	outputConfig := map[string]any{}
	if req.Output.Kind() == providers.ConstraintJSONSchema {
		schema, rep, err := ConvertSchema(req.Output.JSONSchema)
		if err != nil {
			return nil, notSent(err)
		}
		reps = append(reps, rep)
		p.structured = true
		outputConfig["format"] = map[string]any{"type": "json_schema", "schema": json.RawMessage(schema)}
	}
	if c.cfg.Effort != "" {
		outputConfig["effort"] = c.cfg.Effort
	}
	if len(outputConfig) > 0 {
		body["output_config"] = outputConfig
	}

	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			schema, rep, err := ConvertSchema(t.Parameters)
			if err != nil {
				return nil, notSent(fmt.Errorf("tool %q: %w", t.Name, err))
			}
			reps = append(reps, rep)
			tool := map[string]any{"name": t.Name, "input_schema": json.RawMessage(schema), "strict": true}
			if t.Description != "" {
				tool["description"] = t.Description
			}
			tools = append(tools, tool)
		}
		body["tools"] = tools
		switch req.ToolChoice {
		case providers.ToolChoiceNone:
			body["tool_choice"] = map[string]any{"type": "none"}
		case providers.ToolChoiceRequired:
			if model.forced {
				body["tool_choice"] = map[string]any{"type": "any"}
			} else {
				// The newest models answer 400 to a forced choice. The request
				// goes out as auto and "required" is enforced on the answer:
				// providers.ValidateProposals turns a missing proposal into an
				// error, so the guarantee the caller relies on is unchanged.
				body["tool_choice"] = map[string]any{"type": "auto"}
			}
		default:
			body["tool_choice"] = map[string]any{"type": "auto"}
		}
	}
	if err := checkComplexity(reps); err != nil {
		return nil, notSent(err)
	}
	p.body = body
	return p, nil
}

// wireMessages maps the messages to the Messages API: system messages (only at
// the start) become the top-level system blocks; an assistant turn that
// proposed tool calls becomes its text plus one tool_use block per proposal; a
// run of tool messages becomes ONE user message of tool_result blocks. Message
// order is preserved.
func wireMessages(msgs []providers.RichMessage) (system []any, out []wireMessage, err error) {
	started := false
	for i, m := range msgs {
		text := ""
		for j, part := range m.Parts {
			if part.Kind != providers.ModText {
				return nil, nil, notSent(providers.UnsupportedError(string(part.Kind) + "_input"))
			}
			if j > 0 {
				text += "\n"
			}
			text += part.Text
		}
		if m.Role == providers.MsgSystem {
			if started {
				return nil, nil, localFail("a system message may only come before the first turn")
			}
			system = append(system, map[string]any{"type": "text", "text": text})
			continue
		}
		started = true
		last := len(out) - 1
		switch m.Role {
		case providers.MsgUser:
			block := map[string]any{"type": "text", "text": text}
			if last >= 0 && out[last].toolResults {
				// text after tool results goes into the same user turn, after them
				out[last].Content = append(out[last].Content, block)
				out[last].toolResults = false
				continue
			}
			out = append(out, wireMessage{Role: "user", Content: []any{block}})
		case providers.MsgAssistant:
			var content []any
			if text != "" {
				content = append(content, map[string]any{"type": "text", "text": text})
			}
			for _, call := range m.ToolCalls {
				input := json.RawMessage(call.Arguments)
				if len(input) == 0 {
					input = json.RawMessage(`{}`)
				}
				content = append(content, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": input})
			}
			out = append(out, wireMessage{Role: "assistant", Content: content})
		case providers.MsgTool:
			block := map[string]any{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": text}
			if last >= 0 && out[last].toolResults {
				out[last].Content = append(out[last].Content, block)
				continue
			}
			out = append(out, wireMessage{Role: "user", Content: []any{block}, toolResults: true})
		default:
			return nil, nil, localFail(fmt.Sprintf("message %d has an unknown role", i))
		}
	}
	if len(out) == 0 || out[0].Role != "user" {
		return nil, nil, localFail("the conversation must start with a user turn")
	}
	if out[len(out)-1].Role != "user" {
		return nil, nil, localFail("the conversation must end with a user turn (assistant prefill is not supported)")
	}
	return system, out, nil
}
