package repo

import (
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Container rules held for every compose file in the repository (the
// self-hosted and fail-closed rules): ports are
// published on loopback only, nothing runs privileged or in the host network,
// images are pinned by digest, and the shipped LocalAI service keeps its
// administrative and tool surfaces off.

const shippedLocalAICompose = "deploy/container/localai/compose.yaml"

var (
	composeName = regexp.MustCompile(`(^|/)((docker|podman)-)?compose([.-][^/]*)?\.ya?ml$|\.compose\.ya?ml$`)
	servicesKey = regexp.MustCompile(`(?m)^services:`)
	digestPin   = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
)

// composeFiles lists every compose file of the checkout: by name anywhere, and
// any YAML under deploy/ that declares top-level services.
func composeFiles(t testing.TB) []string {
	t.Helper()
	root := repoRoot(t)
	var out []string
	for _, rel := range repoFiles(t) {
		ext := path.Ext(rel)
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		if composeName.MatchString(rel) {
			out = append(out, rel)
			continue
		}
		if under(rel, "deploy/") {
			st, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
			if err != nil {
				t.Fatalf("stat %s: %v", rel, err)
			}
			if st.Size() > 1<<20 {
				continue // a compose file is small; this is data
			}
			data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			if servicesKey.Match(data) {
				out = append(out, rel)
			}
		}
	}
	return out
}

// composeServices decodes the services of one compose file.
func composeServices(data []byte) (map[string]map[string]any, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	raw, ok := doc["services"]
	if !ok || raw == nil {
		return map[string]map[string]any{}, nil // an override that adds only top-level keys
	}
	svcs, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("services is %T, want a mapping", raw)
	}
	out := make(map[string]map[string]any, len(svcs))
	for name, v := range svcs {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("service %s is %T, want a mapping", name, v)
		}
		out[name] = m
	}
	return out, nil
}

// shortPortProblem judges a short-syntax port mapping:
// [HOST_IP:]HOST:CONTAINER[/PROTOCOL]. A mapping without a host address, with
// a variable in the host address or with a non-loopback address would publish
// the port on the network. The host address must be a literal loopback IP.
func shortPortProblem(s string) string {
	spec := strings.TrimSpace(s)
	if i := strings.LastIndex(spec, "/"); i >= 0 {
		spec = spec[:i] // protocol
	}
	var host string
	if strings.HasPrefix(spec, "[") {
		end := strings.Index(spec, "]")
		if end < 0 || !strings.HasPrefix(spec[end+1:], ":") {
			return fmt.Sprintf("port %q is malformed", s)
		}
		host = spec[1:end]
	} else {
		i := strings.Index(spec, ":")
		if i < 0 {
			return fmt.Sprintf("port %q has no host address, so it binds every interface; write 127.0.0.1:HOST:CONTAINER", s)
		}
		host = spec[:i]
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Sprintf("port %q does not bind a literal loopback address (host part %q); write 127.0.0.1:HOST:CONTAINER", s, host)
	}
	return ""
}

// portProblem judges one entry of a service's ports list, in any syntax.
func portProblem(entry any) string {
	switch v := entry.(type) {
	case string:
		return shortPortProblem(v)
	case int, int64, uint64, float64:
		return fmt.Sprintf("port %v has no host address, so it binds every interface; write 127.0.0.1:HOST:CONTAINER", v)
	case map[string]any: // long syntax
		raw, ok := v["host_ip"]
		if !ok {
			return fmt.Sprintf("port %v has no host_ip, so it binds every interface", v["target"])
		}
		s, ok := raw.(string)
		if !ok {
			return fmt.Sprintf("port %v has a non-string host_ip %v", v["target"], raw)
		}
		if ip := net.ParseIP(s); ip == nil || !ip.IsLoopback() {
			return fmt.Sprintf("port %v has host_ip %q, which is not a literal loopback address", v["target"], s)
		}
		return ""
	}
	return fmt.Sprintf("port entry of unsupported form %T", entry)
}

// composeNetworkProblems returns the loopback and isolation problems of one
// compose file, and how many ports it publishes.
func composeNetworkProblems(data []byte) (problems []string, published int, err error) {
	svcs, err := composeServices(data)
	if err != nil {
		return nil, 0, err
	}
	names := make([]string, 0, len(svcs))
	for n := range svcs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		svc := svcs[name]
		if ports, ok := svc["ports"].([]any); ok {
			for _, p := range ports {
				published++
				if msg := portProblem(p); msg != "" {
					problems = append(problems, fmt.Sprintf("service %s: %s", name, msg))
				}
			}
		} else if svc["ports"] != nil {
			problems = append(problems, fmt.Sprintf("service %s: ports is %T, want a list", name, svc["ports"]))
		}
		if mode, _ := svc["network_mode"].(string); strings.TrimSpace(mode) == "host" {
			problems = append(problems, fmt.Sprintf("service %s: network_mode host shares the host network, so nothing is bound to loopback only", name))
		}
		if priv, _ := svc["privileged"].(bool); priv {
			problems = append(problems, fmt.Sprintf("service %s: privileged containers are not allowed", name))
		}
	}
	return problems, published, nil
}

func TestComposeLoopbackOnly(t *testing.T) {
	root := repoRoot(t)
	files := composeFiles(t)
	if len(files) < 4 {
		t.Fatalf("found only %d compose files %v; the discovery is not seeing the repository", len(files), files)
	}

	total := 0
	shipped := false
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		problems, published, err := composeNetworkProblems(data)
		if err != nil {
			t.Errorf("%s: cannot parse: %v", rel, err)
			continue
		}
		total += published
		if rel == shippedLocalAICompose && published > 0 {
			shipped = true
		}
		for _, p := range problems {
			t.Errorf("%s: %s", rel, p)
		}
	}
	if !shipped || total == 0 {
		t.Fatalf("the shipped LocalAI compose file %s publishes no port (total across files: %d); the scan is not seeing ports", shippedLocalAICompose, total)
	}
}

func TestComposeNetworkDetector(t *testing.T) {
	cases := []struct {
		name string
		port string // a YAML list item
		want bool   // a problem
	}{
		{"loopback short", `"127.0.0.1:8080:8080"`, false},
		{"loopback with protocol", `"127.0.0.1:8080:8080/tcp"`, false},
		{"loopback with variable port", `"127.0.0.1:${PORT:-8080}:8080"`, false},
		{"loopback ipv6", `"[::1]:8080:8080"`, false},
		{"loopback range", `"127.0.0.1:8000-8010:8000-8010"`, false},
		{"host and container port", `"8080:8080"`, true},
		{"container port only", `"8080"`, true},
		{"yaml integer", `8080`, true},
		{"all interfaces", `"0.0.0.0:8080:8080"`, true},
		{"all interfaces ipv6", `"[::]:8080:8080"`, true},
		{"lan address", `"192.168.1.5:8080:8080"`, true},
		{"variable host address", `"${BIND_ADDR:-127.0.0.1}:8080:8080"`, true},
		{"hostname", `"localhost:8080:8080"`, true},
		{"long, loopback", "{target: 8080, published: 8080, host_ip: 127.0.0.1}", false},
		{"long, no host_ip", "{target: 8080, published: 8080}", true},
		{"long, all interfaces", "{target: 8080, published: 8080, host_ip: 0.0.0.0}", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := "services:\n  app:\n    image: x\n    ports:\n      - " + tc.port + "\n"
			problems, published, err := composeNetworkProblems([]byte(doc))
			if err != nil {
				t.Fatal(err)
			}
			if published != 1 {
				t.Errorf("published = %d, want 1", published)
			}
			if (len(problems) > 0) != tc.want {
				t.Errorf("problems = %v, want a problem: %v", problems, tc.want)
			}
		})
	}

	for name, doc := range map[string]string{
		"host network": "services:\n  app:\n    network_mode: host\n",
		"privileged":   "services:\n  app:\n    privileged: true\n",
	} {
		problems, _, err := composeNetworkProblems([]byte(doc))
		if err != nil || len(problems) != 1 {
			t.Errorf("%s: problems = %v, err = %v; want exactly one problem", name, problems, err)
		}
	}
	for name, doc := range map[string]string{
		"an override without ports": "services:\n  app:\n    mem_limit: 4g\n",
		"expose is internal":        "services:\n  app:\n    expose: [\"8080\"]\n",
		"bridge network":            "services:\n  app:\n    network_mode: bridge\n    privileged: false\n",
		"no services":               "name: x\n",
	} {
		problems, published, err := composeNetworkProblems([]byte(doc))
		if err != nil || len(problems) != 0 || published != 0 {
			t.Errorf("%s: problems = %v, published = %d, err = %v; want none", name, problems, published, err)
		}
	}
	if _, _, err := composeNetworkProblems([]byte("services: [")); err == nil {
		t.Error("a malformed compose file was accepted")
	}
}

// ---- images pinned by digest ----

func composeImageProblems(data []byte) ([]string, error) {
	svcs, err := composeServices(data)
	if err != nil {
		return nil, err
	}
	var problems []string
	for name, svc := range svcs {
		img, ok := svc["image"].(string)
		if !ok {
			continue // an override that does not set the image
		}
		if !digestPin.MatchString(strings.TrimSpace(img)) {
			problems = append(problems, fmt.Sprintf("service %s: image %q is not pinned by digest (name@sha256:<64 hex>)", name, img))
		}
	}
	sort.Strings(problems)
	return problems, nil
}

func TestComposeImagesPinnedByDigest(t *testing.T) {
	root := repoRoot(t)
	images := 0
	for _, rel := range composeFiles(t) {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		problems, err := composeImageProblems(data)
		if err != nil {
			t.Errorf("%s: cannot parse: %v", rel, err)
			continue
		}
		if svcs, err := composeServices(data); err == nil {
			for _, svc := range svcs {
				if _, ok := svc["image"].(string); ok {
					images++
				}
			}
		}
		for _, p := range problems {
			t.Errorf("%s: %s", rel, p)
		}
	}
	if images == 0 {
		t.Fatal("no compose file names an image; the scan is not seeing services")
	}

	good := "services:\n  a:\n    image: docker.io/localai/localai:v4.10.0@sha256:0632c21ddbe440bb71bc7aa744796c227a51658641b216290e86d6894e1c705b\n"
	for name, doc := range map[string]string{
		"tag only":              "services:\n  a:\n    image: redis:7\n",
		"latest":                "services:\n  a:\n    image: redis\n",
		"short digest":          "services:\n  a:\n    image: redis@sha256:abc\n",
		"digest not at the end": "services:\n  a:\n    image: redis@sha256:0632c21ddbe440bb71bc7aa744796c227a51658641b216290e86d6894e1c705b:x\n",
	} {
		if p, err := composeImageProblems([]byte(doc)); err != nil || len(p) != 1 {
			t.Errorf("%s: problems = %v, err = %v; want one", name, p, err)
		}
	}
	if p, err := composeImageProblems([]byte(good)); err != nil || len(p) != 0 {
		t.Errorf("a digest-pinned image was rejected: %v, %v", p, err)
	}
}

// ---- LocalAI's administrative and tool surfaces ----

// localAIOffFlags are the environment settings that keep LocalAI's own web
// UI, gallery and settings endpoints, MCP and agent subsystems and peer
// networking off (docs/security/localai-hardening-checklist.md). The value
// is the one that keeps the surface off. Exit gate: "no LocalAI
// administrative or arbitrary tool surface is exposed".
var localAIOffFlags = map[string]string{
	"LOCALAI_DISABLE_WEBUI":            "true",
	"LOCALAI_DISABLE_GALLERY_ENDPOINT": "true",
	"LOCALAI_DISABLE_RUNTIME_SETTINGS": "true",
	"LOCALAI_DISABLE_MCP":              "true",
	"LOCALAI_DISABLE_AGENTS":           "true",
	"LOCALAI_P2P":                      "false",
	"LOCALAI_FEDERATED":                "false",
	"LOCALAI_DISTRIBUTED":              "false",
}

// serviceEnv returns a service's environment, from either the mapping or the
// KEY=VALUE list form.
func serviceEnv(svc map[string]any) map[string]string {
	env := map[string]string{}
	switch v := svc["environment"].(type) {
	case map[string]any:
		for k, val := range v {
			env[k] = strings.ToLower(strings.TrimSpace(fmt.Sprint(val)))
		}
	case []any:
		for _, item := range v {
			k, val, _ := strings.Cut(fmt.Sprint(item), "=")
			env[strings.TrimSpace(k)] = strings.ToLower(strings.TrimSpace(val))
		}
	}
	return env
}

// localAISurfaceProblems checks one compose file. Every file is held to "never
// switch a surface back on"; the shipped base file (shipped true) is also held
// to switching every one off and to requiring an API key.
func localAISurfaceProblems(data []byte, shipped bool) ([]string, error) {
	svcs, err := composeServices(data)
	if err != nil {
		return nil, err
	}
	var problems []string
	names := make([]string, 0, len(svcs))
	for n := range svcs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		env := serviceEnv(svcs[name])
		for flag, off := range localAIOffFlags {
			val, set := env[flag]
			switch {
			case set && val != off:
				problems = append(problems, fmt.Sprintf("service %s sets %s to %q; the surface it guards must stay off (%s=%s)", name, flag, val, flag, off))
			case shipped && name == "localai" && !set:
				problems = append(problems, fmt.Sprintf("service %s does not set %s=%s", name, flag, off))
			}
		}
		if shipped && name == "localai" {
			if key, ok := env["LOCALAI_API_KEY"]; !ok || !strings.Contains(key, ":?") {
				problems = append(problems, fmt.Sprintf("service %s must require LOCALAI_API_KEY (${LOCALAI_API_KEY:?...}) so it cannot start open; got %q", name, key))
			}
		}
	}
	sort.Strings(problems)
	return problems, nil
}

func TestLocalAIAdminSurfaceStaysOff(t *testing.T) {
	root := repoRoot(t)
	sawShipped := false
	for _, rel := range composeFiles(t) {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		shipped := rel == shippedLocalAICompose
		sawShipped = sawShipped || shipped
		problems, err := localAISurfaceProblems(data, shipped)
		if err != nil {
			t.Errorf("%s: cannot parse: %v", rel, err)
			continue
		}
		for _, p := range problems {
			t.Errorf("%s: %s", rel, p)
		}
	}
	if !sawShipped {
		t.Fatalf("%s is not among the compose files", shippedLocalAICompose)
	}

	all := "services:\n  localai:\n    environment:\n      LOCALAI_API_KEY: ${LOCALAI_API_KEY:?set it}\n"
	for flag, off := range localAIOffFlags {
		all += "      " + flag + ": \"" + off + "\"\n"
	}
	if p, err := localAISurfaceProblems([]byte(all), true); err != nil || len(p) != 0 {
		t.Fatalf("a compliant service was rejected: %v, %v", p, err)
	}
	// List-form environment is understood too.
	list := "services:\n  localai:\n    environment:\n      - LOCALAI_DISABLE_MCP=false\n"
	if p, _ := localAISurfaceProblems([]byte(list), false); len(p) != 1 {
		t.Errorf("a surface switched on in list form went unnoticed: %v", p)
	}
	on := strings.Replace(all, "LOCALAI_DISABLE_WEBUI: \"true\"", "LOCALAI_DISABLE_WEBUI: \"false\"", 1)
	if p, _ := localAISurfaceProblems([]byte(on), true); len(p) != 1 {
		t.Errorf("the web UI switched on went unnoticed: %v", p)
	}
	missing := strings.Replace(all, "      LOCALAI_P2P: \"false\"\n", "", 1)
	if p, _ := localAISurfaceProblems([]byte(missing), true); len(p) != 1 {
		t.Errorf("a missing off switch went unnoticed in the shipped file: %v", p)
	}
	if p, _ := localAISurfaceProblems([]byte(missing), false); len(p) != 0 {
		t.Errorf("an override that does not mention a flag was flagged: %v", p)
	}
	noKey := strings.Replace(all, "${LOCALAI_API_KEY:?set it}", "${LOCALAI_API_KEY:-}", 1)
	if p, _ := localAISurfaceProblems([]byte(noKey), true); len(p) != 1 {
		t.Errorf("an optional API key went unnoticed: %v", p)
	}
}
