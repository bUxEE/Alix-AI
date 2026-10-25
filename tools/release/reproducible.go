package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// reproReport is the outcome of building one package for one target twice.
type reproReport struct {
	Package      string
	GOOS, GOARCH string
	GoVersion    string
	A, B         builtBinary
	// FirstDiff is the offset of the first byte that differs, or -1 when the files are identical.
	FirstDiff int64
}

// Reproducible reports whether both builds produced the same bytes.
func (r reproReport) Reproducible() bool { return r.A.SHA256 == r.B.SHA256 && r.A.Size == r.B.Size }

// reproducibleCheck builds spec twice, each time from a fresh copy of the source in its own directory under base
// and with a cache of its own (unless spec.SharedCache is set), and compares the two binaries. A build that fails
// is an error; two builds that differ are a report with Reproducible() false.
func reproducibleCheck(ctx context.Context, tc toolchain, spec buildSpec, base string) (reproReport, error) {
	rep := reproReport{Package: spec.Package, GOOS: spec.GOOS, GOARCH: spec.GOARCH, GoVersion: tc.goVersion, FirstDiff: -1}
	if rep.GOOS == "" {
		rep.GOOS = runtime.GOOS
	}
	if rep.GOARCH == "" {
		rep.GOARCH = runtime.GOARCH
	}
	var err error
	if rep.A, err = tc.build(ctx, spec, filepath.Join(base, "a")); err != nil {
		return rep, fmt.Errorf("first build: %w", err)
	}
	if rep.B, err = tc.build(ctx, spec, filepath.Join(base, "b")); err != nil {
		return rep, fmt.Errorf("second build: %w", err)
	}
	if !rep.Reproducible() {
		rep.FirstDiff, err = firstDiff(rep.A.Path, rep.B.Path)
		if err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// firstDiff returns the offset of the first byte at which two files differ (the shorter file's length when one is
// a prefix of the other).
func firstDiff(a, b string) (int64, error) {
	fa, err := os.Open(a)
	if err != nil {
		return -1, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return -1, err
	}
	defer fb.Close()
	const chunk = 64 << 10
	ba, bb := make([]byte, chunk), make([]byte, chunk)
	var off int64
	for {
		na, ea := io.ReadFull(fa, ba)
		nb, eb := io.ReadFull(fb, bb)
		n := min(na, nb)
		for i := 0; i < n; i++ {
			if ba[i] != bb[i] {
				return off + int64(i), nil
			}
		}
		if na != nb {
			return off + int64(n), nil
		}
		if ea != nil || eb != nil { // both ended together (equal lengths were checked above)
			return -1, nil
		}
		off += int64(n)
	}
}

// parseTarget splits "linux/amd64" into its two parts.
func parseTarget(t string) (goos, goarch string, err error) {
	goos, goarch, ok := strings.Cut(t, "/")
	if !ok || !tagRE.MatchString(goos) || !tagRE.MatchString(goarch) {
		return "", "", usagef("target %q is not os/arch, for example linux/amd64", t)
	}
	return goos, goarch, nil
}

func cmdReproducibleCheck(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("reproducible-check", stderr)
	root := fs.String("C", "", "module root (default: the directory with go.mod above the working directory)")
	pkg := fs.String("package", "./cmd/alixai", "main package to build, relative to the module root")
	var targets, include multiFlag
	fs.Var(&targets, "target", "os/arch to build (repeatable; default: the host)")
	fs.Var(&include, "include", "limit the source copy to this path of the module, besides go.mod and go.sum (repeatable; default: the whole module)")
	tags := fs.String("tags", "", "comma-separated build tags, for example noexternal")
	version := fs.String("version", "", "stamp internal/version.Version, as a release build does")
	commit := fs.String("commit", "", "stamp internal/version.Commit")
	date := fs.String("date", "", "stamp internal/version.Date")
	shared := fs.Bool("shared-cache", false, "use one Go build cache for both builds (faster, and a weaker check: compilation is not repeated)")
	keep := fs.String("keep", "", "keep both build directories under this path instead of a temporary directory")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return usagef("unexpected argument %q", fs.Arg(0))
	}
	modRoot, err := moduleRootFlag(*root)
	if err != nil {
		return err
	}
	tc, err := detectToolchain(ctx)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		targets = multiFlag{runtime.GOOS + "/" + runtime.GOARCH}
	}
	stamps := map[string]string{}
	for k, v := range map[string]string{"Version": *version, "Commit": *commit, "Date": *date} {
		if v != "" {
			stamps["github.com/buxee/alix-ai/internal/version."+k] = v
		}
	}
	var tagList []string
	if *tags != "" {
		tagList = strings.Split(*tags, ",")
	}

	base := *keep
	if base == "" {
		if base, err = os.MkdirTemp("", "alixai-repro-*"); err != nil {
			return err
		}
		defer os.RemoveAll(base)
	} else if err := os.MkdirAll(base, 0o700); err != nil {
		return err
	}
	sharedCache := ""
	if *shared {
		sharedCache = filepath.Join(base, "shared-cache")
	}

	failed := 0
	for _, t := range targets {
		goos, goarch, err := parseTarget(t)
		if err != nil {
			return err
		}
		spec := buildSpec{Root: modRoot, Package: *pkg, Include: include, GOOS: goos, GOARCH: goarch, Tags: tagList, Stamps: stamps, SharedCache: sharedCache}
		rep, err := reproducibleCheck(ctx, tc, spec, filepath.Join(base, goos+"_"+goarch))
		if err != nil {
			return err
		}
		if rep.Reproducible() {
			fmt.Fprintf(stdout, "reproducible      %s %s/%s  sha256:%s  %d bytes  (%s)\n", rep.Package, rep.GOOS, rep.GOARCH, rep.A.SHA256, rep.A.Size, rep.GoVersion)
			continue
		}
		failed++
		fmt.Fprintf(stdout, "NOT REPRODUCIBLE  %s %s/%s  (%s)\n  build A  sha256:%s  %d bytes\n  build B  sha256:%s  %d bytes\n  first differing byte: %d\n",
			rep.Package, rep.GOOS, rep.GOARCH, rep.GoVersion, rep.A.SHA256, rep.A.Size, rep.B.SHA256, rep.B.Size, rep.FirstDiff)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d targets are not reproducible", failed, len(targets))
	}
	return nil
}
