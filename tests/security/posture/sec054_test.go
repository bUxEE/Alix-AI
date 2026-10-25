package posturesec

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// plantedRoutes is an area of routes that must never exist: it is mounted through
// Options.ExtraAreas (which the product never sets) so that the real server, the real
// Routes() and the real handlers answer for them.
func plantedRoutes(rt *gateway.Router, _ *gateway.Deps) {
	ok := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"registered":true}`))
	}
	rt.V1().HandleFunc("POST /mcp/servers", ok)
	rt.V1().HandleFunc("POST /tools", ok)
	rt.V1().HandleFunc("PUT /agents/{agent_id}", ok)
	rt.HandleFunc("POST /exec", ok)
	rt.HandleFunc("GET /api/cors-proxy", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		ok(w, r)
	})
	rt.HandleFunc("POST /models/apply", ok)
}

func withPlanted(o *rigOptions) { o.extra = map[string]gateway.AreaFunc{"planted": plantedRoutes} }

// aguiExceptionFindings are the only free-form members the specification may have in a
// request body, and only in POST /v1/agui: that body is the AG-UI protocol's
// RunAgentInput, which carries the page's messages, context and the tools it declares as
// data. The route never registers or calls such a tool (a client-declared tool is a UI
// declaration the model may propose only if the host put its name in the application's
// allow-list; the capabilities document reports that); see schemas/api/paths/agui.yaml,
// docs/integration/agui.md and ADR 0035.
var aguiExceptionFindings = []string{
	`body accepts any additional member`,
	`body declares the member "tools"`,
	`body.context[] is an object that declares no member and does not forbid others`,
	`body.messages[] is an object that declares no member and does not forbid others`,
	`body.tools[] is an object that declares no member and does not forbid others`,
}

// SEC-054: no client, setting or route can register an arbitrary MCP server, API, source,
// agent or tool. The suite builds the real gateway with every route area mounted and
// shows: no route accepts a free-form definition (route inventory and every request body
// schema of the OpenAPI document); the two registration routes that exist do only what
// the package declares (a package goes into quarantine and serves nothing; a source
// must be one the active package names); bodies that carry a definition never reach a
// registry; and every path where such a route could live answers 404 or 405.
//
// SEC-054
func TestSecNoArbitraryRegistrationRoutes(t *testing.T) {
	docs := newSpecDocs(t)
	ops := docs.operations()

	t.Run("the detectors see what they must", func(t *testing.T) {
		// registration routes
		planted := []string{
			"POST /v1/mcp/servers", "POST /v1/tools", "PUT /v1/agents/{agent_id}", "POST /v1/apis", "PATCH /v1/plugins/{id}",
			"POST /v1/applications/{application_id}/tools", "POST /v1/models", "PUT /v1/providers/{provider_id}", "POST /v1/connectors",
		}
		clean := []string{
			"POST /v1/applications", "POST /v1/rag/sources", "GET /v1/applications", "GET /v1/rag/sources", "POST /v1/sessions",
			"POST /v1/sessions/{session_id}/turns", "PUT /v1/settings/user", "POST /v1/rag/sources/{source_id}/sync", "POST /v1/events",
			"GET /v1/mcp/servers",
		}
		if got := registrationRoutes(planted); len(got) != len(planted) {
			t.Errorf("the registration detector flags %v of the planted routes %v", got, planted)
		}
		if got := registrationRoutes(clean); len(got) != 0 {
			t.Errorf("the registration detector flags clean routes: %v", got)
		}
		// request body schemas
		for name, schema := range map[string]string{
			"a command member":       `{"type":"object","additionalProperties":false,"properties":{"command":{"type":"string"}}}`,
			"an mcp server":          `{"type":"object","additionalProperties":false,"properties":{"mcp_server":{"type":"object","additionalProperties":false,"properties":{"x":{"type":"string"}}}}}`,
			"a nested url":           `{"type":"object","additionalProperties":false,"properties":{"a":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"url":{"type":"string"}}}}}}`,
			"a tool definition":      `{"type":"object","additionalProperties":false,"properties":{"tools":{"type":"array","items":{"type":"string"}}}}`,
			"an agent":               `{"type":"object","additionalProperties":false,"properties":{"agent":{"type":"string"}}}`,
			"a free object":          `{"type":"object"}`,
			"additional members":     `{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":true}`,
			"members by pattern":     `{"type":"object","additionalProperties":false,"properties":{"a":{"type":"string"}},"patternProperties":{"^x-":{}}}`,
			"a free object in oneOf": `{"oneOf":[{"type":"object","additionalProperties":false,"properties":{"a":{"type":"string"}}},{"type":"object"}]}`,
		} {
			var node any
			if err := json.Unmarshal([]byte(schema), &node); err != nil {
				t.Fatal(err)
			}
			if got := docs.freeform("", node); len(got) == 0 {
				t.Errorf("the free-form detector passes %s", name)
			}
		}
		for name, schema := range map[string]string{
			"the source registration": `{"type":"object","additionalProperties":false,"required":["declaration"],"properties":{"declaration":{"type":"string"}}}`,
			"a closed list":           `{"type":"object","additionalProperties":false,"properties":{"ids":{"type":"array","items":{"type":"string"}},"n":{"type":"integer"}}}`,
		} {
			var node any
			if err := json.Unmarshal([]byte(schema), &node); err != nil {
				t.Fatal(err)
			}
			if got := docs.freeform("", node); len(got) != 0 {
				t.Errorf("the free-form detector flags %s: %v", name, got)
			}
		}
	})

	t.Run("a planted registration route on the real server is found", func(t *testing.T) {
		bad := newRig(t, withPlanted)
		got := registrationRoutes(bad.srv.Routes())
		want := []string{"POST /v1/mcp/servers", "POST /v1/tools", "PUT /v1/agents/{agent_id}"}
		for _, w := range want {
			found := false
			for _, g := range got {
				found = found || g == w
			}
			if !found {
				t.Errorf("the planted route %s is not reported (got %v)", w, got)
			}
		}
		// and the live probe sees it answer
		rec := bad.do(bad.token(libraryApp, capsForTraffic...), http.MethodPost, "/v1/mcp/servers", "application/json", []byte(`{}`))
		if rec.Code != http.StatusOK {
			t.Errorf("the planted route answers %d: the live probe below could not tell it from a missing one", rec.Code)
		}
		notInSpec, _ := routeDiff(bad.srv.Routes(), specRoutes(ops))
		if len(notInSpec) < 6 {
			t.Errorf("the specification comparison reports only %v of the planted routes", notInSpec)
		}
	})

	r := newRig(t)
	r.requireEveryAreaMounted()
	routes := r.srv.Routes()

	t.Run("no route registers a server, tool, agent, API or source but the two documented ones", func(t *testing.T) {
		if len(routes) != len(ops) {
			t.Fatalf("%d routes, %d operations in the specification", len(routes), len(ops))
		}
		if got := registrationRoutes(routes); len(got) != 0 {
			t.Fatalf("registration routes beyond the documented ones: %v", got)
		}
		for route := range documentedRegistrations {
			found := false
			for _, have := range routes {
				found = found || have == route
			}
			if !found {
				t.Errorf("the documented registration route %s does not exist: the allow-list is stale", route)
			}
		}
		for _, route := range routes {
			segs, _ := staticSegments(route)
			for _, s := range segs {
				switch s {
				case "activate", "approve", "unquarantine", "accept":
					t.Errorf("route %s looks like a way to activate a package without the integration audit", route)
				}
			}
		}
	})

	t.Run("no request body accepts a free-form definition", func(t *testing.T) {
		mutating := 0
		for _, o := range ops {
			if o.Method != http.MethodPost && o.Method != http.MethodPut && o.Method != http.MethodPatch {
				continue
			}
			mutating++
			for _, b := range docs.requestBodies(o) {
				switch b.Media {
				case "application/json", "multipart/form-data":
				case "application/gzip":
					if o.Route() != "POST /v1/applications" {
						t.Errorf("%s accepts an archive: only a package registration may", o.Route())
					}
				default:
					t.Errorf("%s accepts the media type %s", o.Route(), b.Media)
				}
				found := docs.freeform(b.File, b.Schema)
				if o.Route() == "POST /v1/agui" && b.Media == "application/json" {
					// the one documented exception: it must be exactly what is documented
					if strings.Join(found, "\n") != strings.Join(sorted(aguiExceptionFindings), "\n") {
						t.Errorf("the AG-UI run body's free-form members changed:\n got %q\nwant %q", found, sorted(aguiExceptionFindings))
					}
					continue
				}
				if len(found) > 0 {
					t.Errorf("%s accepts a free-form definition [%s]: %v", o.Route(), b.Media, found)
				}
			}
		}
		if mutating < 20 {
			t.Fatalf("only %d mutating operations were checked", mutating)
		}
	})

	// The registered package of the rest of the test.
	archive := packArchive(t)
	register := r.token(libraryApp, gateway.CapabilityRegisterApplication)

	t.Run("a package is refused unless it is the token's own, signed and clean", func(t *testing.T) {
		before := records(t, r, libraryApp)
		// another application's token
		rec := r.do(r.token(otherApp, gateway.CapabilityRegisterApplication), http.MethodPost, "/v1/applications", "application/gzip", archive)
		wantError(t, rec, http.StatusForbidden, contracts.ErrScopeViolation)
		// no registration capability
		rec = r.do(r.token(libraryApp, capsForTraffic...), http.MethodPost, "/v1/applications", "application/gzip", archive)
		if rec.Code != http.StatusForbidden {
			t.Errorf("registration without the capability = %d, want 403", rec.Code)
		}
		// no token
		if rec = r.do("", http.MethodPost, "/v1/applications", "application/gzip", archive); rec.Code != http.StatusUnauthorized {
			t.Errorf("registration without a token = %d, want 401", rec.Code)
		}
		// a JSON body that names a server instead of an archive
		rec = r.do(r.token(libraryApp, gateway.CapabilityRegisterApplication), http.MethodPost, "/v1/applications", "application/json", []byte(`{"mcp_server":{"url":"http://evil.example/mcp"}}`))
		if rec.Code != http.StatusUnsupportedMediaType && rec.Code != http.StatusBadRequest {
			t.Errorf("registration with a JSON definition = %d, want 415 or 400", rec.Code)
		}
		// an archive that traverses
		rec = r.do(r.token(libraryApp, gateway.CapabilityRegisterApplication), http.MethodPost, "/v1/applications", "application/gzip", hostileArchive(t))
		if rec.Code < 400 || rec.Code >= 500 {
			t.Errorf("registration of a hostile archive = %d, want 4xx", rec.Code)
		}
		if after := records(t, r, libraryApp); after != before {
			t.Fatalf("a refused registration left %d records (had %d)", after, before)
		}
	})

	t.Run("a registered package is quarantined and serves no traffic", func(t *testing.T) {
		rec := r.do(register, http.MethodPost, "/v1/applications", "application/gzip", archive)
		if rec.Code != http.StatusCreated {
			t.Fatalf("registration = %d %s", rec.Code, rec.Body.String())
		}
		var reg struct {
			ApplicationID string `json:"application_id"`
			Created       bool   `json:"created"`
			Package       struct {
				State string `json:"state"`
			} `json:"package"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &reg); err != nil || reg.ApplicationID != libraryApp || !reg.Created || reg.Package.State != "quarantined" {
			t.Fatalf("registration answered %s", rec.Body.String())
		}
		// Every route that serves traffic refuses with quarantined, and no model or
		// adapter is reached (the engine here has no script and the adapters are absent).
		served := 0
		for _, route := range append(append(append(r.srv.AreaRoutes("v1_sessions"), r.srv.AreaRoutes("v1_actions")...), r.srv.AreaRoutes("v1_agui")...), "GET /v1/capabilities") {
			method, path, _ := strings.Cut(route, " ")
			if route == "POST /v1/applications" || route == "GET /v1/applications/{application_id}" || route == "GET /v1/health" {
				continue
			}
			body := []byte(nil)
			if method == http.MethodPost || method == http.MethodPut {
				body = []byte(`{}`)
			}
			rec := r.do(r.token(libraryApp, capsForTraffic...), method, substitute(path), "application/json", body)
			wantError(t, rec, http.StatusForbidden, contracts.ErrQuarantined)
			served++
		}
		if served < 8 {
			t.Fatalf("only %d traffic routes were checked", served)
		}
		// Control: the same routes serve once the integration audit has activated the
		// package (the audit is the only writer of that state; here its effect is written
		// directly, as the gateway's own tests do).
		if _, err := r.db.ExecContext(context.Background(),
			`UPDATE apppkg_packages SET status = 'active', audit_report_id = 'rep_sec_054' WHERE application_id = ? AND status = 'quarantined'`, libraryApp); err != nil {
			t.Fatal(err)
		}
		rec = r.do(r.token(libraryApp, capsForTraffic...), http.MethodGet, "/v1/capabilities", "", nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "reservations.create") {
			t.Fatalf("control: GET /v1/capabilities after activation = %d %.200s", rec.Code, rec.Body.String())
		}
	})

	t.Run("the AG-UI exception registers no tool", func(t *testing.T) {
		if got := clientProvidedTools(t, r); got {
			t.Fatal("the deployment claims client-provided tools although the host registered none")
		}
		// Control: a host that registers a UI tool for the application is reported as having
		// one, so the document does say so when it is true, and false above means none.
		with := newRig(t, func(o *rigOptions) { o.uiTools = map[string][]string{libraryApp: {"open_details_panel"}} })
		with.registerAndActivate()
		if !clientProvidedTools(t, with) {
			t.Fatal("control: a deployment whose host registered a UI tool is not reported as having client-provided tools")
		}
	})

	t.Run("bodies that carry a definition never reach a registry", func(t *testing.T) {
		tok := func(extra ...string) string { return r.token(libraryApp, append(extra, capsForTraffic...)...) }
		hostile := []string{
			`{"command":"rm -rf /"}`, `{"tools":[{"name":"shell"}]}`, `{"mcp_server":{"url":"http://evil.example/mcp"}}`,
			`{"agent":{"name":"x"}}`, `{"url":"http://evil.example/"}`, `{"openapi":"3.1.0"}`,
		}
		for _, body := range hostile {
			rec := r.do(tok(), http.MethodPost, "/v1/sessions", "application/json", []byte(body))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("POST /v1/sessions %s = %d, want 400", body, rec.Code)
			}
		}
		// Control: the closed body of the same route is accepted.
		if rec := r.do(tok(), http.MethodPost, "/v1/sessions", "application/json", []byte(`{}`)); rec.Code != http.StatusOK {
			t.Errorf("control: POST /v1/sessions {} = %d", rec.Code)
		}
		// Source registration: a body that names anything but the package's declaration is
		// refused before the registry is asked; the declaration alone reaches it.
		before := r.rec.count("rag.register")
		for _, body := range []string{
			`{"declaration":"loan-policies","url":"http://evil.example/"}`, `{"declaration":"x","command":"x"}`, `{"url":"http://evil.example/"}`,
			`{"declaration":"x","root":"/etc"}`, `{"declaration":{"url":"http://evil.example/"}}`,
		} {
			rec := r.do(tok(), http.MethodPost, "/v1/rag/sources", "application/json", []byte(body))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("POST /v1/rag/sources %s = %d, want 400", body, rec.Code)
			}
		}
		if r.rec.count("rag.register") != before {
			t.Fatal("a source registration with a free-form member reached the registry")
		}
		_ = r.do(tok(), http.MethodPost, "/v1/rag/sources", "application/json", []byte(`{"declaration":"loan-policies"}`))
		if r.rec.count("rag.register") != before+1 {
			t.Errorf("control: the declaration alone did not reach the registry (calls %d -> %d)", before, r.rec.count("rag.register"))
		}
		// Settings: a body that tries to enable a tool is a closed-schema violation.
		for _, level := range []string{"user", "tenant", "deployment"} {
			// A nested setting that names a tool is the settings service's to refuse against the
			// closed settings schemas (internal/settings); the gateway refuses top-level members.
			for _, body := range []string{`{"expected_version":0,"tools":["shell"]}`, `{"expected_version":0,"mcp_servers":[{"url":"http://evil.example"}]}`, `{"expected_version":0,"settings":{},"command":"x"}`} {
				rec := r.do(tok("settings."+level+".write"), http.MethodPut, "/v1/settings/"+level, "application/json", []byte(body))
				if rec.Code != http.StatusBadRequest && rec.Code != http.StatusForbidden && rec.Code != http.StatusUnprocessableEntity {
					t.Errorf("PUT /v1/settings/%s %s = %d, want a refusal", level, body, rec.Code)
				}
			}
		}
	})

	t.Run("no path where a registration could live answers", func(t *testing.T) {
		paths := forbiddenPaths()
		have := set(routes...)
		checked := 0
		for _, p := range paths {
			for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
				if have[m+" "+p] {
					t.Errorf("the forbidden path %s %s is a registered route", m, p)
					continue
				}
				for _, tok := range []string{"", r.token(libraryApp, append([]string{gateway.CapabilityRegisterApplication}, capsForTraffic...)...)} {
					rec := r.do(tok, m, p, "application/json", []byte(`{"url":"http://evil.example/","command":"x"}`))
					if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
						t.Errorf("%s %s (token: %v) = %d, want 404 or 405", m, p, tok != "", rec.Code)
					}
					checked++
				}
			}
		}
		if checked < 300 {
			t.Fatalf("only %d probes were sent", checked)
		}
		// Control: on the server with the routes planted, the same probes find them.
		bad := newRig(t, withPlanted)
		found := 0
		for _, c := range []struct{ m, p string }{{"POST", "/v1/mcp/servers"}, {"POST", "/v1/tools"}, {"PUT", "/v1/agents/x"}, {"POST", "/exec"}, {"POST", "/models/apply"}} {
			rec := bad.do(bad.token(libraryApp, capsForTraffic...), c.m, c.p, "application/json", []byte(`{}`))
			if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
				found++
			}
		}
		if found != 5 {
			t.Errorf("the probe found %d of the 5 planted routes", found)
		}
	})
}

// forbiddenPaths are the paths where a free-form registration, an execution surface or a
// model administration route would live.
func forbiddenPaths() []string {
	return []string{
		"/v1/mcp", "/v1/mcp/servers", "/v1/mcp/register", "/v1/tools", "/v1/tools/register", "/v1/agents", "/v1/agents/register", "/v1/apis",
		"/v1/sources", "/v1/connectors", "/v1/plugins", "/v1/integrations", "/v1/servers",
		"/v1/applications/demo-library/tools", "/v1/applications/demo-library/mcp", "/v1/applications/demo-library/activate",
		"/v1/applications/demo-library/endpoints", "/v1/settings/tools", "/v1/settings/mcp", "/v1/rag/sources/x_1/tools",
		"/v1/rag/endpoints", "/v1/models", "/v1/models/apply", "/v1/models/install", "/models/apply", "/v1/backends/install",
		"/v1/gallery", "/cors-proxy", "/api/cors-proxy", "/exec", "/v1/exec", "/shell", "/admin", "/metrics", "/debug/pprof/",
		"/v0/dev/propose", "/v1/chat/completions", "/swagger", "/webui",
	}
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func records(t testing.TB, r *rig, app string) int {
	t.Helper()
	recs, err := r.apps.Records(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	return len(recs)
}

// wantError checks an error envelope's status and code.
func wantError(t testing.TB, rec *httptest.ResponseRecorder, status int, code contracts.ErrCode) {
	t.Helper()
	var env struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("status %d, body %q is not an error envelope", rec.Code, rec.Body.String())
	}
	if rec.Code != status || env.Code != string(code) {
		t.Fatalf("status %d code %q, want %d %q (%s)", rec.Code, env.Code, status, code, rec.Body.String())
	}
}

// hostileArchive is a tar.gz with a path-traversing entry.
func hostileArchive(t testing.TB) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range []struct{ name, data string }{{"./alixai.yaml", "format: x\n"}, {"../../evil.txt", "x"}} {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(e.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// clientProvidedTools reads agent.tools.clientProvided of the AG-UI capabilities document
// of the (active) application of a rig.
func clientProvidedTools(t testing.TB, r *rig) bool {
	t.Helper()
	rec := r.do(r.token(libraryApp, capsForTraffic...), http.MethodGet, "/v1/agui/capabilities", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/agui/capabilities = %d %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Agent struct {
			Tools struct {
				ClientProvided *bool `json:"clientProvided"`
			} `json:"tools"`
		} `json:"agent"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || doc.Agent.Tools.ClientProvided == nil {
		t.Fatalf("the capabilities document has no agent.tools.clientProvided: %v", err)
	}
	return *doc.Agent.Tools.ClientProvided
}
