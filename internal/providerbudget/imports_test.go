package providerbudget

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// providerbudget joins the store, the job priority classes and the generated provider contracts
// (docs/architecture/module-map.md). It must not reach the registry (the router joins them), the
// per-tenant quotas, the network, the SQLite driver or any adapter, and it reaches no provider.
func TestImportsStayWithinBudgetTier(t *testing.T) {
	const module = "github.com/buxee/alix-ai/"
	allowed := map[string]bool{
		"pkg/contracts":               true,
		"pkg/contracts/gen/common":    true,
		"pkg/contracts/gen/providers": true,
		"internal/clock":              true,
		"internal/ids":                true,
		"internal/jobs":               true, // the priority classes that choose the channel
		"internal/store":              true,
		"internal/store/storetest":    true, // tests only
	}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			switch {
			case p == "net" || strings.HasPrefix(p, "net/") || p == "os/exec" || strings.Contains(p, "modernc.org/sqlite"):
				t.Errorf("%s imports %s: no network, shell-out or SQLite driver in the budget", path, p)
			case strings.HasPrefix(p, module) && !allowed[strings.TrimPrefix(p, module)]:
				t.Errorf("%s imports %s, which the budget may not depend on", path, p)
			}
			if !strings.HasSuffix(path, "_test.go") && strings.Contains(p, "storetest") {
				t.Errorf("%s: a test helper in production code", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
