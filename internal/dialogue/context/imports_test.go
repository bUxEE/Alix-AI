package dctx

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

// internal/dialogue/context sits beside the dialogue engine and is imported by it, so it
// must not import internal/dialogue (an import cycle the day the engine adopts it) and it
// may import only what a prompt builder needs: the contracts, the provider request types
// and budget, and the session state. It does no network, shell-out or file work, and it
// has no way to reach the transcript, the audit log, policy or an adapter.
// Test files may also use the clock and the throwaway store of storetest.
func TestImportsStayWithinTheModuleMap(t *testing.T) {
	prod := map[string]bool{
		"pkg/contracts":      true,
		"internal/providers": true,
		"internal/sessions":  true,
	}
	testOnly := map[string]bool{
		"internal/clock":           true,
		"internal/store/storetest": true,
	}
	var files int
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		files++
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		isTest := strings.HasSuffix(path, "_test.go")
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			switch p {
			case "net", "net/http", "os", "os/exec", "plugin", "unsafe", "syscall", "database/sql":
				if !(isTest && p == "os") {
					t.Errorf("%s imports %s: the context builder does no network, shell-out, file or database work", path, p)
				}
			}
			if !strings.HasPrefix(p, modulePath) {
				continue
			}
			rel := strings.TrimPrefix(p, modulePath)
			if rel == "internal/dialogue" || strings.HasPrefix(rel, "internal/dialogue/") && rel != "internal/dialogue/context" {
				t.Errorf("%s imports %s: the engine imports this package, so this package must not import it", path, rel)
			}
			if !prod[rel] && !(isTest && testOnly[rel]) {
				t.Errorf("%s imports %s, which is not allowed for internal/dialogue/context", path, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 8 {
		t.Fatalf("scanned %d files; the scan is not looking at the package", files)
	}
}
