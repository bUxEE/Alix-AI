package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// apiResponse is the part of a GenerateContentResponse that is read. Everything
// else (safety ratings, citations, the echoed request) is ignored. The same
// shape is the payload of every server-sent event of a stream.
type apiResponse struct {
	Candidates     []apiCandidate `json:"candidates"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	UsageMetadata *apiUsage `json:"usageMetadata"`
	ModelVersion  string    `json:"modelVersion"`
	ResponseID    string    `json:"responseId"`
	// Error is set when a stream event carries an error object instead.
	Error *json.RawMessage `json:"error"`
}

type apiCandidate struct {
	Content *struct {
		Role  string    `json:"role"`
		Parts []apiPart `json:"parts"`
	} `json:"content"`
	FinishReason string `json:"finishReason"`
	// Grounding and url-context metadata only exist when a provider tool was
	// requested; none ever is.
	GroundingMetadata  json.RawMessage `json:"groundingMetadata"`
	URLContextMetadata json.RawMessage `json:"urlContextMetadata"`
}

// apiPart is one part of a candidate. Only text, thought, thoughtSignature and
// functionCall are known; any other member (inlineData, fileData,
// executableCode, codeExecutionResult, functionResponse, toolCall, ...) marks
// the part as one that was not requested.
type apiPart struct {
	Text             string
	Thought          bool
	ThoughtSignature string
	Call             *apiFunctionCall
	Unrequested      bool
}

type apiFunctionCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// UnmarshalJSON reads the known members and flags every other.
func (p *apiPart) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	for k, v := range raw {
		var err error
		switch k {
		case "text":
			err = json.Unmarshal(v, &p.Text)
		case "thought":
			err = json.Unmarshal(v, &p.Thought)
		case "thoughtSignature":
			err = json.Unmarshal(v, &p.ThoughtSignature)
		case "functionCall":
			p.Call = new(apiFunctionCall)
			err = json.Unmarshal(v, p.Call)
		case "videoMetadata":
			// metadata of a media part; the media member decides
		default:
			p.Unrequested = true
		}
		if err != nil {
			return err
		}
	}
	return nil
}

type apiUsage struct {
	PromptTokenCount        *int `json:"promptTokenCount"`
	CandidatesTokenCount    *int `json:"candidatesTokenCount"`
	ThoughtsTokenCount      *int `json:"thoughtsTokenCount"`
	ToolUsePromptTokenCount *int `json:"toolUsePromptTokenCount"`
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
	rep, err := c.call(ctx, scope, p.route(), p.body, "application/json")
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
	res, err := c.result(p, ar)
	if err != nil {
		return providers.RichResult{}, err
	}
	res.Timings = providers.Timings{Total: c.clk.Now().Sub(started)}
	c.logCall(ctx, p.route(), http.StatusOK, started, nil, &res.Usage)
	return res, nil
}

// price turns the provider's token counts into the usage record. The provider
// reports counts and no money, so the cost is computed from the price table and
// flagged Estimated. Output is the candidates' tokens plus the thinking tokens
// (thinking bills as output); input is the prompt, which already includes any
// cached tokens, plus the tokens of tool-use prompts. A cached token bills at
// no more than the base input rate, so the figure stays an upper bound.
func (c *Client) price(p modelProfile, u *apiUsage) (providers.CostUsage, error) {
	if u == nil || u.PromptTokenCount == nil {
		return providers.CostUsage{}, malformed("the response carries no usage")
	}
	in := *u.PromptTokenCount
	if u.ToolUsePromptTokenCount != nil {
		in += *u.ToolUsePromptTokenCount
	}
	out := 0
	if u.CandidatesTokenCount != nil {
		out += *u.CandidatesTokenCount
	}
	if u.ThoughtsTokenCount != nil {
		out += *u.ThoughtsTokenCount
	}
	cu := providers.CostUsage{ToksIn: in, ToksOut: out, Estimated: true}
	if err := cu.Validate(); err != nil {
		return providers.CostUsage{}, err
	}
	if p.prices == nil {
		return cu, nil
	}
	priced, err := p.prices.Cost(cu)
	if err != nil {
		return providers.CostUsage{}, contracts.Wrap(contracts.ErrValidationFailed, err, "gemini: the usage cannot be priced")
	}
	return priced, nil
}

// finishFor maps a finishReason (and whether the answer holds a function call)
// to the contract's finish reason. A reason that says the model failed to
// produce what was asked is an error, never a result.
func finishFor(reason string, calls bool) (providers.FinishReason, error) {
	switch reason {
	case "STOP":
		if calls {
			return providers.FinishToolCalls, nil
		}
		return providers.FinishStop, nil
	case "MAX_TOKENS":
		return providers.FinishLength, nil
	case "SAFETY", "RECITATION", "LANGUAGE", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII",
		"IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION":
		return providers.FinishFiltered, nil
	}
	// MALFORMED_FUNCTION_CALL, UNEXPECTED_TOOL_CALL, TOO_MANY_TOOL_CALLS,
	// MISSING_THOUGHT_SIGNATURE, MALFORMED_RESPONSE, OTHER, no reason at all
	// and anything new.
	return "", malformed("the response stopped for an unexpected reason")
}

// blocked is true for a prompt the provider refused to answer.
func blocked(r *apiResponse) bool {
	return len(r.Candidates) == 0 && r.PromptFeedback != nil && r.PromptFeedback.BlockReason != ""
}

// candidate returns the single candidate of an answer, or nil for a blocked
// prompt; more than one candidate was never requested.
func candidate(r *apiResponse) (*apiCandidate, error) {
	switch {
	case len(r.Candidates) > 1:
		return nil, malformed("the response holds more candidates than requested")
	case len(r.Candidates) == 1:
		cand := &r.Candidates[0]
		if len(cand.GroundingMetadata) > 0 || len(cand.URLContextMetadata) > 0 {
			return nil, malformed("the response holds provider tool results that were not requested")
		}
		return cand, nil
	case blocked(r):
		return nil, nil
	}
	return nil, malformed("the response holds no candidate")
}

// result turns a decoded response into the contract's result.
func (c *Client) result(p *prepared, ar apiResponse) (providers.RichResult, error) {
	if ar.Error != nil {
		return providers.RichResult{}, malformed("the response is an error document")
	}
	cand, err := candidate(&ar)
	if err != nil {
		return providers.RichResult{}, err
	}
	var parts []providers.ContentPart
	var calls []providers.ToolCallProposal
	var sigs []string
	reason := ""
	if cand != nil {
		reason = cand.FinishReason
		if cand.Content != nil {
			for _, pt := range cand.Content.Parts {
				switch {
				case pt.Unrequested:
					// Media, code execution or a provider-run tool: nothing of
					// the sort was requested, so the whole answer is rejected.
					return providers.RichResult{}, malformed("the response holds a part that was not requested")
				case pt.Call != nil:
					id := c.ids.callID(pt.Call.ID, ar.ResponseID, len(calls))
					calls = append(calls, providers.ToolCallProposal{ID: id, Name: pt.Call.Name, Arguments: append(json.RawMessage(nil), pt.Call.Args...)})
					sigs = append(sigs, pt.ThoughtSignature)
				case pt.Thought:
					// A thought summary is provider-internal and is not carried.
				case pt.Text != "":
					parts = append(parts, providers.TextPart(pt.Text))
				}
			}
		}
	}
	var finish providers.FinishReason
	if cand == nil {
		finish = providers.FinishFiltered
	} else if finish, err = finishFor(reason, len(calls) > 0); err != nil {
		return providers.RichResult{}, err
	}
	calls, err = providers.ValidateProposals(calls, p.req.Tools, p.req.ToolChoice)
	if err != nil {
		return providers.RichResult{}, err
	}
	for i, cl := range calls {
		c.sigs.put(p.scope, cl.ID, sigs[i])
	}
	usage, err := c.price(p.model, ar.UsageMetadata)
	if err != nil {
		return providers.RichResult{}, err
	}
	res := providers.RichResult{
		Parts: parts, ToolCalls: calls, FinishReason: finish, Usage: usage,
		Model: ar.ModelVersion,
	}
	if external.ExtTraceID(ar.ResponseID) {
		res.TraceID = ar.ResponseID
	}
	if p.structured {
		if finish == providers.FinishFiltered {
			// A blocked answer does not follow the schema; it is never a result.
			return providers.RichResult{}, malformed("the provider filtered the answer and produced no structured output")
		}
		j, err := providers.ParseJSONOutput(res.Text())
		if err != nil {
			return providers.RichResult{}, err
		}
		res.JSON = bytes.TrimSpace(j)
	}
	return res, nil
}
