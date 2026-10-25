package answer

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/buxee/alix-ai/"

// Grounded answering is core logic, not an adapter and not an external provider: it
// reaches models only through internal/providers, sources only through the query
// package's SourceCatalog, and it opens no network connection, reads no file, runs no
// process and links neither LocalAI nor any external-provider package (the data-egress
// filter of the external mode acts on the package before this one sees it).
func TestProductionImportsStayInsideTheLayer(t *testing.T) {
	allowed := map[string]bool{
		"internal/clock":                 true,
		"internal/config":                true,
		"internal/ids":                   true,
		"internal/providers":             true,
		"internal/rag/query":             true,
		"pkg/contracts":                  true,
		"pkg/contracts/gen/actions":      true,
		"pkg/contracts/gen/common":       true,
		"pkg/contracts/gen/conversation": true,
		"pkg/contracts/gen/rag":          true,
	}
	forbidden := map[string]bool{
		"os/exec": true, "net": true, "net/http": true, "plugin": true, "unsafe": true, "syscall": true, "os": true,
		"io/ioutil": true, "math/rand": true, "crypto/rand": true,
	}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) < 6 {
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
				t.Errorf("%s imports %s: answering has no network, process, file or entropy of its own (ids come from internal/ids)", f, p)
			case strings.HasPrefix(p, "modernc.org/"):
				t.Errorf("%s imports %s: only internal/store may import the SQLite driver", f, p)
			case strings.HasPrefix(p, modulePath):
				if rel := strings.TrimPrefix(p, modulePath); !allowed[rel] {
					t.Errorf("%s imports %s: not one of the collaborators of grounded answering (docs/architecture/module-map.md)", f, rel)
				}
			case strings.Contains(strings.Split(p, "/")[0], "."):
				t.Errorf("%s imports third-party %s: production code is stdlib plus this module", f, p)
			}
		}
	}
	if checked < 6 {
		t.Fatalf("checked only %d production files", checked)
	}
}
