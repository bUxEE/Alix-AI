package external

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const modulePath = "github.com/buxee/alix-ai/"

// externalPackages are the packages of external provider mode. Only the composition roots (cmd, internal/cli, tools) may
// import them, and on the serve path only internal/cli/providers_external.go, behind !noexternal. The build that
// carries the noexternal tag holds none of them.
var externalPackages = []string{
	"adapters/external", "internal/credentials", "internal/oauth", "internal/dataegress", "internal/providerreg",
	"internal/providerbudget", "internal/modes", "internal/extmodal", "internal/dialogue/extmode",
}

// isExternalPkg reports whether the import path is one of the packages above or below one of them.
func isExternalPkg(importPath string) bool {
	rest, ok := strings.CutPrefix(importPath, modulePath)
	if !ok {
		return false
	}
	for _, p := range externalPackages {
		if rest == p || strings.HasPrefix(rest, p+"/") {
			return true
		}
	}
	return false
}

// ---- the checkout ----------------------------------------------------------------------------------------

// repoRoot is the checkout this test belongs to: the nearest parent holding go.mod (a main checkout and a
// .worktrees/<task> copy each resolve to their own root).
func repoRoot(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			t.Fatalf("no go.mod above %s", dir)
		}
	}
}

func readRepoFile(t testing.TB, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return string(b)
}

func goTool() string {
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	name := "go"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(runtime.GOROOT(), "bin", name)
}

// runGo runs the go command in the module root, pure Go like every build of this repository, and returns its
// standard output. A failure fails the test with everything the command printed.
func runGo(t testing.TB, timeout time.Duration, args ...string) string {
	t.Helper()
	out, errOut, err := runGoRaw(t, timeout, args...)
	if err != nil {
		t.Fatalf("go %s: %v\n%s%s", strings.Join(args, " "), err, out, errOut)
	}
	return out
}

func runGoRaw(t testing.TB, timeout time.Duration, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, goTool(), args...)
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err = cmd.Run()
	return so.String(), se.String(), err
}

// goListDeps lists the import paths of the packages and all their dependencies under an optional build tag.
func goListDeps(t testing.TB, tag string, pkgs ...string) []string {
	t.Helper()
	args := []string{"list"}
	if tag != "" {
		args = append(args, "-tags", tag)
	}
	args = append(args, "-deps", "-f", "{{.ImportPath}}")
	args = append(args, pkgs...)
	var out []string
	for _, l := range strings.Split(runGo(t, 5*time.Minute, args...), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
