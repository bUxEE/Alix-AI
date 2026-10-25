package copilotkit

import (
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The owner's rule (ADR 0035): the SDKs are compatible with the CopilotKit UI API and AG-UI, including
// all features. The mapping is the feature matrix of section 12 of docs/integration/agui.md; what is
// proven, and by which test, is the matrix of docs/integration/copilotkit-compatibility.md, whose rows
// are the describe blocks of the gate in tests/copilotkit (M3-28). This file holds the matrix to its
// own words: no row of agui.md may be left without a test, or without the reason it is not emitted.

// aguiRow is a row of the feature matrix of docs/integration/agui.md section 12.
type aguiRow struct {
	feature, mechanism, alixai, status string
}

// proofRow is a row of the matrix of docs/integration/copilotkit-compatibility.md.
type proofRow struct {
	id, feature, status, proof, note string
}

// tableCells splits a markdown table row into its trimmed cells.
func tableCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(strings.TrimSuffix(line, "|"), "|")
	cells := strings.Split(line, "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

// tableRows are the data rows of the markdown table that follows the first line of text with the
// given prefix in a section: header and delimiter rows are dropped.
func tableRows(section string) [][]string {
	var rows [][]string
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "| ---") {
			continue
		}
		rows = append(rows, tableCells(line))
	}
	if len(rows) > 0 {
		rows = rows[1:] // the header
	}
	return rows
}

// sectionOf returns the text from the heading that starts with from to the heading that starts with to.
func sectionOf(doc, from, to string) string {
	_, rest, ok := strings.Cut(doc, "\n"+from)
	if !ok {
		return ""
	}
	if body, _, ok := strings.Cut(rest, "\n"+to); ok {
		return body
	}
	return rest
}

func parseAguiMatrix(doc string) ([]aguiRow, []string) {
	var out []aguiRow
	var problems []string
	for _, c := range tableRows(sectionOf(doc, "## 12. Feature matrix", "## 13.")) {
		if len(c) != 4 {
			problems = append(problems, "a row of the agui.md matrix has "+strconv.Itoa(len(c))+" cells, want 4: "+strings.Join(c, " | "))
			continue
		}
		out = append(out, aguiRow{feature: c[0], mechanism: c[1], alixai: c[2], status: c[3]})
	}
	return out, problems
}

func parseProofMatrix(doc string) ([]proofRow, []string) {
	var out []proofRow
	var problems []string
	for _, c := range tableRows(sectionOf(doc, "## The feature matrix", "### ")) {
		if len(c) != 5 {
			problems = append(problems, "a row of the compatibility matrix has "+strconv.Itoa(len(c))+" cells, want 5: "+strings.Join(c, " | "))
			continue
		}
		out = append(out, proofRow{id: strings.Trim(c[0], "`"), feature: c[1], status: c[2], proof: c[3], note: c[4]})
	}
	return out, problems
}

// matrixEnv is what the matrix check asks of the repository.
type matrixEnv struct {
	// testFile reports whether a test file the matrix names exists: one of the gate (tests/copilotkit/test)
	// or of the adapter (sdk/copilotkit-adapter/test).
	testFile func(name string) bool
	// gateTests is how many tests the gate's describe(row("<id>")) blocks of a row hold.
	gateTests func(id string) int
	// goTest reports whether a Go test function exists in the repository.
	goTest func(name string) bool
}

// notClaimedAccepted are the rows the matrix may state as not claimed. The brief of this gate allows two
// outcomes for a row: supported with a test, or not emitted by design with a reason. A row stated as not
// claimed is neither, unless the owner's own text says so: ADR 0035 (the list of decisions) states that
// full duplex voice is not claimed until a test proves it. Nothing else is accepted: a row that is
// neither fails until the owner reclassifies it in both documents or the feature is built.
var notClaimedAccepted = map[string]string{
	"voice": "ADR 0035: \"full duplex voice is not claimed until a test proves it (capabilities endpoint says so)\"",
}

var (
	gateFileRE = regexp.MustCompile("`((?:[a-z]+)\\.test\\.tsx?)`")
	goTestRE   = regexp.MustCompile("`(Test[A-Za-z0-9_]+)`")
	// a test of the gate: it(, test( or it.each(...)( at the start of a line (it.skip does not count)
	gateTestRE = regexp.MustCompile(`(?m)^\s+(?:it|test)(?:\.each)?\(`)
)

// matrixProblems holds the matrix to the rule: every row of agui.md section 12 is a feature that is
// supported, with a test that exists, or one that is not emitted by design, with its reason (or, for
// the two the documents state as not claimed, one stated and pinned by a test that nothing implies
// the opposite). It returns what is wrong; nothing is wrong when it returns none.
func matrixProblems(agui []aguiRow, proofs []proofRow, env matrixEnv) []string {
	var out []string
	bad := func(feature, format string, args ...any) {
		out = append(out, "row "+strconv.Quote(feature)+": "+fmt.Sprintf(format, args...))
	}
	byFeature := map[string]proofRow{}
	for _, p := range proofs {
		byFeature[p.feature] = p
	}
	seen := map[string]bool{}
	for _, row := range agui {
		if seen[row.feature] {
			bad(row.feature, "listed twice")
		}
		seen[row.feature] = true
		p, ok := byFeature[row.feature]
		if !ok {
			bad(row.feature, "has no row in the compatibility matrix, so no test is named for it")
			continue
		}
		if strings.TrimSpace(row.alixai) == "" {
			bad(row.feature, "agui.md says nothing about what AlixAI does")
		}
		if strings.TrimSpace(p.proof) == "" {
			bad(row.feature, "the compatibility matrix names no test")
		}
		// every named test exists: the gate's files and Go tests, in both documents
		for _, m := range gateFileRE.FindAllStringSubmatch(p.proof, -1) {
			if !env.testFile(m[1]) {
				bad(row.feature, "names the test file %s, which does not exist", m[1])
			}
		}
		for _, text := range []string{p.proof, row.alixai} {
			for _, m := range goTestRE.FindAllStringSubmatch(text, -1) {
				if !env.goTest(m[1]) {
					bad(row.feature, "names the test %s, which does not exist", m[1])
				}
			}
		}
		if n := env.gateTests(p.id); n == 0 {
			bad(row.feature, "no test of the gate's describe(row(%q)) block", p.id)
		}

		switch p.status {
		case "shown", "shown with a limit":
			if !strings.Contains(row.status, "core") && !strings.Contains(row.status, "gateway") && !strings.Contains(row.status, "adapter") {
				bad(row.feature, "agui.md says %q, the compatibility matrix says it is %s", row.status, p.status)
			}
			if gateFileRE.FindString(p.proof) == "" && goTestRE.FindString(p.proof) == "" {
				bad(row.feature, "supported, and no test of the gate or of Go is named")
			}
		case "not emitted by design":
			if low := strings.ToLower(row.alixai); !strings.Contains(low, "not emitted") && !strings.Contains(low, "not produced") {
				bad(row.feature, "marked not emitted by design, and agui.md does not say it is not emitted or not produced")
			}
			if strings.TrimSpace(p.note) == "" || p.note == "-" {
				bad(row.feature, "not emitted by design, with no reason")
			}
		case "not claimed":
			reason, accepted := notClaimedAccepted[p.id]
			switch {
			case !accepted:
				bad(row.feature, "not claimed, which is neither supported with a test nor not emitted by design (accepted by the owner's text: %v): reclassify it in agui.md and the compatibility matrix, or build it", sortedMapKeys(notClaimedAccepted))
			case strings.TrimSpace(p.note) == "" || p.note == "-" || reason == "":
				bad(row.feature, "not claimed, with no reason")
			}
		default:
			bad(row.feature, "status %q is not one of shown, shown with a limit, not emitted by design, not claimed", p.status)
		}
	}
	return out
}

func sortedMapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// skipDirs are the directories a scan of the repository never enters.
var skipDirs = map[string]bool{
	"node_modules": true, ".venv": true, "venv": true, ".git": true, ".worktrees": true, "dist": true, "build": true,
	"__pycache__": true, ".pytest_cache": true, ".mypy_cache": true, "coverage": true, ".turbo": true,
}

// walkFiles calls fn for every file below dir that a scan reads.
func walkFiles(t *testing.T, dir string, fn func(path string)) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		fn(p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// repoEnv reads the real repository.
func repoEnv(t *testing.T, root string) matrixEnv {
	t.Helper()
	gateDir := filepath.Join(root, "tests", "copilotkit", "test")
	gateSources := map[string]string{}
	entries, err := os.ReadDir(gateDir)
	if err != nil {
		t.Fatalf("the CopilotKit gate is missing: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() && (strings.HasSuffix(e.Name(), ".test.ts") || strings.HasSuffix(e.Name(), ".test.tsx")) {
			b, err := os.ReadFile(filepath.Join(gateDir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			gateSources[e.Name()] = string(b)
		}
	}
	goTests := map[string]bool{}
	funcRE := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
	walkFiles(t, root, func(p string) {
		if !strings.HasSuffix(p, "_test.go") {
			return
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range funcRE.FindAllStringSubmatch(string(b), -1) {
			goTests[m[1]] = true
		}
	})
	adapterTests := map[string]bool{}
	if es, err := os.ReadDir(filepath.Join(root, "sdk", "copilotkit-adapter", "test")); err == nil {
		for _, e := range es {
			adapterTests[e.Name()] = true
		}
	}
	return matrixEnv{
		testFile: func(name string) bool { _, ok := gateSources[name]; return ok || adapterTests[name] },
		gateTests: func(id string) int {
			// the tests (it, test, each) that follow a describe(row("<id>")) up to the next describe(row(
			n := 0
			head := `describe(row("` + id + `")`
			for _, src := range gateSources {
				rest := src
				for {
					_, after, ok := strings.Cut(rest, head)
					if !ok {
						break
					}
					block, _, _ := strings.Cut(after, `describe(row("`)
					n += strings.Count(block, "\n  it(") + strings.Count(block, "\n  it.each") + strings.Count(block, "\n  test(") + strings.Count(block, "\n  it.skip")*0
					rest = after
				}
			}
			return n
		},
		goTest: func(name string) bool { return goTests[name] },
	}
}

// TestCopilotKitMatrixIsComplete: every row of the feature matrix of docs/integration/agui.md
// section 12 is either supported, with a test that exists (the gate in tests/copilotkit, which runs the
// real @ag-ui/client and @copilotkit/runtime, and the Go tests the documents name), or marked not
// emitted by design with its reason; a row that is neither fails the test. And no SDK package imports
// CopilotKit: only sdk/copilotkit-adapter does (the optional integration, ADR 0035).
func TestCopilotKitMatrixIsComplete(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	t.Run("every row of the matrix is supported with a test or not emitted by design with a reason", func(t *testing.T) {
		agui, p1 := parseAguiMatrix(read("docs/integration/agui.md"))
		proofs, p2 := parseProofMatrix(read("docs/integration/copilotkit-compatibility.md"))
		for _, p := range append(p1, p2...) {
			t.Error(p)
		}
		// a parser that finds nothing proves nothing
		if len(agui) < 24 || len(proofs) < len(agui) {
			t.Fatalf("read %d rows of agui.md section 12 and %d of the compatibility matrix: want at least 24, and a proof for each", len(agui), len(proofs))
		}
		for _, p := range matrixProblems(agui, proofs, repoEnv(t, root)) {
			t.Error(p)
		}
		var supported, notEmitted, notClaimed int
		for _, p := range proofs {
			switch p.status {
			case "shown", "shown with a limit":
				supported++
			case "not emitted by design":
				notEmitted++
			case "not claimed":
				notClaimed++
			}
		}
		// the rows ADR 0035 (and its addendum) decides are not emitted by design: a fifth needs a decision too
		var emitted []string
		for _, p := range proofs {
			if p.status == "not emitted by design" {
				emitted = append(emitted, p.id)
			}
		}
		slices.Sort(emitted)
		if want := []string{"predictive-state", "raw", "reasoning", "subagents"}; !slices.Equal(emitted, want) {
			t.Errorf("rows not emitted by design = %v, want %v (ADR 0035 and its addendum)", emitted, want)
		}
		t.Logf("%d rows of agui.md section 12: %d supported with a test, %d not emitted by design with a reason, %d not claimed (accepted: %v)",
			len(agui), supported, notEmitted, notClaimed, sortedMapKeys(notClaimedAccepted))
	})

	// The checker itself: a row that is not supported with a test, nor not emitted with a reason, is found.
	t.Run("the check fails a row with no test, no reason or no row", func(t *testing.T) {
		env := matrixEnv{
			testFile:  func(name string) bool { return name == "chat.test.ts" },
			gateTests: func(id string) int { return map[string]int{"chat": 3, "raw": 1, "bare": 0, "claimed": 1}[id] },
			goTest:    func(name string) bool { return name == "TestReal" },
		}
		good := []aguiRow{
			{"Chat", "events", "answers", "core + gateway"},
			{"Raw provider events", "RAW", "not emitted", "not claimed"},
		}
		proofs := []proofRow{
			{"chat", "Chat", "shown", "gate `chat.test.ts`; Go `TestReal`", "n"},
			{"raw", "Raw provider events", "not emitted by design", "gate `chat.test.ts`", "untrusted data"},
		}
		if got := matrixProblems(good, proofs, env); len(got) != 0 {
			t.Fatalf("a complete matrix has problems: %v", got)
		}
		cases := []struct {
			name   string
			agui   []aguiRow
			proofs []proofRow
		}{
			{"a row with no proof entry", append(slices.Clone(good), aguiRow{"Voice", "x", "y", "not claimed"}), proofs},
			{"a supported row naming no test", good, []proofRow{{"chat", "Chat", "shown", "", "n"}, proofs[1]}},
			{"a supported row naming a gate file that does not exist", good, []proofRow{{"chat", "Chat", "shown", "gate `gone.test.ts`", "n"}, proofs[1]}},
			{"a supported row naming a Go test that does not exist", good, []proofRow{{"chat", "Chat", "shown", "gate `chat.test.ts`; Go `TestGone`", "n"}, proofs[1]}},
			{"a row whose gate block holds no test", good, []proofRow{{"bare", "Chat", "shown", "gate `chat.test.ts`", "n"}, proofs[1]}},
			{"a not emitted row with no reason", good, []proofRow{proofs[0], {"raw", "Raw provider events", "not emitted by design", "gate `chat.test.ts`", ""}}},
			{"a row that is not emitted but agui.md does not say so", []aguiRow{good[0], {"Raw provider events", "RAW", "relayed", "not claimed"}}, proofs},
			{"a not claimed row nobody decided on", good, []proofRow{proofs[0], {"claimed", "Raw provider events", "not claimed", "gate `chat.test.ts`", "because"}}},
			{"a status that is none of the four", good, []proofRow{proofs[0], {"raw", "Raw provider events", "planned", "gate `chat.test.ts`", "later"}}},
			{"a supported row that agui.md does not implement", []aguiRow{{"Chat", "events", "answers", "planned"}, good[1]}, proofs},
		}
		for _, c := range cases {
			if got := matrixProblems(c.agui, c.proofs, env); len(got) == 0 {
				t.Errorf("%s: no problem found", c.name)
			}
		}
	})

	t.Run("no SDK package imports CopilotKit except sdk/copilotkit-adapter", func(t *testing.T) {
		var sdkFiles int
		var offences []string
		sdkRoots := []string{filepath.Join(root, "pkg", "sdk-go")}
		entries, err := os.ReadDir(filepath.Join(root, "sdk"))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.IsDir() && e.Name() != "copilotkit-adapter" {
				sdkRoots = append(sdkRoots, filepath.Join(root, "sdk", e.Name()))
			}
		}
		for _, dir := range sdkRoots {
			if _, err := os.Stat(dir); err != nil {
				t.Fatalf("an SDK package is missing: %v", err)
			}
			walkFiles(t, dir, func(p string) {
				sdkFiles++
				src, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				rel, _ := filepath.Rel(root, p)
				for _, f := range copilotKitImports(filepath.ToSlash(rel), src) {
					offences = append(offences, rel+": "+f)
				}
			})
		}
		for _, o := range offences {
			t.Errorf("an SDK package depends on CopilotKit: %s", o)
		}
		if sdkFiles < 50 || len(sdkRoots) < 3 {
			t.Fatalf("scanned %d file(s) of %d SDK package(s): the scan is not looking at the SDKs", sdkFiles, len(sdkRoots))
		}

		// The control: the adapter is where CopilotKit is imported, and the scan sees it there.
		var adapter []string
		walkFiles(t, filepath.Join(root, "sdk", "copilotkit-adapter"), func(p string) {
			src, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			rel, _ := filepath.Rel(root, p)
			adapter = append(adapter, copilotKitImports(filepath.ToSlash(rel), src)...)
		})
		if len(adapter) < 3 {
			t.Errorf("the scan finds %d CopilotKit import(s) in sdk/copilotkit-adapter: it cannot be trusted to find one elsewhere", len(adapter))
		}
	})

	t.Run("the import scan sees every way to import CopilotKit", func(t *testing.T) {
		cases := []struct {
			path, src string
			want      bool
		}{
			{"a.ts", `import { CopilotRuntime } from "@copilotkit/runtime";`, true},
			{"a.ts", "import {\n  a,\n  b,\n} from '@copilotkit/react-core/v2'\n", true},
			{"a.ts", `const x = await import("@copilotkit/runtime");`, true},
			{"a.js", `const x = require('@copilotkit/shared');`, true},
			{"a.ts", `import "@copilotkit/react-ui/styles.css";`, true},
			{"a.ts", `// serves a CopilotKit frontend, imports nothing of it`, false},
			{"a.ts", `import { x } from "./copilotkit-free";`, false},
			{"package.json", `{"dependencies":{"@copilotkit/runtime":"1.0.0"}}`, true},
			{"package.json", `{"peerDependencies":{"@copilotkit/react-core":"1"}}`, true},
			{"package.json", `{"devDependencies":{"vitest":"1"},"name":"@alixai/sdk"}`, false},
			{"a.py", "import copilotkit\n", true},
			{"a.py", "from copilotkit.runtime import X\n", true},
			{"a.py", "# serves a CopilotKit frontend\nimport httpx\n", false},
			{"pyproject.toml", "dependencies = [\n  \"httpx>=0.27\",\n  \"copilotkit>=0.1\",\n]\n", true},
			{"pyproject.toml", "dependencies = [\n  \"httpx>=0.27\",\n]\n# no copilotkit\n", false},
			{"a.go", "package a\nimport \"github.com/CopilotKit/go-sdk\"\n", true},
			{"a.go", "package a\nimport \"net/http\"\n// CopilotKit\n", false},
		}
		for _, c := range cases {
			if got := len(copilotKitImports(c.path, []byte(c.src))) > 0; got != c.want {
				t.Errorf("%s %q: found %v, want %v", c.path, c.src, got, c.want)
			}
		}
	})
}

var (
	jsImportRE = regexp.MustCompile(`(?:\bfrom\s*|\bimport\s*\(?\s*|\brequire\s*\(\s*)['"]([^'"]+)['"]`)
	pyImportRE = regexp.MustCompile(`(?m)^\s*(?:from|import)\s+([A-Za-z0-9_.]+)`)
	// the package a module specifier names: @copilotkit/x, copilotkit, copilotkit/x, copilotkit-x, copilotkit.x
	ckPackageRE = regexp.MustCompile(`(?i)^(?:@copilotkit/|copilotkit(?:$|[/.\-]))`)
)

// copilotKitImports lists what a source or manifest file imports or depends on that is CopilotKit's.
// Prose, comments and strings that are not imports do not count.
func copilotKitImports(path string, src []byte) []string {
	// a dependency or a Go import path is CopilotKit's when its name says so; a JavaScript or Python
	// module specifier is when its package is (a relative path that happens to contain the word is not)
	isCK := func(s string) bool { return strings.Contains(strings.ToLower(s), "copilotkit") }
	isCKPackage := func(s string) bool { return ckPackageRE.MatchString(s) }
	var out []string
	switch ext := filepath.Ext(path); {
	case filepath.Base(path) == "package.json":
		var m map[string]any
		if json.Unmarshal(src, &m) != nil {
			return []string{"package.json that cannot be read"}
		}
		for _, key := range []string{"dependencies", "devDependencies", "peerDependencies", "optionalDependencies", "bundledDependencies"} {
			if deps, ok := m[key].(map[string]any); ok {
				for name := range deps {
					if isCK(name) {
						out = append(out, key+" "+name)
					}
				}
			}
		}
	case filepath.Base(path) == "pyproject.toml" || filepath.Base(path) == "requirements.txt":
		for _, line := range strings.Split(string(src), "\n") {
			if code, _, _ := strings.Cut(line, "#"); isCK(code) {
				out = append(out, "dependency "+strings.TrimSpace(code))
			}
		}
	case ext == ".ts" || ext == ".tsx" || ext == ".mts" || ext == ".cts" || ext == ".js" || ext == ".mjs" || ext == ".cjs" || ext == ".jsx":
		for _, m := range jsImportRE.FindAllStringSubmatch(string(src), -1) {
			if isCKPackage(m[1]) {
				out = append(out, "import "+m[1])
			}
		}
	case ext == ".py":
		for _, m := range pyImportRE.FindAllStringSubmatch(string(src), -1) {
			if isCKPackage(m[1]) {
				out = append(out, "import "+m[1])
			}
		}
	case ext == ".go":
		f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ImportsOnly)
		if err != nil {
			return []string{"go file that cannot be parsed"}
		}
		for _, imp := range f.Imports {
			if isCK(imp.Path.Value) {
				out = append(out, "import "+imp.Path.Value)
			}
		}
	}
	return out
}

// moduleRoot is the nearest parent of the test's directory holding go.mod.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
