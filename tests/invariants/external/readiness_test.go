package external

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestReadinessIsLocalAndNeverActivates (ADR 0037): the readiness commands are part of the binary built with
// the noexternal tag, and the readiness packages and their commands import nothing that signs, installs,
// activates or audits. What a report says (advisory first, no verdict) is tested in internal/readiness/report.
func TestReadinessIsLocalAndNeverActivates(t *testing.T) {
	t.Run("the noexternal build holds the readiness packages", func(t *testing.T) {
		bare := goListDeps(t, "noexternal", "./cmd/alixai")
		for _, want := range []string{"internal/readiness/scan", "internal/readiness/verify", "internal/readiness/report", "internal/readiness/store", "internal/readiness/assist", "internal/readiness/scaffold"} {
			if !slices.Contains(bare, modulePath+want) {
				t.Errorf("the noexternal build does not hold %s: readiness is not part of the local binary", want)
			}
		}
	})

	t.Run("nothing of readiness imports what signs, installs, activates or audits", func(t *testing.T) {
		root := repoRoot(t)
		forbidden := []string{
			"internal/trust", "internal/apppkg", "internal/integrationaudit", "internal/apps", "internal/supervisor", "internal/release",
			"internal/upgrade", "internal/gateway", "internal/identity", "tools/devkeys",
		}
		var files []string
		err := filepath.WalkDir(filepath.Join(root, "internal", "readiness"), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "testdata" {
				return filepath.SkipDir
			}
			if !d.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(files) < 6 {
			t.Fatalf("found %d readiness sources: the scan is not looking at internal/readiness", len(files))
		}
		files = append(files, filepath.Join(root, "internal", "cli", "cmd_readiness.go"), filepath.Join(root, "internal", "cli", "cmd_readiness_scaffold.go"))
		for _, p := range files {
			f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range f.Imports {
				ip, _ := strconv.Unquote(imp.Path.Value)
				rel, ok := strings.CutPrefix(ip, modulePath)
				if !ok {
					continue
				}
				for _, bad := range forbidden {
					if rel == bad || strings.HasPrefix(rel, bad+"/") {
						t.Errorf("%s imports %s, which signs, installs, activates or audits", path.Base(filepath.ToSlash(p)), rel)
					}
				}
			}
		}
	})
}
