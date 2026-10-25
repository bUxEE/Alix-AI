package decision

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

// repoRoot is the module root: the gate reads schemas, documents and the registry from it.
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
		t.Fatal(err)
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

// runGo runs the go command in the module root (pure Go, like every build of this repository) and returns its
// stdout.
func runGo(t testing.TB, timeout time.Duration, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, goTool(), args...)
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go %s: %v\n%s%s", strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// goListDeps returns the import paths of the packages and all their dependencies, under an optional build tag.
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

// goListImports returns the packages' own imports (not their dependencies).
func goListImports(t testing.TB, pkg string) []string {
	t.Helper()
	var out []string
	for _, l := range strings.Split(runGo(t, 5*time.Minute, "list", "-f", `{{range .Imports}}{{.}} {{end}}`, pkg), " ") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// goBuild compiles pkg to a throwaway directory.
func goBuild(t testing.TB, tag, pkg string) {
	t.Helper()
	dir := t.TempDir()
	name := "out"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	args := []string{"build"}
	if tag != "" {
		args = append(args, "-tags", tag)
	}
	args = append(args, "-o", filepath.Join(dir, name), pkg)
	runGo(t, 10*time.Minute, args...)
}

// externalModePackages are the packages of milestone X (external provider mode): no package outside that
// milestone imports them except the composition roots.
var externalModePackages = []string{
	"internal/dataegress", "internal/providerreg", "internal/credentials", "internal/oauth",
	"internal/providerbudget", "internal/modes", "internal/extmodal", "adapters/external",
}

func isExternal(importPath string) bool {
	for _, p := range externalModePackages {
		if strings.HasSuffix(importPath, "/"+p) || strings.Contains(importPath, "/"+p+"/") {
			return true
		}
	}
	return false
}

func TestDecisionPackagesDoNotImportExternalProviderPackages(t *testing.T) {
	// The decision layer stays local: decision-scoring requests are never routed to an external provider
	// (docs/architecture/decision-layer.md section 8). Nothing the scorer path or its adapters
	// import, directly or transitively, may be an external-mode package.
	decisionPackages := []string{
		"./internal/decision/...", "./internal/dialogue/decision/...", "./adapters/decision/...",
		"./tools/decbench/...",
	}
	for _, pkg := range decisionPackages {
		t.Run(pkg, func(t *testing.T) {
			deps := goListDeps(t, "", pkg)
			if len(deps) < 20 {
				t.Fatalf("go list returned %d packages for %s: the detector sees nothing", len(deps), pkg)
			}
			for _, d := range deps {
				if isExternal(d) {
					t.Errorf("%s depends on the external-provider package %s", pkg, d)
				}
			}
		})
	}

	t.Run("control: the detector sees an external-mode package where one is wired", func(t *testing.T) {
		deps := goListDeps(t, "", "./internal/cli")
		found := false
		for _, d := range deps {
			if isExternal(d) {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("internal/cli, the composition root that registers the external providers, shows none: the detector is blind")
		}
	})
}
