package actions

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

// internal/actions is tier 5 of docs/architecture/module-map.md: it may import
// the base packages, store, jobs, audit, identity, policy and adapters. It never
// reaches a concrete adapter or LocalAI, opens no network connection and starts
// no process: the adapter and the package come from the AdapterSource and
// PackageSource the composition root provides.
func TestImportsStayWithinTier(t *testing.T) {
	allowed := map[string]bool{
		"pkg/contracts":     true,
		"internal/clock":    true,
		"internal/ids":      true,
		"internal/config":   true,
		"internal/log":      true,
		"internal/store":    true,
		"internal/jobs":     true,
		"internal/audit":    true,
		"internal/identity": true,
		"internal/policy":   true,
		"internal/adapters": true,
	}
	testOnly := map[string]bool{
		"internal/adapters/fake":   true,
		"internal/store/storetest": true,
	}
	files := 0
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
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			switch p {
			case "os/exec", "plugin", "net", "net/http":
				t.Errorf("%s imports %s: internal/actions has no network or process code", path, p)
			}
			if strings.Contains(p, "modernc.org/sqlite") {
				t.Errorf("%s imports the SQLite driver; only internal/store does", path)
			}
			if !strings.HasPrefix(p, modulePath) {
				continue
			}
			rel := strings.TrimPrefix(p, modulePath)
			if strings.HasPrefix(rel, "pkg/contracts/gen/") {
				continue // generated contract types, part of the public contract
			}
			if strings.HasSuffix(path, "_test.go") && testOnly[rel] {
				continue
			}
			if !allowed[rel] {
				t.Errorf("%s imports %s, which module-map.md does not allow for internal/actions", path, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 10 {
		t.Fatalf("scanned only %d files; the walk is not seeing the package", files)
	}
}
