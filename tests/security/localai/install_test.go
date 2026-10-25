package localaisec

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/tests/security/secutil"
)

// installSegments are the path segments of model and backend management on LocalAI (install, apply, galleries,
// backends) and of its CORS proxy: nothing AlixAI serves may carry one.
var installSegments = map[string]bool{
	"apply": true, "install": true, "installs": true, "gallery": true, "galleries": true,
	"backend": true, "backends": true, "pull": true, "cors-proxy": true,
}

// modelSubpaths are the sub-paths of a "models" resource that manage the model set instead of reading it.
var modelSubpaths = map[string]bool{"available": true, "delete": true, "jobs": true, "import": true}

// installSurfaceProblem reports why a route lets a caller install, apply or manage models or backends, or "" when it
// does not. A route with no method (a path found in a schema or a constant) is judged as if it were a write.
func installSurfaceProblem(method, route string) string {
	segs := strings.Split(strings.Trim(strings.ToLower(route), "/"), "/")
	for i, s := range segs {
		switch {
		case installSegments[s]:
			return fmt.Sprintf("the path segment %q is model or backend management", s)
		case s == "models" && method != "GET" && method != "":
			return fmt.Sprintf("%s on a models resource changes the model set", method)
		case s == "models" && i+1 < len(segs) && modelSubpaths[segs[i+1]]:
			return fmt.Sprintf("models/%s manages the model set", segs[i+1])
		}
	}
	return ""
}

var (
	routeLiteralRE = regexp.MustCompile(`"(GET|POST|PUT|PATCH|DELETE) (/[^"\s]*)"`)
	pathLiteralRE  = regexp.MustCompile(`"(/[A-Za-z0-9_{}.\-/]+)"`)
)

type route struct{ method, path, from string }

// routesInGoSource finds the route patterns and path constants a Go source file registers.
func routesInGoSource(file, src string) []route {
	var out []route
	for _, m := range routeLiteralRE.FindAllStringSubmatch(src, -1) {
		out = append(out, route{m[1], m[2], file})
	}
	for _, m := range pathLiteralRE.FindAllStringSubmatch(src, -1) {
		out = append(out, route{"", m[1], file})
	}
	return out
}

// routesInOpenAPI finds the paths of an OpenAPI document or fragment: every mapping key that starts with a slash.
func routesInOpenAPI(file, text string) ([]route, error) {
	var doc any
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	var out []route
	walkYAML(doc, "", func(_, k string, v any) {
		if !strings.HasPrefix(k, "/") {
			return
		}
		if ops, ok := v.(map[string]any); ok {
			for method := range ops {
				switch strings.ToUpper(method) {
				case "GET", "POST", "PUT", "PATCH", "DELETE":
					out = append(out, route{strings.ToUpper(method), k, file})
				}
			}
		}
		out = append(out, route{"", k, file})
	}, func(string, string) {})
	return out, nil
}

func routeProblems(rs []route) []string {
	var ps []string
	for _, r := range rs {
		if why := installSurfaceProblem(r.method, r.path); why != "" {
			ps = append(ps, fmt.Sprintf("%s: %s %s: %s", r.from, r.method, r.path, why))
		}
	}
	sort.Strings(ps)
	return dedupeStrings(ps)
}

var (
	sha256RE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hfCommitRE = regexp.MustCompile(`^/[^/]+/[^/]+/resolve/[0-9a-f]{40}/.+`)
)

// modelDownloadProblems judges the download_files of the model definitions: a download is allowed only with its
// sha256 pin, from an https source, and a Hugging Face file only at a commit, never a branch or tag that can move.
func modelDownloadProblems(tr tree, roots ...string) (problems []string, entries int) {
	for _, rel := range tr.sorted() {
		under := false
		for _, r := range roots {
			under = under || strings.HasPrefix(rel, r)
		}
		if !under || (path.Ext(rel) != ".yaml" && path.Ext(rel) != ".yml") {
			continue
		}
		var doc any
		if yaml.Unmarshal([]byte(tr[rel]), &doc) != nil {
			continue
		}
		walkYAML(doc, "", func(at, k string, v any) {
			if k != "download_files" {
				return
			}
			list, _ := v.([]any)
			for i, e := range list {
				entries++
				m, _ := e.(map[string]any)
				where := fmt.Sprintf("%s: %s[%d]", rel, at, i)
				sum, _ := m["sha256"].(string)
				if !sha256RE.MatchString(strings.ToLower(strings.TrimSpace(sum))) {
					problems = append(problems, where+": no sha256 pin: an unverified download")
				}
				uri, _ := m["uri"].(string)
				u, err := url.Parse(uri)
				switch {
				case err != nil || u.Scheme != "https":
					problems = append(problems, fmt.Sprintf("%s: uri %q is not an https source", where, uri))
				case u.Host == "huggingface.co" && !hfCommitRE.MatchString(u.Path):
					problems = append(problems, fmt.Sprintf("%s: uri %q is not pinned to a commit", where, uri))
				}
			}
		}, func(string, string) {})
	}
	sort.Strings(problems)
	return problems, entries
}

// SEC-069
func TestSecNoModelInstallSurface(t *testing.T) {
	doc := string(secutil.ReadFile(t, checklistPath))

	t.Run("no_deployment_enables_a_gallery_or_an_install_source", func(t *testing.T) {
		ex, problems := parseExceptions(doc)
		for _, p := range problems {
			t.Error(p)
		}
		tr := repoTree(t)
		d := discover(tr)
		if err := scanned(d); err != nil {
			t.Fatalf("discovery: %v", err)
		}
		c := checkCtx{ex: exceptions{byRow: ex, now: time.Now()}, tr: tr}
		for _, l := range d.Launchers {
			for _, f := range c.check(l) {
				// gallery endpoint and galleries (HC-14), external backends (HC-18), the safety scan before a download (HC-22)
				if f.Rule == "HC-14" || f.Rule == "HC-18" || f.Rule == "HC-22" {
					t.Error(f)
				}
			}
			s := l.Env["LOCALAI_DISABLE_GALLERY_ENDPOINT"]
			if !l.EnvOnly && (s == nil || len(s.Values) != 1 || s.Values[0] != "true") {
				t.Errorf("%s: the gallery endpoint is not disabled", l.Name)
			}
		}
	})

	t.Run("gallery_detector_flags_planted_violations", func(t *testing.T) {
		for name, mut := range map[string]func(*sample){
			"gallery endpoint on":   func(s *sample) { s.env["LOCALAI_DISABLE_GALLERY_ENDPOINT"] = "false" },
			"gallery endpoint gone": func(s *sample) { s.absent["LOCALAI_DISABLE_GALLERY_ENDPOINT"] = true },
			"a model gallery":       func(s *sample) { s.env["LOCALAI_GALLERIES"] = `[{"name":"hf","url":"https://example.invalid/g.yaml"}]` },
			"autoload galleries":    func(s *sample) { s.env["LOCALAI_AUTOLOAD_GALLERIES"] = "true" },
			"a URL as backend":      func(s *sample) { s.env["LOCALAI_EXTERNAL_BACKENDS"] = "oci://example.invalid/b:latest" },
			"safety scan off":       func(s *sample) { s.env["LOCALAI_DISABLE_PREDOWNLOAD_SCAN"] = "true" },
		} {
			s := cleanSample()
			mut(&s)
			fs, n, _ := checkTree(composeTree(s), exceptionsDoc("2027-04-03", allExceptions...))
			if n == 0 || !(hasRule(fs, "HC-14") || hasRule(fs, "HC-18") || hasRule(fs, "HC-22")) {
				t.Errorf("%s: %d launchers, findings %v: not flagged", name, n, fs)
			}
		}
	})

	t.Run("no_alixai_route_accepts_an_install", func(t *testing.T) {
		var rs []route

		// 1. The routes of a real gateway server: every area the binary registers, as far as a server with no
		// application services mounts them (the others are covered by the source and schema scans below).
		srv, err := gateway.NewServer(gateway.Options{Dev: gateway.DevConfig{SpikeEndpoint: true}})
		if err != nil {
			t.Fatalf("a gateway server could not be built to list its routes: %v", err)
		}
		live := srv.Routes()
		if len(live) < 3 {
			t.Fatalf("the real server lists %d routes: %v", len(live), live)
		}
		for _, p := range live {
			m, rp, _ := strings.Cut(p, " ")
			rs = append(rs, route{m, rp, "gateway.NewServer"})
		}

		// 2. Every route pattern and path constant in the gateway's Go source, including the areas that need services.
		goFiles := 0
		for _, rel := range secutil.RepoFiles(t) {
			if strings.HasPrefix(rel, "internal/gateway/") && strings.HasSuffix(rel, ".go") && !strings.HasSuffix(rel, "_test.go") {
				goFiles++
				rs = append(rs, routesInGoSource(rel, string(secutil.ReadFile(t, rel)))...)
			}
		}
		if goFiles < 10 {
			t.Fatalf("only %d gateway source files were scanned", goFiles)
		}

		// 3. Every path of the published API description.
		specs := 0
		for _, rel := range secutil.RepoFiles(t) {
			if strings.HasPrefix(rel, "schemas/api/") && (strings.HasSuffix(rel, ".yaml") || strings.HasSuffix(rel, ".yml")) {
				specs++
				found, err := routesInOpenAPI(rel, string(secutil.ReadFile(t, rel)))
				if err != nil {
					t.Error(err)
					continue
				}
				rs = append(rs, found...)
			}
		}
		if specs == 0 {
			t.Fatal("no OpenAPI file was scanned")
		}
		v1 := 0
		for _, r := range rs {
			if strings.HasPrefix(r.path, "/v1/") || strings.HasPrefix(r.path, "/sessions") || strings.HasPrefix(r.path, "/actions") {
				v1++
			}
		}
		if v1 < 10 {
			t.Fatalf("only %d /v1-style routes were found across the three sources: the scans are blind", v1)
		}
		for _, p := range routeProblems(rs) {
			t.Error(p)
		}
		t.Logf("%d routes and paths judged (%d gateway source files, %d API files)", len(rs), goFiles, specs)

		// Control on the real source: one planted registration in a copy of a real gateway file is found.
		file := "internal/gateway/routes_ops.go"
		src := string(secutil.ReadFile(t, file))
		for _, plant := range []string{
			`rt.HandleFunc("POST /models/apply", nil)`,
			`v1.HandleFunc("POST /backends/apply", h.apply)`,
			`v1.HandleFunc("GET /models/galleries", h.list)`,
			`rt.HandleFunc("POST /v1/voice/packs/install", nil)`,
			`rt.HandleFunc("GET /api/cors-proxy", nil)`,
		} {
			if got := routeProblems(routesInGoSource(file, src+"\nfunc planted() { "+plant+" }\n")); len(got) == 0 {
				t.Errorf("planting %s in %s was not found", plant, file)
			}
		}
	})

	t.Run("route_detector_judges_planted_routes", func(t *testing.T) {
		for _, bad := range []string{
			"POST /v1/models/apply", "POST /models/apply", "POST /backends/apply", "GET /backends", "GET /v1/backends/available",
			"GET /models/available", "GET /models/galleries", "POST /models/galleries", "POST /models/install", "DELETE /models/delete/x",
			"POST /v1/models", "DELETE /v1/models/{id}", "PUT /v1/models/{id}", "POST /api/models/install/{id}", "POST /v1/voice/packs/install",
			"POST /v1/gallery/refresh", "GET /api/cors-proxy", "POST /v1/ops/pull",
		} {
			m, p, _ := strings.Cut(bad, " ")
			if installSurfaceProblem(m, p) == "" {
				t.Errorf("%s was not flagged", bad)
			}
		}
		for _, ok := range []string{
			"GET /v1/models", "GET /healthz", "GET /readyz", "POST /v1/sessions", "POST /v1/sessions/{session_id}/turns", "POST /v1/actions/{preparation_id}/confirm",
			"GET /v1/ops/providers", "POST /v1/files", "GET /v1/files/{file_id}/content", "GET /v1/settings/effective", "PUT /v1/settings/user", "POST /v1/rag/sources",
		} {
			m, p, _ := strings.Cut(ok, " ")
			if why := installSurfaceProblem(m, p); why != "" {
				t.Errorf("%s was flagged: %s", ok, why)
			}
		}
		rs, err := routesInOpenAPI("planted.yaml", "paths:\n  /v1/models/apply:\n    post: {}\n  /v1/sessions:\n    get: {}\n")
		if err != nil || len(routeProblems(rs)) == 0 {
			t.Errorf("an OpenAPI path /v1/models/apply was not flagged: %v %v", rs, err)
		}
		if got := routeProblems(routesInGoSource("planted.go", `h.HandleFunc("GET /v1/sessions", f); const p = "/admin/registry/refresh"`)); len(got) != 0 {
			t.Errorf("benign routes were flagged: %v", got)
		}
	})

	t.Run("model_downloads_are_pinned", func(t *testing.T) {
		tr := repoTree(t)
		ps, n := modelDownloadProblems(tr, "deploy/", "models/")
		if n == 0 {
			t.Fatal("no download_files entry was found: the scan is blind")
		}
		for _, p := range ps {
			t.Error(p)
		}
		pin := strings.Repeat("a", 64)
		okEntry := "- name: m\n  download_files:\n    - filename: m.gguf\n      uri: https://huggingface.co/o/r/resolve/" + strings.Repeat("b", 40) + "/m.gguf\n      sha256: " + pin + "\n"
		if ps, n := modelDownloadProblems(tree{"deploy/m.yaml": okEntry}, "deploy/"); n != 1 || len(ps) != 0 {
			t.Errorf("a pinned download: %d entries, problems %v", n, ps)
		}
		for name, bad := range map[string]string{
			"no sha256":       "- download_files:\n    - filename: m\n      uri: https://huggingface.co/o/r/resolve/" + strings.Repeat("b", 40) + "/m.gguf\n",
			"short sha256":    "- download_files:\n    - filename: m\n      uri: https://huggingface.co/o/r/resolve/" + strings.Repeat("b", 40) + "/m.gguf\n      sha256: abc\n",
			"a branch":        "- download_files:\n    - filename: m\n      uri: https://huggingface.co/o/r/resolve/main/m.gguf\n      sha256: " + pin + "\n",
			"plain http":      "- download_files:\n    - filename: m\n      uri: http://example.invalid/m.gguf\n      sha256: " + pin + "\n",
			"a file source":   "- download_files:\n    - filename: m\n      uri: file:///etc/passwd\n      sha256: " + pin + "\n",
			"an OCI source":   "- download_files:\n    - filename: m\n      uri: oci://example.invalid/m:latest\n      sha256: " + pin + "\n",
			"a nested config": "x:\n  - download_files:\n      - filename: m\n        uri: https://example.invalid/m\n",
		} {
			if ps, _ := modelDownloadProblems(tree{"deploy/m.yaml": bad}, "deploy/"); len(ps) == 0 {
				t.Errorf("%s was not flagged", name)
			}
		}
	})

	// The live half: a running stack refuses the install and apply surfaces, with and without the key.
	secLocalAILiveInstallProbe(t)
}
