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

// SearXNG is a self-hosted SearXNG adapter (official JSON API).
type SearXNG struct {
	Origin  string // https origin, no path/query/fragment
	Secrets SecretResolver
	CredRef string // optional
	Client  *http.Client
	Timeout time.Duration
}

func (s *SearXNG) Name() string { return "searxng" }

func (s *SearXNG) Search(ctx context.Context, query string) ([]SearchHit, error) {
	if s == nil || strings.TrimSpace(s.Origin) == "" {
		return nil, contracts.New(contracts.ErrUnavailable, "web: searxng origin is not configured")
	}
	origin, err := url.Parse(strings.TrimRight(strings.TrimSpace(s.Origin), "/"))
	if err != nil || (origin.Scheme != "https" && origin.Scheme != "http") || origin.Host == "" || origin.RawQuery != "" || origin.Fragment != "" {
		return nil, contracts.New(contracts.ErrValidationFailed, "web: searxng origin is malformed")
	}
	u := *origin
	u.Path = strings.TrimRight(origin.Path, "/") + "/search"
	q := u.Query()
	q.Set("q", query)
	q.Set("format", "json")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrInternal, err, "web: searxng request")
	}
	req.Header.Set("Accept", "application/json")
	if s.Secrets != nil && strings.TrimSpace(s.CredRef) != "" {
		secret, err := s.Secrets.Resolve(ctx, s.CredRef)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: pickTimeout(s.Timeout)}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, stripURL(err), "web: searxng search failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "web: searxng body")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, contracts.New(contracts.ErrUnavailable, "web: searxng returned a non-success status").
			WithDetails("http_status", resp.StatusCode)
	}
	return parseSearXNG(body)
}

type searxResp struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

func parseSearXNG(body []byte) ([]SearchHit, error) {
	var r searxResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, contracts.Wrap(contracts.ErrSchemaInvalid, err, "web: searxng response is not JSON")
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
