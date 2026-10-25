package providers_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/buxee/alix-ai/"

// internal/providers is tier 2 of docs/architecture/module-map.md: it may
// import the base packages (pkg/contracts, internal/clock, internal/ids,
// internal/config, internal/log) and nothing else from this module. In
// particular it must never import adapters/localai (the dependency points the
// other way), and it contains no HTTP.
func TestImportsStayWithinBase(t *testing.T) {
	allowed := map[string]bool{
		"pkg/contracts":   true,
		"internal/clock":  true,
		"internal/ids":    true,
		"internal/config": true,
		"internal/log":    true,
	}
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
		pkgDir := filepath.ToSlash(filepath.Dir(path))
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == "net/http" || p == "net" {
				t.Errorf("%s imports %s: internal/providers has no HTTP or network code", path, p)
			}
			if !strings.HasPrefix(p, modulePath) {
				continue
			}
			rel := strings.TrimPrefix(p, modulePath)
			// The package, its fake and its tests may import each other.
			if rel == "internal/providers" || strings.HasPrefix(rel, "internal/providers/") {
				continue
			}
			if !allowed[rel] {
				t.Errorf("%s (in %s) imports %s, which module-map.md does not allow for internal/providers", path, pkgDir, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 10 {
		t.Fatalf("scanned only %d files; the walk is not seeing the package", files)
	}
}
