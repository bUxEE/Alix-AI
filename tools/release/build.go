package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/internal/release"
)

// toolchain is the Go toolchain a build runs with, and the two settings of the operator's Go environment that a
// hermetic build still needs: where the module cache is.
type toolchain struct {
	goBin     string
	goVersion string
	modCache  string
	goPath    string
}

// goBinary finds the go command: on PATH first, then in the GOROOT this tool was built with.
func goBinary() (string, error) {
	if p, err := exec.LookPath("go"); err == nil {
		return p, nil
	}
	name := "go"
	if runtime.GOOS == "windows" {
		name = "go.exe"
	}
	if root := runtime.GOROOT(); root != "" {
		p := filepath.Join(root, "bin", name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p, nil
		}
	}
	return "", errNoGo
}

// detectToolchain asks the go command where the module cache is. It runs in an empty directory with
// GOTOOLCHAIN=local, so it neither reads a go.mod nor downloads a toolchain.
func detectToolchain(ctx context.Context) (toolchain, error) {
	bin, err := goBinary()
	if err != nil {
		return toolchain{}, err
	}
	dir, err := os.MkdirTemp("", "alixai-release-go-*")
	if err != nil {
		return toolchain{}, err
	}
	defer os.RemoveAll(dir)
	cmd := exec.CommandContext(ctx, bin, "env", "-json", "GOMODCACHE", "GOPATH", "GOVERSION")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return toolchain{}, fmt.Errorf("go env: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var env struct{ GOMODCACHE, GOPATH, GOVERSION string }
	if err := json.Unmarshal(out, &env); err != nil {
		return toolchain{}, fmt.Errorf("go env: unreadable output: %w", err)
	}
	if env.GOMODCACHE == "" || env.GOVERSION == "" {
		return toolchain{}, fmt.Errorf("go env reported no module cache or version")
	}
	return toolchain{goBin: bin, goVersion: env.GOVERSION, modCache: env.GOMODCACHE, goPath: env.GOPATH}, nil
}

// buildSpec says what to build.
type buildSpec struct {
	// Root is the module root (the directory with go.mod) the source is copied from.
	Root string
	// Package is the main package to build, relative to Root: "./cmd/alixai".
	Package string
	// Include limits the copy to these slash paths of Root (go.mod and go.sum are always copied). Empty means the
	// whole module.
	Include []string
	// GOOS and GOARCH default to the host.
	GOOS, GOARCH string
	Tags         []string
	// Stamps are linker -X settings: import path of a string variable to its value.
	Stamps map[string]string
	// SharedCache, when set, is the GOCACHE of the build. Empty means a cache that starts empty.
	SharedCache string

	// unsafeNoTrimpath builds without -trimpath. Tests only: it proves the comparison notices a build that leaks
	// its directory.
	unsafeNoTrimpath bool
}

// builtBinary is the product of one build.
type builtBinary struct {
	Path   string
	SHA256 string
	Size   int64
}

var (
	stampKeyRE   = regexp.MustCompile(`^[A-Za-z0-9_./-]+\.[A-Za-z_][A-Za-z0-9_]*$`)
	stampValueRE = regexp.MustCompile(`^[A-Za-z0-9._+:/@-]*$`)
	tagRE        = regexp.MustCompile(`^[A-Za-z0-9_.]+$`)
)

// ldflags is "-s -w" plus the stamps in sorted order, so the same spec always gives the same command line.
func (s buildSpec) ldflags() (string, error) {
	flags := []string{"-s", "-w"}
	keys := make([]string, 0, len(s.Stamps))
	for k := range s.Stamps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := s.Stamps[k]
		if !stampKeyRE.MatchString(k) || !stampValueRE.MatchString(v) {
			return "", fmt.Errorf("stamp %q=%q is outside the allowed grammar", k, v)
		}
		flags = append(flags, "-X", k+"="+v)
	}
	return strings.Join(flags, " "), nil
}

// buildEnv is the environment of a hermetic build: the operator's PATH, home and temp variables, with every
// Go-related variable dropped and replaced. GOPROXY=off makes "works offline" something the build enforces;
// GOENV=off ignores `go env -w` settings; GOTOOLCHAIN=local forbids a toolchain download.
func (tc toolchain) buildEnv(goos, goarch, cache, tmp string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		up := strings.ToUpper(name)
		if strings.HasPrefix(up, "GO") || strings.HasPrefix(up, "CGO_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"CGO_ENABLED=0",
		"GOOS="+goos,
		"GOARCH="+goarch,
		"GOFLAGS=-mod=readonly",
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOTOOLCHAIN=local",
		"GOWORK=off",
		"GOENV=off",
		"GOCACHE="+cache,
		"GOTMPDIR="+tmp,
		"GOMODCACHE="+tc.modCache,
		"GOPATH="+tc.goPath,
	)
}

// build copies the source into work/src and builds the package there with a cache of its own. Everything the
// build writes is under work.
func (tc toolchain) build(ctx context.Context, spec buildSpec, work string) (builtBinary, error) {
	if spec.Root == "" || spec.Package == "" {
		return builtBinary{}, fmt.Errorf("build needs a module root and a package")
	}
	pkgName := path.Base(filepath.ToSlash(spec.Package))
	if pkgName == "." || pkgName == "..." || pkgName == "/" || strings.ContainsAny(pkgName, `\*`) {
		return builtBinary{}, fmt.Errorf("package %q does not name one main package directory", spec.Package)
	}
	for _, tag := range spec.Tags {
		if !tagRE.MatchString(tag) {
			return builtBinary{}, fmt.Errorf("build tag %q is outside the allowed grammar", tag)
		}
	}
	ldflags, err := spec.ldflags()
	if err != nil {
		return builtBinary{}, err
	}
	goos, goarch := spec.GOOS, spec.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	src, cache, tmp, out := filepath.Join(work, "src"), filepath.Join(work, "cache"), filepath.Join(work, "tmp"), filepath.Join(work, "out")
	if spec.SharedCache != "" {
		cache = spec.SharedCache
	}
	for _, d := range []string{src, cache, tmp, out} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return builtBinary{}, err
		}
	}
	if err := copyModuleTree(spec.Root, src, spec.Include); err != nil {
		return builtBinary{}, fmt.Errorf("copy source: %w", err)
	}

	binName := pkgName
	if goos == "windows" {
		binName += ".exe"
	}
	outPath := filepath.Join(out, binName)
	args := []string{"build", "-buildvcs=false"}
	if !spec.unsafeNoTrimpath {
		args = append(args, "-trimpath")
	}
	if len(spec.Tags) > 0 {
		args = append(args, "-tags", strings.Join(spec.Tags, ","))
	}
	args = append(args, "-ldflags", ldflags, "-o", outPath, spec.Package)

	cmd := exec.CommandContext(ctx, tc.goBin, args...)
	cmd.Dir = src
	cmd.Env = tc.buildEnv(goos, goarch, cache, tmp)
	if combined, err := cmd.CombinedOutput(); err != nil {
		return builtBinary{}, fmt.Errorf("go build %s (%s/%s): %w\n%s", spec.Package, goos, goarch, err, tail(string(combined), 20))
	}
	sum, size, err := release.HashFile(outPath)
	if err != nil {
		return builtBinary{}, err
	}
	return builtBinary{Path: outPath, SHA256: sum, Size: size}, nil
}

// tail returns the last n lines of s.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// copyModuleTree copies the source of the module at root into dst. It never follows a symbolic link or copies any
// other non-regular entry, and it leaves out dot-directories (.git, .worktrees), node_modules and the root-level
// bin, dist and data directories: none of them is build input. include, when not empty, limits the copy to those
// slash paths; go.mod and go.sum are always copied.
func copyModuleTree(root, dst string, include []string) error {
	paths := []string{"go.mod", "go.sum"}
	if len(include) == 0 {
		paths = []string{"."}
	}
	for _, p := range include {
		if !fs.ValidPath(p) || p == "." || strings.ContainsAny(p, `\:`) {
			return fmt.Errorf("include path %q is not a clean relative slash path", p)
		}
		paths = append(paths, p)
	}
	for _, p := range paths {
		start := filepath.Join(root, filepath.FromSlash(p))
		if _, err := os.Lstat(start); err != nil {
			if os.IsNotExist(err) && p == "go.sum" {
				continue // a module with no dependencies has no go.sum
			}
			return err
		}
		err := filepath.WalkDir(start, func(cur string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, cur)
			if err != nil {
				return err
			}
			target := filepath.Join(dst, rel)
			switch {
			case d.IsDir():
				if rel != "." && skipDir(filepath.ToSlash(rel), d.Name()) {
					return filepath.SkipDir
				}
				return os.MkdirAll(target, 0o700)
			case d.Type().IsRegular():
				if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
					return err
				}
				return copyFile(cur, target)
			default:
				return nil // symbolic links and special files are never followed
			}
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func skipDir(rel, name string) bool {
	if strings.HasPrefix(name, ".") || name == "node_modules" {
		return true
	}
	switch rel {
	case "bin", "dist", "data":
		return true
	}
	return false
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
