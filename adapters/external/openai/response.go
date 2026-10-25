package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// apiResponse is the part of a Responses API object that is read. Everything
// else (ids of output items, annotations, reasoning summaries, the echoed
// request) is ignored.
type apiResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Model  string `json:"model"`
	Error  *struct {
		Code string `json:"code"`
	} `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []apiItem `json:"output"`
	Usage  *apiUsage `json:"usage"`
}

type apiItem struct {
	ID        string       `json:"id"`
	Type      string       `json:"type"`
	Content   []apiContent `json:"content"`
	CallID    string       `json:"call_id"`
	Name      string       `json:"name"`
	Arguments string       `json:"arguments"`
}

type apiContent struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

type apiUsage struct {
	InputTokens  *int `json:"input_tokens"`
	OutputTokens *int `json:"output_tokens"`
}

// Generate implements providers.MultimodalGenerator: one POST, one answer. The
// adapter never follows a function call with a second request; a proposal goes
// back to the caller as data.
func (c *Client) Generate(ctx context.Context, scope contracts.Scope, req providers.RichRequest) (providers.RichResult, error) {
	p, err := c.prepare(scope, req, false)
	if err != nil {
		return providers.RichResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer cancel()
	started := c.clk.Now()
	rep, err := c.call(ctx, scope, routes[0], p.body, "application/json")
	if err != nil {
		return providers.RichResult{}, err
	}
	defer rep.resp.Body.Close()
	raw, err := c.readBody(ctx, rep.resp.Body)
	if err != nil {
		return providers.RichResult{}, err
	}
	var ar apiResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return providers.RichResult{}, c.wrapDecode("response")
	}
	res, err := c.result(p, ar, rep)
	if err != nil {
		return providers.RichResult{}, err
	}
	res.Timings = providers.Timings{Total: c.clk.Now().Sub(started)}
	c.logCall(ctx, routes[0], http.StatusOK, started, nil, &res.Usage)
	return res, nil
}

// price turns the provider's token counts into the usage record. The provider
// reports counts and no money, so the cost is computed from the price table
// and flagged Estimated. Cached input is billed at the full rate, which keeps
// the figure an upper bound.
func (c *Client) price(p modelProfile, u *apiUsage) (providers.CostUsage, error) {
	if u == nil || u.InputTokens == nil || u.OutputTokens == nil {
		return providers.CostUsage{}, malformed("the response carries no usage")
	}
	cu := providers.CostUsage{ToksIn: *u.InputTokens, ToksOut: *u.OutputTokens, Estimated: true}
	if err := cu.Validate(); err != nil {
		return providers.CostUsage{}, err
	}
	if p.prices == nil {
		return cu, nil
	}
	priced, err := p.prices.Cost(cu)
	if err != nil {
		return providers.CostUsage{}, contracts.Wrap(contracts.ErrValidationFailed, err, "openai: the usage cannot be priced")
	}
	return priced, nil
}

// result turns a decoded response object into the contract's result.
func (c *Client) result(p *prepared, ar apiResponse, rep *reply) (providers.RichResult, error) {
	finish := providers.FinishStop
	switch ar.Status {
	case "completed":
	case "incomplete":
		reason := ""
		if ar.IncompleteDetails != nil {
			reason = ar.IncompleteDetails.Reason
		}
		switch reason {
		case "max_output_tokens":
			finish = providers.FinishLength
		case "content_filter":
			finish = providers.FinishFiltered
		default:
			return providers.RichResult{}, malformed("the response is incomplete for an unknown reason")
		}
	case "failed":
		code := ""
		if ar.Error != nil {
			code = ar.Error.Code
		}
		return providers.RichResult{}, mapFailureCode(code)
	default:
		return providers.RichResult{}, malformed("the response has an unexpected status")
	}

	var parts []providers.ContentPart
	var calls []providers.ToolCallProposal
	refused := false
	for _, it := range ar.Output {
		switch it.Type {
		case "reasoning":
			// Reasoning items are provider-internal and are not carried.
		case "message":
			for _, ct := range it.Content {
				switch ct.Type {
				case "output_text":
					if ct.Text != "" {
						parts = append(parts, providers.TextPart(ct.Text))
					}
				case "refusal":
					refused = true
				default:
					return providers.RichResult{}, malformed("the response holds an unknown content type")
				}
			}
		case "function_call":
			calls = append(calls, providers.ToolCallProposal{ID: it.CallID, Name: it.Name, Arguments: json.RawMessage(it.Arguments)})
		default:
			// A hosted tool call (web search, file search, MCP, code
			// interpreter, ...) was never requested: reject the whole answer.
			return providers.RichResult{}, malformed("the response holds an item that was not requested")
		}
	}

	calls, err := providers.ValidateProposals(calls, p.req.Tools, p.req.ToolChoice)
	if err != nil {
		return providers.RichResult{}, err
	}
	for i := range calls {
		args, err := p.tools[calls[i].Name].Restore(calls[i].Arguments)
		if err != nil {
			return providers.RichResult{}, err
		}
		calls[i].Arguments = args
	}
	switch {
	case refused:
		finish = providers.FinishFiltered
	case len(calls) > 0:
		finish = providers.FinishToolCalls
	}

	usage, err := c.price(p.model, ar.Usage)
	if err != nil {
		return providers.RichResult{}, err
	}
	res := providers.RichResult{
		Parts: parts, ToolCalls: calls, FinishReason: finish, Usage: usage, RateLimit: rep.rate,
		TraceID: rep.traceID, Model: ar.Model,
	}
	if res.TraceID == "" && external.ExtTraceID(ar.ID) {
		res.TraceID = ar.ID
	}
	if p.outPlan != nil {
		if refused {
			// A refusal does not follow the schema; it is never a result.
			return providers.RichResult{}, schemaInvalid("the model refused and produced no structured output")
		}
		j, err := providers.ParseJSONOutput(res.Text())
		if err != nil {
			return providers.RichResult{}, err
		}
		if j, err = p.outPlan.Restore(j); err != nil {
			return providers.RichResult{}, err
		}
		res.JSON = bytes.TrimSpace(j)
	}
	return res, nil
}
