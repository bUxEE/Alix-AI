package apppkg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// Two directories whose names differ only by letter case are one directory on a
// case-insensitive file system, so a package holding Docs/a.md and docs/b.md is
// refused when it is read for an install. A package already installed on a
// case-sensitive file system is read back as it was written.
func TestSnapshotFoldsDirectoryCase(t *testing.T) {
	fsys := fstest.MapFS{
		"alixai.yaml": {Data: []byte("x")},
		"Docs/a.md":   {Data: []byte("a")},
		"docs/b.md":   {Data: []byte("b")},
	}
	lim := DefaultLimits()
	snap, probs, err := snapshotFS(context.Background(), fsys, lim, true)
	if err != nil {
		t.Fatal(err)
	}
	if snap != nil || len(probs) != 1 || probs[0].check != chkUnsafeEntry || !strings.Contains(probs[0].msg, "letter case") {
		t.Fatalf("snapshot = %v, problems = %+v, want one letter-case problem", snap != nil, probs)
	}
	snap, probs, err = snapshotFS(context.Background(), fsys, lim, false)
	if err != nil || snap == nil || len(probs) != 0 {
		t.Fatalf("an installed package: snapshot = %v, problems = %+v, %v", snap != nil, probs, err)
	}
}

// An installed package directory is read back without directory case folding,
// whoever reads it (Installer.Open, the integration audit); a package directory
// handed in for an install is folded.
func TestInstalledDirIsNotFoldedByDirectory(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"Docs/a.md", "docs/b.md"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(p)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Skip("the file system folds letter case: Docs and docs are one directory here")
	}
	lim := DefaultLimits()
	if snap, probs, err := FromDir(dir).snapshot(context.Background(), lim); err != nil || snap != nil || len(probs) != 1 {
		t.Fatalf("FromDir: snapshot = %v, problems = %+v, %v; want one letter-case problem", snap != nil, probs, err)
	}
	if snap, probs, err := FromInstalledDir(dir).snapshot(context.Background(), lim); err != nil || snap == nil || len(probs) != 0 {
		t.Fatalf("FromInstalledDir: snapshot = %v, problems = %+v, %v", snap != nil, probs, err)
	}
}

// Paging through a directory one entry at a time builds each entry once.
func TestSnapshotReadDirPagesInLinearTime(t *testing.T) {
	const n = 2000
	b := newBuilder(DefaultLimits())
	for i := 0; i < n; i++ {
		b.addFile(fmt.Sprintf("docs/f%04d.md", i), []byte("x"))
	}
	snap, probs := b.finish()
	if snap == nil {
		t.Fatalf("problems: %+v", probs)
	}
	seen := 0
	allocs := testing.AllocsPerRun(1, func() {
		f, err := snap.Open("docs")
		if err != nil {
			t.Fatal(err)
		}
		d := f.(fs.ReadDirFile)
		seen = 0
		for {
			es, err := d.ReadDir(1)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil || len(es) != 1 || es[0].Name() != fmt.Sprintf("f%04d.md", seen) {
				t.Fatalf("entry %d: %v, %v", seen, es, err)
			}
			seen++
		}
	})
	if seen != n {
		t.Fatalf("paged through %d entries, want %d", seen, n)
	}
	if allocs > 10*n {
		t.Errorf("paging through %d entries allocated %.0f times: every page rebuilds the rest of the directory", n, allocs)
	}
}
