package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// apiCompletion is the part of a chat.completion object that is read.
// Everything else (system fingerprint, logprobs, the echoed request, host
// extensions) is ignored, except the message members listed in hostRunKeys.
type apiCompletion struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Model   string      `json:"model"`
	Choices []apiChoice `json:"choices"`
	Usage   *apiUsage   `json:"usage"`
}

type apiChoice struct {
	Index        int            `json:"index"`
	Message      *apiMessageRaw `json:"message"`
	FinishReason *string        `json:"finish_reason"`
}

// apiMessageRaw keeps the message as a map so that members which signal work
// done by the host's own tools can be recognised.
type apiMessageRaw map[string]json.RawMessage

type apiUsage struct {
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
}

type apiToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// hostRunKeys are message members that report work the host's own tools did
// (a built-in web search, code execution, a legacy function call, audio or image
// output). Nothing of the sort is ever requested, so an answer that carries one
// is rejected.
var hostRunKeys = []string{"executed_tools", "function_call", "audio", "images", "search_results"}

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
	rep, err := c.call(ctx, scope, c.routes.chat, p.body, "application/json")
	if err != nil {
		return providers.RichResult{}, err
	}
	defer rep.resp.Body.Close()
	raw, err := c.readBody(ctx, rep.resp.Body)
	if err != nil {
		return providers.RichResult{}, err
	}
	var am apiCompletion
	if err := json.Unmarshal(raw, &am); err != nil {
		return providers.RichResult{}, c.wrapDecode("response")
	}
	res, err := c.result(p, am, rep)
	if err != nil {
		return providers.RichResult{}, err
	}
	res.Timings = providers.Timings{Total: c.clk.Now().Sub(started)}
	c.logCall(ctx, c.routes.chat, http.StatusOK, started, nil, &res.Usage)
	return res, nil
}

// price turns the provider's token counts into the usage record. The provider
// reports counts and no money, so the cost is computed from the price table and
// flagged Estimated.
func (c *Client) price(p modelProfile, u *apiUsage) (providers.CostUsage, error) {
	if u == nil || u.PromptTokens == nil || u.CompletionTokens == nil {
		return providers.CostUsage{}, malformed("the response carries no usage")
	}
	cu := providers.CostUsage{ToksIn: *u.PromptTokens, ToksOut: *u.CompletionTokens, Estimated: true}
	if err := cu.Validate(); err != nil {
		return providers.CostUsage{}, err
	}
	if p.prices == nil {
		return cu, nil
	}
	priced, err := p.prices.Cost(cu)
	if err != nil {
		return providers.CostUsage{}, contracts.Wrap(contracts.ErrValidationFailed, err, "meta: the usage cannot be priced")
	}
	return priced, nil
}

// finishFor maps a finish_reason to the contract's finish reason. An absent or
// unknown value is not guessed at.
func finishFor(reason *string) (providers.FinishReason, error) {
	if reason == nil {
		return "", malformed("the response has no finish reason")
	}
	switch *reason {
	case "stop":
		return providers.FinishStop, nil
	case "length":
		return providers.FinishLength, nil
	case "tool_calls":
		return providers.FinishToolCalls, nil
	case "content_filter":
		return providers.FinishFiltered, nil
	}
	return "", malformed("the response stopped for an unexpected reason")
}

// contentText reads message.content: a string, null, or a list of text parts.
// A part of any other type (an image, audio, a refusal object) rejects the
// answer: nothing but text was requested.
func contentText(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", nil
	}
	var s string
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", malformed("the message content is not text")
		}
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", malformed("the message content is not text")
	}
	for _, p := range parts {
		if p.Type != "text" {
			return "", malformed("the message holds a part that was not requested")
		}
		s += p.Text
	}
	return s, nil
}

// result turns a decoded completion into the contract's result.
func (c *Client) result(p *prepared, am apiCompletion, rep *reply) (providers.RichResult, error) {
	if len(am.Choices) != 1 || am.Choices[0].Message == nil {
		return providers.RichResult{}, malformed("the response does not hold exactly one choice")
	}
	ch := am.Choices[0]
	msg := *ch.Message
	for _, k := range hostRunKeys {
		if v, ok := msg[k]; ok && !isEmptyJSON(v) {
			return providers.RichResult{}, malformed("the response holds output that was not requested")
		}
	}
	finish, err := finishFor(ch.FinishReason)
	if err != nil {
		return providers.RichResult{}, err
	}
	text, err := contentText(msg["content"])
	if err != nil {
		return providers.RichResult{}, err
	}
	var parts []providers.ContentPart
	if text != "" {
		parts = append(parts, providers.TextPart(text))
	}
	var calls []providers.ToolCallProposal
	if rawCalls, ok := msg["tool_calls"]; ok && !isEmptyJSON(rawCalls) {
		var tcs []apiToolCall
		if err := json.Unmarshal(rawCalls, &tcs); err != nil {
			return providers.RichResult{}, malformed("the tool calls are not the expected JSON")
		}
		for _, tc := range tcs {
			if tc.Type != "function" {
				return providers.RichResult{}, malformed("the response holds a tool call that was not requested")
			}
			args := tc.Function.Arguments
			if args == "" {
				args = "{}"
			}
			calls = append(calls, providers.ToolCallProposal{ID: tc.ID, Name: tc.Function.Name, Arguments: json.RawMessage(args)})
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
		TraceID: rep.traceID, Model: c.replyModel(am.Model),
	}
	if res.TraceID == "" && external.ExtTraceID(am.ID) {
		res.TraceID = am.ID
	}
	if p.structured {
		if finish == providers.FinishFiltered {
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

// replyModel is the model a reply is reported under: Config.ReplyModel when
// the service named the configured model, else what the service named.
func (c *Client) replyModel(named string) string {
	if c.cfg.ReplyModel != "" && named == c.cfg.Model {
		return c.cfg.ReplyModel
	}
	return named
}

func isEmptyJSON(v json.RawMessage) bool {
	t := bytes.TrimSpace(v)
	return len(t) == 0 || bytes.Equal(t, []byte("null")) || bytes.Equal(t, []byte("[]")) || bytes.Equal(t, []byte("{}"))
}
