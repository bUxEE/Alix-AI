package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// apiMessage is the part of a Messages API object that is read. Everything
// else (ids of blocks, citations, container, service tier, the echoed request)
// is ignored.
type apiMessage struct {
	ID         string     `json:"id"`
	Type       string     `json:"type"`
	Model      string     `json:"model"`
	Content    []apiBlock `json:"content"`
	StopReason *string    `json:"stop_reason"`
	Usage      *apiUsage  `json:"usage"`
}

type apiBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type apiUsage struct {
	InputTokens         *int `json:"input_tokens"`
	OutputTokens        *int `json:"output_tokens"`
	CacheCreationTokens *int `json:"cache_creation_input_tokens"`
	CacheReadTokens     *int `json:"cache_read_input_tokens"`
}

// Generate implements providers.MultimodalGenerator: one POST, one answer. The
// adapter never follows a tool call with a second request; a proposal goes back
// to the caller as data.
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
	var am apiMessage
	if err := json.Unmarshal(raw, &am); err != nil {
		return providers.RichResult{}, c.wrapDecode("response")
	}
	res, err := c.result(p, am, rep)
	if err != nil {
		return providers.RichResult{}, err
	}
	res.Timings = providers.Timings{Total: c.clk.Now().Sub(started)}
	c.logCall(ctx, routes[0], http.StatusOK, started, nil, &res.Usage)
	return res, nil
}

// price turns the provider's token counts into the usage record. The provider
// reports counts and no money, so the cost is computed from the price table and
// flagged Estimated. The input count is the total of fresh, cache-read and
// cache-write tokens, every one billed at no less than the base input rate, and
// a cache write is counted twice because it bills up to twice the base rate (the
// one-hour lifetime). The adapter never sends cache_control, so the cache counts
// are expected to be zero; the rule only keeps the figure an upper bound if the
// provider ever reports otherwise.
func (c *Client) price(p modelProfile, u *apiUsage) (providers.CostUsage, error) {
	if u == nil || u.InputTokens == nil || u.OutputTokens == nil {
		return providers.CostUsage{}, malformed("the response carries no usage")
	}
	in := *u.InputTokens
	if u.CacheReadTokens != nil {
		in += *u.CacheReadTokens
	}
	if u.CacheCreationTokens != nil {
		in += 2 * *u.CacheCreationTokens
	}
	cu := providers.CostUsage{ToksIn: in, ToksOut: *u.OutputTokens, Estimated: true}
	if err := cu.Validate(); err != nil {
		return providers.CostUsage{}, err
	}
	if p.prices == nil {
		return cu, nil
	}
	priced, err := p.prices.Cost(cu)
	if err != nil {
		return providers.CostUsage{}, contracts.Wrap(contracts.ErrValidationFailed, err, "anthropic: the usage cannot be priced")
	}
	return priced, nil
}

// finishFor maps a stop_reason to the contract's finish reason.
func finishFor(stop *string) (providers.FinishReason, error) {
	if stop == nil {
		return "", malformed("the response has no stop reason")
	}
	switch *stop {
	case "end_turn", "stop_sequence":
		return providers.FinishStop, nil
	case "max_tokens", "model_context_window_exceeded":
		return providers.FinishLength, nil
	case "tool_use":
		return providers.FinishToolCalls, nil
	case "refusal":
		return providers.FinishFiltered, nil
	}
	// pause_turn belongs to provider-run tools, which were never requested.
	return "", malformed("the response stopped for an unexpected reason")
}

// result turns a decoded message into the contract's result.
func (c *Client) result(p *prepared, am apiMessage, rep *reply) (providers.RichResult, error) {
	if am.Type != "message" {
		return providers.RichResult{}, malformed("the response is not a message")
	}
	finish, err := finishFor(am.StopReason)
	if err != nil {
		return providers.RichResult{}, err
	}
	var parts []providers.ContentPart
	var calls []providers.ToolCallProposal
	for _, b := range am.Content {
		switch b.Type {
		case "thinking", "redacted_thinking":
			// Reasoning blocks are provider-internal and are not carried.
		case "text":
			if b.Text != "" {
				parts = append(parts, providers.TextPart(b.Text))
			}
		case "tool_use":
			calls = append(calls, providers.ToolCallProposal{ID: b.ID, Name: b.Name, Arguments: append(json.RawMessage(nil), b.Input...)})
		default:
			// A provider-run tool block (server_tool_use, web_search_tool_result,
			// mcp_tool_use, code execution, ...) or anything unknown: nothing of
			// the sort was requested, so the whole answer is rejected.
			return providers.RichResult{}, malformed("the response holds a block that was not requested")
		}
	}
	calls, err = providers.ValidateProposals(calls, p.req.Tools, p.req.ToolChoice)
	if err != nil {
		return providers.RichResult{}, err
	}
	switch {
	case finish == providers.FinishFiltered:
	case len(calls) > 0:
		finish = providers.FinishToolCalls
	case finish == providers.FinishToolCalls:
		return providers.RichResult{}, malformed("the response stopped for a tool call but holds none")
	}
	usage, err := c.price(p.model, am.Usage)
	if err != nil {
		return providers.RichResult{}, err
	}
	res := providers.RichResult{
		Parts: parts, ToolCalls: calls, FinishReason: finish, Usage: usage, RateLimit: rep.rate,
		TraceID: rep.traceID, Model: am.Model,
	}
	if res.TraceID == "" && external.ExtTraceID(am.ID) {
		res.TraceID = am.ID
	}
	if p.structured {
		if finish == providers.FinishFiltered {
			// A refusal does not follow the schema; it is never a result.
			return providers.RichResult{}, malformed("the model refused and produced no structured output")
		}
		j, err := providers.ParseJSONOutput(res.Text())
		if err != nil {
			return providers.RichResult{}, err
		}
		res.JSON = bytes.TrimSpace(j)
	}
	return res, nil
}
