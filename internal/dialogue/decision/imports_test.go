package decision

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The scorer path imports no database, filesystem-writing, network or commit package, and no
// RAG package: the layer cannot read evidence, write anything or reach an adapter's commit.
func TestImportsKeepTheScorerPathAwayFromCommitRAGAndIO(t *testing.T) {
	const mod = "github.com/buxee/alix-ai/"
	forbidden := []string{
		mod + "internal/rag", mod + "internal/actions", mod + "internal/adapters", mod + "internal/store",
		mod + "internal/sessions", mod + "internal/files", mod + "adapters/",
		"database/sql", "net", "net/http", "os", "os/exec", "io/ioutil", "plugin", "unsafe", "syscall",
	}
	var files int
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		files++
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			for _, bad := range forbidden {
				if p == bad || (strings.HasSuffix(bad, "/") && strings.HasPrefix(p, bad)) {
					t.Errorf("%s imports %s", path, p)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 5 {
		t.Fatalf("scanned %d files", files)
	}
}
