package httpapi

import (
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// seeded is a harness with five documents, scanned once.
func seeded(t *testing.T) (*harness, source.Definition, source.ChangeSet) {
	t.Helper()
	h := newHarness(t, "/api/docs")
	for _, k := range []string{"a.md", "b.md", "c.md", "d.md", "e.md"} {
		h.api.put(k, "content of "+k+"\n")
	}
	def := h.def()
	cs := h.discover(def, nil)
	if len(cs.Added) != 5 {
		t.Fatalf("seed: Added = %v", keysOf(cs.Added))
	}
	h.clk.Advance(time.Hour)
	h.api.reset()
	return h, def, cs
}

// TestDeletedRecordTombstone: an item is gone only when a complete listing says
// so (it is absent, or a deletion marker names it); then it is tombstoned at
// once with the declaration's purge window. A listing that failed anywhere, a
// refusing API, a wrong base path or an item that cannot be read are never
// deletions.
func TestDeletedRecordTombstone(t *testing.T) {
	t.Run("absent from a complete listing, and named by a deletion marker", func(t *testing.T) {
		h, def, cs1 := seeded(t)
		h.api.remove("b.md")
		h.api.mark("c.md")
		h.api.put("z.md", "never indexed\n")
		h.api.mark("z.md") // a marker for something the caller never had: nothing to delete

		cs := h.discover(def, cs1.Next)
		got := tombstoneReasons(cs.Deleted)
		if len(got) != 2 || got["b.md"] != source.ReasonDeleted || got["c.md"] != source.ReasonDeleted {
			t.Fatalf("tombstones = %v, want b.md and c.md deleted", got)
		}
		if got := recordKeys(cs.Next); !equalStrings(got, []string{"a.md", "d.md", "e.md"}) {
			t.Errorf("Next = %v", got)
		}
		if len(cs.Added) != 0 || len(cs.Skipped) != 0 || len(cs.Failed) != 0 {
			t.Errorf("a deletion marker is not an item: %+v", cs)
		}
		if cs.Counts.Tombstoned != 2 || cs.Counts.Discovered != 3 || cs.Counts.Unchanged != 3 {
			t.Errorf("counts = %+v", cs.Counts)
		}
		prev := map[string]source.Record{}
		for _, r := range cs1.Next {
			prev[r.Key] = r
		}
		for _, ts := range cs.Deleted {
			if !ts.At.Equal(h.clk.Now()) || !ts.PurgeAfter.Equal(h.clk.Now().Add(24*time.Hour)) {
				t.Errorf("%s: tombstone times At=%v PurgeAfter=%v", ts.Key, ts.At, ts.PurgeAfter)
			}
			if ts.Last != prev[ts.Key].Version() {
				t.Errorf("%s: Last = %+v, want the last live version %+v", ts.Key, ts.Last, prev[ts.Key].Version())
			}
		}
		// The tombstoned items are not requested at all.
		if n := h.api.count(http.MethodGet, true); n != 0 {
			t.Errorf("%d item requests for a scan that only deleted", n)
		}
		// A scan from the new snapshot has nothing left to delete.
		if cs2 := h.discover(def, cs.Next); cs2.Changed() {
			t.Errorf("second scan: %+v", cs2)
		}
	})

	failures := []struct {
		name string
		page int
		hook func(w http.ResponseWriter, r *http.Request)
		code contracts.ErrCode
	}{
		{"a server error on the second page", 2, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) }, contracts.ErrUnavailable},
		{"a refused credential on the second page", 2, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 403) }, contracts.ErrPermissionDenied},
		{"rate limiting on the second page", 2, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "slow", 429) }, contracts.ErrRateLimited},
		{"a wrong base path (404) on the second page", 2, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, contracts.ErrUnavailable},
		{"a page that is not JSON", 2, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "<html>gateway</html>") }, contracts.ErrSchemaInvalid},
		{"an empty 200", 2, func(w http.ResponseWriter, r *http.Request) {}, contracts.ErrSchemaInvalid},
		{"a redirect", 2, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/login", http.StatusFound) }, contracts.ErrSchemaInvalid},
	}
	for _, tc := range failures {
		t.Run("a listing that fails: "+tc.name, func(t *testing.T) {
			h, def, cs1 := seeded(t)
			h.api.remove("a.md") // would be a tombstone if the listing were trusted
			h.api.listHook = func(w http.ResponseWriter, r *http.Request, page int) bool {
				if page == tc.page {
					tc.hook(w, r)
					return true
				}
				return false
			}
			cs, err := h.p.Discover(h.ctx, scope(), def, cs1.Next)
			if contracts.CodeOf(err) != tc.code {
				t.Errorf("err = %v, want code %s", err, tc.code)
			}
			noChange(t, cs)
		})
	}

	t.Run("an item that cannot be read keeps its record and is not a deletion", func(t *testing.T) {
		h, def, cs1 := seeded(t)
		for _, k := range []string{"a.md", "b.md", "c.md"} {
			h.api.update(k, "changed "+k+"\n", oldTime.Add(time.Hour))
		}
		h.api.itemHook = func(w http.ResponseWriter, r *http.Request, key string) bool {
			switch key {
			case "a.md":
				http.Error(w, "forbidden", http.StatusForbidden)
			case "b.md":
				http.Error(w, "broken", http.StatusInternalServerError)
			case "c.md":
				http.NotFound(w, r) // listed, then gone: a race, not a verdict
			default:
				return false
			}
			return true
		}
		cs := h.discover(def, cs1.Next)
		if len(cs.Failed) != 3 || len(cs.Deleted) != 0 || len(cs.Modified) != 0 {
			t.Fatalf("cs = %+v", cs)
		}
		if got := recordKeys(cs.Next); len(got) != 5 {
			t.Errorf("Next = %v: a failure must keep the previous record", got)
		}
		if cs.Counts.Failed != 3 {
			t.Errorf("counts = %+v", cs.Counts)
		}
	})

	t.Run("an API that is down for every item stops the scan", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		h.api.perPage = 100
		for i := range 40 {
			h.api.put("k"+string(rune('A'+i%26))+string(rune('a'+i/26))+".md", "x\n")
		}
		h.api.itemHook = func(w http.ResponseWriter, r *http.Request, key string) bool {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return true
		}
		cs, err := h.p.Discover(h.ctx, scope(), h.def(), nil)
		if contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Errorf("err = %v", err)
		}
		noChange(t, cs)
		if n := h.api.count(http.MethodGet, true); n > maxConsecutiveFailures {
			t.Errorf("%d item requests after the API said 503 every time", n)
		}
	})

	t.Run("a rate limit mid-scan stops it", func(t *testing.T) {
		h, def, cs1 := seeded(t)
		h.api.update("a.md", "changed\n", oldTime.Add(time.Hour))
		h.api.update("b.md", "changed\n", oldTime.Add(time.Hour))
		h.api.itemHook = func(w http.ResponseWriter, r *http.Request, key string) bool {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return true
		}
		cs, err := h.p.Discover(h.ctx, scope(), def, cs1.Next)
		if contracts.CodeOf(err) != contracts.ErrRateLimited {
			t.Errorf("err = %v", err)
		}
		noChange(t, cs)
		if n := h.api.count(http.MethodGet, true); n != 1 {
			t.Errorf("%d item requests after a 429, want 1", n)
		}
	})

	t.Run("DetectDeletions tombstones what the API says is gone, and only that", func(t *testing.T) {
		h, def, cs1 := seeded(t)
		h.api.remove("b.md")
		h.api.mark("d.md") // the item endpoint answers 404 for a marked item
		h.api.itemHook = func(w http.ResponseWriter, r *http.Request, key string) bool {
			if key == "a.md" {
				http.Error(w, "forbidden", http.StatusForbidden)
				return true
			}
			return false
		}
		cs, err := h.p.DetectDeletions(h.ctx, scope(), def, cs1.Next)
		if err != nil {
			t.Fatal(err)
		}
		got := tombstoneReasons(cs.Deleted)
		if len(got) != 2 || got["b.md"] != source.ReasonDeleted || got["d.md"] != source.ReasonDeleted {
			t.Errorf("tombstones = %v", got)
		}
		if len(cs.Failed) != 1 || cs.Failed[0].Key != "a.md" {
			t.Errorf("Failed = %+v: a forbidden item is not a deleted one", cs.Failed)
		}
		if got := recordKeys(cs.Next); !equalStrings(got, []string{"a.md", "c.md", "e.md"}) {
			t.Errorf("Next = %v", got)
		}
		if len(cs.Added)+len(cs.Modified) != 0 {
			t.Errorf("DetectDeletions reports deletions only: %+v", cs)
		}
		if h.api.count(http.MethodGet, true) != 0 {
			t.Errorf("DetectDeletions must not read content")
		}
	})

	t.Run("DetectDeletions believes a 404 only if the list endpoint answers", func(t *testing.T) {
		h, def, cs1 := seeded(t)
		// A wrong base path (or a proxy that answers 404 to everything) makes
		// every item look deleted.
		wrong := h.def(func(d *ap.RAGSource) {
			u := ap.HTTPURL(h.api.srv.URL + "/api/wrong")
			d.Root = ap.RAGSourceRoot{Value: ap.RAGSourceRootAPI{Kind: "api", URL: &u}}
		})
		cs, err := h.p.DetectDeletions(h.ctx, scope(), wrong, cs1.Next)
		if contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Errorf("err = %v, want unavailable", err)
		}
		noChange(t, cs)

		// The list endpoint fails after the items were inspected.
		h.api.remove("b.md")
		calls := 0
		h.api.listHook = func(w http.ResponseWriter, r *http.Request, page int) bool {
			calls++
			if calls == 2 {
				http.Error(w, "boom", 500)
				return true
			}
			return false
		}
		cs, err = h.p.DetectDeletions(h.ctx, scope(), def, cs1.Next)
		if err == nil {
			t.Error("a final probe that failed was ignored")
		}
		noChange(t, cs)
	})

	t.Run("DetectDeletions tombstones what the rules no longer admit", func(t *testing.T) {
		h, _, cs1 := seeded(t)
		strict := h.def(func(d *ap.RAGSource) {
			d.Exclude = []ap.GlobPattern{"e.md"}
			d.Formats = []ap.MimeType{source.MIMEJSON}
		})
		cs, err := h.p.DetectDeletions(h.ctx, scope(), strict, cs1.Next)
		if err != nil {
			t.Fatal(err)
		}
		got := tombstoneReasons(cs.Deleted)
		if got["e.md"] != source.ReasonExcluded || got["a.md"] != source.ReasonFormat || len(got) != 5 {
			t.Errorf("tombstones = %v", got)
		}
	})

	t.Run("Stat, FetchVersion and Open say not found only when the root answers", func(t *testing.T) {
		h, def, _ := seeded(t)
		h.api.remove("b.md")
		for name, call := range map[string]func() error{
			"Stat":         func() error { _, err := h.p.Stat(h.ctx, scope(), def, "b.md"); return err },
			"FetchVersion": func() error { _, err := h.p.FetchVersion(h.ctx, scope(), def, "b.md"); return err },
			"Open":         func() error { _, err := h.p.Open(h.ctx, scope(), def, "b.md"); return err },
		} {
			if err := call(); !errors.Is(err, source.ErrNotFound) {
				t.Errorf("%s: %v, want ErrNotFound", name, err)
			}
		}
		h.api.listHook = func(w http.ResponseWriter, r *http.Request, page int) bool {
			http.Error(w, "boom", 500)
			return true
		}
		for name, call := range map[string]func() error{
			"Stat":         func() error { _, err := h.p.Stat(h.ctx, scope(), def, "b.md"); return err },
			"FetchVersion": func() error { _, err := h.p.FetchVersion(h.ctx, scope(), def, "b.md"); return err },
			"Open":         func() error { _, err := h.p.Open(h.ctx, scope(), def, "b.md"); return err },
		} {
			err := call()
			if errors.Is(err, source.ErrNotFound) || contracts.CodeOf(err) != contracts.ErrUnavailable {
				t.Errorf("%s: %v, want unavailable and not a claim of absence", name, err)
			}
		}
	})
}
