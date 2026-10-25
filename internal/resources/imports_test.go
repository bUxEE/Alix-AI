package resources

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

// internal/resources may import the base packages, internal/jobs and
// internal/providers (docs/architecture/module-map.md, tier 3). The governor
// is policy over injected probes: it opens no socket and starts no process, and
// it never imports an adapter (the runtime reaches it through the optional
// MemoryReporter and Unloader interfaces). Its tests may also use the ingest
// pause, the store test helpers and the fake provider.
func TestImportsStayWithinMap(t *testing.T) {
	allowed := map[string]bool{
		"pkg/contracts":            true,
		"internal/clock":           true,
		"internal/config":          true,
		"internal/jobs":            true,
		"internal/providers":       true,
		"internal/rag/ingest":      false, // _test.go files only
		"internal/providers/fake":  false,
		"internal/store/storetest": false,
	}
	forbidden := map[string]bool{"net": true, "net/http": true, "os/exec": true, "plugin": true}

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
			rel, ok := strings.CutPrefix(p, modulePath)
			if !ok {
				continue
			}
			if strings.HasPrefix(rel, "adapters/") {
				t.Errorf("%s imports %s: the governor never imports an adapter", path, rel)
				continue
			}
			prod, known := allowed[rel]
			switch {
			case !known:
				t.Errorf("%s imports %s, which the module map does not allow", path, rel)
			case !prod && !isTest:
				t.Errorf("%s imports %s outside tests", path, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("no Go files were checked")
	}
}
