package ingest

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// another is a second source registered beside the fixture's: its own root, its
// own scope (tenant and workspace), the same pipeline.
type another struct {
	scope contracts.Scope
	def   source.Definition
	root  string
}

// addSource registers a source for the tenant and workspace over a new root
// holding n documents named after tag.
func (f *fixture) addSource(tenant, workspace, tag string, n int, mutate ...func(*ap.RAGSource)) another {
	f.t.Helper()
	root := filepath.Join(filepath.Dir(f.root), "root-"+tag)
	if err := os.MkdirAll(root, 0o755); err != nil {
		f.t.Fatal(err)
	}
	sc := testScope(workspace)
	sc.TenantID = tenant
	def := f.mkDef(ids.New(source.IDPrefix), tenant, workspace, root, mutate...)
	f.reg.set(def)
	for i := 0; i < n; i++ {
		f.writeIn(root, fmt.Sprintf("%s%d.md", tag, i), doc(fmt.Sprintf("Title %s%d", tag, i),
			para(fmt.Sprintf("%s%da", tag, i)), para(fmt.Sprintf("%s%db", tag, i))))
	}
	return another{scope: sc, def: def, root: root}
}

// syncOther syncs another source and drains the queue.
func (f *fixture) syncOther(a another) {
	f.t.Helper()
	f.syncs++
	if _, err := f.svc.RequestSync(bg, a.scope, a.def.SourceID, SyncOptions{Key: fmt.Sprintf("other:%d", f.syncs)}); err != nil {
		f.t.Fatalf("RequestSync: %v", err)
	}
	f.drain()
}

func (f *fixture) vectorsIn(scope contracts.Scope) int {
	f.t.Helper()
	st, err := f.vec.Stats(bg, scope)
	if err != nil {
		f.t.Fatal(err)
	}
	return st.Vectors
}

func (f *fixture) statusOf(a another) SourceInfo {
	f.t.Helper()
	st, err := f.svc.Status(bg, a.scope, a.def.SourceID)
	if err != nil {
		f.t.Fatalf("Status: %v", err)
	}
	return st
}
