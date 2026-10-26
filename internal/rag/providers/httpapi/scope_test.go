package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// scopeAllowed is the query_scope whitelist a package author writes for a
// source whose tenant and workspace levels are bound.
var scopeAllowed = []string{"limit", "cursor", "application_id", "tenant_id", "workspace_id"}

func withQueryScope(allowed []string, fixed map[string]string) func(*ap.RAGSource) {
	return func(d *ap.RAGSource) {
		d.QueryScope = &ap.RAGSourceQueryScope{AllowedParameters: allowed, FixedParameters: fixed}
	}
}

// onlyOne asserts that a query holds the parameter exactly once with the value.
func onlyOne(t *testing.T, r rec, name, want string) {
	t.Helper()
	if got := r.query[name]; len(got) != 1 || got[0] != want {
		t.Errorf("%s %s: parameter %s = %q, want exactly [%s]", r.method, r.path, name, got, want)
	}
}

// TestScopeParamsAlwaysSent: the verified scope and the declaration's fixed
// parameters are on every request the provider makes (list pages, item fetches,
// metadata requests, the probe), exactly once, whatever the key or the cursor
// contains, and nothing outside the declared query scope is ever sent.
func TestScopeParamsAlwaysSent(t *testing.T) {
	h := newHarness(t, "/api/docs")
	// A hostile cursor tries to add and override scope parameters, and the keys
	// carry characters that mean something in a URL.
	h.api.curEnc = func(offset int) string { return fmt.Sprintf("p%d&tenant_id=evil&workspace_id=evil&x=1", offset) }
	h.api.curDec = func(c string) (int, bool) {
		var n int
		_, err := fmt.Sscanf(strings.SplitN(c, "&", 2)[0], "p%d", &n)
		return n, err == nil
	}
	keys := []string{"plain.md", "a b/c?tenant_id=evil&x=1#frag%41.md", "unicode/é ü.md", "deep/er/path/d.md"}
	for _, k := range keys {
		h.api.put(k, "body of "+k+"\n")
	}
	fixed := map[string]string{"collection": "policies", "dataset": "v2 beta&x"}
	def := h.def(withQueryScope(scopeAllowed, fixed))

	if hl, err := h.p.Probe(h.ctx, scope(), def); err != nil || !hl.Reachable() {
		t.Fatalf("Probe = %+v, %v", hl, err)
	}
	cs := h.discover(def, nil)
	if len(cs.Added) != len(keys) {
		t.Fatalf("Added = %v", keysOf(cs.Added))
	}
	h.clk.Advance(1)
	if _, err := h.p.DetectDeletions(h.ctx, scope(), def, cs.Next); err != nil {
		t.Fatal(err)
	}
	if _, err := h.p.Stat(h.ctx, scope(), def, keys[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := h.p.FetchVersion(h.ctx, scope(), def, keys[1]); err != nil {
		t.Fatal(err)
	}
	if c, err := h.p.Open(h.ctx, scope(), def, keys[2]); err != nil {
		t.Fatal(err)
	} else {
		_ = c.Close()
	}

	allowed := map[string]bool{"collection": true, "dataset": true}
	for _, n := range scopeAllowed {
		allowed[n] = true
	}
	lists, gets, heads := 0, 0, 0
	for _, r := range h.api.requests() {
		onlyOne(t, r, "application_id", testApp)
		onlyOne(t, r, "tenant_id", testTenant)
		onlyOne(t, r, "workspace_id", testWS)
		onlyOne(t, r, "collection", "policies")
		onlyOne(t, r, "dataset", "v2 beta&x")
		for name := range r.query {
			if !allowed[name] {
				t.Errorf("%s %s: parameter %q is outside the declared query scope", r.method, r.path, name)
			}
		}
		isItem := strings.HasPrefix(r.path, "/api/docs/")
		switch {
		case !isItem:
			lists++
			if r.query.Get("limit") == "" {
				t.Errorf("a list request without a page size: %v", r.query)
			}
		case r.method == http.MethodHead:
			heads++
		default:
			gets++
		}
		if isItem {
			if key := strings.TrimPrefix(r.path, "/api/docs/"); !contains(keys, key) {
				t.Errorf("request for %q, which is not a key the test listed (the key leaked into the query or was rewritten)", key)
			}
			if r.query.Get("limit")+r.query.Get("cursor") != "" {
				t.Errorf("paging parameters on an item request: %v", r.query)
			}
		}
	}
	if lists < 5 || gets < 5 || heads < len(keys)+1 {
		t.Errorf("the test did not exercise every kind of request: %d list, %d get, %d head", lists, gets, heads)
	}

	t.Run("a shared level is not sent and nothing else changes", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		h.api.put("a.md", "a\n")
		def := h.defBound(source.Binding{}, func(d *ap.RAGSource) {
			d.Scope.Tenant.Mode = ap.RAGSourceScopeBindingModeShared
			d.Scope.Workspace.Mode = ap.RAGSourceScopeBindingModeShared
			withQueryScope([]string{"limit", "cursor", "application_id"}, map[string]string{"collection": "shared"})(d)
		})
		if cs := h.discover(def, nil); len(cs.Added) != 1 {
			t.Fatalf("Added = %v", keysOf(cs.Added))
		}
		for _, r := range h.api.requests() {
			onlyOne(t, r, "application_id", testApp)
			onlyOne(t, r, "collection", "shared")
			for _, n := range []string{"tenant_id", "workspace_id"} {
				if _, ok := r.query[n]; ok {
					t.Errorf("%s %s: a shared level must not name the caller's %s", r.method, r.path, n)
				}
			}
		}
	})

	t.Run("a shared workspace under a bound tenant sends only the tenant", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		h.api.put("a.md", "a\n")
		def := h.defBound(source.Binding{TenantID: testTenant}, func(d *ap.RAGSource) {
			d.Scope.Workspace.Mode = ap.RAGSourceScopeBindingModeShared
		})
		h.discover(def, nil)
		for _, r := range h.api.requests() {
			onlyOne(t, r, "tenant_id", testTenant)
			if _, ok := r.query["workspace_id"]; ok {
				t.Errorf("%s %s: workspace_id sent for a shared workspace", r.method, r.path)
			}
		}
	})

	t.Run("the parameter names are configurable", func(t *testing.T) {
		h := newHarness(t, "/api/docs", func(c *Config) {
			c.Params = Params{Cursor: "after", Limit: "page_size", Application: "app", Tenant: "org", Workspace: "ws"}
		})
		h.api.put("a.md", "a\n")
		h.api.put("b.md", "b\n")
		h.api.put("c.md", "c\n")
		h.discover(h.def(), nil)
		for _, r := range h.api.requests() {
			onlyOne(t, r, "app", testApp)
			onlyOne(t, r, "org", testTenant)
			onlyOne(t, r, "ws", testWS)
			for _, n := range []string{"application_id", "tenant_id", "workspace_id"} {
				if _, ok := r.query[n]; ok {
					t.Errorf("default name %s sent although it was renamed", n)
				}
			}
		}
	})

	t.Run("include_principal_id is opt-in and off by default", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		h.api.put("a.md", "a\n")
		h.discover(h.def(), nil)
		for _, r := range h.api.requests() {
			if _, ok := r.query[DefaultParamPrincipal]; ok {
				t.Errorf("%s %s: principal_id sent without scope_params.include_principal_id", r.method, r.path)
			}
		}
		h2 := newHarness(t, "/api/docs")
		h2.api.put("a.md", "a\n")
		on := true
		def := h2.def(func(d *ap.RAGSource) {
			d.ScopeParams = &ap.RAGSourceScopeParams{IncludePrincipalID: &on}
			withQueryScope(append(scopeAllowed, DefaultParamPrincipal), nil)(d)
		})
		h2.discover(def, nil)
		for _, r := range h2.api.requests() {
			onlyOne(t, r, DefaultParamPrincipal, scope().PrincipalID)
		}
	})
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// TestScopeIsCheckedBeforeAnything: a scope the registration does not admit
// never reaches the network, and neither does a definition the provider
// refuses.
func TestScopeIsCheckedBeforeAnything(t *testing.T) {
	h := newHarness(t, "/api/docs")
	h.api.put("a.md", "a\n")
	def := h.def()

	other := scope()
	other.TenantID = "tenant-2"
	if _, err := h.p.Discover(h.ctx, other, def, nil); contracts.CodeOf(err) != contracts.ErrScopeViolation {
		t.Errorf("another tenant: %v", err)
	}
	other = scope()
	other.ApplicationID = "another-app"
	if _, err := h.p.Probe(h.ctx, other, def); contracts.CodeOf(err) != contracts.ErrScopeViolation {
		t.Errorf("another application: %v", err)
	}
	if _, err := h.p.Stat(h.ctx, contracts.Scope{}, def, "a.md"); err == nil {
		t.Error("an empty scope was accepted")
	}

	refused := []struct {
		name   string
		mutate func(*ap.RAGSource)
	}{
		{"query_scope that does not list a parameter the provider sends", withQueryScope([]string{"limit", "cursor"}, nil)},
		{"a fixed parameter that reuses the tenant name", withQueryScope(scopeAllowed, map[string]string{"tenant_id": "other"})},
		{"a fixed parameter that reuses a name in another case", withQueryScope(scopeAllowed, map[string]string{"Limit": "9999"})},
		{"a fixed parameter with a malformed name", withQueryScope(scopeAllowed, map[string]string{"a b": "x"})},
		{"an endpoint root without a resolver", func(d *ap.RAGSource) {
			e := ap.Slug("docs-endpoint")
			d.Root = ap.RAGSourceRoot{Value: ap.RAGSourceRootAPI{Kind: "api", Endpoint: &e}}
		}},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			h.api.reset()
			def := h.def(tc.mutate)
			if _, err := h.p.Discover(h.ctx, scope(), def, nil); contracts.CodeOf(err) != contracts.ErrValidationFailed {
				t.Errorf("Discover: %v, want validation_failed", err)
			}
			if _, err := h.p.Probe(h.ctx, scope(), def); contracts.CodeOf(err) != contracts.ErrValidationFailed {
				t.Errorf("Probe: %v, want validation_failed", err)
			}
			if n := len(h.api.requests()); n != 0 {
				t.Errorf("%d requests were made for a refused definition", n)
			}
		})
	}

	t.Run("a resolver maps a manifest endpoint to its URL", func(t *testing.T) {
		var base string
		h := newHarness(t, "/api/docs", func(c *Config) {
			c.Endpoints = func(app, id string) (string, bool) {
				if app == testApp && id == "docs-endpoint" {
					return base, true
				}
				return "", false
			}
		})
		base = h.api.rootURL()
		h.api.put("a.md", "a\n")
		endpoint := func(id string) func(*ap.RAGSource) {
			return func(d *ap.RAGSource) {
				e := ap.Slug(id)
				d.Root = ap.RAGSourceRoot{Value: ap.RAGSourceRootAPI{Kind: "api", Endpoint: &e}}
			}
		}
		if cs := h.discover(h.def(endpoint("docs-endpoint")), nil); len(cs.Added) != 1 {
			t.Errorf("Added = %v", keysOf(cs.Added))
		}
		h.api.reset()
		if _, err := h.p.Discover(h.ctx, scope(), h.def(endpoint("unknown-endpoint")), nil); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("an unknown endpoint: %v", err)
		}
		if n := len(h.api.requests()); n != 0 {
			t.Errorf("%d requests for an unknown endpoint", n)
		}
	})

	t.Run("the wrong kind of source is refused", func(t *testing.T) {
		h.api.reset()
		d := baseDecl(h.api.rootURL())
		d.Type = ap.RAGSourceTypeLocalFolder
		d.Root = ap.RAGSourceRoot{Value: ap.RAGSourceRootFolder{Kind: "folder", Path: "/tmp/x"}}
		wrong, err := source.FromDeclaration(d, source.Binding{SourceID: ids.New(source.IDPrefix), TenantID: testTenant, WorkspaceID: testWS})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.p.Discover(h.ctx, scope(), wrong, nil); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("err = %v", err)
		}
	})
}
