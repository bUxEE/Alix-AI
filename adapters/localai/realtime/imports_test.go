package realtime

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/buxee/alix-ai/"

// The package reaches the rest of the module only through the base packages and
// the provider seams (module-map.md tier 4), starts no process, and writes no
// file (audio is never kept: master plan section 13).
func TestImportsStayWithinBase(t *testing.T) {
	prod := map[string]bool{
		"adapters/localai":         true,
		"pkg/contracts":            true,
		"internal/clock":           true,
		"internal/providers":       true,
		"internal/providers/voice": true,
	}
	test := map[string]bool{"internal/providers/voice/fake": true}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) < 8 {
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
			if !isTest && (p == "os" || p == "io/ioutil") && f != "secret.go" && f != "transport.go" {
				t.Errorf("%s imports %s: the adapter writes no files", f, p)
			}
			if !strings.HasPrefix(p, modulePath) {
				continue
			}
			rel := strings.TrimPrefix(p, modulePath)
			if prod[rel] || (isTest && test[rel]) {
				continue
			}
			t.Errorf("%s imports %s, which module-map.md does not allow here", f, rel)
		}
	}
}
