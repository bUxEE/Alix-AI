package repo

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// modulePath is the Go module the gate guards (go.mod, master plan section 20).
const modulePath = "github.com/buxee/alix-ai"

// findRepoRoot walks up from start to the directory whose go.mod declares
// modulePath. It never guesses a path: a checkout in any location (main or a
// .worktrees/<task> copy) resolves to its own root.
func findRepoRoot(start string) (string, error) {
	for dir := start; ; dir = filepath.Dir(dir) {
		if declared, err := goModModule(filepath.Join(dir, "go.mod")); err == nil && declared == modulePath {
			return dir, nil
		}
		if filepath.Dir(dir) == dir {
			return "", fmt.Errorf("no go.mod declaring module %s above %s", modulePath, start)
		}
	}
}

// goModModule returns the module path declared by the go.mod file at path.
func goModModule(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "module"); ok {
			if fields := strings.Fields(rest); len(fields) > 0 {
				return strings.Trim(fields[0], `"`), nil
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s has no module directive", path)
}

var (
	rootOnce sync.Once
	rootDir  string
	rootErr  error
)

// repoRoot is the root of the checkout this test binary runs in. `go test`
// runs a package test in the package directory, so the root is found by
// walking up from there.
func repoRoot(t testing.TB) string {
	t.Helper()
	rootOnce.Do(func() {
		wd, err := os.Getwd()
		if err != nil {
			rootErr = err
			return
		}
		rootDir, rootErr = findRepoRoot(wd)
	})
	if rootErr != nil {
		t.Fatalf("cannot locate the repository root: %v", rootErr)
	}
	return rootDir
}

// skipDir reports whether the directory called name is outside every scan:
// the same directories the Go tool ignores (leading "." or "_", testdata), plus
// dependency trees. Skipping ".worktrees" matters: in the main checkout it
// holds full copies of the repository with other tasks' unmerged code.
func skipDir(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
		name == "testdata" || name == "node_modules" || name == "vendor"
}

// isIgnoredTree reports whether the directory rel (slash path from the root)
// is local build output or model data that .gitignore keeps out of the
// repository: bin/, dist/, data/, benchmarks/tmp/ and deploy/container/*/data/
// (gigabytes of downloaded models and backends on a machine that ran the
// spike). The gate checks what is tracked, not what a developer has lying
// around.
func isIgnoredTree(rel string) bool {
	switch rel {
	case "bin", "dist", "data", "benchmarks/tmp":
		return true
	}
	return under(rel+"/", "deploy/container/") && path.Base(rel) == "data"
}

// walkRepo calls visit with the slash-separated path (relative to root) of
// every regular file, skipping skipDir directories and nested modules (a
// directory with its own go.mod is a different codebase).
func walkRepo(root string, visit func(rel string) error) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == root {
				return nil
			}
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			if rel, err := filepath.Rel(root, p); err == nil && isIgnoredTree(filepath.ToSlash(rel)) {
				return filepath.SkipDir
			}
			if st, err := os.Stat(filepath.Join(p, "go.mod")); err == nil && !st.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		return visit(filepath.ToSlash(rel))
	})
}

var (
	filesOnce sync.Once
	filesList []string
	filesErr  error
)

// repoFiles lists every scannable file of the checkout once, sorted.
func repoFiles(t testing.TB) []string {
	t.Helper()
	root := repoRoot(t)
	filesOnce.Do(func() {
		filesErr = walkRepo(root, func(rel string) error {
			filesList = append(filesList, rel)
			return nil
		})
		sort.Strings(filesList)
	})
	if filesErr != nil {
		t.Fatalf("cannot walk the repository: %v", filesErr)
	}
	return filesList
}

// readRepoFile reads a file given by its slash-separated path from the root.
func readRepoFile(t testing.TB, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return data
}

// nonEmptyFileProblem describes why rel is not a regular, non-empty file, or returns "".
func nonEmptyFileProblem(root, rel string) string {
	st, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Sprintf("%s: missing", rel)
	case err != nil:
		return fmt.Sprintf("%s: unreadable (%v)", rel, err)
	case st.IsDir():
		return fmt.Sprintf("%s: is a directory, want a file", rel)
	case st.Size() == 0:
		return fmt.Sprintf("%s: empty", rel)
	}
	return ""
}

// under reports whether the slash path rel is inside the directory prefix
// (which ends with "/"). Matching is on a path boundary, so "internal/cli/"
// does not match "internal/client/x.go".
func under(rel, prefix string) bool {
	return strings.HasPrefix(rel, prefix)
}

func TestWalkerSkipsWhatGitIgnoreExcludes(t *testing.T) {
	skipped := []string{"bin", "dist", "data", "benchmarks/tmp", "deploy/container/localai/data", "deploy/container/clm/data"}
	for _, rel := range skipped {
		if !isIgnoredTree(rel) {
			t.Errorf("%s should be skipped: .gitignore excludes it", rel)
		}
	}
	scanned := []string{"internal/data", "internal/binary", "pkg/data", "benchmarks/results", "deploy/container/localai", "deploy/examples/data", "tests/data", "docs/data"}
	for _, rel := range scanned {
		if isIgnoredTree(rel) {
			t.Errorf("%s is tracked content and must be scanned", rel)
		}
	}
	for _, name := range []string{".git", ".worktrees", ".github", "_x", "testdata", "node_modules", "vendor"} {
		if !skipDir(name) {
			t.Errorf("directory name %s should be skipped", name)
		}
	}
	for _, name := range []string{"internal", "data", "tests", "gates"} {
		if skipDir(name) {
			t.Errorf("directory name %s must not be skipped by name", name)
		}
	}
}
