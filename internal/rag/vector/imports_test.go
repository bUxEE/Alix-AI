package vector

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/buxee/alix-ai/"

// ADR 0009 confines the SQLite driver to internal/store, and ADR 0011 keeps
// sqlite-vec out of the production binary unless it wins the benchmark (it
// registers itself for EVERY connection of the process and rules out the
// driver's page cache). So the flat provider reaches the database only through
// internal/store, and nothing here links internal/store/vec; the benchmark
// harness that does is a _test.go file of the sqlitevec subpackage.
func TestProductionCodeStaysBehindTheStore(t *testing.T) {
	allowed := map[string]bool{
		"internal/clock":        true,
		"internal/config":       true,
		"internal/jobs":         true,
		"internal/providers":    true,
		"internal/rag/chunk":    true,
		"internal/store":        true,
		"pkg/contracts":         true,
		"pkg/contracts/gen/rag": true,
	}
	forbidden := map[string]bool{
		"os/exec": true, "net": true, "net/http": true, "plugin": true, "unsafe": true, "syscall": true,
	}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) < 8 {
		t.Fatalf("found %d files (%v); the glob is not seeing the package", len(files), err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		checked++
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			switch {
			case forbidden[p]:
				t.Errorf("%s imports %s: the index has no network, process or unsafe memory access", f, p)
			case strings.HasPrefix(p, "modernc.org/"):
				t.Errorf("%s imports %s: only internal/store may import the SQLite driver (ADR 0009)", f, p)
			case strings.HasPrefix(p, modulePath):
				if rel := strings.TrimPrefix(p, modulePath); !allowed[rel] {
					t.Errorf("%s imports %s, which the vector index may not use (docs/architecture/module-map.md)", f, rel)
				}
			case strings.Contains(strings.Split(p, "/")[0], "."):
				t.Errorf("%s imports third-party %s: production code is stdlib plus this module", f, p)
			}
		}
	}
	if checked < 7 {
		t.Fatalf("checked only %d production files", checked)
	}
}
