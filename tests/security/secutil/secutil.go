// Package secutil is the small toolbox the security suites under tests/security
// share: finding the repository root, walking the tracked files and recording a
// known defect.
//
// It is a non-test package so every suite can import it; it imports "testing"
// and is only ever linked into test binaries.
package secutil

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// RepoRoot returns the checkout root: the nearest parent of the test's working
// directory that holds go.mod.
func RepoRoot(t testing.TB) string {
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
			t.Fatal("secutil: go.mod not found above the test directory")
		}
		dir = parent
	}
}

// ReadFile reads a file given by its slash-separated path from the repository root.
func ReadFile(t testing.TB, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(RepoRoot(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("secutil: read %s: %v", rel, err)
	}
	return data
}

// skipDirName reports whether a directory is outside every scan: version
// control, other checkouts, dependency trees and local caches.
func skipDirName(name string) bool {
	switch name {
	case ".git", ".worktrees", "node_modules", "vendor", ".venv", "__pycache__", ".pnpm-store":
		return true
	}
	return false
}

// ignoredTree reports whether the directory rel (slash path from the root) is
// local build output or model data that .gitignore keeps out of the repository
// (/bin, /dist, /data, benchmarks/tmp, deploy/container/**/data). A scan covers
// what is tracked, not what a developer has lying around.
func ignoredTree(rel string) bool {
	switch rel {
	case "bin", "dist", "data", "benchmarks/tmp":
		return true
	}
	return strings.HasPrefix(rel+"/", "deploy/container/") && path.Base(rel) == "data"
}

// Walk calls visit with the slash path (relative to root) of every regular file
// that is not under a skipped or gitignored tree, in lexical order. Unlike a
// scan that skips every dot-directory it does include .github. A nested module
// (a directory with its own go.mod) is a different codebase and is skipped.
func Walk(root string, visit func(rel string) error) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == root {
				return nil
			}
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				return rerr
			}
			if skipDirName(d.Name()) || ignoredTree(filepath.ToSlash(rel)) {
				return filepath.SkipDir
			}
			if st, serr := os.Stat(filepath.Join(p, "go.mod")); serr == nil && !st.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		return visit(filepath.ToSlash(rel))
	})
}

// RepoFiles lists every scannable file of the checkout, sorted. It fails the
// test when it finds none, so a scan can never pass by looking at nothing.
func RepoFiles(t testing.TB) []string {
	t.Helper()
	var files []string
	if err := Walk(RepoRoot(t), func(rel string) error {
		files = append(files, rel)
		return nil
	}); err != nil {
		t.Fatalf("secutil: walk the repository: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("secutil: the repository walk found no files")
	}
	sort.Strings(files)
	return files
}

// Known records that a security test cannot pass because the control it checks
// is wrong or missing in code outside the M5-08 write scope. The test must keep
// its real assertions after the call: they are what proves the defect and what
// will start passing when it is fixed.
//
// Without ALIXAI_SEC_DEFECTS=fail the test is skipped, so the suite stays green
// while the defect is open; with it the assertions run and fail. The coverage
// tool (tests/security/cmd/coverage) finds every call, lists the id on each run
// and fails -strict while any remains, and a skipped test never counts as
// coverage. id is the SEC id, ref a short label (the backlog gap, `G6`, or the
// follow-up), why one sentence on what is wrong and which package needs the fix.
func Known(t testing.TB, id, ref, why string) {
	t.Helper()
	if os.Getenv("ALIXAI_SEC_DEFECTS") == "fail" {
		return
	}
	t.Skip(fmt.Sprintf("KNOWN DEFECT %s (%s): %s", id, ref, why))
}
