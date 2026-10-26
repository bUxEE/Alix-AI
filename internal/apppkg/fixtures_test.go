package apppkg

import (
	"bytes"
	"context"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"testing/fstest"

	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

var updateFixtures = flag.Bool("update-fixtures", false, "rewrite examples/packages/broken-* from examples/packages/valid-minimal")

// brokenFixture is one committed broken package: valid-minimal with one defect,
// signed with the published TEST-ONLY key (or deliberately not, or changed after
// signing). They exist for people and for the command line:
//
//	go run ./cmd/alixai package validate examples/packages/broken-unsigned
type brokenFixture struct {
	dir   string
	build func(t *testing.T, p pkgFiles) fstest.MapFS
	// want are the critical checks the loader must report, exactly.
	want []string
}

func signedWith(edit func(t *testing.T, p pkgFiles)) func(t *testing.T, p pkgFiles) fstest.MapFS {
	return func(t *testing.T, p pkgFiles) fstest.MapFS {
		edit(t, p)
		return p.signed(t)
	}
}

var brokenFixtures = []brokenFixture{
	{
		dir: "broken-unsigned",
		build: func(t *testing.T, p pkgFiles) fstest.MapFS {
			return p.fsys()
		},
		want: []string{"signature.missing"},
	},
	{
		dir: "broken-tampered",
		build: func(t *testing.T, p pkgFiles) fstest.MapFS {
			m := p.signed(t)
			f := m["documentation/catalog/find-title.md"]
			f.Data = append(append([]byte(nil), f.Data...), []byte("\nA line added after the package was signed.\n")...)
			return m
		},
		want: []string{"signature.tampered"},
	},
	{
		dir: "broken-missing-doc-facet",
		build: signedWith(func(t *testing.T, p pkgFiles) {
			p.dropBlock(t, "documentation/index.yaml", "- id: rc-risk")
			p.dropLine(t, "capabilities/reservations-create.yaml", "- rc-risk")
		}),
		want: []string{"docs.facet_missing"},
	},
	{
		dir: "broken-missing-contract",
		build: signedWith(func(t *testing.T, p pkgFiles) {
			p.remove(t, "conversation-contracts/reservations-create.it.yaml")
		}),
		want: []string{"contract.missing"},
	},
	{
		dir: "broken-no-adapter-mapping",
		build: signedWith(func(t *testing.T, p pkgFiles) {
			p.edit(t, "capabilities/catalog-find-title.yaml", "endpoint: library-mcp", "endpoint: no-such-endpoint")
		}),
		want: []string{"capability.adapter_mapping"},
	},
	{
		dir: "broken-rag-no-scope",
		build: signedWith(func(t *testing.T, p pkgFiles) {
			p.edit(t, "rag-sources/loan-policies.yaml", "scope:\n  application_id: demo-library\n  tenant:\n    mode: shared\n  workspace:\n    mode: shared\n", "")
		}),
		want: []string{"schema.rag_source"},
	},
	{
		dir: "broken-missing-safety-probes",
		build: signedWith(func(t *testing.T, p pkgFiles) {
			p.dropBlock(t, "tests/core-safety.yaml", "- id: reserve-uncertain")
			p.dropBlock(t, "tests/core-safety.yaml", "- id: reserve-stale")
		}),
		want: []string{"tests.safety_probe_missing"},
	},
	{
		dir: "broken-incompatible-core",
		build: signedWith(func(t *testing.T, p pkgFiles) {
			p.edit(t, "alixai.yaml", "    min: 1.0.0\n    max_exclusive: 2.0.0", "    min: 5.0.0\n    max_exclusive: 6.0.0")
		}),
		want: []string{"compat.core_out_of_range"},
	},
}

const fixturesRoot = "../../examples/packages"

// TestBrokenFixtures keeps the committed broken packages in step with
// valid-minimal, and checks that each fails exactly as advertised. Run with
// -update-fixtures after changing valid-minimal.
func TestBrokenFixtures(t *testing.T) {
	for _, b := range brokenFixtures {
		t.Run(b.dir, func(t *testing.T) {
			want := b.build(t, valid(t))
			dir := filepath.Join(fixturesRoot, b.dir)
			if *updateFixtures {
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
				writeMapFS(t, dir, want)
			}
			have := map[string][]byte{}
			if _, err := os.Stat(dir); err == nil {
				for n, c := range readDirAll(t, dir) {
					have[n] = c
				}
			}
			wantFiles := map[string][]byte{}
			err := fs.WalkDir(want, ".", func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				wantFiles[p], err = fs.ReadFile(want, p)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if !sameFiles(have, wantFiles) {
				t.Fatalf("examples/packages/%s is not what valid-minimal plus its defect builds; run: go test ./internal/apppkg -run TestBrokenFixtures -update-fixtures", b.dir)
			}

			res, err := Load(context.Background(), FromDir(dir), loadOptions())
			if err != nil {
				t.Fatal(err)
			}
			requireCritical(t, res, b.want...)
		})
	}
}

func sameFiles(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for n, x := range a {
		y, ok := b[n]
		if !ok || !bytes.Equal(x, y) {
			return false
		}
	}
	return true
}

func writeMapFS(t *testing.T, dir string, m fstest.MapFS) {
	t.Helper()
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := filepath.Join(dir, filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, m[n].Data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// validShippedPackages are the committed packages that must load cleanly against
// both the development core (1.1.0-dev) and a stamped 1.1.0 release.
var validShippedPackages = []string{
	"valid-minimal",
	"valid-help-assistant",
	"egress-local-only",
	"egress-allow-excerpt",
}

// TestValidShippedPackagesAcceptDevAndReleaseCore: every valid example package
// declares [1.0.0, 2.0.0) and validates against DevCoreVersion and 1.1.0.
func TestValidShippedPackagesAcceptDevAndReleaseCore(t *testing.T) {
	for _, dir := range validShippedPackages {
		t.Run(dir, func(t *testing.T) {
			path := filepath.Join(fixturesRoot, dir)
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			for _, core := range []string{DevCoreVersion, "1.1.0"} {
				t.Run(core, func(t *testing.T) {
					opts := loadOptions()
					opts.CoreVersion = core
					res, err := Load(context.Background(), FromDir(path), opts)
					if err != nil {
						t.Fatal(err)
					}
					requireCritical(t, res)
					if !res.OK() {
						t.Fatalf("findings: %s", dump(res.Findings))
					}
				})
			}
		})
	}
}

// TestValidMinimalHasNoStrayFiles: the committed valid package holds exactly
// the files its signature lists, plus the signature manifest, so it is what the
// command line validates.
func TestValidMinimalHasNoStrayFiles(t *testing.T) {
	signed, err := trust.BuildSignedDir(validMinimalDir, trust.PackageIdentity{ApplicationID: "demo-library", PackageVersion: "1.0.0"}, tSignedAt)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Load(context.Background(), FromDir(validMinimalDir), loadOptions())
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != len(signed.Files)+1 {
		t.Fatalf("the directory holds %d files, its signature lists %d plus the manifest", res.Files, len(signed.Files))
	}
	if got := ids(res.Findings, contracts.SeverityWarning); len(got) != 0 {
		t.Fatalf("valid-minimal has warnings: %v", got)
	}
}
