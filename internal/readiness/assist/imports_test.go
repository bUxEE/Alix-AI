package assist

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const assistModulePath = "github.com/buxee/alix-ai/"

// The assist leg takes its model and its guard as interfaces from the CLI composition root. It must
// not import the external-mode packages (they are reachable only from composition roots), any
// adapter, the network or process execution.
func TestAssistImportsNoExternalModePackages(t *testing.T) {
	forbidden := []string{
		"internal/modes", "internal/dataegress", "internal/providerreg", "internal/credentials", "internal/oauth",
		"internal/providerbudget", "internal/extmodal", "adapters/", "internal/cli", "internal/gateway", "internal/dialogue",
	}
	allowedInternal := map[string]bool{
		"internal/providers":          true,
		"internal/readiness/catalog":  true,
		"internal/readiness/scan":     true,
		"internal/readiness/verify":   true,
		"internal/readiness/report":   true,
		"pkg/contracts":               true,
		"pkg/contracts/gen/readiness": true,
	}
	testOnly := map[string]bool{"internal/providers/fake": true, "internal/clock": true, "internal/ids": true}
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
			if p == "net" || p == "net/http" || p == "os/exec" || strings.HasPrefix(p, "net/") {
				t.Errorf("%s imports %s: the assist leg has no network and runs no process", path, p)
			}
			rel, ok := strings.CutPrefix(p, assistModulePath)
			if !ok {
				continue
			}
			for _, bad := range forbidden {
				if rel == bad || strings.HasPrefix(rel, bad+"/") || (strings.HasSuffix(bad, "/") && strings.HasPrefix(rel, bad)) {
					t.Errorf("%s imports %s, which only a composition root may import", path, rel)
				}
			}
			if allowedInternal[rel] || strings.HasPrefix(rel, "pkg/contracts/gen/") || (isTest && testOnly[rel]) {
				continue
			}
			t.Errorf("%s imports %s, which module-map.md does not allow for internal/readiness/assist", path, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("no Go files found")
	}
}
