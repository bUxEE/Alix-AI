package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/adapters/external"
)

func TestSearchWebMinimalQuery(t *testing.T) {
	const query = "italian public holidays 2026"
	payload := `{
		"id":"resp_1","status":"completed","model":"gpt-4.1-mini",
		"output":[
			{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"italian public holidays 2026"}},
			{"type":"message","role":"assistant","status":"completed","content":[
				{"type":"output_text","text":"Results include gov.it.","annotations":[
					{"type":"url_citation","url":"https://gov.it/holidays","title":"Holidays"},
					{"type":"url_citation","url":"https://evil.example/x","title":"Evil"}
				]}
			]}
		],
		"usage":{"input_tokens":20,"output_tokens":40}
	}`
	r := newRig(t, replyJSON(payload), nil)
	res, err := r.client.SearchWeb(context.Background(), testScope(), external.NativeSearchRequest{
		Query: query, AllowDomains: []string{"gov.it"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.SearchCalls != 1 || len(res.Hits) != 2 {
		t.Fatalf("got %+v", res)
	}
	reqs := r.api.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests=%d", len(reqs))
	}
	body := string(reqs[0].Body)
	if !strings.Contains(body, query) {
		t.Fatalf("body missing query: %s", body)
	}
	for _, bad := range []string{"revenue", "VAT", "dataset", "attendee", "system prompt"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(bad)) {
			t.Fatalf("body leaked %q", bad)
		}
	}
	var m map[string]any
	if err := json.Unmarshal(reqs[0].Body, &m); err != nil {
		t.Fatal(err)
	}
	tools, _ := m["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%v", tools)
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "web_search" {
		t.Fatalf("tool=%v", tool)
	}
	filters, _ := tool["filters"].(map[string]any)
	domains, _ := filters["allowed_domains"].([]any)
	if len(domains) != 1 || domains[0] != "gov.it" {
		t.Fatalf("filters=%v", filters)
	}
	if reqs[0].Method != http.MethodPost {
		t.Fatalf("method=%s", reqs[0].Method)
	}
}
