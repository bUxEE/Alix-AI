package web

import (
	"context"
	"time"

	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
)

// SecretResolver resolves an opaque credentials-service reference. The returned
// secret must never be logged or put in errors.
type SecretResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// SearchHit is one untrusted search result.
type SearchHit struct {
	URL     string
	Title   string
	Snippet string
}

// SearchResult is a fenced search response.
type SearchResult struct {
	Hits   []SearchHit
	Fenced string // prompt-safe fence wrapping the hits as data
}

// FetchResult is a fenced page fetch plus citation and content hash.
type FetchResult struct {
	URL         string
	Title       string
	Text        string
	ContentHash string // sha256 hex of the extracted text
	RetrievedAt time.Time
	Citation    an.WebCitation
	Fenced      string
}

// HostPolicy is the effective allow/deny host suffix policy (deny wins).
type HostPolicy struct {
	Allow []string
	Deny  []string
}

// Budget bounds searches/fetches in a turn.
type Budget struct {
	MaxSearches int
	MaxFetches  int
	searches    int
	fetches     int
}

// Turn holds per-turn state: search hits that may be fetched, and budget.
type Turn struct {
	Budget Budget
	policy HostPolicy
	hits   map[string]SearchHit // normalised URL → hit
}

// NewTurn starts a turn with host policy and budget (0 means the safe default 3).
func NewTurn(policy HostPolicy, budget Budget) *Turn {
	if budget.MaxSearches <= 0 {
		budget.MaxSearches = 3
	}
	if budget.MaxFetches <= 0 {
		budget.MaxFetches = 3
	}
	return &Turn{Budget: budget, policy: policy, hits: map[string]SearchHit{}}
}

// RecordHits marks URLs returned by search as fetchable in this turn.
func (t *Turn) RecordHits(hits []SearchHit) {
	if t == nil {
		return
	}
	for _, h := range hits {
		if u := normalizeURL(h.URL); u != "" {
			t.hits[u] = h
		}
	}
}

// AllowedFetch reports whether url was returned by a prior search this turn.
func (t *Turn) AllowedFetch(url string) bool {
	if t == nil {
		return false
	}
	_, ok := t.hits[normalizeURL(url)]
	return ok
}

func (t *Turn) takeSearch() bool {
	if t == nil || t.Budget.searches >= t.Budget.MaxSearches {
		return false
	}
	t.Budget.searches++
	return true
}

func (t *Turn) takeFetch() bool {
	if t == nil || t.Budget.fetches >= t.Budget.MaxFetches {
		return false
	}
	t.Budget.fetches++
	return true
}
