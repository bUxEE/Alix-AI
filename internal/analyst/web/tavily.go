package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

const tavilySearchURL = "https://api.tavily.com/search"

// Tavily is the Tavily Search API adapter.
type Tavily struct {
	Secrets SecretResolver
	CredRef string
	Client  *http.Client
	Timeout time.Duration
}

func (t *Tavily) Name() string { return "tavily" }

func (t *Tavily) Search(ctx context.Context, query string) ([]SearchHit, error) {
	if t == nil || t.Secrets == nil || strings.TrimSpace(t.CredRef) == "" {
		return nil, contracts.New(contracts.ErrUnavailable, "web: tavily credential is not configured")
	}
	secret, err := t.Secrets.Resolve(ctx, t.CredRef)
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]any{
		"api_key":        secret,
		"query":          query,
		"max_results":    10,
		"include_answer": false,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tavilySearchURL, bytes.NewReader(payload))
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrInternal, err, "web: tavily request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := t.Client
	if client == nil {
		client = &http.Client{Timeout: pickTimeout(t.Timeout)}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, stripURL(err), "web: tavily search failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "web: tavily body")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, contracts.New(contracts.ErrUnavailable, "web: tavily returned a non-success status").
			WithDetails("http_status", resp.StatusCode)
	}
	return parseTavily(body)
}

type tavilyResp struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

func parseTavily(body []byte) ([]SearchHit, error) {
	var r tavilyResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, contracts.Wrap(contracts.ErrSchemaInvalid, err, "web: tavily response is not JSON")
	}
	var out []SearchHit
	for _, it := range r.Results {
		if it.URL == "" {
			continue
		}
		out = append(out, SearchHit{URL: it.URL, Title: it.Title, Snippet: it.Content})
	}
	return out, nil
}
