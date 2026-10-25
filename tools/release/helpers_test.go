package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var (
	toolchainOnce sync.Once
	toolchainVal  toolchain
	toolchainErr  error
)

// testToolchain finds the Go toolchain once.
func testToolchain(t *testing.T) toolchain {
	t.Helper()
	toolchainOnce.Do(func() { toolchainVal, toolchainErr = detectToolchain(context.Background()) })
	if toolchainErr != nil {
		t.Fatalf("no usable Go toolchain: %v", toolchainErr)
	}
	return toolchainVal
}

// repoRoot is the module root of the checkout the tests run in.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := findModuleRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// writeFixtureModule writes a tiny standard-library-only module with one main package and returns its directory.
// It builds in a few seconds even with an empty build cache.
func writeFixtureModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":         "module example.test/fixture\n\ngo 1.27\n",
		"cmd/hi/main.go": "package main\n\nimport \"os\"\n\n// stamp is set by the linker.\nvar stamp = \"unstamped\"\n\nfunc main() { os.Stdout.WriteString(\"hi \" + stamp + \"\\n\") }\n",
	}
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// testExe is the running test binary: a real Go executable with real build information.
func testExe(t *testing.T) string {
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// runTool runs one invocation of the tool and returns the exit status and both output streams.
func runTool(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// hex64 is a syntactically valid SHA-256 made of one repeated byte.
func hex64(b string) string { return strings.Repeat(b, 32) }
