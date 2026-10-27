package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

const braveSearchURL = "https://api.search.brave.com/res/v1/web/search"

// Brave is the Brave Search API adapter (official API, API key via cred_ref).
type Brave struct {
	Secrets SecretResolver
	CredRef string
	Client  *http.Client // optional; tests inject a fixture transport
	Timeout time.Duration
}

func (b *Brave) Name() string { return "brave" }

func (b *Brave) Search(ctx context.Context, query string) ([]SearchHit, error) {
	if b == nil || b.Secrets == nil || strings.TrimSpace(b.CredRef) == "" {
		return nil, contracts.New(contracts.ErrUnavailable, "web: brave credential is not configured")
	}
	secret, err := b.Secrets.Resolve(ctx, b.CredRef)
	if err != nil {
		return nil, err
	}
	u, _ := url.Parse(braveSearchURL)
	q := u.Query()
	q.Set("q", query)
	q.Set("count", "10")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrInternal, err, "web: brave request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", secret)
	client := b.Client
	if client == nil {
		client = &http.Client{Timeout: pickTimeout(b.Timeout)}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, stripURL(err), "web: brave search failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "web: brave body")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, contracts.New(contracts.ErrUnavailable, "web: brave returned a non-success status").
			WithDetails("http_status", resp.StatusCode)
	}
	return parseBrave(body)
}

type braveResp struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"results"`
	} `json:"web"`
}

func parseBrave(body []byte) ([]SearchHit, error) {
	var r braveResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, contracts.Wrap(contracts.ErrSchemaInvalid, err, "web: brave response is not JSON")
	}
	var out []SearchHit
	for _, it := range r.Web.Results {
		if it.URL == "" {
			continue
		}
		out = append(out, SearchHit{URL: it.URL, Title: it.Title, Snippet: it.Description})
	}
	return out, nil
}

func pickTimeout(d time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return defaultHTTPTimeout
}
