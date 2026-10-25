package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// SEC-090: two clean builds of a shipped binary produce identical bytes. The binary is the loopback forwarder of
// the container stack (deploy/container/alixai/netctl), built from the real module with the real flags; each
// build gets a source copy in its own directory and a Go build cache that starts empty, offline (GOPROXY=off).
func TestReproducibleBuild(t *testing.T) {
	tc := testToolchain(t)
	spec := buildSpec{
		Root:    repoRoot(t),
		Package: "./deploy/container/alixai/netctl",
		Include: []string{"deploy/container/alixai/netctl"},
		Stamps: map[string]string{
			"github.com/buxee/alix-ai/internal/version.Version": "0.0.0-repro",
		},
	}
	rep, err := reproducibleCheck(context.Background(), tc, spec, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Reproducible() {
		t.Fatalf("two clean builds differ: %s vs %s, first difference at byte %d", rep.A.SHA256, rep.B.SHA256, rep.FirstDiff)
	}
	if rep.A.Size == 0 || !sha256HexRE.MatchString(rep.A.SHA256) {
		t.Errorf("the report carries no real hash: %+v", rep.A)
	}
	if rep.A.Path == rep.B.Path || filepath.Dir(rep.A.Path) == filepath.Dir(rep.B.Path) {
		t.Errorf("both builds wrote to the same place: %s, %s", rep.A.Path, rep.B.Path)
	}
	if rep.FirstDiff != -1 {
		t.Errorf("FirstDiff = %d for identical builds", rep.FirstDiff)
	}
}

// The comparison must be able to fail: a build that leaks its own directory into the binary (no -trimpath) is
// reported as not reproducible, with the offset of the first differing byte.
func TestReproducibleCheckReportsDifferingBuilds(t *testing.T) {
	tc := testToolchain(t)
	spec := buildSpec{Root: writeFixtureModule(t), Package: "./cmd/hi", unsafeNoTrimpath: true, SharedCache: filepath.Join(t.TempDir(), "gocache")}
	rep, err := reproducibleCheck(context.Background(), tc, spec, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reproducible() {
		t.Fatal("builds in different directories without -trimpath came out identical; the check proves nothing")
	}
	if rep.FirstDiff < 0 {
		t.Errorf("no first-difference offset for differing builds: %d", rep.FirstDiff)
	}
}

func TestReproducibleCheckCommand(t *testing.T) {
	root := writeFixtureModule(t)
	keep := filepath.Join(t.TempDir(), "keep")
	code, out, errOut := runTool(t, "reproducible-check", "-C", root, "-package", "./cmd/hi", "-shared-cache", "-keep", keep, "-version", "1.2.3", "-commit", "abc123")
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.HasPrefix(out, "reproducible ") || !strings.Contains(out, "sha256:") {
		t.Errorf("unexpected report: %q", out)
	}
	if _, err := os.Stat(filepath.Join(keep, runtime.GOOS+"_"+runtime.GOARCH, "a", "out")); err != nil {
		t.Errorf("-keep did not keep the build directories: %v", err)
	}

	if code, _, _ := runTool(t, "reproducible-check", "-C", root, "-package", "./cmd/hi", "-target", "linux"); code != 2 {
		t.Errorf("a malformed -target exits %d, want 2", code)
	}
	if code, _, _ := runTool(t, "reproducible-check", "-C", root, "-package", "./cmd/missing", "-shared-cache"); code != 1 {
		t.Errorf("a package that does not build exits %d, want 1", code)
	}
}

func TestBuildStampsChangeTheBinary(t *testing.T) {
	tc := testToolchain(t)
	root := writeFixtureModule(t)
	cache := filepath.Join(t.TempDir(), "gocache")
	build := func(stamp string) string {
		spec := buildSpec{Root: root, Package: "./cmd/hi", SharedCache: cache, Stamps: map[string]string{"main.stamp": stamp}}
		b, err := tc.build(context.Background(), spec, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return b.SHA256
	}
	if a, b := build("one"), build("two"); a == b {
		t.Error("different stamps gave the same binary")
	}
	if build("one") != build("one") {
		t.Error("the same stamp gave different binaries")
	}
}

func TestBuildRefusesUnsafeSettings(t *testing.T) {
	tc := testToolchain(t)
	root := writeFixtureModule(t)
	for name, spec := range map[string]buildSpec{
		"stamp value with a space":  {Root: root, Package: "./cmd/hi", Stamps: map[string]string{"main.stamp": "a b"}},
		"stamp key that is no path": {Root: root, Package: "./cmd/hi", Stamps: map[string]string{"x y": "v"}},
		"tag with a comma":          {Root: root, Package: "./cmd/hi", Tags: []string{"a,b"}},
		"package pattern":           {Root: root, Package: "./..."},
		"include that climbs":       {Root: root, Package: "./cmd/hi", Include: []string{"../x"}},
		"include with a backslash":  {Root: root, Package: "./cmd/hi", Include: []string{`cmd\hi`}},
	} {
		if _, err := tc.build(context.Background(), spec, t.TempDir()); err == nil {
			t.Errorf("%s: the build was not refused", name)
		}
	}
}

func TestCopyModuleTreeLeavesOutWhatIsNotBuildInput(t *testing.T) {
	src := t.TempDir()
	for name, body := range map[string]string{
		"go.mod":                    "module m\n",
		"go.sum":                    "",
		"a/a.go":                    "package a\n",
		".git/config":               "x",
		".worktrees/w/go.mod":       "x",
		"node_modules/n/index.js":   "x",
		"dist/out.bin":              "x",
		"a/node_modules/n/index.js": "x",
	} {
		p := filepath.Join(src, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(src, "a", "a.go"), filepath.Join(src, "a", "link.go")); err != nil {
		t.Logf("symbolic links are not available here (%v); that case is not exercised", err)
	}
	dst := t.TempDir()
	if err := copyModuleTree(src, dst, nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"go.mod", "go.sum", "a/a.go"} {
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(want))); err != nil {
			t.Errorf("%s was not copied: %v", want, err)
		}
	}
	for _, not := range []string{".git", ".worktrees", "node_modules", "dist", "a/node_modules", "a/link.go"} {
		if _, err := os.Lstat(filepath.Join(dst, filepath.FromSlash(not))); err == nil {
			t.Errorf("%s was copied", not)
		}
	}
}
