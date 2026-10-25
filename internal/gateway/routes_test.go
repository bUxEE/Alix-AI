package gateway

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// published maps each embedded schema file to the published original.
var published = map[string]string{
	"task-proposal.schema.json": filepath.Join("..", "..", "schemas", "conversation", "task-proposal.schema.json"),
	"ids.schema.json":           filepath.Join("..", "..", "schemas", "common", "ids.schema.json"),
}

// TestEmbeddedSchemasMatchPublished keeps the copies of the schemas this
// package embeds identical to the published ones in schemas/. A change to a
// published schema that is not copied here would let the runtime check and the
// contract drift apart without anything failing.
func TestEmbeddedSchemasMatchPublished(t *testing.T) {
	embedded := EmbeddedSchemaNames()
	sort.Strings(embedded)
	var want []string
	for name := range published {
		want = append(want, name)
	}
	sort.Strings(want)
	if strings.Join(embedded, ",") != strings.Join(want, ",") {
		t.Fatalf("embedded schemas = %v, want %v", embedded, want)
	}
	for name, path := range published {
		orig, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		copy, err := EmbeddedSchema(name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(orig, copy) {
			t.Errorf("internal/gateway/schemas/%s differs from %s; copy the published file over the embedded one", name, filepath.ToSlash(path))
		}
	}
}

func TestEmbeddedSchemasCompile(t *testing.T) {
	s, err := loadProposalSchemas()
	if err != nil || s == nil || s.model == nil || s.full == nil {
		t.Fatalf("compile: %v", err)
	}
}

// The model-authority projection drops exactly what the core owns.
func TestModelSchemaExcludesTraceID(t *testing.T) {
	s, err := loadProposalSchemas()
	if err != nil {
		t.Fatal(err)
	}
	parse := func(doc map[string]any) any {
		v, err := jsonschema.UnmarshalJSON(strings.NewReader(mustJSON(doc)))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	valid := proposalJSON(nil)
	if err := s.model.Validate(parse(valid)); err != nil {
		t.Errorf("the model schema refuses a valid model proposal: %v", err)
	}
	if err := s.full.Validate(parse(valid)); err == nil {
		t.Error("the full schema accepts a proposal without a trace_id")
	}
	withTrace := proposalJSON(map[string]any{"trace_id": "x"})
	if err := s.model.Validate(parse(withTrace)); err == nil {
		t.Error("the model schema accepts a model-written trace_id")
	}
	if err := s.full.Validate(parse(withTrace)); err != nil {
		t.Errorf("the full schema refuses a complete proposal: %v", err)
	}
}

func TestModelProjectionFailsClosedWithoutAnnotation(t *testing.T) {
	_, err := modelProjection(map[string]any{
		"properties": map[string]any{"a": map[string]any{"x-alixai-authority": "model"}, "b": map[string]any{}},
	})
	if err == nil {
		t.Error("an unannotated property was projected instead of refused")
	}
	if _, err := modelProjection(map[string]any{"properties": map[string]any{"a": map[string]any{"x-alixai-authority": "alixai"}}}); err == nil {
		t.Error("a projection with nothing left was accepted")
	}
}

func TestRegisterAreaGuards(t *testing.T) {
	mustPanic := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s did not panic", name)
			}
		}()
		f()
	}
	mustPanic("bad name", func() { RegisterArea("Bad-Name", func(*Router, *Deps) {}) })
	mustPanic("nil func", func() { RegisterArea("somearea", nil) })
	mustPanic("duplicate", func() { RegisterArea("health", func(*Router, *Deps) {}) })
}

func TestRouterRefusesBadPatterns(t *testing.T) {
	for name, pattern := range map[string]string{
		"no method":        "/things",
		"unknown method":   "TRACE /things",
		"root":             "GET /",
		"relative":         "GET things",
		"v1 direct":        "GET /v1/things",
		"v1 root":          "GET /v1",
		"method not alone": "GET  /things",
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("Handle(%q) did not panic", pattern)
				}
			}()
			rt := &Router{mux: http.NewServeMux()}
			rt.Handle(pattern, http.NotFoundHandler())
		})
	}
}

// A new area is a new file: an extra area registers routes, and they get the
// envelope, request ID and body limit like every other.
func TestExtraAreaRoutes(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.ExtraAreas = map[string]AreaFunc{"things": func(rt *Router, _ *Deps) {
			rt.HandleFunc("GET /things/{id}", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusOK, map[string]string{"id": r.PathValue("id")})
			})
			rt.HandleFunc("POST /things", func(w http.ResponseWriter, r *http.Request) {
				var body struct{ Name string }
				if !decodeJSON(w, r, &body) {
					return
				}
				writeJSON(w, http.StatusCreated, body)
			})
		}}
	})
	if rec := h.do(http.MethodGet, "/things/42", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"42"`) {
		t.Errorf("GET /things/42 = %d %s", rec.Code, rec.Body.String())
	}
	rec := h.do(http.MethodDelete, "/things/42", nil)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("DELETE /things/42 = %d Allow=%q, want 405 \"GET, HEAD\"", rec.Code, rec.Header().Get("Allow"))
	}
	mustEnvelope(t, rec)
	if rec := h.do(http.MethodPost, "/things", map[string]string{"Name": "x"}); rec.Code != http.StatusCreated {
		t.Errorf("POST /things = %d %s", rec.Code, rec.Body.String())
	}
	if got := strings.Join(h.srv.Routes(), ","); !strings.Contains(got, "GET /things/{id}") || !strings.Contains(got, "POST /things") {
		t.Errorf("routes = %s", got)
	}
}

// The /v1 sub-router fails closed: with no identity middleware every /v1 route
// refuses, and with one the middleware runs before the handler.
func TestV1SubRouterFailsClosedWithoutIdentity(t *testing.T) {
	called := false
	area := map[string]AreaFunc{"v1test": func(rt *Router, _ *Deps) {
		rt.V1().HandleFunc("GET /sessions", func(w http.ResponseWriter, _ *http.Request) {
			called = true
			writeJSON(w, http.StatusOK, map[string]string{"ok": "yes"})
		})
	}}

	h := newHarness(t, func(o *Options) { o.ExtraAreas = area })
	rec := h.do(http.MethodGet, "/v1/sessions", nil)
	if rec.Code != http.StatusServiceUnavailable || called {
		t.Fatalf("GET /v1/sessions with no identity middleware = %d (handler called: %v), want 503 and no call", rec.Code, called)
	}
	if env := mustEnvelope(t, rec); env.Code != "unavailable" {
		t.Errorf("envelope = %+v", env)
	}
	if !strings.Contains(strings.Join(h.srv.Routes(), ","), "GET /v1/sessions") {
		t.Errorf("routes = %v, want the v1 route listed", h.srv.Routes())
	}

	authed := newHarness(t, func(o *Options) {
		o.ExtraAreas = area
		o.V1Auth = func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test" {
					WriteError(w, r, contracts.New(contracts.ErrUnauthenticated, "no credential"))
					return
				}
				next.ServeHTTP(w, r)
			})
		}
	})
	called = false
	rec = authed.do(http.MethodGet, "/v1/sessions", nil)
	if rec.Code != http.StatusUnauthorized || called {
		t.Fatalf("an unauthenticated /v1 call = %d (handler called: %v), want 401", rec.Code, called)
	}
	rec = authed.do(http.MethodGet, "/v1/sessions", nil, "Authorization", "Bearer test")
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("an authenticated /v1 call = %d (handler called: %v)", rec.Code, called)
	}
	// An unknown /v1 path is the 404 envelope, not a handler.
	if rec := authed.do(http.MethodGet, "/v1/nope", nil, "Authorization", "Bearer test"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /v1/nope = %d, want 404", rec.Code)
	}
}
