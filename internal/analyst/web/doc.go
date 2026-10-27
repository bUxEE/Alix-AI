// Package web is the analyst web search/fetch seam (ADR 0047).
//
// WebSearcher and WebFetcher are the integrator interfaces. Adapters cover Brave
// Search, Tavily and SearXNG (self-hosted). Credentials resolve through
// internal/credentials via an opaque cred_ref (never YAML or logs). Fetches are
// https-only, SSRF-protected (no private/loopback/link-local after DNS, no
// redirects to them, ports 80/443 only), host allow/deny lists apply, and a URL
// may be fetched only if it was returned by a prior search in the same turn.
// Results are fenced as untrusted; each fetch yields a WebCitation plus a content
// hash. web_query egress refuses queries that embed confidential/restricted
// dataset field values.
package web
