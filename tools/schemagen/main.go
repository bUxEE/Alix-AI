package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main without the process exit, so tests can drive it.
func run(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("schemagen", flag.ContinueOnError)
	fl.SetOutput(stderr)
	check := fl.Bool("check", false, "write nothing; exit 1 when the committed output differs from what the schemas generate")
	root := fl.String("root", "", "repository root (default: the nearest ancestor of the working directory holding go.mod and schemas/)")
	schemas := fl.String("schemas", "schemas", "schemas directory, relative to the root")
	goOut := fl.String("go-out", "pkg/contracts/gen", "Go output directory, relative to the root")
	tsOut := fl.String("ts-out", "sdk/typescript/src/gen", "TypeScript output directory, relative to the root")
	fl.Usage = func() {
		fmt.Fprintln(stderr, "usage: schemagen [-check] [-root dir] [-schemas dir] [-go-out dir] [-ts-out dir]")
		fmt.Fprintln(stderr, "  generates Go and TypeScript types from every schema area under the schemas directory;")
		fmt.Fprintln(stderr, "  with -check it only verifies that the committed output is current (exit 1 when stale)")
		fl.PrintDefaults()
	}
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() != 0 {
		fl.Usage()
		return 2
	}

	repo := *root
	if repo == "" {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "schemagen: %v\n", err)
			return 1
		}
		if repo = findRepoRoot(wd, *schemas); repo == "" {
			fmt.Fprintf(stderr, "schemagen: no go.mod with a %s/ directory found above %s (use -root)\n", *schemas, wd)
			return 1
		}
	}
	module, err := modulePath(filepath.Join(repo, "go.mod"))
	if err != nil {
		fmt.Fprintf(stderr, "schemagen: %v\n", err)
		return 1
	}
	cfg := Config{
		Schemas:    os.DirFS(filepath.Join(repo, filepath.FromSlash(*schemas))),
		SchemasRel: path.Clean(filepath.ToSlash(*schemas)),
		ModulePath: module,
		GoOut:      path.Clean(filepath.ToSlash(*goOut)),
		TSOut:      path.Clean(filepath.ToSlash(*tsOut)),
	}
	files, err := Generate(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "schemagen: %v\n", err)
		return 1
	}
	managed := []string{cfg.GoOut, cfg.TSOut}

	if *check {
		problems, err := diff(repo, files, managed)
		if err != nil {
			fmt.Fprintf(stderr, "schemagen: %v\n", err)
			return 1
		}
		if len(problems) > 0 {
			fmt.Fprintf(stderr, "schemagen: STALE - the committed output differs from the schemas (%d file(s)):\n", len(problems))
			for _, p := range problems {
				fmt.Fprintf(stderr, "  %s\n", p)
			}
			fmt.Fprintln(stderr, "run: go run ./tools/schemagen")
			return 1
		}
		fmt.Fprintf(stdout, "schemagen: ok - %d generated files are up to date\n", len(files))
		return 0
	}

	changed, err := write(repo, files, managed)
	if err != nil {
		fmt.Fprintf(stderr, "schemagen: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "schemagen: wrote %d files (%d changed)\n", len(files), changed)
	return 0
}

// findRepoRoot walks up from dir to the nearest directory with a go.mod and a
// schemas directory.
func findRepoRoot(dir, schemas string) string {
	for d := dir; ; d = filepath.Dir(d) {
		_, errMod := os.Stat(filepath.Join(d, "go.mod"))
		st, errSch := os.Stat(filepath.Join(d, filepath.FromSlash(schemas)))
		if errMod == nil && errSch == nil && st.IsDir() {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

// modulePath reads the module path from a go.mod file.
func modulePath(goMod string) (string, error) {
	data, err := os.ReadFile(goMod)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`), nil
		}
	}
	return "", fmt.Errorf("%s has no module line", goMod)
}

// isGenerated reports whether content starts with the generated-file marker.
func isGenerated(content []byte) bool {
	line, _, _ := bytes.Cut(content, []byte("\n"))
	return bytes.Contains(line, []byte(generatedMarker))
}

// existingGenerated lists the generated files (those carrying the marker on
// their first line) below the managed directories, as slash paths relative to
// the repository root. Hand-written files in those directories (tests, doc.go)
// have no marker and are never touched.
func existingGenerated(repo string, managed []string) ([]string, error) {
	var out []string
	for _, dir := range managed {
		base := filepath.Join(repo, filepath.FromSlash(dir))
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				if d.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			head := make([]byte, 256)
			n, _ := io.ReadFull(f, head)
			f.Close()
			if isGenerated(head[:n]) {
				rel, err := filepath.Rel(repo, p)
				if err != nil {
					return err
				}
				out = append(out, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(out)
	return out, nil
}

// diff compares the generated files with what is on disk. A problem is a file
// that is missing, differs, or is a generated file nothing generates any more.
func diff(repo string, files map[string][]byte, managed []string) ([]string, error) {
	var problems []string
	paths := sortedPaths(files)
	for _, p := range paths {
		have, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(p)))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			problems = append(problems, p+": missing")
		case err != nil:
			return nil, err
		case !bytes.Equal(have, files[p]):
			problems = append(problems, p+": differs from the generated content")
		}
	}
	existing, err := existingGenerated(repo, managed)
	if err != nil {
		return nil, err
	}
	for _, p := range existing {
		if _, ok := files[p]; !ok {
			problems = append(problems, p+": generated file that no schema produces any more")
		}
	}
	sort.Strings(problems)
	return problems, nil
}

// write brings the managed directories in line with files: it writes new and
// changed files, removes generated files nothing produces any more (and the
// directories that leaves empty) and returns how many files it changed.
func write(repo string, files map[string][]byte, managed []string) (int, error) {
	changed := 0
	for _, p := range sortedPaths(files) {
		full := filepath.Join(repo, filepath.FromSlash(p))
		if have, err := os.ReadFile(full); err == nil && bytes.Equal(have, files[p]) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return changed, err
		}
		if err := os.WriteFile(full, files[p], 0o644); err != nil {
			return changed, err
		}
		changed++
	}
	existing, err := existingGenerated(repo, managed)
	if err != nil {
		return changed, err
	}
	for _, p := range existing {
		if _, ok := files[p]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(repo, filepath.FromSlash(p))); err != nil {
			return changed, err
		}
		changed++
	}
	for _, dir := range managed {
		removeEmptyDirs(filepath.Join(repo, filepath.FromSlash(dir)), true)
	}
	return changed, nil
}

// removeEmptyDirs deletes empty directories below dir (and dir itself unless
// keep is set).
func removeEmptyDirs(dir string, keep bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			removeEmptyDirs(filepath.Join(dir, e.Name()), false)
		}
	}
	if keep {
		return
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
		os.Remove(dir)
	}
}

func sortedPaths(files map[string][]byte) []string {
	out := make([]string, 0, len(files))
	for p := range files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
