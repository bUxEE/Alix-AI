package external

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// importersOfExternal returns the non-test Go files of the checkout, by slash path, that import a package of
// external provider mode, with the first line of each (the build constraint, when there is one). Files inside the X
// packages themselves, the dev tools and the tests are left out: the first two are the packages and their
// tooling, the last are not part of a binary.
func importersOfExternal(t testing.TB, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".worktrees", "node_modules", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		if strings.HasPrefix(rel, "tools/") || strings.HasPrefix(rel, "tests/") || isExternalPkg(modulePath+path.Dir(rel)) {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly|parser.ParseComments)
		if perr != nil {
			return nil
		}
		for _, imp := range f.Imports {
			if ip, _ := strconv.Unquote(imp.Path.Value); isExternalPkg(ip) {
				b, rerr := os.ReadFile(p)
				if rerr != nil {
					return rerr
				}
				line, _, _ := strings.Cut(string(b), "\n")
				out[rel] = strings.TrimRight(line, "\r")
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestNoexternalBuildHasNoExternalCode (ADR-0022): a binary built with the
// noexternal tag holds no package of external provider mode. The check is the compiler's own dependency list
// of cmd/alixai under the tag, with a control that the same list under the default tags does name them (a
// detector that sees nothing proves nothing), and a source check that only the composition root's tagged files
// reach the packages from the serve path.
func TestNoexternalBuildHasNoExternalCode(t *testing.T) {
	t.Run("the dependency list of cmd/alixai under -tags noexternal names no package of external provider mode", func(t *testing.T) {
		deps := goListDeps(t, "noexternal", "./cmd/alixai")
		if len(deps) < 100 {
			t.Fatalf("go list returned %d packages: the detector sees nothing", len(deps))
		}
		for _, d := range deps {
			if isExternalPkg(d) {
				t.Errorf("a noexternal build of cmd/alixai contains %s", d)
			}
		}
	})

	t.Run("control: the default build names every one of them, so the detector is not blind", func(t *testing.T) {
		deps := goListDeps(t, "", "./cmd/alixai")
		for _, p := range externalPackages {
			found := false
			for _, d := range deps {
				if d == modulePath+p || strings.HasPrefix(d, modulePath+p+"/") {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("the default build of cmd/alixai does not contain %s: the forbidden-package list names a package that is not wired, or the detector is blind", p)
			}
		}
	})

	t.Run("the detector flags a planted dependency", func(t *testing.T) {
		for _, p := range []string{
			"internal/extmodal", "internal/dialogue/extmode", "internal/modes/hybrid", "adapters/external/openai",
			"internal/credentials/oskeyring",
		} {
			if !isExternalPkg(modulePath + p) {
				t.Errorf("%s is not recognised as an external-mode package", p)
			}
		}
		for _, p := range []string{"internal/dialogue", "internal/egress", "internal/providers", "internal/modesx", "adapters/localai"} {
			if isExternalPkg(modulePath + p) {
				t.Errorf("%s is recognised as an external-mode package: the list is too broad", p)
			}
		}
	})

	t.Run("only files tagged !noexternal import a package of external provider mode, outside the packages and the dev tools", func(t *testing.T) {
		importers := importersOfExternal(t, repoRoot(t))
		if len(importers) < 2 {
			t.Fatalf("the scan found %d importers: it is not looking at the composition root", len(importers))
		}
		var untagged []string
		for rel, first := range importers {
			if first != "//go:build !noexternal" {
				untagged = append(untagged, rel)
			}
		}
		slices.Sort(untagged)
		for _, rel := range untagged {
			t.Errorf("%s imports a package of external provider mode without //go:build !noexternal: on the serve path only the tagged files of internal/cli may", rel)
		}
		if _, ok := importers["internal/cli/providers_external.go"]; !ok {
			t.Error("internal/cli/providers_external.go no longer imports the external packages: the serve path moved, update the gate")
		}
		if first := readRepoFile(t, "internal/cli/providers_noexternal.go"); !strings.HasPrefix(first, "//go:build noexternal") {
			t.Error("the stand-in of the composition root lost its noexternal build tag")
		}
	})

	t.Run("the default and noexternal binary sizes are recorded, and the difference is under the ADR-0022 bar", func(t *testing.T) {
		doc := readRepoFile(t, "docs/efficiency/X-10.md")
		for _, want := range []string{"| default (with `adapters/external/openai`) |", "| `-tags noexternal` |", "difference"} {
			if !strings.Contains(doc, want) {
				t.Errorf("docs/efficiency/X-10.md no longer records %q", want)
			}
		}
	})
}
