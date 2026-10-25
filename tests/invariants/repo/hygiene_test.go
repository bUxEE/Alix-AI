package repo

import (
	"bytes"
	"fmt"
	"go/format"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ---- gofmt ----

// gofmtProblem returns "" when src is already gofmt-formatted. go/format is
// what gofmt runs, so this is `gofmt -l` without starting a process.
func gofmtProblem(src []byte) string {
	out, err := format.Source(src)
	if err != nil {
		return fmt.Sprintf("does not parse: %v", err)
	}
	if !bytes.Equal(src, out) {
		return "is not gofmt-formatted (run gofmt -w)"
	}
	return ""
}

// TestGoSourcesAreGofmtFormatted holds every Go file of the checkout to gofmt,
// build-tagged and test files included. The CI step is
// `test -z "$(gofmt -l $(go list -f '{{.Dir}}' ./...))"`.
func TestGoSourcesAreGofmtFormatted(t *testing.T) {
	root := repoRoot(t)
	n := 0
	for _, rel := range repoFiles(t) {
		if !strings.HasSuffix(rel, ".go") {
			continue
		}
		n++
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		if p := gofmtProblem(data); p != "" {
			t.Errorf("%s %s", rel, p)
		}
	}
	if n < 200 {
		t.Fatalf("checked only %d Go files; the walker is not seeing the repository", n)
	}

	if p := gofmtProblem([]byte("package x\n\nfunc f() {}\n")); p != "" {
		t.Errorf("formatted source rejected: %s", p)
	}
	if p := gofmtProblem([]byte("package x\nfunc f( ) {}\n")); p == "" {
		t.Error("unformatted source accepted")
	}
	if p := gofmtProblem([]byte("package x\nfunc (")); p == "" {
		t.Error("source that does not parse accepted")
	}
}

// ---- GitHub Actions pinned by commit SHA ----

var (
	actionRef      = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(/[A-Za-z0-9_./-]+)?@[0-9a-f]{40}$`)
	dockerActionRe = regexp.MustCompile(`^docker://[^@\s]+@sha256:[0-9a-f]{64}$`)
	versionComment = regexp.MustCompile(`^#\s*v?[0-9][^\s]*\s*$`)
)

// actionPinProblems finds every `uses:` in a workflow or composite action and
// returns those that are not pinned: a third-party action must be
// owner/repo@<40 hex commit SHA> followed by a "# vX.Y.Z" comment (a tag or a
// branch can be moved to different code; a commit cannot), a docker:// action
// must carry a sha256 digest, and only a local path (./...) is exempt. It also
// returns how many references it checked.
func actionPinProblems(name string, data []byte) (problems []string, checked int, err error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, 0, err
	}
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n == nil {
			return
		}
		if n.Kind == yaml.AliasNode {
			walk(n.Alias)
			return
		}
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				key, val := n.Content[i], n.Content[i+1]
				if key.Value == "uses" && val.Kind == yaml.ScalarNode {
					ref := strings.TrimSpace(val.Value)
					comment := strings.TrimSpace(val.LineComment + key.LineComment)
					if msg := usesProblem(ref, comment); msg != "" {
						problems = append(problems, fmt.Sprintf("%s line %d: %s", name, val.Line, msg))
					}
					if !strings.HasPrefix(ref, "./") {
						checked++
					}
					continue
				}
				walk(val)
			}
			return
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&doc)
	return problems, checked, nil
}

func usesProblem(ref, comment string) string {
	switch {
	case ref == "":
		return "empty uses"
	case strings.HasPrefix(ref, "./"):
		return "" // a local action or workflow in this repository
	case strings.HasPrefix(ref, "docker://"):
		if !dockerActionRe.MatchString(ref) {
			return fmt.Sprintf("%q is not pinned by a sha256 digest", ref)
		}
		return ""
	case !actionRef.MatchString(ref):
		return fmt.Sprintf("%q is not pinned by a full 40-hex commit SHA (owner/repo@<sha> # vX.Y.Z)", ref)
	case !versionComment.MatchString(comment):
		return fmt.Sprintf("%q has no trailing version comment (want \"# vX.Y.Z\", got %q)", ref, comment)
	}
	return ""
}

// githubYAML lists the workflow and action definitions under .github/.
// repoFiles skips dot-directories, so this walks .github itself.
func githubYAML(root string) ([]string, error) {
	var out []string
	base := filepath.Join(root, ".github")
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (path.Ext(p) == ".yml" || path.Ext(p) == ".yaml") {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

func TestCIActionsPinnedBySHA(t *testing.T) {
	root := repoRoot(t)
	files, err := githubYAML(root)
	if err != nil {
		t.Fatalf("cannot read .github: %v", err)
	}
	hasCI := false
	checked := 0
	for _, rel := range files {
		hasCI = hasCI || rel == ".github/workflows/ci.yml"
		problems, n, err := actionPinProblems(rel, readRepoFile(t, rel))
		if err != nil {
			t.Errorf("%s: cannot parse: %v", rel, err)
			continue
		}
		checked += n
		for _, p := range problems {
			t.Error(p)
		}
	}
	if !hasCI {
		t.Fatal(".github/workflows/ci.yml is missing")
	}
	if checked < 5 {
		t.Fatalf("checked only %d action references; the scan is not seeing the workflows", checked)
	}
}

func TestActionPinDetector(t *testing.T) {
	const sha = "3d3c42e5aac5ba805825da76410c181273ba90b1"
	cases := []struct {
		name string
		uses string
		want bool // a problem
	}{
		{"pinned with version", "actions/checkout@" + sha + " # v7.0.1", false},
		{"pinned with bare version", "actions/checkout@" + sha + " # 7.0.1", false},
		{"pinned subpath action", "github/codeql-action/analyze@" + sha + " # v3", false},
		{"local action", "./.github/actions/setup", false},
		{"docker digest", "docker://alpine@sha256:" + strings.Repeat("a", 64), false},
		{"major tag", "actions/checkout@v4", true},
		{"full tag", "actions/checkout@v4.2.2", true},
		{"branch", "actions/checkout@main", true},
		{"no ref", "actions/checkout", true},
		{"short sha", "actions/checkout@3d3c42e # v7", true},
		{"upper case sha", "actions/checkout@" + strings.ToUpper(sha) + " # v7", true},
		{"sha without version comment", "actions/checkout@" + sha, true},
		{"sha with a non-version comment", "actions/checkout@" + sha + " # latest", true},
		{"docker tag", "docker://alpine:3.20", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := "jobs:\n  a:\n    steps:\n      - name: x\n        uses: " + tc.uses + "\n"
			problems, checked, err := actionPinProblems("t.yml", []byte(doc))
			if err != nil {
				t.Fatal(err)
			}
			if (len(problems) > 0) != tc.want {
				t.Errorf("problems = %v, want a problem: %v", problems, tc.want)
			}
			if !strings.HasPrefix(tc.uses, "./") && checked != 1 {
				t.Errorf("checked = %d, want 1", checked)
			}
		})
	}

	t.Run("reusable workflow call", func(t *testing.T) {
		doc := "jobs:\n  call:\n    uses: org/repo/.github/workflows/x.yml@main\n"
		if problems, _, _ := actionPinProblems("t.yml", []byte(doc)); len(problems) != 1 {
			t.Errorf("an unpinned reusable workflow went unnoticed: %v", problems)
		}
	})
	t.Run("composite action steps", func(t *testing.T) {
		doc := "runs:\n  using: composite\n  steps:\n    - uses: actions/cache@v4\n"
		if problems, _, _ := actionPinProblems("action.yml", []byte(doc)); len(problems) != 1 {
			t.Errorf("an unpinned composite step went unnoticed: %v", problems)
		}
	})
	t.Run("a shell command that mentions uses is not an action", func(t *testing.T) {
		doc := "jobs:\n  a:\n    steps:\n      - run: echo 'uses: actions/checkout@v4'\n"
		if problems, checked, _ := actionPinProblems("t.yml", []byte(doc)); len(problems) != 0 || checked != 0 {
			t.Errorf("run text was treated as an action: %v (%d)", problems, checked)
		}
	})
	if _, _, err := actionPinProblems("t.yml", []byte("jobs: [")); err == nil {
		t.Error("a malformed workflow was accepted")
	}
}
