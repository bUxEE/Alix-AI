package adapterssec

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/tests/security/secutil"
	"github.com/buxee/alix-ai/tools/devkeys"
)

const (
	// validMinimalDir is the committed fixture package.
	validMinimalDir = "examples/packages/valid-minimal"
	libraryApp      = "demo-library"
)

var (
	signedAt = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	expires  = time.Date(2036, 10, 1, 0, 0, 0, 0, time.UTC)
	loadNow  = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
)

// pkgFiles is a package as name -> content, without its signature.
type pkgFiles map[string][]byte

// validMinimal reads the committed valid-minimal package (signatures/ excluded).
func validMinimal(t testing.TB) pkgFiles {
	t.Helper()
	root := filepath.Join(secutil.RepoRoot(t), filepath.FromSlash(validMinimalDir))
	out := pkgFiles{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "signatures/") {
			return nil
		}
		b, err := os.ReadFile(p)
		out[rel] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 10 {
		t.Fatalf("the fixture package has only %d files", len(out))
	}
	return out
}

func (p pkgFiles) clone() pkgFiles {
	out := make(pkgFiles, len(p))
	for k, v := range p {
		out[k] = bytes.Clone(v)
	}
	return out
}

// edit replaces the first occurrence of old in a file; old must be there, so a
// fixture that drifts from the test fails loudly instead of testing nothing.
func (p pkgFiles) edit(t testing.TB, name, old, replacement string) pkgFiles {
	t.Helper()
	b, ok := p[name]
	if !ok || !bytes.Contains(b, []byte(old)) {
		t.Fatalf("fixture file %s does not contain %q", name, old)
	}
	p[name] = bytes.Replace(b, []byte(old), []byte(replacement), 1)
	return p
}

// signed signs the package for an application with the published TEST-ONLY publisher
// key, as `alixai package sign` does.
func (p pkgFiles) signed(t testing.TB, app, version string) fstest.MapFS {
	t.Helper()
	m := fstest.MapFS{}
	for n, b := range p {
		m[n] = &fstest.MapFile{Data: b}
	}
	signed, err := trust.BuildSigned(m, trust.PackageIdentity{ApplicationID: app, PackageVersion: version}, signedAt)
	if err != nil {
		t.Fatalf("BuildSigned: %v", err)
	}
	sig, err := trust.SignPayload(signed, devkeys.KeyID, devkeys.PrivateKey(), signedAt, expires)
	if err != nil {
		t.Fatalf("SignPayload: %v", err)
	}
	sm := &trust.SignatureManifest{Format: trust.FormatV1, Signed: signed, Signatures: []trust.Signature{sig}}
	b, err := sm.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	m[trust.SignatureManifestPath] = &fstest.MapFile{Data: b}
	return m
}

// verifier trusts the TEST-ONLY key for the given applications.
func verifier(t testing.TB, apps ...string) *trust.Verifier {
	t.Helper()
	clk := clock.NewFake(loadNow)
	ts := trust.NewStore(storetest.Open(t, nil), clk, trust.AllowTestOnlyKeys())
	for _, app := range apps {
		if _, err := ts.RegisterKey(context.Background(), app, devkeys.KeyID, devkeys.PublicKey(), "security suite"); err != nil {
			t.Fatal(err)
		}
	}
	return trust.NewVerifier(ts, clk)
}

// load loads a signed package with the production loader.
func load(t testing.TB, v *trust.Verifier, fsys fs.FS) *apppkg.Result {
	t.Helper()
	res, err := apppkg.Load(context.Background(), apppkg.FromFS(fsys), apppkg.Options{Verifier: v, CoreVersion: apppkg.DevCoreVersion})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return res
}

func criticalIDs(res *apppkg.Result) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range res.Findings {
		if f.Severity == contracts.SeverityCritical && !seen[f.CheckID] {
			seen[f.CheckID] = true
			out = append(out, f.CheckID)
		}
	}
	sort.Strings(out)
	return out
}

func hasID(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
