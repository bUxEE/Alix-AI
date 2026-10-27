package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// SearchWeb runs a separate Responses API call with only the web_search tool
// and a fixed instruction + vetted query. It does not reuse turn context.
// Official shape: https://platform.openai.com/docs/guides/tools-web-search (type web_search).
func (c *Client) SearchWeb(ctx context.Context, scope contracts.Scope, req external.NativeSearchRequest) (external.NativeSearchResult, error) {
	if err := scope.Validate(); err != nil {
		return external.NativeSearchResult{}, err
	}
	q := strings.TrimSpace(req.Query)
	if q == "" {
		return external.NativeSearchResult{}, contracts.New(contracts.ErrValidationFailed, "openai: search query is empty")
	}
	tool := map[string]any{"type": "web_search"}
	if len(req.AllowDomains) > 0 {
		tool["filters"] = map[string]any{"allowed_domains": req.AllowDomains}
	}
	body := map[string]any{
		"model":             c.model.id,
		"store":             false,
		"truncation":        "disabled",
		"max_output_tokens": 1024,
		"tools":             []any{tool},
		"tool_choice":       "auto",
		"input": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type": "input_text",
						"text": "search the web for this query and list the results\n\n" + q,
					},
				},
			},
		},
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer cancel()
	started := c.clk.Now()
	rep, err := c.call(ctx, scope, routes[0], body, "application/json")
	if err != nil {
		return external.NativeSearchResult{}, err
	}
	defer rep.resp.Body.Close()
	raw, err := c.readBody(ctx, rep.resp.Body)
	if err != nil {
		return external.NativeSearchResult{}, err
	}
	var ar searchAPIResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return external.NativeSearchResult{}, c.wrapDecode("search response")
	}
	out := parseSearchResponse(ar)
	c.logCall(ctx, routes[0], http.StatusOK, started, nil, nil)
	return out, nil
}

type searchAPIResponse struct {
	Output []searchAPIItem `json:"output"`
	Usage  *apiUsage       `json:"usage"`
}

type searchAPIItem struct {
	Type    string             `json:"type"`
	Status  string             `json:"status"`
	Action  *searchAPIAction   `json:"action"`
	Content []searchAPIContent `json:"content"`
}

type searchAPIAction struct {
	Type  string `json:"type"`
	Query string `json:"query"`
}

type searchAPIContent struct {
	Type        string                `json:"type"`
	Text        string                `json:"text"`
	Annotations []searchAPIAnnotation `json:"annotations"`
}

type searchAPIAnnotation struct {
	Type  string `json:"type"`
	URL   string `json:"url"`
	Title string `json:"title"`
}

func parseSearchResponse(ar searchAPIResponse) external.NativeSearchResult {
	out := external.NativeSearchResult{}
	if ar.Usage != nil {
		if ar.Usage.InputTokens != nil {
			out.ToksIn = *ar.Usage.InputTokens
		}
		if ar.Usage.OutputTokens != nil {
			out.ToksOut = *ar.Usage.OutputTokens
		}
	}
	seen := map[string]bool{}
	for _, it := range ar.Output {
		if it.Type == "web_search_call" {
			if it.Action != nil && it.Action.Type == "search" {
				out.SearchCalls++
			} else if it.Status == "completed" {
				out.SearchCalls++
			}
			continue
		}
		if it.Type != "message" {
			continue
		}
		for _, c := range it.Content {
			for _, a := range c.Annotations {
				if a.Type != "url_citation" || a.URL == "" || seen[a.URL] {
					continue
				}
				seen[a.URL] = true
				out.Hits = append(out.Hits, external.NativeSearchHit{
					URL: a.URL, Title: a.Title, Snippet: truncateSnippet(c.Text),
				})
			}
		}
	}
	if out.SearchCalls == 0 && len(out.Hits) > 0 {
		out.SearchCalls = 1
	}
	return out
}

func truncateSnippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 500 {
		return s[:500]
	}
	return s
}
