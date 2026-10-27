package web

import (
	"net/url"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// MergeHostPolicy intersects allow lists (empty allow = deny all fetches until
// configured when requireAllow) and unions deny lists. Deny wins.
func MergeHostPolicy(parts ...HostPolicy) HostPolicy {
	var allow []string
	allowSet := false
	deny := map[string]bool{}
	for _, p := range parts {
		for _, d := range p.Deny {
			if h := normalizeHost(d); h != "" {
				deny[h] = true
			}
		}
		if p.Allow != nil {
			if !allowSet {
				allow = normalizeHosts(p.Allow)
				allowSet = true
				continue
			}
			allow = intersectHosts(allow, normalizeHosts(p.Allow))
		}
	}
	out := HostPolicy{Allow: allow, Deny: sortedHostKeys(deny)}
	return out
}

func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimPrefix(h, "*.")
	return strings.Trim(h, ".")
}

func normalizeHosts(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range in {
		if n := normalizeHost(h); n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

func intersectHosts(a, b []string) []string {
	set := map[string]bool{}
	for _, h := range b {
		set[h] = true
	}
	var out []string
	for _, h := range a {
		if set[h] {
			out = append(out, h)
		}
	}
	return out
}

func sortedHostKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for h := range m {
		out = append(out, h)
	}
	// insertion order is fine for tests; sort for stability
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// HostAllowed reports whether host (or a suffix match) is permitted.
func (p HostPolicy) HostAllowed(host string) bool {
	host = normalizeHost(host)
	if host == "" {
		return false
	}
	for _, d := range p.Deny {
		if host == d || strings.HasSuffix(host, "."+d) {
			return false
		}
	}
	if len(p.Allow) == 0 {
		return false
	}
	for _, a := range p.Allow {
		if host == a || strings.HasSuffix(host, "."+a) {
			return true
		}
	}
	return false
}

func checkHostPolicy(p HostPolicy, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return contracts.New(contracts.ErrWebQueryRefused, "web: URL host is malformed").
			WithDetails("reason", "bad_host")
	}
	host := u.Hostname()
	if !p.HostAllowed(host) {
		return contracts.New(contracts.ErrWebQueryRefused, "web: host is not allowed").
			WithDetails("reason", "host_denied")
	}
	return nil
}
