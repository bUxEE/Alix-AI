package gemini

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const (
	// maxTools is the number of function declarations one request may carry.
	// The vendor's page stated no limit; this is the contract's own bound
	// (providers.ValidateToolSpecs) and keeps the request small.
	maxTools = 32
	// maxStopSequences is the documented limit of generationConfig.stopSequences.
	maxStopSequences = 5
	// maxTemperature is the upper end of the documented 0.0 to 2.0 range.
	maxTemperature = 2.0
)

// prepared is a validated request, ready to send, with what is needed to read
// the answer.
type prepared struct {
	body  map[string]any
	req   providers.RichRequest
	scope contracts.Scope
	model modelProfile
	// structured is true when a JSON schema output constraint was sent.
	structured bool
	stream     bool
}

// route is the generation route of the request's model.
func (p *prepared) route() route { return generateRoute(p.model.id, p.stream) }

// capabilities returns the capability record of a model profile.
func (c *Client) capabilities(p modelProfile) providers.ModelCapabilities {
	return providers.ModelCapabilities{
		Provider:         ProviderName,
		Model:            p.id,
		InputModalities:  []providers.Modality{providers.ModText},
		OutputModalities: []providers.Modality{providers.ModText},
		Streaming:        true,
		StructuredOutput: providers.StructuredStrict,
		Tools: providers.ToolCapability{
			Supported: true, StrictArguments: c.cfg.ToolMode != ToolModeAuto, ParallelCalls: true, MaxTools: maxTools,
		},
		MaxContextToks: p.context,
		MaxOutputToks:  p.output,
		Pricing:        p.prices,
		Metering:       providers.MeteringPerResponse,
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
	return contracts.New(contracts.ErrValidationFailed, "gemini: "+msg).WithDetails("sent", false)
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
// generateContent body. It sends nothing. Everything the contract does not
// allow is refused here (fail closed), never dropped:
//
//   - non-text parts (ADR-0024 keeps them off in the first release);
//   - a grammar constraint (the API has no grammar for json output);
//   - a temperature outside 0 to 2, more than 5 stop sequences;
//   - a conversation that does not start with a user turn, a system message
//     after the first turn, or a conversation that ends with an assistant turn
//     (a prefill);
//   - a tool result that answers no earlier proposal;
//   - a schema outside the provider's subset (see schema.go);
//   - structured output together with tools (not combined: the answer would
//     have two competing shapes).
//
// The body never asks for a provider-native feature: no grounding, code
// execution, url context, file search, cached content, safety override or
// thinking setting is ever set, and the only tool kind is the function
// declaration.
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
	if req.Temperature > maxTemperature {
		return nil, notSent(providers.UnsupportedError("temperature"))
	}
	if len(req.Stop) > maxStopSequences {
		return nil, notSent(providers.UnsupportedError("that many stop sequences"))
	}
	if len(req.Tools) > 0 && req.Output.Kind() == providers.ConstraintJSONSchema {
		return nil, notSent(providers.UnsupportedError("structured_output together with tools"))
	}

	p := &prepared{req: req.Clone(), scope: scope, model: model, stream: stream}
	maxOut := req.MaxToks
	if maxOut == 0 {
		maxOut = c.cfg.DefaultMaxOutputToks
		if maxOut > model.output {
			maxOut = model.output
		}
	}
	genCfg := map[string]any{"maxOutputTokens": maxOut}
	if req.Temperature > 0 {
		genCfg["temperature"] = req.Temperature
	}
	if len(req.Stop) > 0 {
		genCfg["stopSequences"] = req.Stop
	}
	body := map[string]any{}

	system, contents, err := c.wireMessages(scope, req.Messages)
	if err != nil {
		return nil, err
	}
	if len(system) > 0 {
		body["systemInstruction"] = map[string]any{"parts": system}
	}
	body["contents"] = contents

	if req.Output.Kind() == providers.ConstraintJSONSchema {
		schema, _, err := ConvertSchema(req.Output.JSONSchema)
		if err != nil {
			return nil, notSent(err)
		}
		p.structured = true
		genCfg["responseMimeType"] = "application/json"
		genCfg["responseJsonSchema"] = json.RawMessage(schema)
	}
	body["generationConfig"] = genCfg

	if len(req.Tools) > 0 {
		decls := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			schema, _, err := ConvertSchema(t.Parameters)
			if err != nil {
				return nil, notSent(fmt.Errorf("tool %q: %w", t.Name, err))
			}
			d := map[string]any{"name": t.Name, "parametersJsonSchema": json.RawMessage(schema)}
			if t.Description != "" {
				d["description"] = t.Description
			}
			decls = append(decls, d)
		}
		body["tools"] = []any{map[string]any{"functionDeclarations": decls}}
		mode := c.cfg.ToolMode
		switch req.ToolChoice {
		case providers.ToolChoiceNone:
			mode = "NONE"
		case providers.ToolChoiceRequired:
			// ANY constrains the answer to a call whose arguments follow the
			// declaration; an answer without one is also an error here
			// (providers.ValidateProposals).
			mode = "ANY"
		}
		body["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": mode}}
	}
	p.body = body
	return p, nil
}

// wireMessages maps the messages to generateContent: system messages (only at
// the start) become the systemInstruction parts; an assistant turn becomes a
// "model" content with its text plus one functionCall part per proposal; a run
// of tool messages becomes ONE "user" content of functionResponse parts. The
// name a functionResponse needs is looked up from the proposal it answers.
// Message order is preserved.
func (c *Client) wireMessages(scope contracts.Scope, msgs []providers.RichMessage) (system []any, out []map[string]any, err error) {
	started := false
	names := map[string]string{} // call id -> tool name
	toolResults := false         // the last content holds only functionResponse parts
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
			system = append(system, map[string]any{"text": text})
			continue
		}
		started = true
		last := len(out) - 1
		switch m.Role {
		case providers.MsgUser:
			if text == "" {
				return nil, nil, localFail(fmt.Sprintf("message %d is empty", i))
			}
			part := map[string]any{"text": text}
			if last >= 0 && toolResults {
				// text after tool results goes into the same user turn, after them
				out[last]["parts"] = append(out[last]["parts"].([]any), part)
				toolResults = false
				continue
			}
			out = append(out, map[string]any{"role": "user", "parts": []any{part}})
			toolResults = false
		case providers.MsgAssistant:
			var parts []any
			if text != "" {
				parts = append(parts, map[string]any{"text": text})
			}
			for _, call := range m.ToolCalls {
				args := json.RawMessage(call.Arguments)
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				fc := map[string]any{"name": call.Name, "args": args}
				if !synthesised(call.ID) {
					fc["id"] = call.ID
				}
				part := map[string]any{"functionCall": fc}
				if sig := c.sigs.get(scope, call.ID); sig != "" {
					part["thoughtSignature"] = sig
				}
				parts = append(parts, part)
				names[call.ID] = call.Name
			}
			if len(parts) == 0 {
				return nil, nil, localFail(fmt.Sprintf("message %d is empty", i))
			}
			out = append(out, map[string]any{"role": "model", "parts": parts})
			toolResults = false
		case providers.MsgTool:
			name, ok := names[m.ToolCallID]
			if !ok {
				return nil, nil, localFail("a tool result answers no earlier proposal")
			}
			fr := map[string]any{"name": name, "response": map[string]any{"result": text}}
			if !synthesised(m.ToolCallID) {
				fr["id"] = m.ToolCallID
			}
			part := map[string]any{"functionResponse": fr}
			if last >= 0 && toolResults {
				out[last]["parts"] = append(out[last]["parts"].([]any), part)
				continue
			}
			out = append(out, map[string]any{"role": "user", "parts": []any{part}})
			toolResults = true
		default:
			return nil, nil, localFail(fmt.Sprintf("message %d has an unknown role", i))
		}
	}
	if len(out) == 0 || out[0]["role"] != "user" {
		return nil, nil, localFail("the conversation must start with a user turn")
	}
	if out[len(out)-1]["role"] != "user" {
		return nil, nil, localFail("the conversation must end with a user turn (assistant prefill is not supported)")
	}
	return system, out, nil
}
