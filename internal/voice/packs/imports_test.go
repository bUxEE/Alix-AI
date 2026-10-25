package packs_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The pack store never reaches for the network or a subprocess (a runtime
// connection is the provider factory's business, and nothing is downloaded: the
// operator fetches the pinned files), and never imports an adapter.
func TestImportsStayLocal(t *testing.T) {
	forbidden := map[string]bool{"net": true, "net/http": true, "os/exec": true}
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
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if forbidden[p] || strings.Contains(p, "/adapters/") {
				t.Errorf("%s imports %s", path, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 4 {
		t.Fatalf("walked %d files: the guard is not looking at the package", files)
	}
}
