package docpkg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// secret is written into every file the policies source must never serve: if it shows up in a
// listing, a version, a key, an error or a byte stream, a read left the declared folder.
const secret = "OUTSIDE-THE-DECLARED-FOLDER-7c41e0"

const (
	app = "clinic-booking"
	// hashV1 and hashV2 stand for the content addresses of two versions of the package.
	hashV1 = "1111111111111111111111111111111111111111111111111111111111111111"
	hashV2 = "2222222222222222222222222222222222222222222222222222222222222222"
)

var (
	t0 = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	// installed1 and installed2 are when the installer wrote the files of each version: a replaced
	// package has the modification times of its own install.
	installed1 = time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	installed2 = time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
)

// packageV1 is an installed package: its files by package-relative path. The policies folder holds
// what the source serves; everything else carries the secret, on every kind of place a source
// declaration could be pointed at.
var packageV1 = map[string]string{
	"alixai.yaml":                            "application:\n  id: clinic-booking\n" + secret + "\n",
	"README.md":                              "# Readme\n\n" + secret + "\n",
	"signatures/signature-manifest.md":       "# Signatures\n\n" + secret + "\n",
	"rag-sources/clinic-policies.md":         "# Declaration\n\n" + secret + "\n",
	"documentation/index.yaml":               "documents: []\n" + secret + "\n",
	"documentation/other/secret.md":          "# Other\n\n" + secret + "\n",
	"documentation/policies/booking.md":      "# Booking\n\nA visit is booked at the harbour desk.\n",
	"documentation/policies/booking.it.md":   "# Prenotazioni\n\nUna visita si prenota al banco del porto.\n",
	"documentation/policies/sub/deep.md":     "# Deep\n\nThe deep policy lists the cancellation window.\n",
	"documentation/policies/drafts/wip.md":   "# Draft\n\n" + secret + "\n",
	"documentation/policies/notes.txt":       "notes " + secret + "\n",
	"documentation/policies/sub/ignored.pdf": "%PDF-1.4 " + secret + "\n",
}

// policyKeys are the keys the policies source serves from packageV1: below the declared folder,
// admitted by the include and exclude rules and the accepted formats.
var policyKeys = []string{"booking.it.md", "booking.md", "sub/deep.md"}

// packageV2 replaces packageV1: booking.md has new text, booking.it.md the same, sub/deep.md is
// gone and renewals.md is new.
var packageV2 = map[string]string{
	"alixai.yaml":                          "application:\n  id: clinic-booking\n",
	"documentation/policies/booking.md":    "# Booking\n\nA visit is booked at the new harbour desk, from nine.\n",
	"documentation/policies/booking.it.md": "# Prenotazioni\n\nUna visita si prenota al banco del porto.\n",
	"documentation/policies/renewals.md":   "# Renewals\n\nA renewal needs the previous reference.\n",
}

// world is a temp directory laid out like a data directory (base/packages/<app>/<hash>) with a
// secret outside every package.
type world struct {
	t       *testing.T
	base    string
	outside string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, base: base, outside: filepath.Join(base, "outside")}
	put(t, w.outside, "secret.md", "# Outside\n\n"+secret+"\n", installed1)
	return w
}

func (w *world) dir(application, hash string) string {
	return filepath.Join(w.base, "packages", application, hash)
}

// install writes the files of a package the way the installer does, with the modification time of
// its install, and returns its directory.
func (w *world) install(application, hash string, files map[string]string, at time.Time) string {
	w.t.Helper()
	dir := w.dir(application, hash)
	for rel, content := range files {
		put(w.t, dir, rel, content, at)
	}
	return dir
}

func put(t *testing.T, dir, rel, content string, at time.Time) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
	return p
}

// resolver is the package source of a test: per application, the package that is active, or an error.
type resolver struct {
	mu    sync.Mutex
	pkgs  map[string]Package
	err   error
	calls int
}

func newResolver() *resolver { return &resolver{pkgs: map[string]Package{}} }

func (r *resolver) activate(p Package) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pkgs[p.ApplicationID], r.err = p, nil
}

func (r *resolver) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *resolver) called() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *resolver) Package(_ context.Context, application string) (Package, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		return Package{}, r.err
	}
	p, ok := r.pkgs[application]
	if !ok {
		return Package{}, errors.New("no active package")
	}
	return p, nil
}

// harness is a provider over a resolver, on a fake clock.
type harness struct {
	t   *testing.T
	w   *world
	res *resolver
	p   *Provider
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	w := newWorld(t)
	res := newResolver()
	return &harness{t: t, w: w, res: res, p: New(Config{Clock: clock.NewFake(t0), Packages: res})}
}

// activateV1 installs packageV1 and makes it the active package of the clinic.
func (h *harness) activateV1() Package {
	h.t.Helper()
	pkg := Package{ApplicationID: app, ManifestHash: hashV1, Dir: h.w.install(app, hashV1, packageV1, installed1)}
	h.res.activate(pkg)
	return pkg
}

func (h *harness) activateV2() Package {
	h.t.Helper()
	pkg := Package{ApplicationID: app, ManifestHash: hashV2, Dir: h.w.install(app, hashV2, packageV2, installed2)}
	h.res.activate(pkg)
	return pkg
}

// decl is the clinic's declaration of its policies: shared at both levels, below
// documentation/policies, markdown only, drafts excluded.
func decl(application string, mutate ...func(*ap.RAGSource)) ap.RAGSource {
	d := ap.RAGSource{
		ID:    "clinic-policies",
		Type:  ap.RAGSourceTypeDocumentationPackage,
		Owner: ap.RAGSourceOwner{Name: "Test owner"},
		Scope: ap.RAGSourceScope{
			ApplicationID: ap.ApplicationID(application),
			Tenant:        ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeShared},
			Workspace:     ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeShared},
		},
		Root:     ap.RAGSourceRoot{Value: ap.RAGSourceRootPackage{Kind: "package", Path: "documentation/policies"}},
		Include:  []ap.GlobPattern{"**/*.md"},
		Exclude:  []ap.GlobPattern{"drafts/**"},
		Formats:  []ap.MimeType{source.MIMEMarkdown},
		Limits:   ap.RAGSourceLimits{MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 30, MaxFiles: 200},
		Sync:     ap.RAGSourceSync{Method: ap.RAGSourceSyncMethodManual},
		Deletion: ap.RAGSourceDeletion{PurgeAfterHours: 0, OnSourceRemoved: ap.RAGSourceDeletionOnSourceRemovedPurge},
	}
	for _, m := range mutate {
		m(&d)
	}
	return d
}

// rootAt declares the source's folder.
func rootAt(path string) func(*ap.RAGSource) {
	return func(d *ap.RAGSource) {
		d.Root = ap.RAGSourceRoot{Value: ap.RAGSourceRootPackage{Kind: "package", Path: path}}
	}
}

// defOf builds the definition a registration of the declaration holds, pinned to the package
// version it was built from (source.FromPackage sets ManifestHash; the audit's definition has none).
func defOf(t *testing.T, hash string, d ap.RAGSource) source.Definition {
	t.Helper()
	def, err := source.FromDeclaration(d, source.Binding{SourceID: ids.New(source.IDPrefix)})
	if err != nil {
		t.Fatal(err)
	}
	def.ManifestHash = hash
	return def
}

func scopeOf(application string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep-1", ApplicationID: application, TenantID: "tenant-1", WorkspaceID: "ws-1",
		PrincipalID: "svc-sync", Roles: []string{"sync"}, CapabilityScopes: []string{"rag.sync"}, SessionID: "sess-1",
	}
}

func hashOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return source.HashPrefix + hex.EncodeToString(sum[:])
}

func keysOf(items []source.Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Key
	}
	sort.Strings(out)
	return out
}

func tombstoneKeys(ts []source.Tombstone) []string {
	out := make([]string, len(ts))
	for i, tb := range ts {
		out[i] = tb.Key
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

func code(err error) contracts.ErrCode { return contracts.CodeOf(err) }

// readAll reads a content and closes it.
func readAll(t *testing.T, c source.Content) string {
	t.Helper()
	defer c.Close()
	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// noHostPath fails when an error spells a directory of the test's world or the secret.
func (w *world) noHostPath(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	for _, bad := range []string{w.base, secret} {
		if strings.Contains(err.Error(), bad) {
			t.Errorf("%s: the error carries %q: %v", what, bad, err)
		}
	}
}

// tree lists a directory: relative path, content and modification time of every file, so a test can
// show that nothing was written, touched or removed.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			out[filepath.ToSlash(rel)+"/"] = ""
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(b) + "@" + fi.ModTime().UTC().Format(time.RFC3339Nano)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
