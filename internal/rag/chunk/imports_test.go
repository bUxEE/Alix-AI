package chunk

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/buxee/alix-ai/"

// The chunker is pure: no model, network, filesystem, process or clock. Its
// production files import the standard library and the generated contract
// types, nothing else from this module (docs/architecture/module-map.md: base
// packages only), so ingestion can run it in any worker without wiring.
func TestImportsStayPure(t *testing.T) {
	allowed := map[string]bool{
		"pkg/contracts":                  true,
		"pkg/contracts/gen/actions":      true,
		"pkg/contracts/gen/rag":          true,
		"pkg/contracts/gen/conversation": true,
	}
	forbidden := map[string]bool{
		"os": true, "os/exec": true, "net": true, "net/http": true, "plugin": true,
		"syscall": true, "time": true, "math/rand": true, "math/rand/v2": true, "crypto/rand": true,
		"io/ioutil": true, "unsafe": true,
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
			if forbidden[p] {
				t.Errorf("%s imports %s: the chunker has no I/O, clock or randomness", f, p)
			}
			if rel, ok := strings.CutPrefix(p, modulePath); ok && !allowed[rel] {
				t.Errorf("%s imports %s, which the chunker may not use", f, rel)
			}
			if strings.Contains(p, ".") && !strings.HasPrefix(p, modulePath) {
				t.Errorf("%s imports third-party %s: production code is stdlib plus contracts", f, p)
			}
		}
	}
	if checked < 8 {
		t.Fatalf("checked only %d production files", checked)
	}
}
