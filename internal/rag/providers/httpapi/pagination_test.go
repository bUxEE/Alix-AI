package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return source.HashPrefix + hex.EncodeToString(sum[:])
}

func openAll(t *testing.T, h *harness, def source.Definition, key string) []byte {
	t.Helper()
	c, err := h.p.Open(h.ctx, scope(), def, key)
	if err != nil {
		t.Fatalf("Open(%q): %v", key, err)
	}
	defer c.Close()
	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("reading %q: %v", key, err)
	}
	return b
}

// TestPaginationAndVersioning: a listing over several pages is read to its end
// with the cursor round-tripped; an untouched source costs list requests and no
// item fetch; a changed item is the only one fetched again and lands in
// Modified; and the version a scan reports is the hash of exactly the bytes
// Open later delivers (the ingestion pipeline re-hashes them).
func TestPaginationAndVersioning(t *testing.T) {
	h := newHarness(t, "/api/docs")
	api := h.api
	bodies := map[string]string{}
	for i, k := range []string{"a.md", "b.md", "c.md", "d.md", "e.md"} {
		bodies[k] = fmt.Sprintf("# doc %d\nbody of %s\n", i, k)
		api.put(k, bodies[k])
	}
	def := h.def()

	// First scan: five items on three pages.
	cs1 := h.discover(def, nil)
	if got, want := keysOf(cs1.Added), []string{"a.md", "b.md", "c.md", "d.md", "e.md"}; !equalStrings(got, want) {
		t.Fatalf("Added = %v, want %v", got, want)
	}
	if len(cs1.Modified)+len(cs1.Deleted)+len(cs1.Failed)+len(cs1.Skipped) != 0 {
		t.Fatalf("unexpected changes: %+v", cs1)
	}
	if n := api.count(http.MethodGet, false); n != 3 {
		t.Errorf("list requests = %d, want 3 pages", n)
	}
	if n := api.count(http.MethodGet, true); n != 5 {
		t.Errorf("item requests = %d, want 5", n)
	}
	var cursors []string
	for _, r := range api.requests() {
		if r.path == "/api/docs" {
			cursors = append(cursors, r.query.Get("cursor"))
		}
	}
	if len(cursors) != 3 || cursors[0] != "" || cursors[1] == "" || cursors[2] == "" || cursors[1] == cursors[2] {
		t.Errorf("cursors over the pages = %q, want none then two distinct ones", cursors)
	}
	for _, it := range cs1.Added {
		if it.Version.Hash != hashOf(bodies[it.Key]) {
			t.Errorf("%s: Version.Hash = %s, want the sha256 of its bytes", it.Key, it.Version.Hash)
		}
		if it.Version.Size != int64(len(bodies[it.Key])) || it.Version.MIME != source.MIMEMarkdown || !it.Version.ModTime.Equal(oldTime) {
			t.Errorf("%s: version = %+v", it.Key, it.Version)
		}
		// The ingestion invariant: what Open delivers hashes to the version.
		if got := hashOf(string(openAll(t, h, def, it.Key))); got != it.Version.Hash {
			t.Errorf("%s: Open delivers %s, the scan recorded %s", it.Key, got, it.Version.Hash)
		}
	}
	if got, want := recordKeys(cs1.Next), keysOf(cs1.Added); !equalStrings(got, want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
	if cs1.Counts.Discovered != 5 || cs1.Counts.Hashed != 5 || cs1.Counts.Unchanged != 0 {
		t.Errorf("counts = %+v", cs1.Counts)
	}

	// Second scan, nothing changed: every page is listed, no item is fetched.
	h.clk.Advance(time.Hour)
	api.reset()
	cs2 := h.discover(def, cs1.Next)
	if cs2.Changed() || cs2.Counts.Unchanged != 5 || cs2.Counts.Hashed != 0 {
		t.Errorf("an untouched source reported %+v", cs2)
	}
	if n := api.count(http.MethodGet, true) + api.count(http.MethodHead, true); n != 0 {
		t.Errorf("an untouched source cost %d item requests, want 0", n)
	}
	if n := api.count(http.MethodGet, false); n != 3 {
		t.Errorf("list requests = %d, want 3", n)
	}
	if d := cmp.Diff(cs1.Next, cs2.Next); d != "" {
		t.Errorf("the snapshot of an untouched source changed:\n%s", d)
	}

	// One item changes: only it is fetched, and it is Modified.
	api.update("c.md", "# changed\nnew body\n", oldTime.Add(48*time.Hour))
	api.reset()
	cs3 := h.discover(def, cs2.Next)
	if got := keysOf(cs3.Modified); !equalStrings(got, []string{"c.md"}) || len(cs3.Added) != 0 {
		t.Fatalf("Modified = %v Added = %v, want only c.md modified", got, keysOf(cs3.Added))
	}
	if n := api.count(http.MethodGet, true); n != 1 {
		t.Errorf("item requests = %d, want only the changed item", n)
	}
	if cs3.Modified[0].Version.Hash != hashOf("# changed\nnew body\n") || cs3.Modified[0].Version.Hash == cs1.Added[2].Version.Hash {
		t.Errorf("modified version = %+v", cs3.Modified[0].Version)
	}
	if cs3.Counts.Unchanged != 4 || cs3.Counts.Hashed != 1 {
		t.Errorf("counts = %+v", cs3.Counts)
	}

	// An addition.
	api.put("f.md", "# new\n")
	cs4 := h.discover(def, cs3.Next)
	if got := keysOf(cs4.Added); !equalStrings(got, []string{"f.md"}) || len(cs4.Modified) != 0 {
		t.Errorf("Added = %v Modified = %v, want f.md added", got, keysOf(cs4.Modified))
	}

	// The same bytes under a new version and time are fetched, found equal and
	// not reported as modified; the record gets the new time so the next scan
	// is cheap again.
	api.update("d.md", bodies["d.md"], oldTime.Add(72*time.Hour))
	api.reset()
	cs5 := h.discover(def, cs4.Next)
	if cs5.Changed() || cs5.Counts.Hashed != 1 {
		t.Errorf("same bytes, new time: %+v", cs5)
	}
	api.reset()
	if cs6 := h.discover(def, cs5.Next); cs6.Counts.Hashed != 0 || api.count(http.MethodGet, true) != 0 {
		t.Errorf("after the refresh the item should be skipped again: %+v", cs6.Counts)
	}
}

// TestPaginationShortcuts: the cheap checks that spare a fetch, and the cases
// where they must not apply.
func TestPaginationShortcuts(t *testing.T) {
	t.Run("a record inside the racy window is not trusted", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		it := h.api.put("r.md", "racy\n")
		it.mod = clockStart // changed in the same instant the scan runs
		def := h.def()
		cs1 := h.discover(def, nil)
		h.api.reset()
		cs2 := h.discover(def, cs1.Next)
		if cs2.Counts.Hashed != 1 {
			t.Errorf("a record seen at the item's own modification time must be read again, got %+v", cs2.Counts)
		}
		h.clk.Advance(time.Minute)
		h.api.reset()
		cs3 := h.discover(def, cs2.Next) // read again: its record now postdates the item by a minute
		h.clk.Advance(time.Minute)
		h.api.reset()
		cs4 := h.discover(def, cs3.Next)
		if cs4.Counts.Hashed != 0 || h.api.count(http.MethodGet, true) != 0 {
			t.Errorf("once the record is old enough the shortcut applies: %+v", cs4.Counts)
		}
	})

	t.Run("an API that lists no size or time is fetched every scan", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		it := h.api.put("n.md", "opaque\n")
		it.hideSize, it.hideMod = true, true
		def := h.def()
		cs1 := h.discover(def, nil)
		if len(cs1.Added) != 1 || !cs1.Added[0].Version.ModTime.Equal(oldTime) {
			t.Fatalf("Added = %+v (the Last-Modified header is the fallback time)", cs1.Added)
		}
		h.clk.Advance(time.Hour)
		cs2 := h.discover(def, cs1.Next)
		if cs2.Counts.Hashed != 1 || cs2.Changed() {
			t.Errorf("counts = %+v, want one fetch and no change", cs2.Counts)
		}
	})

	t.Run("a content-address version spares the fetch when it matches the record", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		body := "hashed by the application\n"
		it := h.api.put("h.md", body)
		it.version, it.hideSize, it.hideMod = hashOf(body), true, true
		def := h.def()
		cs1 := h.discover(def, nil)
		if len(cs1.Added) != 1 || cs1.Added[0].Version.Hash != hashOf(body) {
			t.Fatalf("Added = %+v", cs1.Added)
		}
		h.api.reset()
		cs2 := h.discover(def, cs1.Next)
		if cs2.Counts.Hashed != 0 || h.api.count(http.MethodGet, true) != 0 {
			t.Errorf("a matching content address should not be fetched: %+v", cs2.Counts)
		}
		// A new content address is a modification.
		h.api.update("h.md", "now different\n", oldTime)
		h.api.setVersion("h.md", hashOf("now different\n"))
		cs3 := h.discover(def, cs2.Next)
		if got := keysOf(cs3.Modified); !equalStrings(got, []string{"h.md"}) {
			t.Errorf("Modified = %v", got)
		}
	})

	t.Run("a previous type that is no longer accepted leaves retrieval", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		h.api.put("a.md", "# a\n")
		cs1 := h.discover(h.def(), nil)
		h.clk.Advance(time.Hour)
		cs2 := h.discover(h.def(func(d *ap.RAGSource) { d.Formats = []ap.MimeType{source.MIMEJSON} }), cs1.Next)
		if got := skipReasons(cs2.Skipped)["a.md"]; got != source.ReasonFormat {
			t.Errorf("skipped = %v", cs2.Skipped)
		}
		if got := tombstoneReasons(cs2.Deleted)["a.md"]; got != source.ReasonFormat {
			t.Errorf("tombstones = %v", cs2.Deleted)
		}
	})
}

// TestVersionConsistency: what the listing says about an item is checked
// against what is fetched; a mismatch means the item changed during the scan
// and is a failure of that item for that scan, never a result.
func TestVersionConsistency(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(it *fakeItem)
	}{
		{"etag differs from the listed version", func(it *fakeItem) { it.etag = `"another"` }},
		{"a weak etag of another version", func(it *fakeItem) { it.etag = `W/"another"` }},
		{"content address differs from the bytes", func(it *fakeItem) { it.version = hashOf("something else"); it.etag = "-" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "/api/docs")
			h.api.put("ok.md", "fine\n")
			tc.mutate(h.api.put("moving.md", "in flight\n"))
			cs := h.discover(h.def(), nil)
			if got := keysOf(cs.Added); !equalStrings(got, []string{"ok.md"}) {
				t.Errorf("Added = %v, want only the consistent item", got)
			}
			if len(cs.Failed) != 1 || cs.Failed[0].Key != "moving.md" {
				t.Fatalf("Failed = %+v", cs.Failed)
			}
			if r := cs.Failed[0].Err.Details["reason"]; r != ReasonVersionChanged {
				t.Errorf("reason = %v", r)
			}
		})
	}

	t.Run("a matching etag, weak or quoted or bare, is fine", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		h.api.put("q.md", "quoted\n")
		w := h.api.put("w.md", "weak\n")
		w.etag = `W/"` + w.version + `"`
		b := h.api.put("b.md", "bare\n")
		b.etag = b.version
		h.api.put("n.md", "no etag at all\n").etag = "-"
		cs := h.discover(h.def(), nil)
		if len(cs.Failed) != 0 || len(cs.Added) != 4 {
			t.Errorf("Added = %v Failed = %+v", keysOf(cs.Added), cs.Failed)
		}
	})

	t.Run("a changed item keeps its previous record", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		h.api.put("m.md", "first\n")
		cs1 := h.discover(h.def(), nil)
		h.clk.Advance(time.Hour)
		h.api.update("m.md", "second\n", oldTime.Add(time.Hour))
		h.api.items["m.md"].etag = `"stale"`
		cs2 := h.discover(h.def(), cs1.Next)
		if len(cs2.Failed) != 1 || len(cs2.Deleted) != 0 || len(cs2.Modified) != 0 {
			t.Fatalf("cs = %+v", cs2)
		}
		if d := cmp.Diff(cs1.Next, cs2.Next); d != "" {
			t.Errorf("the failed item's record must be kept:\n%s", d)
		}
	})

	t.Run("a listed size that is not the body is a failure", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		h.api.put("s.md", "twelve bytes")
		h.api.itemHook = func(w http.ResponseWriter, r *http.Request, key string) bool {
			w.Header().Set("ETag", `"v1"`)
			_, _ = w.Write([]byte("only this much and more"))
			return true
		}
		cs := h.discover(h.def(), nil)
		if len(cs.Failed) != 1 || len(cs.Added) != 0 {
			t.Errorf("cs = %+v", cs)
		}
	})
}

// TestPaginationHostile: a server that never ends its pagination, repeats a
// cursor, sends a malformed page or lists too much cannot hold or fool a scan.
func TestPaginationHostile(t *testing.T) {
	t.Run("a cursor that comes back is a loop", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		for i := range 6 {
			h.api.put(fmt.Sprintf("k%d.md", i), "x\n")
		}
		h.api.listHook = func(w http.ResponseWriter, r *http.Request, page int) bool {
			if page >= 2 {
				_, _ = io.WriteString(w, `{"items":[],"next_cursor":"same"}`)
				return true
			}
			return false
		}
		h.api.curDec = func(c string) (int, bool) { return 2, true }
		cs, err := h.p.Discover(h.ctx, scope(), h.def(), nil)
		if contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
			t.Errorf("err = %v, want schema_invalid", err)
		}
		noChange(t, cs)
	})

	t.Run("an endless pagination stops at the page limit", func(t *testing.T) {
		h := newHarness(t, "/api/docs", func(c *Config) { c.MaxPages = 5 })
		n := 0
		h.api.curDec = func(c string) (int, bool) { return 0, true }
		h.api.listHook = func(w http.ResponseWriter, r *http.Request, page int) bool {
			n++
			_, _ = fmt.Fprintf(w, `{"items":[],"next_cursor":"c%d"}`, n)
			return true
		}
		cs, err := h.p.Discover(h.ctx, scope(), h.def(), nil)
		if contracts.CodeOf(err) != contracts.ErrResourceExhausted {
			t.Errorf("err = %v, want resource_exhausted", err)
		}
		noChange(t, cs)
		if n > 5 {
			t.Errorf("the provider asked for %d pages with a limit of 5", n)
		}
	})

	t.Run("more items than the source declares is resource_exhausted and applies nothing", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		for i := range 5 {
			h.api.put(fmt.Sprintf("k%d.md", i), "x\n")
		}
		cs, err := h.p.Discover(h.ctx, scope(), h.def(func(d *ap.RAGSource) { d.Limits.MaxFiles = 3 }), nil)
		if contracts.CodeOf(err) != contracts.ErrResourceExhausted {
			t.Errorf("err = %v, want resource_exhausted", err)
		}
		noChange(t, cs)
	})

	t.Run("an API that lists far more entries than the source can hold is refused while listing", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		h.api.perPage = 5000
		for i := range 10_100 {
			it := h.api.put(fmt.Sprintf("k%05d.md", i), "x")
			it.hideSize, it.hideMod = true, true
		}
		def := h.def(func(d *ap.RAGSource) { d.Limits.MaxFiles = 1 })
		cs, err := h.p.Discover(h.ctx, scope(), def, nil)
		if contracts.CodeOf(err) != contracts.ErrResourceExhausted {
			t.Errorf("err = %v, want resource_exhausted", err)
		}
		noChange(t, cs)
		if n := h.api.count(http.MethodGet, true); n != 0 {
			t.Errorf("%d items were fetched from a listing that was already refused", n)
		}
	})

	t.Run("more bytes than the source declares is resource_exhausted", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		for i := range 3 {
			h.api.put(fmt.Sprintf("k%d.md", i), strings.Repeat("x", 100))
		}
		cs, err := h.p.Discover(h.ctx, scope(), h.def(func(d *ap.RAGSource) { d.Limits.MaxTotalBytes = 250 }), nil)
		if contracts.CodeOf(err) != contracts.ErrResourceExhausted {
			t.Errorf("err = %v, want resource_exhausted", err)
		}
		noChange(t, cs)
	})

	pages := []struct{ name, body string }{
		{"not json", `<html>oops</html>`},
		{"trailing data", `{"items":[]} {"items":[]}`},
		{"items missing", `{"next_cursor":null}`},
		{"items null", `{"items":null}`},
		{"entry without key", `{"items":[{"version":"1"}]}`},
		{"entry without version", `{"items":[{"key":"a.md"}]}`},
		{"version with a space", `{"items":[{"key":"a.md","version":"a b"}]}`},
		{"negative size", `{"items":[{"key":"a.md","version":"1","size":-1}]}`},
		{"size is not an integer", `{"items":[{"key":"a.md","version":"1","size":1.5}]}`},
		{"modified_at is not a time", `{"items":[{"key":"a.md","version":"1","modified_at":"yesterday"}]}`},
		{"cursor with a control character", "{\"items\":[],\"next_cursor\":\"a\\u0001b\"}"},
		{"cursor too long", `{"items":[],"next_cursor":"` + strings.Repeat("x", maxCursorBytes+1) + `"}`},
	}
	for _, tc := range pages {
		t.Run("malformed page: "+tc.name, func(t *testing.T) {
			h := newHarness(t, "/api/docs")
			h.api.put("a.md", "x\n")
			h.api.listHook = func(w http.ResponseWriter, r *http.Request, page int) bool {
				_, _ = io.WriteString(w, tc.body)
				return true
			}
			prev := []source.Record{{Key: "a.md", Hash: hashOf("x\n"), MIME: source.MIMEText, SeenAt: clockStart}}
			cs, err := h.p.Discover(h.ctx, scope(), h.def(), prev)
			if contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
				t.Errorf("err = %v, want schema_invalid", err)
			}
			noChange(t, cs)
		})
	}

	t.Run("extra members are ignored", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		h.api.put("a.md", "x\n")
		h.api.listHook = func(w http.ResponseWriter, r *http.Request, page int) bool {
			_, _ = io.WriteString(w, `{"items":[{"key":"a.md","version":"v1","title":"A","acl":["x"]}],"total":1,"next_cursor":""}`)
			return true
		}
		if cs := h.discover(h.def(), nil); len(cs.Added) != 1 {
			t.Errorf("Added = %v", keysOf(cs.Added))
		}
	})
}

// TestKeysAndRules: item keys are hostile input, and the include and exclude
// rules apply to them before anything is fetched.
func TestKeysAndRules(t *testing.T) {
	h := newHarness(t, "/api/docs")
	h.api.perPage = 50
	h.api.put("policies/refund.md", "refund\n")
	h.api.put("policies/private/salary.md", "secret\n")
	h.api.put("drafts/x.md", "draft\n")
	h.api.put("../escape.md", "escape\n")
	h.api.put("a/../b.md", "dots\n")
	h.api.put("/abs.md", "abs\n")
	h.api.put("back\\slash.md", "back\n")
	h.api.put("ctl\x01.md", "ctl\n")
	h.api.put("CON.md", "windows names are fine in a URL\n")
	def := h.def(func(d *ap.RAGSource) {
		d.Include = []ap.GlobPattern{"**"}
		d.Exclude = []ap.GlobPattern{"policies/private/**", "drafts/**"}
	})
	h.api.reset()
	cs := h.discover(def, nil)
	if got, want := keysOf(cs.Added), []string{"CON.md", "policies/refund.md"}; !equalStrings(got, want) {
		t.Errorf("Added = %v, want %v", got, want)
	}
	unsafe := 0
	for _, s := range cs.Skipped {
		if s.Reason == source.ReasonUnsafeName {
			unsafe++
		}
	}
	if unsafe != 5 {
		t.Errorf("skipped = %v, want 5 unsafe names", cs.Skipped)
	}
	for _, r := range h.api.requests() {
		for _, bad := range []string{"salary", "drafts", "escape", "abs.md", "slash"} {
			if strings.Contains(r.path, bad) {
				t.Errorf("an item the rules or the key checks refuse was requested: %s", r.path)
			}
		}
	}

	t.Run("an item the rules stop admitting is tombstoned as excluded", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		h.api.put("a/one.md", "1\n")
		h.api.put("b/two.md", "2\n")
		cs1 := h.discover(h.def(), nil)
		h.clk.Advance(time.Hour)
		h.api.reset()
		cs2 := h.discover(h.def(func(d *ap.RAGSource) { d.Exclude = []ap.GlobPattern{"b/**"} }), cs1.Next)
		if got := tombstoneReasons(cs2.Deleted); len(got) != 1 || got["b/two.md"] != source.ReasonExcluded {
			t.Errorf("tombstones = %v", got)
		}
		if got := recordKeys(cs2.Next); !equalStrings(got, []string{"a/one.md"}) {
			t.Errorf("Next = %v", got)
		}
	})

	t.Run("a key listed twice is ambiguous and keeps its record", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		h.api.put("a.md", "a\n")
		h.api.put("d.md", "d\n")
		cs1 := h.discover(h.def(), nil)
		h.clk.Advance(time.Hour)
		h.api.listHook = func(w http.ResponseWriter, r *http.Request, page int) bool {
			_, _ = io.WriteString(w, `{"items":[{"key":"d.md","version":"1"},{"key":"d.md","version":"2"},{"key":"a.md","version":"v1","size":2,"modified_at":"2025-01-01T08:00:00Z"}]}`)
			return true
		}
		cs2 := h.discover(h.def(), cs1.Next)
		if len(cs2.Failed) != 1 || cs2.Failed[0].Key != "d.md" || len(cs2.Deleted) != 0 {
			t.Fatalf("cs = %+v", cs2)
		}
		if got := recordKeys(cs2.Next); !equalStrings(got, []string{"a.md", "d.md"}) {
			t.Errorf("Next = %v", got)
		}
	})
}

// TestItemAccess: Stat, FetchVersion and Open follow the same rules as a scan.
func TestItemAccess(t *testing.T) {
	h := newHarness(t, "/api/docs")
	body := "# title\ntext\n"
	h.api.put("doc.md", body)
	h.api.put("rec/42", `{"id":42,"name":"Ada"}`).contentType = "application/json; charset=utf-8"
	h.api.put("pic.md", "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	h.api.put("lie.pdf", "this is plain text, not a pdf\n")
	def := h.def()

	info, err := h.p.Stat(h.ctx, scope(), def, "doc.md")
	if err != nil || info.Key != "doc.md" || info.Size != int64(len(body)) || !info.ModTime.Equal(oldTime) {
		t.Errorf("Stat = %+v, %v", info, err)
	}
	if h.api.count(http.MethodHead, true) != 1 || h.api.count(http.MethodGet, true) != 0 {
		t.Errorf("Stat must use HEAD and read no content")
	}
	v, err := h.p.FetchVersion(h.ctx, scope(), def, "doc.md")
	if err != nil || v.Hash != hashOf(body) || v.Size != int64(len(body)) || v.MIME != source.MIMEMarkdown {
		t.Errorf("FetchVersion = %+v, %v", v, err)
	}
	if got := string(openAll(t, h, def, "doc.md")); got != body {
		t.Errorf("Open = %q", got)
	}

	t.Run("the declared content type chooses among text types", func(t *testing.T) {
		c, err := h.p.Open(h.ctx, scope(), def, "rec/42")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if c.MIME != source.MIMEJSON || c.Info.Size != int64(len(`{"id":42,"name":"Ada"}`)) {
			t.Errorf("Content = %+v", c)
		}
	})

	t.Run("the bytes win over the name and the declared type", func(t *testing.T) {
		for _, key := range []string{"pic.md", "lie.pdf"} {
			if _, err := h.p.Open(h.ctx, scope(), def, key); !errors.Is(err, source.ErrNotAdmitted) {
				t.Errorf("Open(%s) = %v, want ErrNotAdmitted", key, err)
			}
			if _, err := h.p.FetchVersion(h.ctx, scope(), def, key); !errors.Is(err, source.ErrNotAdmitted) {
				t.Errorf("FetchVersion(%s) = %v, want ErrNotAdmitted", key, err)
			}
		}
		cs := h.discover(def, nil)
		got := skipReasons(cs.Skipped)
		if got["pic.md"] != source.ReasonMIMEMismatch || got["lie.pdf"] != source.ReasonMIMEMismatch {
			t.Errorf("skipped = %v", got)
		}
	})

	t.Run("keys the rules do not admit are refused before any request", func(t *testing.T) {
		h.api.reset()
		strict := h.def(func(d *ap.RAGSource) { d.Include = []ap.GlobPattern{"rec/**"} })
		if _, err := h.p.Stat(h.ctx, scope(), strict, "doc.md"); !errors.Is(err, source.ErrNotAdmitted) {
			t.Errorf("err = %v", err)
		}
		if _, err := h.p.Open(h.ctx, scope(), strict, "rec/../doc.md"); !errors.Is(err, source.ErrEscapesRoot) {
			t.Errorf("err = %v", err)
		}
		if n := len(h.api.requests()); n != 0 {
			t.Errorf("%d requests were made", n)
		}
	})

	t.Run("a server without HEAD is asked with a GET whose body is not read", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		h.api.put("a.md", "abc\n")
		h.api.itemHook = func(w http.ResponseWriter, r *http.Request, key string) bool {
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return true
			}
			return false
		}
		info, err := h.p.Stat(h.ctx, scope(), h.def(), "a.md")
		if err != nil || info.Size != 4 {
			t.Errorf("Stat = %+v, %v", info, err)
		}
	})

	t.Run("an open reader honours the context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(h.ctx)
		c, err := h.p.Open(ctx, scope(), def, "doc.md")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		cancel()
		if _, err := io.ReadAll(c); contracts.CodeOf(err) != contracts.ErrTimeout {
			t.Errorf("err = %v, want timeout", err)
		}
	})
}
