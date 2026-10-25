package openai

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// structuredFormatName is the fixed name of the json_schema format; the
// provider requires one and the schema's own title is not trusted to fit.
const structuredFormatName = "alixai_output"

// prepared is a validated request, ready to send, with what is needed to read
// the answer.
type prepared struct {
	body    map[string]any
	req     providers.RichRequest
	model   modelProfile
	outPlan *StrictPlan            // nil without a schema constraint
	tools   map[string]*StrictPlan // by tool name
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
		Tools:            providers.ToolCapability{Supported: true, StrictArguments: true, ParallelCalls: true, MaxTools: 32},
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
	return contracts.New(contracts.ErrValidationFailed, "openai: "+msg).WithDetails("sent", false)
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

// prepare validates a request against the model's capabilities and builds the
// Responses API body. It sends nothing. Everything the contract does not allow
// is refused here (fail closed), never dropped:
//
//   - non-text parts (ADR-0024 keeps them off in the first release);
//   - a grammar constraint (the API has no grammar for json output);
//   - stop sequences (the Responses API has no such parameter);
//   - a temperature on a reasoning model (the API rejects it).
//
// The body never asks for a provider-native feature: store is false, and
// previous_response_id, conversation, background, include, prompt and
// max_tool_calls are never set; the only tool type is function.
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
	if len(req.Stop) > 0 {
		return nil, notSent(providers.UnsupportedError("stop_sequences"))
	}
	if req.Temperature > 0 && model.reasoning {
		return nil, notSent(providers.UnsupportedError("temperature"))
	}

	p := &prepared{req: req.Clone(), model: model, tools: map[string]*StrictPlan{}}
	maxOut := req.MaxToks
	if maxOut == 0 {
		maxOut = c.cfg.DefaultMaxOutputToks
		if maxOut > model.output {
			maxOut = model.output
		}
	}
	body := map[string]any{
		"model":             model.id,
		"store":             false,
		"truncation":        "disabled",
		"max_output_tokens": maxOut,
	}
	if stream {
		body["stream"] = true
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}
	if model.reasoning && c.cfg.ReasoningEffort != "" {
		body["reasoning"] = map[string]any{"effort": c.cfg.ReasoningEffort}
	}

	input, err := inputItems(req.Messages)
	if err != nil {
		return nil, err
	}
	body["input"] = input

	if req.Output.Kind() == providers.ConstraintJSONSchema {
		schema, plan, err := ConvertStrict(req.Output.JSONSchema)
		if err != nil {
			return nil, notSent(err)
		}
		if stream && !plan.Identity() {
			return nil, notSent(providers.UnsupportedError("streamed_structured_output"))
		}
		p.outPlan = plan
		body["text"] = map[string]any{"format": map[string]any{
			"type": "json_schema", "name": structuredFormatName, "schema": json.RawMessage(schema), "strict": true,
		}}
	}

	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			schema, plan, err := ConvertStrict(t.Parameters)
			if err != nil {
				return nil, notSent(fmt.Errorf("tool %q: %w", t.Name, err))
			}
			if stream && !plan.Identity() {
				return nil, notSent(providers.UnsupportedError("streamed_tool_arguments"))
			}
			p.tools[t.Name] = plan
			tool := map[string]any{"type": "function", "name": t.Name, "parameters": json.RawMessage(schema), "strict": true}
			if t.Description != "" {
				tool["description"] = t.Description
			}
			tools = append(tools, tool)
		}
		body["tools"] = tools
		switch req.ToolChoice {
		case providers.ToolChoiceNone:
			body["tool_choice"] = "none"
		case providers.ToolChoiceRequired:
			body["tool_choice"] = "required"
		default:
			body["tool_choice"] = "auto"
		}
	}
	p.body = body
	return p, nil
}

// inputItems maps the messages to Responses input items. A system message
// becomes a developer message (the current name of the instruction role); an
// assistant turn that proposed tool calls becomes its text plus one
// function_call item per proposal; a tool message becomes a
// function_call_output. Message order is preserved.
func inputItems(msgs []providers.RichMessage) ([]any, error) {
	var items []any
	for i, m := range msgs {
		text := ""
		for j, part := range m.Parts {
			if part.Kind != providers.ModText {
				return nil, notSent(providers.UnsupportedError(string(part.Kind) + "_input"))
			}
			if j > 0 {
				text += "\n"
			}
			text += part.Text
		}
		switch m.Role {
		case providers.MsgSystem:
			items = append(items, map[string]any{"role": "developer", "content": []any{map[string]any{"type": "input_text", "text": text}}})
		case providers.MsgUser:
			items = append(items, map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}})
		case providers.MsgAssistant:
			if text != "" {
				items = append(items, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}})
			}
			for _, call := range m.ToolCalls {
				items = append(items, map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Name, "arguments": string(call.Arguments)})
			}
		case providers.MsgTool:
			items = append(items, map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": text})
		default:
			return nil, localFail(fmt.Sprintf("message %d has an unknown role", i))
		}
	}
	return items, nil
}
