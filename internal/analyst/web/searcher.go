package web

import (
	"context"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
)

// WebSearcher runs one web search for an analyst turn.
type WebSearcher interface {
	Search(ctx context.Context, turn *Turn, query string, datasets []an.Dataset, policyAllows bool) (SearchResult, error)
}

// Provider is a concrete search backend (Brave, Tavily, SearXNG).
type Provider interface {
	Name() string
	Search(ctx context.Context, query string) ([]SearchHit, error)
}

// Searcher is the default WebSearcher: egress check → provider → record hits.
type Searcher struct {
	Provider Provider
}

// NewSearcher wraps a Provider.
func NewSearcher(p Provider) *Searcher { return &Searcher{Provider: p} }

// Search checks web_query egress, budget and policy, then calls the provider.
func (s *Searcher) Search(ctx context.Context, turn *Turn, query string, datasets []an.Dataset, policyAllows bool) (SearchResult, error) {
	if s == nil || s.Provider == nil {
		return SearchResult{}, contracts.New(contracts.ErrUnavailable, "web: search provider is not configured")
	}
	if err := PolicyAllowsWebQuery(policyAllows); err != nil {
		return SearchResult{}, err
	}
	if err := CheckQuery(query, datasets); err != nil {
		return SearchResult{}, err
	}
	if turn == nil || !turn.takeSearch() {
		return SearchResult{}, contracts.New(contracts.ErrAnalystCapExceeded, "web: search budget exhausted")
	}
	hits, err := s.Provider.Search(ctx, strings.TrimSpace(query))
	if err != nil {
		return SearchResult{}, err
	}
	if len(hits) > 20 {
		hits = hits[:20]
	}
	for i := range hits {
		hits[i].URL = normalizeURL(hits[i].URL)
		hits[i].Title = truncateRunes(collapseSpace(hits[i].Title), 512)
		hits[i].Snippet = truncateRunes(collapseSpace(hits[i].Snippet), 2000)
	}
	turn.RecordHits(hits)
	return SearchResult{Hits: hits, Fenced: fenceHits(hits)}, nil
}
