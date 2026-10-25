package localai

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/buxee/alix-ai/"

// adapters/localai is tier 4 of docs/architecture/module-map.md: the base
// packages and internal/providers, nothing else from this module, and no
// shell-out. (Tests additionally use internal/config and internal/log.)
func TestImportsStayWithinBase(t *testing.T) {
	prod := map[string]bool{
		"pkg/contracts":      true,
		"internal/clock":     true,
		"internal/providers": true,
	}
	test := map[string]bool{"internal/config": true, "internal/log": true}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) < 10 {
		t.Fatalf("found %d files (%v); the glob is not seeing the package", len(files), err)
	}
	for _, f := range files {
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		isTest := strings.HasSuffix(f, "_test.go")
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == "os/exec" || p == "plugin" {
				t.Errorf("%s imports %s: the adapter never starts processes", f, p)
			}
			if !strings.HasPrefix(p, modulePath) {
				continue
			}
			rel := strings.TrimPrefix(p, modulePath)
			if prod[rel] || (isTest && test[rel]) {
				continue
			}
			t.Errorf("%s imports %s, which module-map.md does not allow for adapters/localai", f, rel)
		}
	}
}
