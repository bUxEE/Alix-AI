package supplychainsec

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/buxee/alix-ai/tests/security/secutil"
)

const securityWorkflow = ".github/workflows/security.yml"

// workflow is the part of a GitHub workflow file the SEC-089 check reads.
type workflow struct {
	On          map[string]any `yaml:"on"`
	Permissions map[string]any `yaml:"permissions"`
	Jobs        map[string]job `yaml:"jobs"`
}

type job struct {
	ContinueOnError any      `yaml:"continue-on-error"`
	If              string   `yaml:"if"`
	Needs           any      `yaml:"needs"`
	Steps           []step   `yaml:"steps"`
	Strategy        strategy `yaml:"strategy"`
}

type step struct {
	Name            string `yaml:"name"`
	Uses            string `yaml:"uses"`
	Run             string `yaml:"run"`
	If              string `yaml:"if"`
	ContinueOnError any    `yaml:"continue-on-error"`
}

type strategy struct {
	Matrix struct {
		Include []struct {
			Pkg    string `yaml:"pkg"`
			Target string `yaml:"target"`
		} `yaml:"include"`
	} `yaml:"matrix"`
}

// neverExcluded are the gosec rules the workflow may not switch off, globally
// or for any path: injection, unbounded decompression and reads, weak crypto,
// blocklisted imports, bind-to-all-interfaces and request-forgery rules guard
// exactly what the threat model lists (16.4). Excluding one is a decision for
// the maintainers, recorded by changing this list in the same commit. G404
// (weak random) is not here: it is excluded for two named paths that only use
// it for retry jitter and fixtures.
var neverExcluded = []string{
	"G102", "G107", "G108", "G109", "G110", "G111", "G112", "G114",
	"G201", "G202", "G203",
	"G401", "G402", "G403", "G405", "G406", "G407", "G408", "G409",
	"G501", "G502", "G503", "G504", "G505", "G506", "G507",
	"G601", "G602",
}

var (
	ruleID       = regexp.MustCompile(`G\d{3}`)
	excludeFlag  = regexp.MustCompile(`-{1,2}exclude=([^\s]+)`)
	excludeRules = regexp.MustCompile(`-{1,2}exclude-rules=("[^"]*"|'[^']*'|[^\s]+)`)
	severityFlag = regexp.MustCompile(`-{1,2}(severity|confidence)=(\w+)`)
	swallowed    = regexp.MustCompile(`\|\|\s*(true|exit\s+0|:)\b|;\s*true\s*$|\bset \+e\b`)
	pinnedUses   = regexp.MustCompile(`^[A-Za-z0-9._/-]+@[0-9a-f]{40}$`)
)

func isFalse(s string) bool {
	s = strings.ToLower(strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "${} ")))
	return s == "false" || s == "0"
}

func isTrue(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return strings.EqualFold(strings.Trim(strings.TrimSpace(x), "${} "), "true")
	}
	return false
}

// triggers reports the event names a workflow runs on.
func triggers(w workflow) map[string]bool {
	out := map[string]bool{}
	for k := range w.On {
		out[k] = true
	}
	return out
}

// stepsRunning returns the steps whose script runs a command containing sub.
func stepsRunning(w workflow, sub string) []struct {
	Job  string
	Step step
} {
	var out []struct {
		Job  string
		Step step
	}
	names := make([]string, 0, len(w.Jobs))
	for n := range w.Jobs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, s := range w.Jobs[n].Steps {
			if strings.Contains(s.Run, sub) {
				out = append(out, struct {
					Job  string
					Step step
				}{n, s})
			}
		}
	}
	return out
}

// scannerProblems checks that a scan step really gates the build.
func scannerProblems(w workflow, label, sub string, extra func(run string) []string) []string {
	steps := stepsRunning(w, sub)
	if len(steps) == 0 {
		return []string{label + ": no step runs " + sub}
	}
	var problems []string
	for _, st := range steps {
		where := fmt.Sprintf("%s (job %s, step %q)", label, st.Job, st.Step.Name)
		run := strings.Join(strings.Fields(st.Step.Run), " ")
		if isTrue(st.Step.ContinueOnError) || isTrue(w.Jobs[st.Job].ContinueOnError) {
			problems = append(problems, where+": continue-on-error lets findings pass")
		}
		if isFalse(st.Step.If) || isFalse(w.Jobs[st.Job].If) {
			problems = append(problems, where+": the step is switched off with if: false")
		}
		if swallowed.MatchString(run) {
			problems = append(problems, where+": the exit status is swallowed (|| true)")
		}
		if extra != nil {
			for _, p := range extra(run) {
				problems = append(problems, where+": "+p)
			}
		}
	}
	return problems
}

// scansEverything requires a scanner to cover every package of the module.
func scansEverything(run string) []string {
	if !strings.Contains(run, "./...") {
		return []string{"does not scan ./..."}
	}
	return nil
}

// gosecProblems are the gosec-specific rules: it must not be told to ignore
// its findings, must not scan only high severity, and must keep the rules of
// neverExcluded on, for every path.
func gosecProblems(run string) []string {
	var problems []string
	if strings.Contains(run, "-no-fail") || strings.Contains(run, "--no-fail") {
		problems = append(problems, "-no-fail makes gosec report and pass")
	}
	for _, m := range severityFlag.FindAllStringSubmatch(run, -1) {
		if m[2] != "low" && m[2] != "medium" {
			problems = append(problems, fmt.Sprintf("-%s=%s hides medium findings", m[1], m[2]))
		}
	}
	excluded := map[string]bool{}
	for _, m := range excludeFlag.FindAllStringSubmatch(run, -1) {
		for _, id := range ruleID.FindAllString(m[1], -1) {
			excluded[id] = true
		}
	}
	for _, m := range excludeRules.FindAllStringSubmatch(run, -1) {
		// "path:RULE,RULE;path:RULE": a path of * excludes every rule for it.
		for _, entry := range strings.Split(strings.Trim(m[1], `"'`), ";") {
			path, rules, _ := strings.Cut(entry, ":")
			if strings.TrimSpace(rules) == "*" {
				problems = append(problems, fmt.Sprintf("all rules are excluded for %q", path))
			}
			for _, id := range ruleID.FindAllString(rules, -1) {
				excluded[id] = true
			}
		}
	}
	for _, id := range neverExcluded {
		if excluded[id] {
			problems = append(problems, "rule "+id+" may not be excluded")
		}
	}
	return problems
}

// fuzzTargets reads the Fuzz* functions of the Go package at dir.
func fuzzTargets(root, pkg string) (map[string]bool, error) {
	dir := filepath.Join(root, filepath.FromSlash(strings.TrimSuffix(strings.TrimPrefix(pkg, "./"), "/")))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Fuzz") {
				out[fn.Name.Name] = true
			}
		}
	}
	return out, nil
}

// workflowProblems is the whole SEC-089 rule set over one workflow file. have
// says whether package pkg has a fuzz target of that name; mustFuzz lists the
// pkg/target pairs the workflow has to run.
func workflowProblems(data []byte, have func(pkg, target string) bool, mustFuzz map[string][]string) []string {
	var w workflow
	if err := yaml.Unmarshal(data, &w); err != nil {
		return []string{"cannot parse the workflow: " + err.Error()}
	}
	var problems []string

	tr := triggers(w)
	if !tr["pull_request"] && !tr["push"] {
		problems = append(problems, "the workflow runs on neither push nor pull_request")
	}
	if tr["pull_request_target"] {
		problems = append(problems, "pull_request_target runs with write access on untrusted code")
	}
	if v, ok := w.Permissions["contents"]; !ok || fmt.Sprint(v) != "read" {
		problems = append(problems, "permissions.contents must be read")
	}
	for k, v := range w.Permissions {
		if k != "contents" && fmt.Sprint(v) == "write" {
			problems = append(problems, "permissions."+k+" is write")
		}
	}

	problems = append(problems, scannerProblems(w, "govulncheck", "golang.org/x/vuln/cmd/govulncheck", scansEverything)...)
	problems = append(problems, scannerProblems(w, "gosec", "github.com/securego/gosec/v2/cmd/gosec", func(run string) []string {
		return append(scansEverything(run), gosecProblems(run)...)
	})...)
	problems = append(problems, scannerProblems(w, "secret scan", "TestSecRepoSecretScan", func(run string) []string {
		if !strings.Contains(run, "./tests/security/secrets") {
			return []string{"does not run the test in ./tests/security/secrets"}
		}
		return nil
	})...)
	problems = append(problems, scannerProblems(w, "history secret scan", "/gitleaks/v8", func(run string) []string {
		var p []string
		if !strings.Contains(run, "tests/security/gitleaks.toml") {
			p = append(p, "does not use tests/security/gitleaks.toml")
		}
		if !regexp.MustCompile(`--exit-code[ =]+[1-9]`).MatchString(run) {
			p = append(p, "gitleaks is not told to fail on a leak (--exit-code 1)")
		}
		return p
	})...)
	problems = append(problems, scannerProblems(w, "security suite", "./tests/security/...", nil)...)
	problems = append(problems, scannerProblems(w, "SEC coverage", "tests/security/cmd/coverage", nil)...)

	// Fuzz matrix: every entry names a target that exists, and every required one is there.
	seen := map[string]bool{}
	var fuzzJob job
	for _, j := range w.Jobs {
		if len(j.Strategy.Matrix.Include) > 0 {
			fuzzJob = j
			for _, e := range j.Strategy.Matrix.Include {
				seen[e.Pkg+" "+e.Target] = true
				if have != nil && !have(e.Pkg, e.Target) {
					problems = append(problems, fmt.Sprintf("fuzz matrix names %s in %s, which does not exist", e.Target, e.Pkg))
				}
			}
		}
	}
	if len(fuzzJob.Steps) == 0 {
		problems = append(problems, "no fuzz job with a matrix of targets")
	} else {
		ran := false
		for _, s := range fuzzJob.Steps {
			if strings.Contains(s.Run, "-fuzz") && strings.Contains(s.Run, "-fuzztime") {
				ran = true
				if isTrue(s.ContinueOnError) || swallowed.MatchString(s.Run) {
					problems = append(problems, "the fuzz step swallows its failure")
				}
			}
		}
		if !ran {
			problems = append(problems, "the fuzz job never runs go test -fuzz with a -fuzztime")
		}
	}
	for pkg, targets := range mustFuzz {
		for _, tgt := range targets {
			if !seen[pkg+" "+tgt] {
				problems = append(problems, fmt.Sprintf("the fuzz matrix is missing %s in %s", tgt, pkg))
			}
		}
	}

	// Every action pinned by commit SHA; nothing reads secrets.
	for name, j := range w.Jobs {
		for _, s := range j.Steps {
			if s.Uses != "" && !pinnedUses.MatchString(s.Uses) {
				problems = append(problems, fmt.Sprintf("job %s: action %q is not pinned by a full commit SHA", name, s.Uses))
			}
		}
	}
	if strings.Contains(string(data), "secrets.") {
		problems = append(problems, "the workflow reads secrets; it must need none")
	}
	sort.Strings(problems)
	return problems
}

// SEC-089
//
// govulncheck and gosec run in CI and fail the build on findings: the workflow
// file runs both on every push and pull request, over ./..., with no
// continue-on-error, no swallowed exit status, gosec not told -no-fail and not
// told to skip any rule that guards a threat-model item. It also runs the fixed
// security suite, the SEC coverage check, the secret scan and one fuzz run per
// target, and every fuzz target the matrix names exists in the package it names.
func TestSecStaticAnalysisConfigured(t *testing.T) {
	root := secutil.RepoRoot(t)
	data := secutil.ReadFile(t, securityWorkflow)

	mustFuzz := map[string][]string{
		"./internal/identity/": {"FuzzTokenVerify"},
		"./internal/apppkg/":   {"FuzzPackageLoad"},
	}
	// Every fuzz target under tests/security/fuzz must be in the matrix, so a new one is never forgotten.
	local, err := fuzzTargets(root, "./tests/security/fuzz/")
	if err != nil {
		t.Fatalf("tests/security/fuzz: %v", err)
	}
	if len(local) == 0 {
		t.Fatal("tests/security/fuzz has no fuzz targets")
	}
	for tgt := range local {
		mustFuzz["./tests/security/fuzz/"] = append(mustFuzz["./tests/security/fuzz/"], tgt)
	}

	have := func(pkg, target string) bool {
		targets, err := fuzzTargets(root, pkg)
		return err == nil && targets[target]
	}
	for _, p := range workflowProblems(data, have, mustFuzz) {
		t.Error(p)
	}
}

// SEC-089
//
// The workflow rules are proven against planted violations: a workflow that
// would let findings through, drop a scanner or name a missing fuzz target must
// be flagged problem by problem, and the clean workflow must pass. Without
// this a rule that cannot see its violation would let the real file pass.
func TestSecStaticAnalysisDetectorsSeePlantedViolations(t *testing.T) {
	const sha = "3d3c42e5aac5ba805825da76410c181273ba90b1"
	base := func(mut func(s string) string) []byte {
		doc := `
on:
  pull_request:
permissions:
  contents: read
jobs:
  suite:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@` + sha + ` # v7.0.1
      - run: go test ./tests/security/...
      - run: go run ./tests/security/cmd/coverage
  govulncheck:
    runs-on: ubuntu-latest
    steps:
      - name: govulncheck
        run: go run golang.org/x/vuln/cmd/govulncheck@latest ./...
  gosec:
    runs-on: ubuntu-latest
    steps:
      - name: gosec
        run: >-
          go run github.com/securego/gosec/v2/cmd/gosec@latest
          -severity=medium -confidence=medium
          -exclude=G104,G304
          -exclude-rules="internal/x\.go:G204"
          ./...
  secrets:
    runs-on: ubuntu-latest
    steps:
      - run: go test -run '^TestSecRepoSecretScan$' ./tests/security/secrets
      - run: go run github.com/zricethezav/gitleaks/v8@latest git --no-banner --exit-code 1 --config tests/security/gitleaks.toml .
  fuzz:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include:
          - { pkg: ./internal/identity/, target: FuzzTokenVerify }
    steps:
      - run: go test -run='^$' -fuzz='^FuzzTokenVerify$' -fuzztime=30s ./internal/identity/
`
		return []byte(mut(doc))
	}
	have := func(pkg, target string) bool { return pkg == "./internal/identity/" && target == "FuzzTokenVerify" }
	must := map[string][]string{"./internal/identity/": {"FuzzTokenVerify"}}

	if p := workflowProblems(base(func(s string) string { return s }), have, must); len(p) != 0 {
		t.Fatalf("the clean workflow was flagged: %v", p)
	}

	type mutation struct {
		name string
		mut  func(s string) string
		want string // substring of one problem
	}
	rep := func(old, new string) func(string) string {
		return func(s string) string {
			if !strings.Contains(s, old) {
				t.Fatalf("test bug: %q not in the workflow", old)
			}
			return strings.Replace(s, old, new, 1)
		}
	}
	for _, m := range []mutation{
		{"govulncheck continue-on-error", rep("      - name: govulncheck\n", "      - name: govulncheck\n        continue-on-error: true\n"), "continue-on-error"},
		{"govulncheck swallowed", rep("govulncheck@latest ./...", "govulncheck@latest ./... || true"), "swallowed"},
		{"govulncheck switched off", rep("      - name: govulncheck\n", "      - name: govulncheck\n        if: false\n"), "if: false"},
		{"govulncheck missing", rep("golang.org/x/vuln/cmd/govulncheck@latest", "echo skipped"), "no step runs golang.org/x/vuln"},
		{"govulncheck not over everything", rep("govulncheck@latest ./...", "govulncheck@latest ./internal/..."), "does not scan ./..."},
		{"gosec missing", rep("github.com/securego/gosec/v2/cmd/gosec@latest", "echo skipped"), "no step runs github.com/securego"},
		{"gosec no-fail", rep("-severity=medium", "-no-fail -severity=medium"), "-no-fail"},
		{"gosec high only", rep("-severity=medium", "-severity=high"), "hides medium findings"},
		{"gosec excludes SQL injection rule", rep("-exclude=G104,G304", "-exclude=G104,G201"), "G201 may not be excluded"},
		{"gosec path excludes crypto rule", rep(`G204"`, `G204;pkg/.*:G401"`), "G401 may not be excluded"},
		{"gosec path excludes everything", rep(`G204"`, `G204;internal/.*:*"`), "all rules are excluded"},
		{"gosec job continue-on-error", rep("  gosec:\n    runs-on: ubuntu-latest\n", "  gosec:\n    runs-on: ubuntu-latest\n    continue-on-error: true\n"), "continue-on-error"},
		{"secret scan missing", rep("TestSecRepoSecretScan", "TestSomethingElse"), "no step runs TestSecRepoSecretScan"},
		{"history scan missing", rep("gitleaks/v8@latest git", "echo skipped"), "no step runs /gitleaks/v8"},
		{"history scan without its config", rep("--config tests/security/gitleaks.toml", ""), "does not use tests/security/gitleaks.toml"},
		{"history scan does not fail", rep("--exit-code 1", "--exit-code 0"), "not told to fail"},
		{"history scan swallowed", rep("gitleaks.toml .", "gitleaks.toml . || true"), "swallowed"},
		{"secret scan wrong package", rep("./tests/security/secrets", "./tests/other"), "does not run the test"},
		{"suite missing", rep("go test ./tests/security/...", "go test ./internal/..."), "no step runs ./tests/security/..."},
		{"coverage missing", rep("go run ./tests/security/cmd/coverage", "echo skipped"), "no step runs tests/security/cmd/coverage"},
		{"no trigger", rep("on:\n  pull_request:\n", "on:\n  workflow_dispatch:\n"), "neither push nor pull_request"},
		{"write permission", rep("contents: read", "contents: write"), "permissions.contents must be read"},
		{"unpinned action", rep("actions/checkout@"+sha+" # v7.0.1", "actions/checkout@v4"), "not pinned"},
		{"reads secrets", rep("steps:\n      - name: govulncheck", "steps:\n      - env: {X: '${{ secrets.TOKEN }}'}\n      - name: govulncheck"), "reads secrets"},
		{"fuzz target does not exist", rep("target: FuzzTokenVerify }", "target: FuzzGone }"), "does not exist"},
		{"required fuzz target dropped", rep("- { pkg: ./internal/identity/, target: FuzzTokenVerify }", "- { pkg: ./internal/apppkg/, target: FuzzPackageLoad }"), "missing FuzzTokenVerify"},
		{"fuzz without time", rep("-fuzztime=30s ", ""), "-fuzztime"},
		{"fuzz swallowed", rep("-fuzztime=30s ./internal/identity/", "-fuzztime=30s ./internal/identity/ || true"), "swallows"},
	} {
		t.Run(m.name, func(t *testing.T) {
			problems := workflowProblems(base(m.mut), have, must)
			for _, p := range problems {
				if strings.Contains(p, m.want) {
					return
				}
			}
			t.Fatalf("no problem mentions %q; got %v", m.want, problems)
		})
	}
}
