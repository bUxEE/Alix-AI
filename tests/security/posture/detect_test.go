package posturesec

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"time"
)

// ---- the route deny-list -----------------------------------------------------

// denyRule is one forbidden surface (master plan 16.1) as the route segments it shows.
type denyRule struct {
	name string
	// any matches when any path segment is in the set.
	any map[string]bool
	// first matches when the first segment after /v1 is in the set.
	first map[string]bool
}

func set(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

var denyRules = []denyRule{
	{name: "browser CORS proxy", any: set("cors", "cors-proxy", "cors_proxy", "corsproxy", "proxy")},
	{name: "code or command execution", any: set("exec", "execute", "shell", "sh", "bash", "eval", "code", "terminal", "pty", "command", "commands", "cmd", "run-code", "run_code")},
	{name: "agent hub", any: set("agent", "agents", "agent-hub", "agenthub")},
	{name: "MCP or tool registration", any: set("mcp", "mcp-servers", "mcp_servers", "tool", "tools", "connectors", "plugins")},
	{name: "model apply, install or gallery", first: set("model", "models", "backend", "backends", "gallery"), any: set("apply", "install", "gallery")},
	{name: "admin or web UI", any: set("admin", "webui", "swagger", "ui", "dashboard", "console", "localai", "studio")},
	{name: "debug or profiling", any: set("debug", "pprof", "expvar", "trace")},
	{name: "metrics", any: set("metrics")},
	{name: "OpenAI-compatible or LocalAI surface", first: set("chat", "completions", "embeddings", "images", "audio", "tts", "transcriptions", "edits")},
	{name: "development route", first: set("v0", "dev")},
}

// staticSegments splits the path of a "METHOD /path" route into its lower-case static
// segments, and returns the first one after an optional v1.
func staticSegments(route string) (segments []string, first string) {
	_, p, _ := strings.Cut(route, " ")
	for _, s := range strings.Split(strings.ToLower(strings.Trim(p, "/")), "/") {
		if s == "" || strings.HasPrefix(s, "{") {
			continue
		}
		segments = append(segments, s)
	}
	rest := segments
	if len(rest) > 0 && rest[0] == "v1" {
		rest = rest[1:]
	}
	if len(rest) > 0 {
		first = rest[0]
	}
	return segments, first
}

// forbidden names the deny-list rules a route matches. A route the specification lists is
// not "metrics" for that reason alone: listed means someone decided it.
func forbidden(route string, inSpec map[string]bool) []string {
	segments, first := staticSegments(route)
	var out []string
	for _, rule := range denyRules {
		hit := rule.first[first]
		for _, s := range segments {
			hit = hit || rule.any[s]
		}
		if hit && !(rule.name == "metrics" && inSpec[route]) {
			out = append(out, rule.name)
		}
	}
	return out
}

// forbiddenRoutes lists "route: rule" for every route that matches a rule.
func forbiddenRoutes(routes []string, inSpec map[string]bool) []string {
	var out []string
	for _, r := range routes {
		for _, rule := range forbidden(r, inSpec) {
			out = append(out, r+": "+rule)
		}
	}
	return out
}

// ---- registration routes -----------------------------------------------------

// registrationNouns are what a route that creates something is named after. A
// POST, PUT or PATCH to a collection named so is a registration route.
var registrationNouns = set(
	"application", "applications", "app", "apps", "source", "sources", "server", "servers", "mcp", "tool", "tools", "agent", "agents",
	"api", "apis", "plugin", "plugins", "connector", "connectors", "integration", "integrations", "endpoint", "endpoints",
	"provider", "providers", "model", "models", "backend", "backends", "adapter", "adapters", "package", "packages",
)

// documentedRegistrations are the two registration routes the product has, and why each
// is safe: the package of an application goes into quarantine and serves nothing until
// the integration audit activates it, and a RAG source can only be one the active
// package declares by name.
var documentedRegistrations = map[string]string{
	"POST /v1/applications": "installs a signed package into quarantine",
	"POST /v1/rag/sources":  "registers a source the active package declares, by name",
}

// registrationRoutes lists the mutating routes whose last static segment is a
// registration noun and that are not documented ones.
func registrationRoutes(routes []string) []string {
	var out []string
	for _, r := range routes {
		method, _, _ := strings.Cut(r, " ")
		if method != http.MethodPost && method != http.MethodPut && method != http.MethodPatch {
			continue
		}
		segments, _ := staticSegments(r)
		if len(segments) == 0 || !registrationNouns[segments[len(segments)-1]] {
			continue
		}
		if _, ok := documentedRegistrations[r]; !ok {
			out = append(out, r)
		}
	}
	return out
}

// ---- the route inventory against the specification ---------------------------

// routeDiff compares the registered routes with the specification's operations.
func routeDiff(routes, spec []string) (notInSpec, noRoute []string) {
	in := set(spec...)
	have := set(routes...)
	for _, r := range routes {
		if !in[r] {
			notInSpec = append(notInSpec, r)
		}
	}
	for _, s := range spec {
		if !have[s] {
			noRoute = append(noRoute, s)
		}
	}
	sort.Strings(notInSpec)
	sort.Strings(noRoute)
	return
}

func specRoutes(ops []specOp) []string {
	out := make([]string, len(ops))
	for i, o := range ops {
		out[i] = o.Route()
	}
	return out
}

// ---- CORS --------------------------------------------------------------------

// probe is one request of the CORS sweep.
type probe struct {
	method, path string
	token        func() string
	body         []byte
	contentType  string
}

// corsViolations sends every probe with an Origin header and a preflight request header
// and returns every response that carries an Access-Control header, with the header.
// It also returns how many requests it sent, so a sweep that sent nothing is visible.
func corsViolations(h http.Handler, probes []probe) (violations []string, sent int, codes map[int]int) {
	codes = map[int]int{}
	for _, p := range probes {
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		req := httptest.NewRequest(p.method, p.path, bytes.NewReader(p.body)).WithContext(ctx)
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Access-Control-Request-Method", "POST")
		req.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
		if p.contentType != "" {
			req.Header.Set("Content-Type", p.contentType)
		}
		if p.token != nil {
			if tok := p.token(); tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		cancel()
		sent++
		codes[rec.Code]++
		for k, v := range rec.Header() {
			if strings.HasPrefix(strings.ToLower(k), "access-control-") {
				violations = append(violations, fmt.Sprintf("%s %s -> %d: %s: %s", p.method, p.path, rec.Code, k, strings.Join(v, ",")))
			}
		}
	}
	sort.Strings(violations)
	return
}
