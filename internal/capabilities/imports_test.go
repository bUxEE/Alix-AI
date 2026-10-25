package capabilities

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/buxee/alix-ai/"

// internal/capabilities is tier 4 of docs/architecture/module-map.md: it may
// import the base packages, store, apppkg, policy and providers, and nothing
// else from this module. Test files may also use the deterministic provider fake
// and the test-support packages the installer-backed test needs.
func TestImportsStayWithinTheModuleMap(t *testing.T) {
	prod := map[string]bool{
		"pkg/contracts":                        true,
		"pkg/contracts/gen/applicationpackage": true,
		"internal/clock":                       true,
		"internal/ids":                         true,
		"internal/config":                      true,
		"internal/log":                         true,
		"internal/store":                       true,
		"internal/apppkg":                      true,
		"internal/policy":                      true,
		"internal/providers":                   true,
	}
	testOnly := map[string]bool{
		"internal/providers/fake":  true,
		"internal/store/storetest": true,
		"internal/trust":           true, // installer_test.go: the installer needs a trust store
		"tools/devkeys":            true, // the published TEST-ONLY publisher key
	}
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
		isTest := strings.HasSuffix(path, "_test.go")
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			switch p {
			case "net", "net/http", "os/exec", "plugin", "unsafe", "syscall":
				t.Errorf("%s imports %s: the router does no network, shell-out or low-level work", path, p)
			}
			if !strings.HasPrefix(p, modulePath) {
				continue
			}
			rel := strings.TrimPrefix(p, modulePath)
			if rel == "internal/capabilities" {
				continue
			}
			if !prod[rel] && !(isTest && testOnly[rel]) {
				t.Errorf("%s imports %s, which module-map.md does not allow for internal/capabilities", path, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 8 {
		t.Fatalf("walked %d files; the test is not looking at the package", files)
	}
}

// TestCapabilityExamplesAreNotRead: the package schema says examples are
// test-only, never a routing rule and never a prompt. No non-test file of this
// package may mention the field, so no later edit can start using it unnoticed.
func TestCapabilityExamplesAreNotRead(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			code := line
			if j := strings.Index(code, "//"); j >= 0 {
				code = code[:j]
			}
			if strings.Contains(code, ".Examples") || strings.Contains(code, "CapabilityExample") {
				t.Errorf("%s:%d reads capability examples: %s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}
