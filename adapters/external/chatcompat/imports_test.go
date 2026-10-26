package chatcompat

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/buxee/alix-ai/"

func TestImportsStayWithinAdapterTier(t *testing.T) {
	allowed := map[string]bool{
		"pkg/contracts":               true,
		"pkg/contracts/gen/providers": true,
		"internal/clock":              true,
		"internal/providers":          true,
		"internal/egress":             true,
		"internal/credentials":        true,
		"adapters/external":           true,
	}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) < 8 {
		t.Fatalf("found %d files (%v)", len(files), err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == "os/exec" || p == "plugin" || p == "unsafe" {
				t.Errorf("%s imports %s", f, p)
			}
			if !strings.HasPrefix(p, modulePath) {
				continue
			}
			rel := strings.TrimPrefix(p, modulePath)
			if !allowed[rel] {
				t.Errorf("%s imports %s, which module-map.md does not allow here", f, rel)
			}
		}
	}
}
