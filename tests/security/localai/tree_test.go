package localaisec

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/tests/security/secutil"
)

// tree is a set of files by slash path relative to the repository root. The real run reads the checkout into
// one; the planted-violation controls build their own, so the same discovery and the same rules judge both.
type tree map[string]string

func (tr tree) read(p string) (string, bool) {
	s, ok := tr[path.Clean(p)]
	return s, ok
}

// sorted returns the paths of the tree in lexical order.
func (tr tree) sorted() []string {
	out := make([]string, 0, len(tr))
	for p := range tr {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// outOfScope lists the trees that hold recorded evidence and throw-away harnesses, never a deployment: the
// measurement runs of the M0 spikes (docs/evidence; one of its override files raises the log level to debug on
// purpose and says it is "never part of a deployment"), the documentation, and the tests themselves.
var outOfScope = []string{"docs/", "tests/", "plan/", ".worktrees/", "node_modules/"}

func inScope(rel string) bool {
	for _, p := range outOfScope {
		if strings.HasPrefix(rel, p) {
			return false
		}
	}
	return true
}

// candidateFile reports whether a file could define how something is launched: a compose file, a Quadlet or
// systemd unit, a drop-in, an env file, a supervisor configuration. The decision whether it launches LocalAI is
// made on its content, structurally, by discover.
func candidateFile(rel string) bool {
	base := path.Base(rel)
	switch {
	case strings.HasPrefix(base, ".env"), strings.HasSuffix(base, ".env"), strings.HasSuffix(base, ".env.example"):
		return true
	}
	switch path.Ext(rel) {
	case ".yaml", ".yml", ".container", ".service", ".conf", ".plist":
		return true
	}
	return false
}

// maxCandidateBytes bounds what the scan reads: a deployment definition is small, and anything larger is data.
const maxCandidateBytes = 1 << 20

// repoTree reads every candidate file of the checkout (gitignore-aware, the same walk the other suites use) that is
// in scope. The scan fails when it finds nothing.
func repoTree(t testing.TB) tree {
	t.Helper()
	root := secutil.RepoRoot(t)
	tr := tree{}
	for _, rel := range secutil.RepoFiles(t) {
		if !inScope(rel) || !candidateFile(rel) {
			continue
		}
		fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("stat %s: %v", rel, err)
		}
		if fi.Size() > maxCandidateBytes {
			continue
		}
		tr[rel] = string(secutil.ReadFile(t, rel))
	}
	if len(tr) == 0 {
		t.Fatal("the repository scan found no deployment candidate file at all")
	}
	return tr
}

// iniUnit is a parsed systemd or Quadlet file: section to key to every value given for it, in order.
type iniUnit map[string]map[string][]string

// parseINI reads a systemd-style file. A line that is not a section, a comment or Key=Value is an error, which
// makes a file this parser does not understand fail loudly instead of being read as empty.
func parseINI(text string) (iniUnit, error) {
	u := iniUnit{}
	section := ""
	var cont string
	for i, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if cont != "" {
			line = cont + " " + line
			cont = ""
		}
		if strings.HasSuffix(line, "\\") {
			cont = strings.TrimSpace(strings.TrimSuffix(line, "\\"))
			continue
		}
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = line[1 : len(line)-1]
			if u[section] == nil {
				u[section] = map[string][]string{}
			}
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok || section == "" {
				return nil, &parseError{line: i + 1, text: line}
			}
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			if v == "" {
				// systemd: an empty assignment resets the list.
				u[section][k] = []string{""}
				continue
			}
			u[section][k] = append(u[section][k], v)
		}
	}
	return u, nil
}

type parseError struct {
	line int
	text string
}

func (e *parseError) Error() string {
	return "line " + strconv.Itoa(e.line) + ": not a directive: " + e.text
}

// merge applies a drop-in to u: values are appended, and an empty assignment in the drop-in clears what came before.
func (u iniUnit) merge(d iniUnit) iniUnit {
	out := iniUnit{}
	for sec, kv := range u {
		out[sec] = map[string][]string{}
		for k, vs := range kv {
			out[sec][k] = append([]string(nil), vs...)
		}
	}
	for sec, kv := range d {
		if out[sec] == nil {
			out[sec] = map[string][]string{}
		}
		for k, vs := range kv {
			for _, v := range vs {
				if v == "" {
					out[sec][k] = nil
					continue
				}
				out[sec][k] = append(out[sec][k], v)
			}
		}
	}
	return out
}

// get returns the effective values of a directive: the empty reset marker is not a value.
func (u iniUnit) get(section, key string) []string {
	var out []string
	for _, v := range u[section][key] {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// present reports whether the directive is given at all, even as an empty reset (CapabilityBoundingSet= is a value).
func (u iniUnit) present(section, key string) bool {
	_, ok := u[section][key]
	return ok
}

// one returns the last value of a directive (the one systemd keeps for a single-valued key), or "".
func (u iniUnit) one(section, key string) string {
	vs := u.get(section, key)
	if len(vs) == 0 {
		return ""
	}
	return vs[len(vs)-1]
}
