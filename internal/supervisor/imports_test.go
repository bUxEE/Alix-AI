package supervisor

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

// internal/supervisor may import the base packages it needs and
// internal/providers (docs/architecture/module-map.md, tier 6). It controls the
// process through the OS and reaches the runtime only through the injected
// provider: it never imports an adapter (LocalAI code), the store or the action
// coordinator, and it opens no socket itself (health is the provider's business).
func TestImportsStayWithinMap(t *testing.T) {
	allowed := map[string]bool{
		"pkg/contracts":             true,
		"pkg/contracts/gen/actions": true,
		"internal/clock":            true,
		"internal/providers":        true,
	}
	forbidden := map[string]bool{"net": true, "net/http": true, "plugin": true}
	external := map[string]bool{ // non-test dependencies
		"golang.org/x/sys/unix":    true,
		"golang.org/x/sys/windows": true,
	}
	externalInTests := map[string]bool{"github.com/google/go-cmp/cmp": true}

	var files int
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
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
			if rel, ok := strings.CutPrefix(p, modulePath); ok {
				if strings.HasPrefix(rel, "adapters/") {
					t.Errorf("%s imports %s: the supervisor never imports an adapter", path, rel)
				} else if !allowed[rel] {
					t.Errorf("%s imports %s, which is outside the module map", path, rel)
				}
				continue
			}
			if first, _, _ := strings.Cut(p, "/"); strings.Contains(first, ".") {
				if !external[p] && !(isTest && externalInTests[p]) {
					t.Errorf("%s imports %s: not an allowed dependency", path, p)
				}
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
