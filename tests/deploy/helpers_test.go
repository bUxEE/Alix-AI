package deploy

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Static checks of the container packaging (M5-01): the stack compose files, the Quadlet units and the
// Dockerfile. Nothing here starts a container; the build smoke is behind the live tag. Every scan fails when
// it finds nothing, and every detector has inline negative cases, so a rule cannot pass by looking at
// nothing or by being unable to see its violation.

const (
	stackDir   = "deploy/container/stack"
	quadletDir = "deploy/container/quadlet"
	localAIDir = "deploy/container/localai"
	dockerfile = "deploy/container/alixai/Dockerfile"
)

// repoRoot returns the checkout root: the nearest parent of the test's directory holding go.mod.
func repoRoot(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

func readRepoFile(t testing.TB, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return data
}

// repoGlob lists files of the checkout matching a slash-separated glob below dir, sorted.
func repoGlob(t testing.TB, dir, pattern string) []string {
	t.Helper()
	root := repoRoot(t)
	matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(dir), filepath.FromSlash(pattern)))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		rel, err := filepath.Rel(root, m)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, filepath.ToSlash(rel))
	}
	sort.Strings(out)
	return out
}

// ---- compose ----

// compose is one decoded compose file.
type compose struct {
	path     string
	doc      map[string]any
	services map[string]map[string]any
}

func parseCompose(path string, data []byte) (compose, error) {
	c := compose{path: path, services: map[string]map[string]any{}}
	if err := yaml.Unmarshal(data, &c.doc); err != nil {
		return c, err
	}
	raw, ok := c.doc["services"]
	if !ok || raw == nil {
		return c, nil
	}
	svcs, ok := raw.(map[string]any)
	if !ok {
		return c, fmt.Errorf("services is %T, want a mapping", raw)
	}
	for name, v := range svcs {
		m, ok := v.(map[string]any)
		if !ok {
			return c, fmt.Errorf("service %s is %T, want a mapping", name, v)
		}
		c.services[name] = m
	}
	return c, nil
}

func loadCompose(t testing.TB, rel string) compose {
	t.Helper()
	c, err := parseCompose(rel, readRepoFile(t, rel))
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return c
}

// stackComposeFiles lists every compose file of the stack, and fails when there are fewer than the six the
// deliverable names: base, three profiles and the two opt-in overrides.
func stackComposeFiles(t testing.TB) []string {
	t.Helper()
	files := repoGlob(t, stackDir, "compose*.yaml")
	if len(files) < 6 {
		t.Fatalf("found %d compose files %v under %s, want at least 6 (base, lite, balanced, 4g, external, noexternal)", len(files), files, stackDir)
	}
	return files
}

func (c compose) names() []string {
	out := make([]string, 0, len(c.services))
	for n := range c.services {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// listStrings returns the string elements of a YAML list.
func listStrings(v any) []string {
	var out []string
	for _, e := range asList(v) {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// envMap reads environment in its mapping or KEY=VALUE list form.
func envMap(svc map[string]any) map[string]string {
	env := map[string]string{}
	switch v := svc["environment"].(type) {
	case map[string]any:
		for k, val := range v {
			env[k] = strings.TrimSpace(fmt.Sprint(val))
		}
	case []any:
		for _, item := range v {
			k, val, _ := strings.Cut(fmt.Sprint(item), "=")
			env[strings.TrimSpace(k)] = strings.TrimSpace(val)
		}
	}
	return env
}

// uidOf returns the numeric uid of a compose `user` value ("10001:10001"), or -1 when it is not numeric.
func uidOf(user string) int {
	uid, _, _ := strings.Cut(strings.TrimSpace(user), ":")
	n := 0
	if uid == "" {
		return -1
	}
	for _, r := range uid {
		if r < '0' || r > '9' {
			return -1
		}
		n = n*10 + int(r-'0')
	}
	return n
}

var composeVar = regexp.MustCompile(`\$\{[^}]*\}`)

// volumeParts splits a short-syntax volume "source:target[:options]". A ${VAR:-default} in the source is
// replaced by a placeholder first: its colon is not a field separator.
func volumeParts(spec string) (source, target string, options []string) {
	masked := composeVar.ReplaceAllString(spec, "$$VAR")
	parts := strings.Split(masked, ":")
	switch len(parts) {
	case 1:
		return "", parts[0], nil // an anonymous volume
	case 2:
		return parts[0], parts[1], nil
	}
	return parts[0], parts[1], strings.Split(parts[2], ",")
}

func hasOption(options []string, want string) bool {
	for _, o := range options {
		if o == want {
			return true
		}
	}
	return false
}

// bindSource reports whether a volume source is a host path (a bind mount): relative, absolute or built from
// a variable. A bare name is a named volume, and an empty source an anonymous one.
func bindSource(source string) bool {
	return strings.HasPrefix(source, ".") || strings.HasPrefix(source, "/") || strings.HasPrefix(source, "$") || strings.HasPrefix(source, "~")
}

// ---- systemd-style units ----

// unit is a parsed Quadlet file: section -> key -> every value in order (a key may repeat).
type unit struct {
	path     string
	sections map[string]map[string][]string
}

func parseUnit(path string, data []byte) unit {
	u := unit{path: path, sections: map[string]map[string][]string{}}
	section := ""
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			if u.sections[section] == nil {
				u.sections[section] = map[string][]string{}
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || section == "" {
			continue
		}
		k = strings.TrimSpace(k)
		u.sections[section][k] = append(u.sections[section][k], strings.TrimSpace(v))
	}
	return u
}

func loadUnit(t testing.TB, rel string) unit {
	t.Helper()
	return parseUnit(rel, readRepoFile(t, rel))
}

// get returns the last value of a key (systemd: a later assignment wins), or "".
func (u unit) get(section, key string) string {
	v := u.sections[section][key]
	if len(v) == 0 {
		return ""
	}
	return v[len(v)-1]
}

func (u unit) all(section, key string) []string { return u.sections[section][key] }

func (u unit) has(section, key string) bool { return len(u.sections[section][key]) > 0 }

// unitName is the file name without directory.
func (u unit) unitName() string { return filepath.Base(u.path) }

func (u unit) keys(section string) []string {
	out := make([]string, 0, len(u.sections[section]))
	for k := range u.sections[section] {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sectionNames(u unit) []string {
	out := make([]string, 0, len(u.sections))
	for s := range u.sections {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// problemsOf joins the problems of several checks, sorted, so a failure lists everything at once.
func problemsOf(ps ...[]string) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p...)
	}
	sort.Strings(out)
	return out
}

func reportProblems(t *testing.T, ps []string) {
	t.Helper()
	for _, p := range ps {
		t.Error(p)
	}
}
