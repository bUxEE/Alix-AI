package fs

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// sentinel is written into every file the provider must never read. If it
// shows up in a version, a key, an error message or a byte stream, a read
// escaped the root.
const sentinel = "TOP-SECRET-SENTINEL-8f2b1c0d"

var (
	clockStart = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	oldTime    = time.Date(2025, 1, 1, 8, 0, 0, 0, time.UTC)
)

// tree is a temp directory laid out as base/root (the declared root),
// base/outside (what must never be read) and base/root-evil (a sibling whose
// name starts with the root's).
type tree struct {
	t       *testing.T
	base    string
	root    string
	outside string
	evil    string
}

func newTree(t *testing.T) *tree {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tr := &tree{t: t, base: base, root: filepath.Join(base, "root"), outside: filepath.Join(base, "outside"), evil: filepath.Join(base, "root-evil")}
	for _, d := range []string{tr.root, tr.outside, tr.evil} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return tr
}

func (tr *tree) put(dir, rel, content string) string {
	tr.t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		tr.t.Fatal(err)
	}
	return p
}

// write creates a file under the root with an old modification time, so that
// a record of it is trusted for the size-and-time shortcut.
func (tr *tree) write(rel, content string) string {
	tr.t.Helper()
	p := tr.put(tr.root, rel, content)
	tr.setTime(rel, oldTime)
	return p
}

func (tr *tree) writeOutside(rel, content string) string { return tr.put(tr.outside, rel, content) }

func (tr *tree) abs(rel string) string { return filepath.Join(tr.root, filepath.FromSlash(rel)) }

func (tr *tree) setTime(rel string, mt time.Time) {
	tr.t.Helper()
	if err := os.Chtimes(tr.abs(rel), mt, mt); err != nil {
		tr.t.Fatal(err)
	}
}

func (tr *tree) remove(rel string) {
	tr.t.Helper()
	if err := os.RemoveAll(tr.abs(rel)); err != nil {
		tr.t.Fatal(err)
	}
}

func baseDecl(root string) ap.RAGSource {
	return ap.RAGSource{
		ID:    "docs",
		Type:  ap.RAGSourceTypeLocalFolder,
		Owner: ap.RAGSourceOwner{Name: "Test owner"},
		Scope: ap.RAGSourceScope{
			ApplicationID: "demo-library",
			Tenant:        ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeRequest},
			Workspace:     ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeRequest},
		},
		Root:     ap.RAGSourceRoot{Value: ap.RAGSourceRootFolder{Kind: "folder", Path: root}},
		Include:  []ap.GlobPattern{"**"},
		Exclude:  []ap.GlobPattern{},
		Formats:  []ap.MimeType{source.MIMEText, source.MIMEMarkdown},
		Limits:   ap.RAGSourceLimits{MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 30, MaxFiles: 10000},
		Sync:     ap.RAGSourceSync{Method: ap.RAGSourceSyncMethodManual},
		Deletion: ap.RAGSourceDeletion{PurgeAfterHours: 24, OnSourceRemoved: ap.RAGSourceDeletionOnSourceRemovedPurge},
	}
}

func (tr *tree) def(mutate ...func(*ap.RAGSource)) source.Definition {
	tr.t.Helper()
	d := baseDecl(tr.root)
	for _, m := range mutate {
		m(&d)
	}
	def, err := source.FromDeclaration(d, source.Binding{SourceID: ids.New(source.IDPrefix), TenantID: "tenant-1", WorkspaceID: "ws-1"})
	if err != nil {
		tr.t.Fatal(err)
	}
	return def
}

func scope() contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep-1", ApplicationID: "demo-library", TenantID: "tenant-1", WorkspaceID: "ws-1",
		PrincipalID: "svc-sync", Roles: []string{"sync"}, CapabilityScopes: []string{"rag.sync"}, SessionID: "sess-1",
	}
}

// harness is a provider on a fake clock that records every file it opens.
type harness struct {
	t   *testing.T
	tr  *tree
	p   *Provider
	clk *clock.Fake
	ctx context.Context

	mu    sync.Mutex
	opens []string
}

func newHarness(t *testing.T, cfg ...func(*Config)) *harness {
	t.Helper()
	h := &harness{t: t, tr: newTree(t), clk: clock.NewFake(clockStart), ctx: context.Background()}
	c := Config{Clock: h.clk}
	for _, f := range cfg {
		f(&c)
	}
	h.p = New(c)
	h.p.onOpen = func(abs string) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.opens = append(h.opens, abs)
	}
	return h
}

func (h *harness) opened() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.opens...)
}

func (h *harness) resetOpens() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.opens = nil
}

func (h *harness) discover(def source.Definition, prev []source.Record) source.ChangeSet {
	h.t.Helper()
	cs, err := h.p.Discover(h.ctx, scope(), def, prev)
	if err != nil {
		h.t.Fatalf("Discover: %v", err)
	}
	return cs
}

func keysOf(items []source.Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Key)
	}
	sort.Strings(out)
	return out
}

func skipKeys(s []source.Skip) map[string]source.Reason {
	out := map[string]source.Reason{}
	for _, k := range s {
		out[k.Key] = k.Reason
	}
	return out
}

func tombstoneReasons(ts []source.Tombstone) map[string]source.Reason {
	out := map[string]source.Reason{}
	for _, k := range ts {
		out[k.Key] = k.Reason
	}
	return out
}

func recordKeys(rs []source.Record) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Key)
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

// makeJunction creates a Windows directory junction link -> target. It reports
// false (and the reason) when it cannot, which is always the case off Windows.
func makeJunction(t *testing.T, link, target string) bool {
	t.Helper()
	if runtime.GOOS != "windows" {
		return false
	}
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Logf("cannot create a junction here: %v: %s", err, bytes.TrimSpace(out))
		return false
	}
	return true
}

// trySymlink creates a symbolic link, which needs a privilege on Windows. It
// reports false when it cannot, so a subtest can skip with a clear message.
func trySymlink(t *testing.T, target, link string) bool {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Logf("cannot create symbolic links here: %v", err)
		return false
	}
	return true
}

func contextCancelled() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx, cancel
}
