package nocloud

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestMain(m *testing.M) {
	code := m.Run()
	cleanScratch()
	os.Exit(code)
}

// moduleRoot is the checkout this test belongs to: the nearest parent of the test's directory holding go.mod.
func moduleRoot(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// readFile reads a file of the checkout by its slash path.
func readFile(t testing.TB, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(moduleRoot(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return b
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func goTool() string {
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	return filepath.Join(runtime.GOROOT(), "bin", "go"+exeSuffix())
}

// built holds the programs the test builds once, the way the release builds them: pure Go, trimmed paths.
type built struct {
	parser string
}

var (
	buildOnce  sync.Once
	buildSet   built
	buildErr   error
	scratchMu  sync.Mutex
	scratchDir string
)

// scratchRoot is the one temporary directory of the package; TestMain removes it.
func scratchRoot() (string, error) {
	scratchMu.Lock()
	defer scratchMu.Unlock()
	if scratchDir == "" {
		d, err := os.MkdirTemp("", "alixai-nocloud-")
		if err != nil {
			return "", err
		}
		scratchDir = d
	}
	return scratchDir, nil
}

func cleanScratch() {
	scratchMu.Lock()
	defer scratchMu.Unlock()
	if scratchDir != "" {
		_ = os.RemoveAll(scratchDir)
	}
}

// programs builds the parser worker from this checkout on first use.
func programs(t testing.TB) built {
	t.Helper()
	buildOnce.Do(func() {
		scratch, err := scratchRoot()
		if err != nil {
			buildErr = err
			return
		}
		buildSet = built{parser: filepath.Join(scratch, "alixai-parser"+exeSuffix())}
		cmd := exec.Command(goTool(), "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", buildSet.parser, "./cmd/alixai-parser")
		cmd.Dir = moduleRoot(t)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=-mod=readonly")
		if b, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build ./cmd/alixai-parser: %w\n%s", err, b)
		}
	})
	if buildErr != nil {
		t.Fatalf("the parser worker could not be built: %v", buildErr)
	}
	return buildSet
}

// ---- Go sources --------------------------------------------------------------------------------

// declaredTests lists the Test functions declared by the _test.go files of one directory of the
// checkout (build constraints are not evaluated: a test behind a tag still counts as declared).
func declaredTests(t testing.TB, dir string) map[string]bool {
	t.Helper()
	abs := filepath.Join(moduleRoot(t), filepath.FromSlash(dir))
	entries, err := os.ReadDir(abs)
	if err != nil {
		t.Fatalf("%s: %v", dir, err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(abs, e.Name()), nil, 0)
		if err != nil {
			t.Fatalf("%s/%s: %v", dir, e.Name(), err)
		}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
				out[fn.Name.Name] = true
			}
		}
	}
	return out
}

// requireTests fails for every named test the directory does not declare.
func requireTests(t testing.TB, dir string, names ...string) {
	t.Helper()
	have := declaredTests(t, dir)
	var missing []string
	for _, n := range names {
		if !have[n] {
			missing = append(missing, n)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%s does not declare %s", dir, strings.Join(missing, ", "))
	}
}
