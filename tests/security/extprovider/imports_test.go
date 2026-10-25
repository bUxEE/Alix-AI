package extprovidersec

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/tests/security/secutil"
)

const modulePath = "github.com/buxee/alix-ai/"

// externalTrees are the production packages of External Provider Mode. None of them can reach the host application:
// they hold no adapter, no action coordinator and no policy gateway, so there is no code path from a provider's
// output to an application operation except the engine's own validate, prepare, confirm and commit path.
var externalTrees = []string{
	"internal/dialogue/extmode", "internal/modes", "internal/modes/hybrid", "internal/dataegress", "internal/providerreg",
	"internal/providerbudget", "internal/extmodal", "internal/credentials", "internal/oauth",
	"adapters/external", "adapters/external/openai", "adapters/external/anthropic", "adapters/external/gemini", "adapters/external/meta",
}

// importsOfDir lists the imports of the non-test Go files of one directory, by file.
func importsOfDir(t testing.TB, dir string) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string][]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			out[e.Name()] = append(out[e.Name()], p)
		}
	}
	return out
}

// forbiddenImports reports every import in the trees that is, or is under, a forbidden path.
func forbiddenImports(t testing.TB, root string, trees, forbidden []string) []string {
	t.Helper()
	var found []string
	files := 0
	for _, tree := range trees {
		for file, imps := range importsOfDir(t, filepath.Join(root, filepath.FromSlash(tree))) {
			files++
			for _, p := range imps {
				for _, bad := range forbidden {
					if p == bad || strings.HasPrefix(p, modulePath+bad) || strings.HasPrefix(p, bad+"/") {
						found = append(found, tree+"/"+file+" imports "+p)
					}
				}
			}
		}
	}
	if files < 40 {
		t.Fatalf("the import scan found only %d files: it is not looking at the packages it names", files)
	}
	return found
}

// plantedImports writes a tree with one file that imports the given paths and returns its directory.
func plantedImports(t testing.TB, imports ...string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "planted")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	src := "package planted\n\nimport (\n"
	for _, p := range imports {
		src += "\t_ \"" + p + "\"\n"
	}
	src += ")\n"
	if err := os.WriteFile(filepath.Join(dir, "planted.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// checkNoPathToHost fails when an external-mode package imports a package that can reach the host application, run
// a process or load code. It runs the checker over a planted source first, so a checker that finds nothing is caught.
func checkNoPathToHost(t *testing.T) {
	t.Helper()
	forbidden := []string{"internal/actions", "internal/adapters", "internal/policy", "adapters/mcp", "adapters/openapi", "adapters/native", "os/exec", "plugin"}
	bad := plantedImports(t, modulePath+"internal/adapters", modulePath+"internal/actions", "os/exec")
	if got := importsOfDir(t, filepath.Join(bad, "planted")); len(got["planted.go"]) != 3 {
		t.Fatalf("control: the checker read %v from the planted source", got)
	}
	planted := forbiddenImportsIn(t, filepath.Join(bad, "planted"), forbidden)
	if len(planted) != 3 {
		t.Fatalf("control: the checker flagged %v in a source that imports an adapter, the coordinator and os/exec", planted)
	}
	if found := forbiddenImports(t, secutil.RepoRoot(t), externalTrees, forbidden); len(found) > 0 {
		t.Fatalf("external-mode packages reach the host application or run code:\n%s", strings.Join(found, "\n"))
	}
}

// forbiddenImportsIn is forbiddenImports for one directory, without the size guard.
func forbiddenImportsIn(t testing.TB, dir string, forbidden []string) []string {
	t.Helper()
	var found []string
	for file, imps := range importsOfDir(t, dir) {
		for _, p := range imps {
			for _, bad := range forbidden {
				if p == bad || strings.HasPrefix(p, modulePath+bad) {
					found = append(found, file+" imports "+p)
				}
			}
		}
	}
	return found
}
