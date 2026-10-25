package httpapi

import (
	"context"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

func TestKinds(t *testing.T) {
	p, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	got := p.Kinds()
	if len(got) != 2 || got[0] != ap.RAGSourceTypeApplicationDocumentAPI || got[1] != ap.RAGSourceTypeHTTPAPI {
		t.Errorf("Kinds = %v", got)
	}
}

// TestProbe: reachable means "the list endpoint answered a well-formed page
// just now"; everything else is an unreachable source with a typed reason, and
// CheckedAt stays zero so a caller cannot advance a last-success time from it.
func TestProbe(t *testing.T) {
	h := newHarness(t, "/api/docs")
	h.api.put("a.md", "a\n")
	def := h.def()

	hl, err := h.p.Probe(h.ctx, scope(), def)
	if err != nil || !hl.Reachable() || !hl.CheckedAt.Equal(clockStart) {
		t.Fatalf("Probe = %+v, %v", hl, err)
	}
	if n := len(h.api.requests()); n != 1 {
		t.Errorf("a probe is one request, got %d", n)
	}
	if got := h.api.requests()[0].query.Get("limit"); got != "1" {
		t.Errorf("a probe asks for one item, limit = %q", got)
	}

	cases := []struct {
		name string
		hook func(w http.ResponseWriter, r *http.Request)
		code contracts.ErrCode
	}{
		{"a server error", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 502) }, contracts.ErrUnavailable},
		{"a wrong base path", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, contracts.ErrUnavailable},
		{"a refused credential", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 401) }, contracts.ErrPermissionDenied},
		{"a forbidden scope", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 403) }, contracts.ErrPermissionDenied},
		{"rate limiting", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 429) }, contracts.ErrRateLimited},
		{"an unexpected status", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 418) }, contracts.ErrSchemaInvalid},
		{"a page that is not a page", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "{}") }, contracts.ErrSchemaInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h.api.listHook = func(w http.ResponseWriter, r *http.Request, page int) bool { tc.hook(w, r); return true }
			defer func() { h.api.listHook = nil }()
			hl, err := h.p.Probe(h.ctx, scope(), def)
			if err != nil {
				t.Fatalf("Probe returned an error, want an unreachable Health: %v", err)
			}
			if hl.Reachable() || hl.Availability != source.Unavailable || !hl.CheckedAt.IsZero() || hl.Err == nil || hl.Err.Code != tc.code {
				t.Errorf("Health = %+v, want unavailable with code %s", hl, tc.code)
			}
		})
	}

	t.Run("a cancelled context is an error, not an unreachable source", func(t *testing.T) {
		ctx, cancel := context.WithCancel(h.ctx)
		cancel()
		h.api.reset()
		if _, err := h.p.Probe(ctx, scope(), def); contracts.CodeOf(err) != contracts.ErrTimeout {
			t.Errorf("err = %v", err)
		}
		if n := len(h.api.requests()); n != 0 {
			t.Errorf("%d requests with a cancelled context", n)
		}
		for name, err := range map[string]error{
			"Discover": func() error { _, err := h.p.Discover(ctx, scope(), def, nil); return err }(),
			"Detect":   func() error { _, err := h.p.DetectDeletions(ctx, scope(), def, nil); return err }(),
			"Stat":     func() error { _, err := h.p.Stat(ctx, scope(), def, "a.md"); return err }(),
			"Open":     func() error { _, err := h.p.Open(ctx, scope(), def, "a.md"); return err }(),
		} {
			if contracts.CodeOf(err) != contracts.ErrTimeout {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("a context that ends during a scan stops it and applies nothing", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		for _, k := range []string{"a.md", "b.md", "c.md", "d.md"} {
			h.api.put(k, "x\n")
		}
		ctx, cancel := context.WithCancel(h.ctx)
		defer cancel()
		h.api.itemHook = func(w http.ResponseWriter, r *http.Request, key string) bool {
			if key == "b.md" {
				cancel()
			}
			return false
		}
		cs, err := h.p.Discover(ctx, scope(), h.def(), nil)
		if contracts.CodeOf(err) != contracts.ErrTimeout {
			t.Errorf("err = %v", err)
		}
		noChange(t, cs)
	})
}

// TestRootValidation: the declared root is checked before anything is sent, a
// base path is honoured, and a bare origin works.
func TestRootValidation(t *testing.T) {
	rootOf := func(h *harness, raw string, basePath *string) func(*ap.RAGSource) {
		return func(d *ap.RAGSource) {
			u := ap.HTTPURL(raw)
			d.Root = ap.RAGSourceRoot{Value: ap.RAGSourceRootAPI{Kind: "api", URL: &u, BasePath: basePath}}
		}
	}
	str := func(s string) *string { return &s }

	h := newHarness(t, "/api/docs")
	h.api.put("a.md", "a\n")
	refused := []struct {
		name string
		root func(srv string) (string, *string)
	}{
		{"user information in the URL", func(s string) (string, *string) {
			return strings.Replace(s, "http://", "http://user:pw@", 1) + "/api/docs", nil
		}},
		{"a query in the URL", func(s string) (string, *string) { return s + "/api/docs?x=1", nil }},
		{"a fragment in the URL", func(s string) (string, *string) { return s + "/api/docs#f", nil }},
		{"a percent-encoded path", func(s string) (string, *string) { return s + "/api%2Fdocs", nil }},
		{"an ftp URL", func(s string) (string, *string) { return "ftp://127.0.0.1/api/docs", nil }},
		{"a base path with a dot-dot segment", func(s string) (string, *string) { return s + "/api", str("/docs/../admin") }},
		{"a base path with a query", func(s string) (string, *string) { return s + "/api", str("/docs?x=1") }},
		{"a base path with a percent sign", func(s string) (string, *string) { return s + "/api", str("/d%2e") }},
		{"a base path that is not absolute", func(s string) (string, *string) { return s + "/api", str("docs") }},
		{"an empty segment in the URL", func(s string) (string, *string) { return s + "/api//docs", nil }},
	}
	for _, tc := range refused {
		t.Run("refused: "+tc.name, func(t *testing.T) {
			h.api.reset()
			raw, bp := tc.root(h.api.srv.URL)
			def := h.def(rootOf(h, raw, bp))
			if _, err := h.p.Discover(h.ctx, scope(), def, nil); contracts.CodeOf(err) != contracts.ErrValidationFailed {
				t.Errorf("err = %v, want validation_failed", err)
			}
			if n := len(h.api.requests()); n != 0 {
				t.Errorf("%d requests were made for a refused root", n)
			}
		})
	}

	t.Run("a base path is joined to the URL's path", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		h.api.put("a.md", "a\n")
		def := h.def(rootOf(h, h.api.srv.URL+"/api/", str("/docs/")))
		if cs := h.discover(def, nil); len(cs.Added) != 1 {
			t.Errorf("Added = %v", keysOf(cs.Added))
		}
		for _, r := range h.api.requests() {
			if r.path != "/api/docs" && r.path != "/api/docs/a.md" {
				t.Errorf("request path %q", r.path)
			}
		}
	})

	t.Run("a bare origin lists at / and serves items below it", func(t *testing.T) {
		h := newHarness(t, "")
		h.api.put("a.md", "a\n")
		h.api.put("dir/b.md", "b\n")
		cs := h.discover(h.def(), nil)
		if got := keysOf(cs.Added); !equalStrings(got, []string{"a.md", "dir/b.md"}) {
			t.Errorf("Added = %v", got)
		}
		for _, r := range h.api.requests() {
			if r.path != "/" && r.path != "/a.md" && r.path != "/dir/b.md" {
				t.Errorf("request path %q", r.path)
			}
		}
	})

	t.Run("a path-escaped key reaches the API exactly", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		key := "q&a/what is 100%? #1.md"
		h.api.put(key, "faq\n")
		if v, err := h.p.FetchVersion(h.ctx, scope(), h.def(), key); err != nil || v.Hash != hashOf("faq\n") {
			t.Errorf("FetchVersion = %+v, %v", v, err)
		}
		if got := h.api.requests()[0]; got.path != "/api/docs/"+key || strings.Contains(got.raw, "#") {
			t.Errorf("request = %+v", got)
		}
	})
}

func TestNewConfigValidation(t *testing.T) {
	bad := []struct {
		name string
		cfg  Config
	}{
		{"a parameter name with a space", Config{Params: Params{Cursor: "a b"}}},
		{"a parameter name that starts with a digit", Config{Params: Params{Limit: "1st"}}},
		{"two parameters with one name", Config{Params: Params{Cursor: "x", Limit: "X"}}},
		{"a credential header that is a protocol header", Config{AuthHeader: "Content-Length"}},
		{"a credential header that is not a token", Config{AuthHeader: "X Key"}},
		{"a credential scheme that is not a token", Config{AuthScheme: "Bear er"}},
		{"a negative page size", Config{PageSize: -1}},
		{"a huge page size", Config{PageSize: maxPageSize + 1}},
		{"a negative page byte limit", Config{MaxPageBytes: -1}},
		{"a negative page limit", Config{MaxPages: -1}},
		{"a negative timeout", Config{RequestTimeout: -time.Second}},
		{"a negative item timeout", Config{ItemTimeout: -time.Second}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); contracts.CodeOf(err) != contracts.ErrValidationFailed {
				t.Errorf("err = %v, want validation_failed", err)
			}
		})
	}
	p, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	c := p.cfg
	if c.PageSize != DefaultPageSize || c.MaxPageBytes != DefaultMaxPageBytes || c.MaxPages != DefaultMaxPages ||
		c.RequestTimeout != DefaultRequestTimeout || c.ItemTimeout != DefaultItemTimeout || c.RacyWindow != DefaultRacyWindow ||
		c.AuthHeader != DefaultAuthHeader || c.AuthScheme != DefaultAuthScheme || c.Params.Tenant != DefaultParamTenant {
		t.Errorf("defaults = %+v", c)
	}
}

// TestImportsStayWithinTheModuleMap: the provider talks to the network only
// through internal/egress, starts no process and uses nothing outside the base
// packages and the source contract.
func TestImportsStayWithinTheModuleMap(t *testing.T) {
	const module = "github.com/buxee/alix-ai/"
	allowed := map[string]bool{
		"pkg/contracts":                        true,
		"pkg/contracts/gen/applicationpackage": true,
		"internal/clock":                       true,
		"internal/egress":                      true,
		"internal/rag/source":                  true,
	}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) < 8 {
		t.Fatalf("found %d files (%v)", len(files), err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			switch {
			case p == "os/exec" || p == "plugin" || p == "net" || p == "unsafe" || p == "os":
				t.Errorf("%s imports %s", f, p)
			case strings.HasPrefix(p, module):
				if rel := strings.TrimPrefix(p, module); !allowed[rel] {
					t.Errorf("%s imports %s, which the provider may not use", f, rel)
				}
			}
		}
	}
}
