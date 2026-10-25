package egress

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/buxee/alix-ai/"

// internal/egress is tier 2 of docs/architecture/module-map.md: it may import
// the base packages and nothing else from this module, and it starts no
// processes. Every connector depends on it, so it must stay a leaf.
func TestImportsStayWithinBase(t *testing.T) {
	allowed := map[string]bool{
		"pkg/contracts":   true,
		"internal/clock":  true,
		"internal/ids":    true,
		"internal/config": true,
		"internal/log":    true,
	}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) < 8 {
		t.Fatalf("found %d files (%v); the glob is not seeing the package", len(files), err)
	}
	for _, f := range files {
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == "os/exec" || p == "plugin" {
				t.Errorf("%s imports %s: the egress gate never starts processes", f, p)
			}
			if !strings.HasPrefix(p, modulePath) {
				continue
			}
			if rel := strings.TrimPrefix(p, modulePath); !allowed[rel] {
				t.Errorf("%s imports %s, which module-map.md does not allow for internal/egress", f, rel)
			}
		}
	}
}
