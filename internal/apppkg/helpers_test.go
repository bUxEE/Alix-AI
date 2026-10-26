package apppkg

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/tools/devkeys"
)

// Times of the fixtures: the signature of examples/packages/valid-minimal was
// made on 2026-10-01 and expires ten years later; tNow is inside that window.
var (
	tSignedAt = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	tExpires  = time.Date(2036, 10, 1, 0, 0, 0, 0, time.UTC)
	tNow      = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
)

// validMinimalDir is the committed fixture package.
const validMinimalDir = "../../examples/packages/valid-minimal"

// testKeys is a trust anchor that knows the published TEST-ONLY publisher key
// for every application, like a developer's trust store would.
type testKeys map[string]trust.Key

func (k testKeys) LookupKey(_ context.Context, app, id string) (trust.Key, error) {
	key, ok := k[id]
	if !ok {
		return trust.Key{}, trust.ErrUnknownKey
	}
	key.ApplicationID = app
	return key, nil
}

func devKeys() testKeys {
	return testKeys{devkeys.KeyID: {KeyID: devkeys.KeyID, PublicKey: devkeys.PublicKey(), Status: trust.KeyActive}}
}

func testVerifier(clk clock.Clock) *trust.Verifier {
	if clk == nil {
		clk = clock.NewFake(tNow)
	}
	return trust.NewVerifier(devKeys(), clk)
}

// loadOptions are the Options of a load that should authenticate.
func loadOptions() Options {
	return Options{Verifier: testVerifier(nil), CoreVersion: DevCoreVersion}
}

// pkgFiles is a package held as name -> content, outside signatures/.
type pkgFiles map[string][]byte

// readDir reads a package directory (signatures/ excluded) into a pkgFiles.
func readDir(t testing.TB, dir string) pkgFiles {
	t.Helper()
	out := pkgFiles{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "signatures/") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = b
		return nil
	})
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	return out
}

// valid returns a fresh copy of the valid-minimal package.
func valid(t testing.TB) pkgFiles { t.Helper(); return readDir(t, validMinimalDir) }

func (p pkgFiles) clone() pkgFiles {
	out := make(pkgFiles, len(p))
	for k, v := range p {
		out[k] = bytes.Clone(v)
	}
	return out
}

// set replaces or adds a file.
func (p pkgFiles) set(name, content string) pkgFiles { p[name] = []byte(content); return p }

// remove deletes a file; it must exist.
func (p pkgFiles) remove(t testing.TB, names ...string) pkgFiles {
	t.Helper()
	for _, n := range names {
		if _, ok := p[n]; !ok {
			t.Fatalf("fixture has no file %s to remove", n)
		}
		delete(p, n)
	}
	return p
}

// edit replaces the first occurrence of old in a file; old must be present, so
// a fixture that drifts from the tests fails loudly instead of testing nothing.
func (p pkgFiles) edit(t testing.TB, name, old, replacement string) pkgFiles {
	t.Helper()
	b, ok := p[name]
	if !ok {
		t.Fatalf("fixture has no file %s", name)
	}
	s := string(b)
	if !strings.Contains(s, old) {
		t.Fatalf("fixture file %s does not contain %q", name, old)
	}
	p[name] = []byte(strings.Replace(s, old, replacement, 1))
	return p
}

// dropBlock deletes the YAML list item that starts with the line marker (for
// example "- id: rc-risk") up to the next line that starts at the same
// indentation or less, or the end of the file.
func (p pkgFiles) dropBlock(t testing.TB, name, marker string) pkgFiles {
	t.Helper()
	s, ok := p[name]
	if !ok {
		t.Fatalf("fixture has no file %s", name)
	}
	lines := strings.Split(string(s), "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == marker {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("fixture file %s has no line %q", name, marker)
	}
	indent := len(lines[start]) - len(strings.TrimLeft(lines[start], " "))
	end := start + 1
	for end < len(lines) {
		l := lines[end]
		ind := len(l) - len(strings.TrimLeft(l, " "))
		if strings.TrimSpace(l) != "" && ind <= indent {
			break
		}
		end++
	}
	out := append(append([]string{}, lines[:start]...), lines[end:]...)
	p[name] = []byte(strings.Join(out, "\n"))
	return p
}

// dropLine deletes the first line whose trimmed text equals marker.
func (p pkgFiles) dropLine(t testing.TB, name, marker string) pkgFiles {
	t.Helper()
	lines := strings.Split(string(p[name]), "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == marker {
			p[name] = []byte(strings.Join(append(lines[:i:i], lines[i+1:]...), "\n"))
			return p
		}
	}
	t.Fatalf("fixture file %s has no line %q", name, marker)
	return p
}

// fsys returns the package as an in-memory file system, without a signature.
func (p pkgFiles) fsys() fstest.MapFS {
	m := fstest.MapFS{}
	for n, b := range p {
		m[n] = &fstest.MapFile{Data: b}
	}
	return m
}

// signed returns the package as an in-memory file system signed by the
// published TEST-ONLY key for demo-library 1.0.0, at the fixtures' times.
func (p pkgFiles) signed(t testing.TB) fstest.MapFS {
	t.Helper()
	return p.signedAs(t, "demo-library", "1.0.0")
}

// signedAs signs for an explicit application and version, to test bindings.
func (p pkgFiles) signedAs(t testing.TB, app, version string) fstest.MapFS {
	t.Helper()
	m := p.fsys()
	signed, err := trust.BuildSigned(m, trust.PackageIdentity{ApplicationID: app, PackageVersion: version}, tSignedAt)
	if err != nil {
		t.Fatalf("BuildSigned: %v", err)
	}
	sig, err := trust.SignPayload(signed, devkeys.KeyID, devkeys.PrivateKey(), tSignedAt, tExpires)
	if err != nil {
		t.Fatalf("SignPayload: %v", err)
	}
	sm := &trust.SignatureManifest{Format: trust.FormatV1, Signed: signed, Signatures: []trust.Signature{sig}}
	b, err := sm.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	m[trust.SignatureManifestPath] = &fstest.MapFile{Data: b}
	return m
}

// mustLoad loads a file system with the default test options.
func mustLoad(t testing.TB, fsys fs.FS, opts Options) *Result {
	t.Helper()
	res, err := Load(context.Background(), FromFS(fsys), opts)
	if err != nil {
		t.Fatalf("Load returned an error: %v", err)
	}
	return res
}

// ids returns the check IDs of the findings of a severity, sorted and unique.
func ids(fs []contracts.Finding, sev contracts.Severity) []string {
	seen := map[string]bool{}
	for _, f := range fs {
		if f.Severity == sev {
			seen[f.CheckID] = true
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// requireCritical fails unless the critical findings of res are exactly the
// wanted check IDs, so a test proves its defect is the only one present.
func requireCritical(t testing.TB, res *Result, want ...string) {
	t.Helper()
	sort.Strings(want)
	got := ids(res.Findings, contracts.SeverityCritical)
	if !equalStrings(got, want) {
		t.Fatalf("critical checks = %v, want %v\nfindings:\n%s", got, want, dump(res.Findings))
	}
	if len(want) > 0 {
		if res.Package != nil {
			t.Fatalf("a package with critical findings must not be returned")
		}
		if !res.Critical() || res.OK() {
			t.Fatalf("Result must report the critical findings")
		}
	}
}

// find returns the first finding of a check, or false.
func find(fs []contracts.Finding, check string) (contracts.Finding, bool) {
	for _, f := range fs {
		if f.CheckID == check {
			return f, true
		}
	}
	return contracts.Finding{}, false
}

func dump(fs []contracts.Finding) string {
	var sb strings.Builder
	for _, f := range fs {
		sb.WriteString("  ")
		sb.WriteString(f.String())
		sb.WriteString("\n")
	}
	return sb.String()
}

// readSignature returns the signature manifest of a package directory.
func readSignature(t testing.TB, dir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "signatures", "signature-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// newVerifierWithoutKeys is a trust store in which nobody is registered.
func newVerifierWithoutKeys() *trust.Verifier {
	return trust.NewVerifier(testKeys{}, clock.NewFake(tNow))
}

// flipFS serves the real content of a file the first time it is opened and
// garbage every time after, and counts the opens of regular files. A loader
// that read a file twice, once to verify it and once to parse it, would see
// both.
type flipFS struct {
	inner fstest.MapFS
	mu    sync.Mutex
	reads map[string]int
}

func (f *flipFS) Open(name string) (fs.File, error) {
	st, err := f.inner.Stat(name)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return f.inner.Open(name)
	}
	f.mu.Lock()
	f.reads[name]++
	n := f.reads[name]
	f.mu.Unlock()
	if n > 1 {
		return fstest.MapFS{name: {Data: []byte("tampered after the first read\n")}}.Open(name)
	}
	return f.inner.Open(name)
}

func (f *flipFS) ReadDir(name string) ([]fs.DirEntry, error) { return f.inner.ReadDir(name) }
func (f *flipFS) Stat(name string) (fs.FileInfo, error)      { return f.inner.Stat(name) }
