package identity_test

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

// internal/identity may import the base packages and internal/store
// (docs/architecture/module-map.md, tier 4). It speaks HTTP only through
// net/http handlers: it never listens on a socket, dials out or starts a
// process, and it never imports an adapter. identitytest is test support and
// no production file anywhere in the module may import it.
func TestImportsStayWithinMap(t *testing.T) {
	allowed := map[string]bool{
		"pkg/contracts":                  true,
		"internal/clock":                 true,
		"internal/ids":                   true,
		"internal/config":                true,
		"internal/log":                   true,
		"internal/store":                 true,
		"internal/identity":              true,
		"internal/identity/identitytest": false, // tests only, below
		"internal/store/storetest":       false, // _test.go files only, below
	}
	forbidden := map[string]bool{"net": true, "os/exec": true, "plugin": true, "unsafe": true}

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
			if forbidden[p] {
				t.Errorf("%s imports %s", path, p)
			}
			if !strings.HasPrefix(p, modulePath) {
				continue
			}
			rel := strings.TrimPrefix(p, modulePath)
			if strings.HasPrefix(rel, "adapters/") {
				t.Errorf("%s imports %s: identity never imports an adapter", path, rel)
			}
			allow, known := allowed[rel]
			switch {
			case !known:
				t.Errorf("%s imports %s, which docs/architecture/module-map.md does not allow internal/identity", path, rel)
			case !allow && !isTest:
				// identitytest is not a _test.go file, so it gets no exemption:
				// storetest is imported only from _test.go files (module map).
				t.Errorf("%s imports %s outside _test.go files", path, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("no Go files walked")
	}
}

// No production file in the module imports identitytest: a test minter linked
// into the binary would be a way to forge tokens if a key ever leaked into it.
func TestNoProductionCodeImportsTestMinter(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	const target = modulePath + "internal/identity/identitytest"
	var walked int
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n != "." && strings.HasPrefix(n, ".") || n == "node_modules" || n == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if strings.HasPrefix(filepath.ToSlash(path), filepath.ToSlash(filepath.Join(root, "internal/identity/identitytest"))+"/") {
			return nil
		}
		walked++
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return nil // a file the toolchain would reject is not this test's business
		}
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == target {
				t.Errorf("%s imports identitytest from production code", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if walked == 0 {
		t.Fatal("walked no production files; the root is wrong")
	}
}
