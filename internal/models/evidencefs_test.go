package models

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// repoEvidenceFS is the repository root as the evidence check of the real registry
// sees it. Where the checkout carries the raw benchmark results (benchmarks/results),
// every cited path must exist. The published source tree carries only the summary
// reports of docs/evidence: a cited raw file (benchmarks/results/..., or a file under
// docs/evidence that is not a .md report) is then accepted as cited, with a log line,
// while every other path, the efficiency reviews and the reports included, must still exist.
func repoEvidenceFS(t testing.TB) fs.FS {
	t.Helper()
	root := os.DirFS(repoRoot)
	if _, err := os.Stat(filepath.Join(repoRoot, "benchmarks", "results")); err == nil {
		return root
	}
	return publishedTreeFS{FS: root, t: t}
}

type publishedTreeFS struct {
	fs.FS
	t testing.TB
}

// rawEvidence reports whether name is raw evidence the published tree leaves out.
func rawEvidence(name string) bool {
	switch {
	case name == "benchmarks/results" || strings.HasPrefix(name, "benchmarks/results/"):
		return true
	case strings.HasPrefix(name, "docs/evidence/"):
		return path.Ext(name) != ".md" && path.Ext(name) != ""
	}
	return false
}

func (p publishedTreeFS) Stat(name string) (fs.FileInfo, error) {
	fi, err := fs.Stat(p.FS, name)
	if err == nil || !rawEvidence(name) {
		return fi, err
	}
	p.t.Logf("%s is raw evidence this checkout does not carry: accepted as cited", name)
	return rawInfo(path.Base(name)), nil
}

type rawInfo string

func (r rawInfo) Name() string       { return string(r) }
func (r rawInfo) Size() int64        { return 0 }
func (r rawInfo) Mode() fs.FileMode  { return 0o444 }
func (r rawInfo) ModTime() time.Time { return time.Time{} }
func (r rawInfo) IsDir() bool        { return false }
func (r rawInfo) Sys() any           { return nil }
