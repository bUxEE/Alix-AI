package repo

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// goSource is one parsed Go file. Imports are read with go/parser, never by
// searching the text, so a comment or a string that mentions a package is not
// an import and a grouped, aliased or blank import is not missed.
type goSource struct {
	Rel     string // slash path relative to the repository root
	Package string
	Test    bool // a _test.go file
	Imports []string
	AST     *ast.File
	Fset    *token.FileSet
}

// parseGoSource parses src (a string, []byte or nil to read the file named by
// path) as the file called rel. Build constraints are not evaluated: a
// `//go:build live` file is parsed like any other, so a rule cannot be dodged
// behind a tag.
func parseGoSource(rel string, src any, mode parser.Mode) (*goSource, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, mode)
	if err != nil {
		return nil, err
	}
	gs := &goSource{Rel: rel, Package: f.Name.Name, Test: strings.HasSuffix(rel, "_test.go"), AST: f, Fset: fset}
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil, fmt.Errorf("%s: bad import path %s: %w", rel, spec.Path.Value, err)
		}
		gs.Imports = append(gs.Imports, p)
	}
	return gs, nil
}

// loadGoSources parses every file in rels ending in ".go".
func loadGoSources(root string, rels []string, mode parser.Mode, keep func(rel string) bool) ([]*goSource, error) {
	var out []*goSource
	for _, rel := range rels {
		if !strings.HasSuffix(rel, ".go") || (keep != nil && !keep(rel)) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		gs, err := parseGoSource(rel, data, mode)
		if err != nil {
			return nil, fmt.Errorf("cannot parse %s: %w", rel, err)
		}
		out = append(out, gs)
	}
	return out, nil
}

var (
	importsOnce sync.Once
	importsList []*goSource
	importsErr  error
)

// repoImports parses the import clauses of every Go file in the checkout once.
func repoImports(t testing.TB) []*goSource {
	t.Helper()
	root, files := repoRoot(t), repoFiles(t)
	importsOnce.Do(func() {
		importsList, importsErr = loadGoSources(root, files, parser.ImportsOnly, nil)
	})
	if importsErr != nil {
		t.Fatalf("%v", importsErr)
	}
	return importsList
}

// ---- rule 2: LocalAI code is imported only by its adapter and the composition roots ----

// isCompositionRoot reports whether the file at rel belongs to a composition
// root: the places that wire concrete providers (docs/architecture/module-map.md
// rule 3). Everything else receives interfaces from internal/providers.
func isCompositionRoot(rel string) bool {
	return under(rel, "cmd/") || under(rel, "internal/cli/") || under(rel, "tools/")
}

// isLocalAIDir reports whether the module-relative directory dir is LocalAI
// adapter code: adapters/localai and its subpackages (voice, realtime), or a
// LocalAI adapter of another area such as adapters/decision/localai.
func isLocalAIDir(dir string) bool {
	segs := strings.Split(dir, "/")
	if len(segs) < 2 || segs[0] != "adapters" {
		return false
	}
	for _, s := range segs[1:] {
		if s == "localai" {
			return true
		}
	}
	return false
}

// isLocalAIImport reports whether the import path is LocalAI code: a LocalAI
// adapter of this module, or LocalAI's own Go module.
func isLocalAIImport(p string) bool {
	if rest, ok := strings.CutPrefix(p, modulePath+"/"); ok {
		return isLocalAIDir(rest)
	}
	return strings.Contains(strings.ToLower(p), "mudler/localai")
}

// localAIImportViolations lists every file that imports LocalAI code from
// outside adapters/localai and the composition roots. _test.go files and
// build-tagged files count: a test that reaches into the adapter is the same
// coupling.
func localAIImportViolations(files []*goSource) []string {
	var out []string
	for _, f := range files {
		if isCompositionRoot(f.Rel) || isLocalAIDir(path.Dir(f.Rel)) {
			continue
		}
		for _, p := range f.Imports {
			if isLocalAIImport(p) {
				out = append(out, fmt.Sprintf("%s imports %s", f.Rel, p))
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestNoLocalAIImportsOutsideAdapterAndCompositionRoots(t *testing.T) {
	files := repoImports(t)

	// Positive controls: a scan that saw nothing proves nothing.
	if len(files) < 200 {
		t.Fatalf("parsed only %d Go files; the walker is not seeing the repository", len(files))
	}
	adapterFiles, rootImporters := 0, 0
	for _, f := range files {
		if isLocalAIDir(path.Dir(f.Rel)) {
			adapterFiles++
		}
		if isCompositionRoot(f.Rel) {
			for _, p := range f.Imports {
				if isLocalAIImport(p) {
					rootImporters++
					break
				}
			}
		}
	}
	if adapterFiles == 0 {
		t.Fatal("found no Go files in adapters/localai; the adapter moved or the scan is broken")
	}
	if rootImporters == 0 {
		t.Fatal("no composition root imports the LocalAI adapter; the import scan is not seeing imports")
	}

	for _, v := range localAIImportViolations(files) {
		t.Errorf("%s: only adapters/localai and the composition roots (cmd/, internal/cli/, tools/) may import LocalAI code; everything else uses internal/providers", v)
	}
}

// TestLocalAIImportDetector exercises the detector on inline sources, so the
// gate cannot pass by finding nothing.
func TestLocalAIImportDetector(t *testing.T) {
	const adapter = `"` + modulePath + `/adapters/localai"`
	cases := []struct {
		name    string
		rel     string
		imports string
		want    bool // a violation
	}{
		{"service package", "internal/rag/x.go", "import " + adapter, true},
		{"service test file", "internal/dialogue/x_test.go", "import _ " + adapter, true},
		{"aliased", "internal/gateway/x.go", "import la " + adapter, true},
		{"grouped", "internal/gateway/x.go", "import (\n\t\"fmt\"\n\t" + adapter + "\n)", true},
		{"subpackage", "internal/rag/x.go", `import "` + modulePath + `/adapters/localai/voice"`, true},
		{"other adapter", "adapters/mcp/x.go", "import " + adapter, true},
		{"tests tree", "tests/e2e/x_test.go", "import " + adapter, true},
		{"pkg", "pkg/sdk-go/x.go", "import " + adapter, true},
		{"decision adapter of another area", "internal/decision/x.go", `import "` + modulePath + `/adapters/decision/localai"`, true},
		{"localai own module", "internal/rag/x.go", `import "github.com/mudler/LocalAI/core/config"`, true},
		{"sibling directory sharing a prefix", "internal/client/x.go", "import " + adapter, true},
		{"root-like name that is not the root", "toolsmith/x.go", "import " + adapter, true},
		{"cli root", "internal/cli/cmd_serve.go", "import " + adapter, false},
		{"cmd root", "cmd/alixai/main.go", "import " + adapter, false},
		{"cmd subtree", "cmd/alixai-parser/x.go", "import " + adapter, false},
		{"tools root", "tools/bench/main.go", "import " + adapter, false},
		{"the adapter itself", "adapters/localai/live_test.go", "import " + adapter, false},
		{"adapter subpackage", "adapters/localai/voice/x.go", "import " + adapter, false},
		{"decision adapter importing the base adapter", "adapters/decision/localai/x.go", "import " + adapter, false},
		{"unrelated import", "internal/rag/x.go", `import "` + modulePath + `/internal/providers"`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "//go:build live\n\npackage x\n\n" + tc.imports + "\n"
			f, err := parseGoSource(tc.rel, src, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			got := localAIImportViolations([]*goSource{f})
			if (len(got) > 0) != tc.want {
				t.Errorf("violations = %v, want violation: %v", got, tc.want)
			}
		})
	}

	t.Run("comments and strings are not imports", func(t *testing.T) {
		src := "package x\n\n// " + adapter + "\nvar s = " + adapter + "\n"
		f, err := parseGoSource("internal/rag/x.go", src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		if got := localAIImportViolations([]*goSource{f}); len(got) != 0 {
			t.Errorf("a mention outside an import clause was reported: %v", got)
		}
	})
}

// ---- rule 1: pkg/contracts imports nothing internal ----

// contractsImportViolations checks the public contract package. Rule 1 of
// docs/architecture/module-map.md forbids any import path under internal/;
// the stated policy is stricter: standard library and third parties only,
// plus pkg/contracts' own subpackages (gen/...). An embedding host must be
// able to depend on the contracts without pulling in the service.
func contractsImportViolations(files []*goSource) []string {
	own := modulePath + "/pkg/contracts"
	var out []string
	for _, f := range files {
		if !under(f.Rel, "pkg/contracts/") {
			continue
		}
		for _, p := range f.Imports {
			switch {
			case p == modulePath+"/internal" || strings.HasPrefix(p, modulePath+"/internal/"):
				out = append(out, fmt.Sprintf("%s imports %s (internal)", f.Rel, p))
			case strings.Contains(p, "/internal/") || strings.HasSuffix(p, "/internal"):
				out = append(out, fmt.Sprintf("%s imports %s (an internal package)", f.Rel, p))
			case p == modulePath || strings.HasPrefix(p, modulePath+"/"):
				if p != own && !strings.HasPrefix(p, own+"/") {
					out = append(out, fmt.Sprintf("%s imports %s (module package outside pkg/contracts)", f.Rel, p))
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestPkgContractsImportsNothingInternal(t *testing.T) {
	files := repoImports(t)

	n, gen := 0, 0
	for _, f := range files {
		if under(f.Rel, "pkg/contracts/") {
			n++
			if under(f.Rel, "pkg/contracts/gen/") {
				gen++
			}
		}
	}
	if n < 10 || gen == 0 {
		t.Fatalf("saw %d files under pkg/contracts (%d under gen/); the scan is not seeing the package", n, gen)
	}

	for _, v := range contractsImportViolations(files) {
		t.Errorf("%s: pkg/contracts must depend only on the standard library, third parties and its own subpackages (module-map.md rule 1)", v)
	}
}

func TestContractsImportDetector(t *testing.T) {
	cases := []struct {
		name string
		rel  string
		imp  string
		want bool
	}{
		{"internal package", "pkg/contracts/x.go", modulePath + "/internal/ids", true},
		{"internal in a test file", "pkg/contracts/x_test.go", modulePath + "/internal/clock", true},
		{"internal in a generated subpackage", "pkg/contracts/gen/common/x.go", modulePath + "/internal/log", true},
		{"adapter", "pkg/contracts/x.go", modulePath + "/adapters/localai", true},
		{"sibling pkg", "pkg/contracts/x.go", modulePath + "/pkg/sdk-go", true},
		{"another module's internal", "pkg/contracts/x.go", "example.com/lib/internal/y", true},
		{"own subpackage", "pkg/contracts/x.go", modulePath + "/pkg/contracts/gen/common", false},
		{"own root from a subpackage", "pkg/contracts/gen/actions/x.go", modulePath + "/pkg/contracts", false},
		{"standard library", "pkg/contracts/x.go", "encoding/json", false},
		{"third party", "pkg/contracts/x.go", "github.com/google/go-cmp/cmp", false},
		{"not pkg/contracts", "internal/rag/x.go", modulePath + "/internal/ids", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := parseGoSource(tc.rel, "package x\n\nimport _ \""+tc.imp+"\"\n", parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			got := contractsImportViolations([]*goSource{f})
			if (len(got) > 0) != tc.want {
				t.Errorf("violations = %v, want violation: %v", got, tc.want)
			}
		})
	}
}
