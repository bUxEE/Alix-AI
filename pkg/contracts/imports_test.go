package contracts

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const genImportPrefix = "github.com/buxee/alix-ai/pkg/contracts/gen/"

// TestNoInternalOrThirdPartyImports pins the package's isolation: it is public
// API, so non-test code may import nothing from internal/ and nothing outside
// the standard library; test code may use go-cmp and the schema validator but
// still nothing from internal/.
func TestNoInternalOrThirdPartyImports(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no Go files found: %v", err)
	}
	allowedInTests := map[string]bool{
		"github.com/google/go-cmp/cmp":             true,
		"github.com/santhosh-tekuri/jsonschema/v6": true,
	}
	fset := token.NewFileSet()
	for _, f := range files {
		parsed, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		isTest := strings.HasSuffix(f, "_test.go")
		for _, imp := range parsed.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if strings.Contains(path, "/internal/") || strings.HasSuffix(path, "/internal") {
				t.Errorf("%s imports %s: pkg/contracts must not import internal/", f, path)
			}
			stdlib := !strings.Contains(strings.SplitN(path, "/", 2)[0], ".")
			// aliases.go re-exports the generated types. The gen packages import only the
			// standard library and each other (pinned by gen's own import test), so
			// pkg/contracts stays stdlib-only transitively.
			generated := strings.HasPrefix(path, genImportPrefix)
			if !stdlib && !generated && !(isTest && allowedInTests[path]) {
				t.Errorf("%s imports %s: non-test code must use the standard library only", f, path)
			}
		}
	}
}
