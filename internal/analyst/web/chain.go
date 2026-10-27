package web

import (
	"context"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
)

// ChainSearcher tries provider_native then an optional fallback Provider.
type ChainSearcher struct {
	Native        NativeCaller
	NativeOK      func(ctx context.Context, scope contracts.Scope) bool
	Fallback      Provider
	FallbackName  string // searxng|brave|tavily
	FallbackOK    func(ctx context.Context, scope contracts.Scope) bool
	AllowDomains  func(ctx context.Context, scope contracts.Scope) []string
	OnNativeMeter func(ctx context.Context, scope contracts.Scope, calls, toksIn, toksOut int)
	HostFilter    func(ctx context.Context, scope contracts.Scope) HostPolicy
}

// Search implements the chain: native → fallback → web_search_unavailable.
// Egress is enforced per backend via NativeOK / FallbackOK (not a single bool).
func (s *ChainSearcher) Search(ctx context.Context, scope contracts.Scope, turn *Turn, query string, datasets []an.Dataset, _ bool) (SearchResult, error) {
	if err := CheckQuery(query, datasets); err != nil {
		return SearchResult{}, err
	}
	if turn == nil || !turn.takeSearch() {
		return SearchResult{}, contracts.New(contracts.ErrAnalystCapExceeded, "web: search budget exhausted")
	}
	q := strings.TrimSpace(query)

	tryNative := s.Native != nil && (s.NativeOK == nil || s.NativeOK(ctx, scope))
	if tryNative {
		domains := []string{}
		if s.AllowDomains != nil {
			domains = DomainsForProviderFilter(s.AllowDomains(ctx, scope))
		}
		res, err := s.Native.SearchWeb(ctx, scope, q, domains)
		if err == nil {
			hits := res.Hits
			if s.HostFilter != nil {
				hits = FilterHitsByHosts(hits, s.HostFilter(ctx, scope))
			}
			if s.OnNativeMeter != nil && res.SearchCalls > 0 {
				s.OnNativeMeter(ctx, scope, res.SearchCalls, res.ToksIn, res.ToksOut)
			}
			return finishHits(turn, hits)
		}
		// Only unsupported / unavailable falls through to a configured fallback.
		// Egress denials and budget refusals must not silently switch backends.
		if contracts.CodeOf(err) != contracts.ErrWebSearchUnavailable {
			return SearchResult{}, err
		}
	}

	tryFallback := s.Fallback != nil && (s.FallbackOK == nil || s.FallbackOK(ctx, scope))
	if tryFallback {
		hits, err := s.Fallback.Search(ctx, q)
		if err != nil {
			return SearchResult{}, err
		}
		if s.HostFilter != nil {
			hits = FilterHitsByHosts(hits, s.HostFilter(ctx, scope))
		}
		return finishHits(turn, hits)
	}

	return SearchResult{}, contracts.New(contracts.ErrWebSearchUnavailable, "web: no search backend is available for this turn")
}

func finishHits(turn *Turn, hits []SearchHit) (SearchResult, error) {
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
