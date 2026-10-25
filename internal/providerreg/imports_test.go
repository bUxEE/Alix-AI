package providerreg

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// providerreg joins the store and the generated provider contracts. It must not reach the data
// egress policy (internal/dataegress: X-04, joined to this package by the router), the
// credentials service (it takes an interface), the network, the SQLite driver or any adapter.
func TestImportsStayWithinRegistryTier(t *testing.T) {
	const module = "github.com/buxee/alix-ai/"
	allowed := map[string]bool{
		"pkg/contracts":               true,
		"pkg/contracts/gen/common":    true,
		"pkg/contracts/gen/providers": true,
		"internal/clock":              true,
		"internal/ids":                true,
		"internal/jobs":               true, // the revalidation job (class 6)
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
				t.Errorf("%s imports %s: no network, shell-out or SQLite driver in the registry", path, p)
			case strings.HasPrefix(p, module) && !allowed[strings.TrimPrefix(p, module)]:
				t.Errorf("%s imports %s, which the registry may not depend on", path, p)
			}
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			if strings.Contains(p, "storetest") {
				t.Errorf("%s: a test helper in production code", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
