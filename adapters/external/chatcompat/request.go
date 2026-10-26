package chatcompat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// structuredFormatName is the fixed name of the json_schema response format; the
// API requires one and the schema's own title is not trusted to fit.
const structuredFormatName = "alixai_output"

// maxSchemaBytes bounds one schema sent to a host.
const maxSchemaBytes = 64 << 10

// prepared is a validated request, ready to send, with what is needed to read
// the answer.
type prepared struct {
	body       map[string]any
	req        providers.RichRequest
	model      modelProfile
	structured bool
}

// capabilities returns the capability record of a model profile. It is honest
// about a generic host: text only (ADR-0024), and a schema that is SENT but
// whose conformance no host in general guarantees (schema_best_effort), tool
// arguments that are not decoding-constrained (strict_arguments false). The
// caller validates the document or the arguments against the schema it owns.
func (c *Client) capabilities(p modelProfile) providers.ModelCapabilities {
	return providers.ModelCapabilities{
		Provider:         c.provider,
		Model:            p.id,
		InputModalities:  []providers.Modality{providers.ModText},
		OutputModalities: []providers.Modality{providers.ModText},
		Streaming:        true,
		StructuredOutput: providers.StructuredBestEffort,
		Tools:            providers.ToolCapability{Supported: true, StrictArguments: false, ParallelCalls: true, MaxTools: c.cfg.MaxTools},
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
		return modelProfile{}, c.localFail("unknown model profile")
	}
	return c.cfg.profileFor(id), nil
}

func (c *Client) localFail(msg string) error {
	return wireFail(c.provider, msg)
}

func wireFail(provider, msg string) error {
	return contracts.New(contracts.ErrValidationFailed, provider+": "+msg).WithDetails("sent", false)
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

// checkSchema accepts a JSON Schema for a host's response_format or tool
// parameters. A generic host's supported subset is not known, so the schema is
// sent as the caller wrote it (the caller validates the answer against it); only
// what would be unsafe or meaningless to send is refused: not JSON, not an object
// schema at the root, oversized, or a $ref that leaves the document.
func checkSchema(raw json.RawMessage) error {
	if len(raw) > maxSchemaBytes {
		return providers.UnsupportedError("schema: larger than 64 KiB")
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil || root == nil {
		return providers.UnsupportedError("schema: not a JSON object")
	}
	if t, _ := root["type"].(string); t != "object" {
		return providers.UnsupportedError("schema: the root must have type object")
	}
	var bad bool
	var walk func(v any, depth int)
	walk = func(v any, depth int) {
		if depth > 32 {
			bad = true
			return
		}
		switch x := v.(type) {
		case map[string]any:
			if r, ok := x["$ref"].(string); ok && !strings.HasPrefix(r, "#") {
				bad = true
			}
			for _, e := range x {
				walk(e, depth+1)
			}
		case []any:
			for _, e := range x {
				walk(e, depth+1)
			}
		}
	}
	walk(root, 0)
	if bad {
		return providers.UnsupportedError("schema: an external reference or nesting beyond 32 levels")
	}
	return nil
}

// prepare validates a request against the model's capabilities and builds the
// Chat Completions body. It sends nothing. Everything the contract does not
// allow is refused here (fail closed), never dropped:
//
//   - non-text parts (ADR-0024 keeps them off in the first release);
//   - a grammar constraint, and stop sequences on the Model API;
//   - more tools than the configured maximum, more output tokens than the
//     model's cap;
//   - a schema outside what checkSchema accepts.
//
// A service that only accepts tool_choice "auto" (the Model API, or
// Config.NoForcedToolChoice) gets the tools left out for "none" and "auto" for
// "required"; both are enforced on the answer.
//
// The body never asks for a host-native feature: no store, user, logprobs,
// service tier, plugins, provider routing, fallback model list, reasoning or
// parallel_tool_calls member is ever set, and the only tool type is function.
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

	p := &prepared{req: req.Clone(), model: model}
	maxOut := req.MaxToks
	if maxOut == 0 {
		maxOut = c.cfg.DefaultMaxOutputToks
		if maxOut > model.output {
			maxOut = model.output
		}
	}
	body := map[string]any{"model": model.id, "max_tokens": maxOut}
	if c.cfg.ReasoningEffort != "" {
		body["reasoning_effort"] = c.cfg.ReasoningEffort
	}
	if stream {
		body["stream"] = true
		// Usage arrives in a final chunk only when asked for; without it the
		// call could not be metered, so a host that ignores it fails the stream.
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}
	if len(req.Stop) > 0 {
		body["stop"] = req.Stop
	}

	msgs, err := wireMessages(c.provider, req.Messages)
	if err != nil {
		return nil, err
	}
	body["messages"] = msgs

	if req.Output.Kind() == providers.ConstraintJSONSchema {
		if err := checkSchema(req.Output.JSONSchema); err != nil {
			return nil, notSent(err)
		}
		p.structured = true
		body["response_format"] = map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": structuredFormatName, "schema": json.RawMessage(req.Output.JSONSchema)},
		}
	}

	autoOnly := c.cfg.NoForcedToolChoice
	if len(req.Tools) > 0 && req.ToolChoice != providers.ToolChoiceNone {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			if err := checkSchema(t.Parameters); err != nil {
				return nil, notSent(fmt.Errorf("tool %q: %w", t.Name, err))
			}
			fn := map[string]any{"name": t.Name, "parameters": json.RawMessage(t.Parameters)}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		body["tools"] = tools
		switch req.ToolChoice {
		case providers.ToolChoiceNone:
			body["tool_choice"] = "none"
		case providers.ToolChoiceRequired:
			if autoOnly {
				// The service rejects a forced choice (the Model API accepts only
				// "auto"). The request goes out as auto and "required" is enforced
				// on the answer: ValidateProposals turns a missing proposal into
				// an error.
				body["tool_choice"] = "auto"
			} else {
				body["tool_choice"] = "required"
			}
		default:
			body["tool_choice"] = "auto"
		}
	}
	p.body = body
	return p, nil
}

// wireMessages maps the messages to the Chat Completions roles. An assistant
// turn that proposed tool calls carries them as tool_calls with the arguments as
// a JSON string; a tool message answers one call by id. Message order is
// preserved.
func wireMessages(provider string, msgs []providers.RichMessage) ([]any, error) {
	out := make([]any, 0, len(msgs))
	for i, m := range msgs {
		var sb strings.Builder
		for j, part := range m.Parts {
			if part.Kind != providers.ModText {
				return nil, notSent(providers.UnsupportedError(string(part.Kind) + "_input"))
			}
			if j > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(part.Text)
		}
		text := sb.String()
		switch m.Role {
		case providers.MsgSystem:
			out = append(out, map[string]any{"role": "system", "content": text})
		case providers.MsgUser:
			out = append(out, map[string]any{"role": "user", "content": text})
		case providers.MsgAssistant:
			msg := map[string]any{"role": "assistant"}
			if text != "" || len(m.ToolCalls) == 0 {
				msg["content"] = text
			} else {
				msg["content"] = nil
			}
			if len(m.ToolCalls) > 0 {
				calls := make([]any, 0, len(m.ToolCalls))
				for _, call := range m.ToolCalls {
					args := string(bytes.TrimSpace(call.Arguments))
					if args == "" {
						args = "{}"
					}
					calls = append(calls, map[string]any{
						"id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": args},
					})
				}
				msg["tool_calls"] = calls
			}
			out = append(out, msg)
		case providers.MsgTool:
			out = append(out, map[string]any{"role": "tool", "tool_call_id": m.ToolCallID, "content": text})
		default:
			return nil, notSent(wireFail(provider, fmt.Sprintf("message %d has an unknown role", i)))
		}
	}
	if len(out) == 0 {
		return nil, wireFail(provider, "the conversation is empty")
	}
	return out, nil
}
