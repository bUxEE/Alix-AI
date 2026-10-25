package query

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/buxee/alix-ai/"

// The pipeline is core retrieval, not an adapter and not an external provider:
// it reaches models only through internal/providers, the database only through
// the index and vector packages, and it opens no network connection, runs no
// process and links neither LocalAI nor any external-provider package (the
// data-egress filter is a later stage that works on the package it builds).
func TestProductionImportsStayInsideTheLayer(t *testing.T) {
	allowed := map[string]bool{
		"internal/clock":                 true,
		"internal/config":                true,
		"internal/ids":                   true,
		"internal/providers":             true,
		"internal/rag/chunk":             true,
		"internal/rag/index":             true,
		"internal/rag/vector":            true,
		"pkg/contracts":                  true,
		"pkg/contracts/gen/actions":      true,
		"pkg/contracts/gen/common":       true,
		"pkg/contracts/gen/conversation": true,
		"pkg/contracts/gen/rag":          true,
	}
	forbidden := map[string]bool{
		"os/exec": true, "net": true, "net/http": true, "plugin": true, "unsafe": true, "syscall": true, "os": true,
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
				t.Errorf("%s imports %s: retrieval has no network, process, file or unsafe access of its own", f, p)
			case strings.HasPrefix(p, "modernc.org/"):
				t.Errorf("%s imports %s: only internal/store may import the SQLite driver", f, p)
			case strings.HasPrefix(p, modulePath):
				if rel := strings.TrimPrefix(p, modulePath); !allowed[rel] {
					t.Errorf("%s imports %s: not one of the collaborators of the query pipeline (docs/architecture/module-map.md)", f, rel)
				}
			case strings.Contains(strings.Split(p, "/")[0], "."):
				t.Errorf("%s imports third-party %s: production code is stdlib plus this module", f, p)
			}
		}
	}
	if checked < 8 {
		t.Fatalf("checked only %d production files", checked)
	}
}
