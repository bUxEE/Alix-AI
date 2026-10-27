package web

import (
	"context"
	"net/url"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Fixed instruction for provider-native search: never include turn data.
const NativeSearchInstruction = "search the web for this query and list the results"

// NativeCaller runs one provider-native web search (separate minimal provider call).
type NativeCaller interface {
	SearchWeb(ctx context.Context, scope contracts.Scope, query string, allowDomains []string) (NativeCallResult, error)
}

// NativeCallResult is what one native search call returns.
type NativeCallResult struct {
	Hits        []SearchHit
	SearchCalls int // provider-reported search tool uses → web_searches meter
	ToksIn      int
	ToksOut     int
}

// FilterHitsByHosts drops hits whose URL host is not allowed by policy.
func FilterHitsByHosts(hits []SearchHit, policy HostPolicy) []SearchHit {
	out := make([]SearchHit, 0, len(hits))
	for _, h := range hits {
		u, err := url.Parse(strings.TrimSpace(h.URL))
		if err != nil || u.Host == "" {
			continue
		}
		if !policy.HostAllowed(u.Hostname()) {
			continue
		}
		out = append(out, h)
	}
	return out
}

// DomainsForProviderFilter turns allow-list host suffixes into bare domains for
// provider APIs that accept allowed_domains (no scheme).
func DomainsForProviderFilter(allow []string) []string {
	out := make([]string, 0, len(allow))
	seen := map[string]bool{}
	for _, h := range allow {
		h = strings.ToLower(strings.TrimSpace(h))
		h = strings.TrimPrefix(h, "*.")
		h = strings.TrimPrefix(h, ".")
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}
