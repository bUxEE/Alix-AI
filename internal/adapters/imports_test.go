package adapters

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

// internal/adapters is tier 4 of docs/architecture/module-map.md: it may import
// the base packages and internal/egress. It holds no transport and no network
// code (the transports are adapters/*, which reach the network through
// internal/egress), starts no process and imports no concrete adapter, and
// above all never LocalAI.
func TestImportsStayWithinBase(t *testing.T) {
	allowed := map[string]bool{
		"pkg/contracts":   true,
		"internal/clock":  true,
		"internal/ids":    true,
		"internal/config": true,
		"internal/log":    true,
		"internal/egress": true,
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
				t.Errorf("%s imports %s: internal/adapters has no transport or network code", path, p)
			}
			if !strings.HasPrefix(p, modulePath) {
				continue
			}
			rel := strings.TrimPrefix(p, modulePath)
			if strings.HasPrefix(rel, "pkg/contracts/gen/") {
				continue // generated contract types, part of the public contract
			}
			if rel == "internal/adapters" || strings.HasPrefix(rel, "internal/adapters/") {
				continue // the package, its fake and its tests may import each other
			}
			if strings.HasPrefix(rel, "tests/conformance/") && strings.HasSuffix(path, "_test.go") {
				continue // the fake is proven by the shared suite
			}
			if !allowed[rel] {
				t.Errorf("%s imports %s, which module-map.md does not allow for internal/adapters", path, rel)
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
