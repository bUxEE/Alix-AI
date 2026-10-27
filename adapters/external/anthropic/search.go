package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// SearchWeb runs a separate Messages API call with the server web_search tool
// and only a fixed instruction + vetted query.
// Official: https://docs.anthropic.com/en/docs/agents-and-tools/tool-use/web-search-tool
// Tool type web_search_20250305; allowed_domains + max_uses.
func (c *Client) SearchWeb(ctx context.Context, scope contracts.Scope, req external.NativeSearchRequest) (external.NativeSearchResult, error) {
	if err := scope.Validate(); err != nil {
		return external.NativeSearchResult{}, err
	}
	q := strings.TrimSpace(req.Query)
	if q == "" {
		return external.NativeSearchResult{}, contracts.New(contracts.ErrValidationFailed, "anthropic: search query is empty")
	}
	maxUses := req.MaxUses
	if maxUses < 1 {
		maxUses = 1
	}
	tool := map[string]any{
		"type": "web_search_20250305", "name": "web_search", "max_uses": maxUses,
	}
	if len(req.AllowDomains) > 0 {
		tool["allowed_domains"] = req.AllowDomains
	}
	body := map[string]any{
		"model":      c.model.id,
		"max_tokens": 1024,
		"messages": []any{
			map[string]any{
				"role":    "user",
				"content": "search the web for this query and list the results\n\n" + q,
			},
		},
		"tools": []any{tool},
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
	var am searchMessage
	if err := json.Unmarshal(raw, &am); err != nil {
		return external.NativeSearchResult{}, c.wrapDecode("search response")
	}
	out := parseAnthropicSearch(am)
	c.logCall(ctx, routes[0], http.StatusOK, started, nil, nil)
	return out, nil
}

type searchMessage struct {
	Content []searchBlock `json:"content"`
	Usage   *apiUsage     `json:"usage"`
}

type searchBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	Citations []searchCite    `json:"citations"`
	Content   json.RawMessage `json:"content"` // web_search_tool_result
}

type searchCite struct {
	Type  string `json:"type"`
	URL   string `json:"url"`
	Title string `json:"title"`
}

func parseAnthropicSearch(am searchMessage) external.NativeSearchResult {
	out := external.NativeSearchResult{}
	if am.Usage != nil {
		if am.Usage.InputTokens != nil {
			out.ToksIn = *am.Usage.InputTokens
		}
		if am.Usage.OutputTokens != nil {
			out.ToksOut = *am.Usage.OutputTokens
		}
	}
	seen := map[string]bool{}
	for _, b := range am.Content {
		if b.Type == "server_tool_use" && b.Name == "web_search" {
			out.SearchCalls++
		}
		if b.Type == "web_search_tool_result" {
			out.SearchCalls++
			var results []struct {
				Type    string `json:"type"`
				URL     string `json:"url"`
				Title   string `json:"title"`
				Snippet string `json:"encrypted_content"` // fallback ignored
			}
			_ = json.Unmarshal(b.Content, &results)
			for _, r := range results {
				if r.URL == "" || seen[r.URL] {
					continue
				}
				seen[r.URL] = true
				out.Hits = append(out.Hits, external.NativeSearchHit{URL: r.URL, Title: r.Title})
			}
		}
		for _, c := range b.Citations {
			if (c.Type == "web_search_result_location" || c.URL != "") && c.URL != "" && !seen[c.URL] {
				seen[c.URL] = true
				out.Hits = append(out.Hits, external.NativeSearchHit{URL: c.URL, Title: c.Title, Snippet: truncateRunes(b.Text, 500)})
			}
		}
	}
	if out.SearchCalls == 0 && len(out.Hits) > 0 {
		out.SearchCalls = 1
	}
	return out
}

func truncateRunes(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
