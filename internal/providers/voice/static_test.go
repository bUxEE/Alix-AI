package voice_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoGoroutineIsStarted keeps the "no goroutines" property of the voice
// roles and of the fake runtime structural: neither package's non-test code
// contains a go statement. Fake streams do all their waiting in the caller's
// Recv, so a cancelled or closed stream leaves nothing running.
func TestNoGoroutineIsStarted(t *testing.T) {
	var files int
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files++
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if g, ok := n.(*ast.GoStmt); ok {
				t.Errorf("%s: go statement in a voice provider package", fset.Position(g.Pos()))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 10 {
		t.Fatalf("scanned only %d files; the walk is not seeing both packages", files)
	}
}
