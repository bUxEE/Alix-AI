package profile_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/buxee/alix-ai/"

// internal/voice/profile may import the base packages, internal/providers and
// its voice sub-package (docs/architecture/module-map.md, tier 5, and the
// task's depends_on). It must never import an adapter: the concrete runtime
// arrives through Deps from the composition root. Test files may also use the
// voice fake. No file may reach for the network or a subprocess: a profile that
// is not loaded must not be able to open a connection, and a loaded one gets
// its connections from the factory.
func TestImportsStayWithinBase(t *testing.T) {
	allowed := map[string]bool{
		"pkg/contracts":                 true,
		"internal/clock":                true,
		"internal/ids":                  true,
		"internal/config":               true,
		"internal/log":                  true,
		"internal/providers":            true,
		"internal/providers/voice":      true,
		"internal/voice/profile":        true,
		"internal/providers/voice/fake": false, // tests only, below
	}
	forbidden := map[string]bool{"net": true, "net/http": true, "os/exec": true}
	var files int
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		files++
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		isTest := strings.HasSuffix(path, "_test.go")
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if forbidden[p] {
				t.Errorf("%s imports %s", path, p)
			}
			if !strings.HasPrefix(p, modulePath) {
				continue
			}
			rel := strings.TrimPrefix(p, modulePath)
			if strings.HasPrefix(rel, "adapters/") {
				t.Errorf("%s imports %s: the profile never imports an adapter", path, rel)
			}
			if ok, listed := allowed[rel]; !listed || (!ok && !isTest) {
				t.Errorf("%s imports %s, which module-map.md does not allow for internal/voice/profile", path, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 6 {
		t.Fatalf("scanned only %d files; the walk is not seeing the package", files)
	}
}

// TestNoGoroutineIsStarted keeps the "no goroutines when disabled, none of its
// own when enabled" property structural: the package's non-test code contains
// no go statement, so nothing it does can leave a worker running.
func TestNoGoroutineIsStarted(t *testing.T) {
	var files int
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files++
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if g, ok := n.(*ast.GoStmt); ok {
				t.Errorf("%s: go statement: the voice profile must not start goroutines", fset.Position(g.Pos()))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 4 {
		t.Fatalf("scanned only %d files", files)
	}
}
