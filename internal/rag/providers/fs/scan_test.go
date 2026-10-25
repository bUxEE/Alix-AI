package fs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// listFS records which directories a scan lists and can make some of them fail.
type listFS struct {
	iofs.FS
	mu     *sync.Mutex
	listed *[]string
	fail   map[string]bool
}

func (l listFS) ReadDir(name string) ([]iofs.DirEntry, error) {
	l.mu.Lock()
	*l.listed = append(*l.listed, name)
	l.mu.Unlock()
	if l.fail[name] {
		return nil, &iofs.PathError{Op: "readdirent", Path: name, Err: iofs.ErrPermission}
	}
	return iofs.ReadDir(l.FS, name)
}

func (h *harness) watchListing(fail ...string) *[]string {
	var listed []string
	var mu sync.Mutex
	failing := map[string]bool{}
	for _, f := range fail {
		failing[f] = true
	}
	h.p.wrapFS = func(base iofs.FS) iofs.FS { return listFS{FS: base, mu: &mu, listed: &listed, fail: failing} }
	return &listed
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return source.HashPrefix + hex.EncodeToString(sum[:])
}

func TestIncludeExcludeRules(t *testing.T) {
	setup := func(t *testing.T, fold bool, mutate func(*ap.RAGSource)) (*harness, source.Definition) {
		h := newHarness(t, func(c *Config) { c.CaseInsensitive = &fold })
		for rel, body := range map[string]string{
			"docs/a.md":           "a",
			"docs/draft-b.md":     "b",
			"docs/private/c.md":   "c",
			"docs/sub/d.md":       "d",
			"docs/sub/e.pdf":      "not markdown",
			"notes.txt":           "n",
			"sub/notes.txt":       "n2",
			"archive/old.md":      "o",
			"deep/x/y/z.md":       "z",
			"deep/x/private/q.md": "q",
			"draft-top.txt":       "t",
			"docs/draft-dir/f.md": "f",
			"other.md":            "x",
		} {
			h.tr.write(rel, body)
		}
		return h, h.tr.def(func(d *ap.RAGSource) {
			d.Include = []ap.GlobPattern{"docs/**/*.md", "*.txt", "deep/**"}
			d.Exclude = []ap.GlobPattern{"**/draft-*", "**/private/**"}
			d.Formats = []ap.MimeType{source.MIMEText, source.MIMEMarkdown}
			if mutate != nil {
				mutate(d)
			}
		})
	}

	t.Run("include and exclude", func(t *testing.T) {
		h, def := setup(t, false, nil)
		listed := h.watchListing()
		cs := h.discover(def, nil)
		want := []string{"deep/x/y/z.md", "docs/a.md", "docs/sub/d.md", "notes.txt"}
		if got := keysOf(cs.Added); !equalStrings(got, want) {
			t.Errorf("added = %v, want %v", got, want)
		}
		// docs/sub/e.pdf matches no include ("*.md" only) and so is neither
		// counted nor opened; the excluded and the not-included files leave no
		// trace at all.
		if cs.Counts.Discovered != 4 || cs.Counts.Skipped != 0 || cs.Counts.Failed != 0 {
			t.Errorf("counts = %+v", cs.Counts)
		}
		var openedKeys []string
		for _, o := range h.opened() {
			rel, _ := h.p.paths.Rel(h.tr.root, o)
			openedKeys = append(openedKeys, rel)
		}
		if !sameSet(openedKeys, want) {
			t.Errorf("opened %v, want exactly the accepted files %v", openedKeys, want)
		}
		// Directories that cannot hold an eligible file are not even listed.
		for _, pruned := range []string{"archive", "docs/private", "docs/draft-dir", "deep/x/private", "sub"} {
			for _, l := range *listed {
				if l == pruned {
					t.Errorf("listed %q, which no include reaches or an exclude removes", pruned)
				}
			}
		}
		for _, entered := range []string{".", "docs", "docs/sub", "deep", "deep/x", "deep/x/y"} {
			found := false
			for _, l := range *listed {
				found = found || l == entered
			}
			if !found {
				t.Errorf("did not list %q", entered)
			}
		}
	})

	t.Run("exclusion wins over inclusion", func(t *testing.T) {
		h, def := setup(t, false, func(d *ap.RAGSource) {
			d.Include = []ap.GlobPattern{"**"}
			d.Exclude = []ap.GlobPattern{"docs/**", "*.txt", "other.md"}
		})
		cs := h.discover(def, nil)
		want := []string{"archive/old.md", "deep/x/private/q.md", "deep/x/y/z.md", "sub/notes.txt"}
		if got := keysOf(cs.Added); !equalStrings(got, want) {
			t.Errorf("added = %v, want %v", got, want)
		}
	})

	t.Run("a directory named by an include does not include its contents", func(t *testing.T) {
		h, def := setup(t, false, func(d *ap.RAGSource) { d.Include = []ap.GlobPattern{"docs"} })
		cs := h.discover(def, nil)
		if len(cs.Added) != 0 || cs.Counts.Discovered != 0 {
			t.Errorf(`include "docs" admitted %v (counts %+v); only "docs/**" does`, keysOf(cs.Added), cs.Counts)
		}
	})

	t.Run("case folding decides whether Private matches the exclude private", func(t *testing.T) {
		for _, fold := range []bool{false, true} {
			h := newHarness(t, func(c *Config) { c.CaseInsensitive = &fold })
			h.tr.write("Secret/x.md", "x")
			h.tr.write("open/y.md", "y")
			def := h.tr.def(func(d *ap.RAGSource) { d.Exclude = []ap.GlobPattern{"secret/**"} })
			got := keysOf(h.discover(def, nil).Added)
			want := []string{"Secret/x.md", "open/y.md"}
			if fold {
				want = []string{"open/y.md"}
			}
			if !equalStrings(got, want) {
				t.Errorf("fold=%v: added = %v, want %v", fold, got, want)
			}
			// By key, too: the spelling does not matter when folding.
			_, err := h.p.Stat(h.ctx, scope(), def, "Secret/x.md")
			if fold && !errors.Is(err, source.ErrNotAdmitted) {
				t.Errorf("fold=true: Stat(Secret/x.md) = %v, want ErrNotAdmitted", err)
			}
			if !fold && err != nil {
				t.Errorf("fold=false: Stat(Secret/x.md) = %v", err)
			}
		}
	})

	t.Run("a folder with no include rule is not a source", func(t *testing.T) {
		h := newHarness(t)
		def := h.tr.def()
		def.Include = nil
		if _, err := h.p.Discover(h.ctx, scope(), def, nil); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("Discover with no include rule = %v, want validation_failed", err)
		}
	})
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		m[s]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}

// SEC-013: an oversized item is rejected from its size before it is opened, a
// source that outgrows its declared limits stops with resource_exhausted
// instead of being truncated, and the MIME type comes from the bytes.
func TestSizeAndMIMELimits(t *testing.T) {
	t.Run("a file over max_file_bytes is rejected before it is opened", func(t *testing.T) {
		h := newHarness(t)
		h.tr.write("small.md", strings.Repeat("s", 100))
		h.tr.write("exact.md", strings.Repeat("e", 1024))
		h.tr.write("big.md", strings.Repeat("b", 1025))
		h.tr.write("huge.md", strings.Repeat("h", 512*1024))
		def := h.tr.def(func(d *ap.RAGSource) { d.Limits.MaxFileBytes = 1024 })
		cs := h.discover(def, nil)
		if got := keysOf(cs.Added); !equalStrings(got, []string{"exact.md", "small.md"}) {
			t.Errorf("added = %v", got)
		}
		skipped := skipKeys(cs.Skipped)
		if skipped["big.md"] != source.ReasonTooLarge || skipped["huge.md"] != source.ReasonTooLarge || len(skipped) != 2 {
			t.Errorf("skipped = %v", skipped)
		}
		if cs.Counts.Discovered != 4 || cs.Counts.Skipped != 2 || cs.Counts.Hashed != 2 {
			t.Errorf("counts = %+v", cs.Counts)
		}
		for _, o := range h.opened() {
			if rel, _ := h.p.paths.Rel(h.tr.root, o); rel == "big.md" || rel == "huge.md" {
				t.Errorf("opened %s although its size is over the limit", rel)
			}
		}

		h.resetOpens()
		for _, key := range []string{"big.md", "huge.md"} {
			for _, r := range h.tryAll(def, key) {
				if !errors.Is(r.err, source.ErrNotAdmitted) {
					t.Errorf("%s(%q) = %v, want ErrNotAdmitted", r.op, key, r.err)
				}
			}
		}
		if got := h.opened(); len(got) != 0 {
			t.Errorf("per-item calls opened %v", got)
		}
	})

	t.Run("a file that grows after the check cannot be read past the limit", func(t *testing.T) {
		h := newHarness(t)
		p := h.tr.write("grow.md", strings.Repeat("g", 1000))
		def := h.tr.def(func(d *ap.RAGSource) { d.Limits.MaxFileBytes = 1024 })
		c, err := h.p.Open(h.ctx, scope(), def, "grow.md")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(strings.Repeat("x", 5000)); err != nil {
			t.Fatal(err)
		}
		f.Close()
		got, err := io.ReadAll(c)
		if contracts.CodeOf(err) != contracts.ErrResourceExhausted {
			t.Errorf("ReadAll error = %v, want resource_exhausted", err)
		}
		if len(got) > 1024 {
			t.Errorf("delivered %d bytes, over the 1024 limit", len(got))
		}
	})

	t.Run("a file exactly at the limit is delivered whole", func(t *testing.T) {
		h := newHarness(t)
		h.tr.write("exact.md", strings.Repeat("e", 1024))
		def := h.tr.def(func(d *ap.RAGSource) { d.Limits.MaxFileBytes = 1024 })
		c, err := h.p.Open(h.ctx, scope(), def, "exact.md")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		got, err := io.ReadAll(c)
		if err != nil || len(got) != 1024 {
			t.Errorf("ReadAll = %d bytes, %v", len(got), err)
		}
	})

	t.Run("more files than max_files stops the scan and applies nothing", func(t *testing.T) {
		h := newHarness(t)
		for _, n := range []string{"a.md", "b.md", "c.md"} {
			h.tr.write(n, "x")
		}
		def := h.tr.def(func(d *ap.RAGSource) { d.Limits.MaxFiles = 2 })
		cs, err := h.p.Discover(h.ctx, scope(), def, nil)
		if contracts.CodeOf(err) != contracts.ErrResourceExhausted {
			t.Fatalf("error = %v, want resource_exhausted", err)
		}
		if len(cs.Added)+len(cs.Modified)+len(cs.Deleted)+len(cs.Next) != 0 || cs.Changed() {
			t.Errorf("a failed scan returned changes: %+v", cs)
		}
		var ce *contracts.Error
		if !errors.As(err, &ce) || ce.Details["limit"] != "max_files" {
			t.Errorf("error details = %+v", ce)
		}
		// Exactly at the limit is fine.
		def.Limits.MaxFiles = 3
		if cs := h.discover(def, nil); len(cs.Added) != 3 {
			t.Errorf("at the limit: added = %v", keysOf(cs.Added))
		}
	})

	t.Run("more bytes than max_total_bytes stops the scan and applies nothing", func(t *testing.T) {
		h := newHarness(t)
		for _, n := range []string{"a.md", "b.md", "c.md"} {
			h.tr.write(n, "12345")
		}
		def := h.tr.def(func(d *ap.RAGSource) { d.Limits.MaxTotalBytes = 14 })
		cs, err := h.p.Discover(h.ctx, scope(), def, nil)
		if contracts.CodeOf(err) != contracts.ErrResourceExhausted {
			t.Fatalf("error = %v, want resource_exhausted", err)
		}
		if cs.Changed() || len(cs.Next) != 0 {
			t.Errorf("a failed scan returned changes: %+v", cs)
		}
		def.Limits.MaxTotalBytes = 15
		if cs := h.discover(def, nil); len(cs.Added) != 3 || cs.Counts.Bytes != 15 {
			t.Errorf("at the limit: added = %v, counts %+v", keysOf(cs.Added), cs.Counts)
		}
	})

	t.Run("the type comes from the bytes", func(t *testing.T) {
		pdf := "%PDF-1.7\n%\xe2\xe3\xcf\xd3\n1 0 obj\n<< >>\nendobj\n"
		h := newHarness(t)
		for rel, body := range map[string]string{
			"ok.md":       "# A heading\n\nBody.\n",
			"real.pdf":    pdf,
			"evil.md":     pdf,                                           // a PDF named .md
			"fake.pdf":    "just some plain text\n",                      // text named .pdf
			"noext":       pdf,                                           // no name to claim anything
			"notes.txt":   "plain notes\n",                               // a type the source does not list
			"pic.png":     "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00", //
			"tool.bin":    "\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00",
			"nulbyte.md":  "text then\x00 a NUL byte",
			"empty.md":    "",
			"liar.txt":    pdf, // a PDF named .txt
			"archive.zip": "PK\x03\x04\x14\x00\x00\x00\x08\x00",
		} {
			h.tr.write(rel, body)
		}
		def := h.tr.def(func(d *ap.RAGSource) {
			d.Formats = []ap.MimeType{source.MIMEMarkdown, source.MIMEPDF}
		})
		cs := h.discover(def, nil)

		gotMIME := map[string]string{}
		for _, it := range cs.Added {
			gotMIME[it.Key] = it.Version.MIME
		}
		wantMIME := map[string]string{"ok.md": source.MIMEMarkdown, "real.pdf": source.MIMEPDF, "noext": source.MIMEPDF, "empty.md": source.MIMEMarkdown}
		if diff := cmp.Diff(wantMIME, gotMIME); diff != "" {
			t.Errorf("accepted types (-want +got):\n%s", diff)
		}
		wantSkip := map[string]source.Reason{
			"evil.md":     source.ReasonMIMEMismatch,
			"fake.pdf":    source.ReasonMIMEMismatch,
			"liar.txt":    source.ReasonMIMEMismatch,
			"nulbyte.md":  source.ReasonMIMEMismatch,
			"notes.txt":   source.ReasonFormat,
			"pic.png":     source.ReasonFormat,
			"tool.bin":    source.ReasonFormat,
			"archive.zip": source.ReasonFormat,
		}
		if diff := cmp.Diff(wantSkip, skipKeys(cs.Skipped)); diff != "" {
			t.Errorf("skipped (-want +got):\n%s", diff)
		}
		// A skipped item was sniffed but not read through to a hash.
		if cs.Counts.Hashed != 4 {
			t.Errorf("hashed %d files, want 4: only accepted items are read through", cs.Counts.Hashed)
		}

		for _, key := range []string{"evil.md", "fake.pdf", "liar.txt", "nulbyte.md", "notes.txt", "pic.png", "tool.bin"} {
			for _, r := range h.tryAll(def, key) {
				if r.op == "Stat" {
					continue // metadata only; the type is not read
				}
				if !errors.Is(r.err, source.ErrNotAdmitted) {
					t.Errorf("%s(%q) = %v, want ErrNotAdmitted", r.op, key, r.err)
				}
			}
		}
		c, err := h.p.Open(h.ctx, scope(), def, "noext")
		if err != nil || c.MIME != source.MIMEPDF {
			t.Fatalf("Open(noext) = %+v, %v", c, err)
		}
		c.Close()
		v, err := h.p.FetchVersion(h.ctx, scope(), def, "ok.md")
		if err != nil || v.MIME != source.MIMEMarkdown || v.Hash != sha("# A heading\n\nBody.\n") {
			t.Errorf("FetchVersion(ok.md) = %+v, %v", v, err)
		}
	})
}

func TestChangeDetectionByHash(t *testing.T) {
	h := newHarness(t)
	h.tr.write("a.md", "alpha")
	h.tr.write("b.md", "bravo")
	h.tr.write("c.md", "charlie")
	def := h.tr.def()

	// First scan: everything is new and read.
	cs1 := h.discover(def, nil)
	if got := keysOf(cs1.Added); !equalStrings(got, []string{"a.md", "b.md", "c.md"}) || cs1.Counts.Hashed != 3 || cs1.Counts.Unchanged != 0 {
		t.Fatalf("first scan: added %v, counts %+v", got, cs1.Counts)
	}
	for _, it := range cs1.Added {
		body, _ := os.ReadFile(h.tr.abs(it.Key))
		if it.Version.Hash != sha(string(body)) || it.Version.Size != int64(len(body)) || it.Version.MIME != source.MIMEMarkdown || !it.Version.ModTime.Equal(oldTime) {
			t.Errorf("version of %s = %+v", it.Key, it.Version)
		}
	}
	if len(cs1.Next) != 3 || !cs1.Next[0].SeenAt.Equal(clockStart) {
		t.Fatalf("next = %+v", cs1.Next)
	}

	// Nothing changed: the size-and-time shortcut reads no file at all.
	h.clk.Advance(time.Hour)
	h.resetOpens()
	cs2 := h.discover(def, cs1.Next)
	if cs2.Changed() || cs2.Counts.Hashed != 0 || cs2.Counts.Unchanged != 3 || len(h.opened()) != 0 {
		t.Errorf("untouched tree: counts %+v, opened %v", cs2.Counts, h.opened())
	}
	if diff := cmp.Diff(cs1.Next, cs2.Next); diff != "" {
		t.Errorf("an untouched tree changes the snapshot (-before +after):\n%s", diff)
	}

	// Touched, bytes identical: the new time is hashed once, found to be the
	// same content, and reported unchanged; the refreshed record is trusted by
	// the next scan again.
	newer := oldTime.Add(48 * time.Hour)
	h.tr.setTime("b.md", newer)
	cs3 := h.discover(def, cs2.Next)
	if cs3.Changed() || len(cs3.Modified) != 0 || cs3.Counts.Hashed != 1 || cs3.Counts.Unchanged != 3 {
		t.Errorf("touch: changed=%v counts %+v", cs3.Changed(), cs3.Counts)
	}
	for _, r := range cs3.Next {
		if r.Key == "b.md" && (!r.ModTime.Equal(newer) || r.Hash != sha("bravo")) {
			t.Errorf("touched record = %+v", r)
		}
	}
	cs4 := h.discover(def, cs3.Next)
	if cs4.Counts.Hashed != 0 || cs4.Changed() {
		t.Errorf("after the refresh the shortcut applies again: %+v", cs4.Counts)
	}

	// Same size, different bytes, new time: modified.
	h.tr.write("b.md", "brAvo")
	h.tr.setTime("b.md", newer.Add(time.Hour))
	cs5 := h.discover(def, cs4.Next)
	if got := keysOf(cs5.Modified); !equalStrings(got, []string{"b.md"}) || len(cs5.Added) != 0 || cs5.Counts.Hashed != 1 {
		t.Fatalf("same-size edit: modified %v, counts %+v", got, cs5.Counts)
	}
	if cs5.Modified[0].Version.Hash != sha("brAvo") {
		t.Errorf("modified version = %+v", cs5.Modified[0].Version)
	}

	// Different size: modified.
	h.tr.write("c.md", "charlie and more")
	h.tr.setTime("c.md", newer)
	cs6 := h.discover(def, cs5.Next)
	if got := keysOf(cs6.Modified); !equalStrings(got, []string{"c.md"}) || cs6.Modified[0].Version.Size != int64(len("charlie and more")) {
		t.Errorf("size change: modified %v", got)
	}

	// A rewrite whose size and time did not move is not seen by the shortcut:
	// that is the price of not reading every file on every scan.
	h.tr.write("a.md", "ALPHA") // same size; write() restores oldTime
	cs7 := h.discover(def, cs6.Next)
	if cs7.Changed() || cs7.Counts.Hashed != 0 {
		t.Errorf("same size and time outside the racy window: changed=%v hashed=%d", cs7.Changed(), cs7.Counts.Hashed)
	}

	// ...but a record taken within the racy window of the file's time is not
	// trusted, so a second write in the same timestamp tick is still found.
	racy := h.clk.Now().Add(-time.Second)
	h.tr.write("r.md", "1111")
	h.tr.setTime("r.md", racy)
	cs8 := h.discover(def, cs7.Next)
	if got := keysOf(cs8.Added); !equalStrings(got, []string{"r.md"}) {
		t.Fatalf("racy file: added %v", got)
	}
	h.tr.write("r.md", "2222")
	h.tr.setTime("r.md", racy) // same size, same time
	cs9 := h.discover(def, cs8.Next)
	if got := keysOf(cs9.Modified); !equalStrings(got, []string{"r.md"}) {
		t.Errorf("a same-tick rewrite was missed: modified %v, counts %+v", got, cs9.Counts)
	}

	// Once the clock has moved past the window, the record is trusted.
	h.clk.Advance(time.Hour)
	cs10 := h.discover(def, cs9.Next)
	if cs10.Counts.Hashed != 1 { // r.md is re-hashed once more (its record was still racy) ...
		t.Errorf("hashed %d, want 1", cs10.Counts.Hashed)
	}
	cs11 := h.discover(def, cs10.Next)
	if cs11.Counts.Hashed != 0 { // ... and then trusted
		t.Errorf("hashed %d, want 0", cs11.Counts.Hashed)
	}
}

// SEC-017: an item that left the source becomes a tombstone, with its last
// version and the declaration's purge window; what a scan could not see is
// never tombstoned.
func TestDeletionProducesTombstone(t *testing.T) {
	h := newHarness(t)
	h.tr.write("a.md", "alpha")
	h.tr.write("b.md", "bravo")
	h.tr.write("sub/c.md", "charlie")
	h.tr.write("notes.txt", "plain notes")
	def := h.tr.def() // purge_after_hours 24
	cs1 := h.discover(def, nil)
	last := map[string]source.Version{}
	for _, it := range cs1.Added {
		last[it.Key] = it.Version
	}

	t.Run("a removed file", func(t *testing.T) {
		h.clk.Advance(10 * time.Minute)
		h.tr.remove("b.md")
		cs := h.discover(def, cs1.Next)
		if len(cs.Deleted) != 1 {
			t.Fatalf("deleted = %+v", cs.Deleted)
		}
		ts := cs.Deleted[0]
		if ts.Key != "b.md" || ts.Reason != source.ReasonDeleted || !ts.At.Equal(h.clk.Now()) || !ts.PurgeAfter.Equal(h.clk.Now().Add(24*time.Hour)) {
			t.Errorf("tombstone = %+v", ts)
		}
		if ts.Last != last["b.md"] {
			t.Errorf("tombstone last version = %+v, want %+v", ts.Last, last["b.md"])
		}
		if cs.Counts.Tombstoned != 1 || cs.Counts.Unchanged != 3 {
			t.Errorf("counts = %+v", cs.Counts)
		}
		if got := recordKeys(cs.Next); !equalStrings(got, []string{"a.md", "notes.txt", "sub/c.md"}) {
			t.Errorf("next = %v: a tombstoned item leaves the live snapshot", got)
		}
		// A tombstone is emitted once: the caller's next snapshot no longer
		// holds the item.
		again := h.discover(def, cs.Next)
		if len(again.Deleted) != 0 || again.Changed() {
			t.Errorf("second scan: %+v", again.Deleted)
		}
		// And it comes back as an addition if the file reappears.
		h.tr.write("b.md", "bravo")
		back := h.discover(def, again.Next)
		if got := keysOf(back.Added); !equalStrings(got, []string{"b.md"}) || len(back.Deleted) != 0 {
			t.Errorf("reappeared: added %v deleted %v", got, back.Deleted)
		}
	})

	t.Run("a removed directory", func(t *testing.T) {
		h.tr.remove("sub")
		cs := h.discover(def, cs1.Next)
		got := tombstoneReasons(cs.Deleted)
		if got["sub/c.md"] != source.ReasonDeleted {
			t.Errorf("tombstones = %v", got)
		}
		h.tr.write("sub/c.md", "charlie")
	})

	t.Run("a directory replaced by a file", func(t *testing.T) {
		h.tr.remove("sub")
		h.tr.write("sub", "now a file")
		cs := h.discover(def, cs1.Next)
		if got := tombstoneReasons(cs.Deleted); got["sub/c.md"] != source.ReasonDeleted {
			t.Errorf("tombstones = %v", got)
		}
		h.tr.remove("sub")
		h.tr.write("sub/c.md", "charlie")
	})

	t.Run("an item the rules no longer admit", func(t *testing.T) {
		excl := h.tr.def(func(d *ap.RAGSource) { d.Exclude = []ap.GlobPattern{"a.md", "sub/**"} })
		cs := h.discover(excl, cs1.Next)
		got := tombstoneReasons(cs.Deleted)
		if len(got) != 2 || got["a.md"] != source.ReasonExcluded || got["sub/c.md"] != source.ReasonExcluded {
			t.Errorf("tombstones = %v", got)
		}
		if k := recordKeys(cs.Next); !equalStrings(k, []string{"b.md", "notes.txt"}) {
			t.Errorf("next = %v", k)
		}
	})

	t.Run("an item whose format is no longer accepted", func(t *testing.T) {
		md := h.tr.def(func(d *ap.RAGSource) { d.Formats = []ap.MimeType{source.MIMEMarkdown} })
		cs := h.discover(md, cs1.Next)
		if got := tombstoneReasons(cs.Deleted); len(got) != 1 || got["notes.txt"] != source.ReasonFormat {
			t.Errorf("tombstones = %v", got)
		}
		if cs.Counts.Skipped != 1 {
			t.Errorf("counts = %+v", cs.Counts)
		}
	})

	t.Run("an item that outgrew the size limit", func(t *testing.T) {
		small := h.tr.def(func(d *ap.RAGSource) { d.Limits.MaxFileBytes = 5 })
		cs := h.discover(small, cs1.Next)
		got := tombstoneReasons(cs.Deleted)
		if got["sub/c.md"] != source.ReasonTooLarge || got["notes.txt"] != source.ReasonTooLarge || got["a.md"] != "" {
			t.Errorf("tombstones = %v", got)
		}
	})

	t.Run("an item whose bytes stopped matching its name", func(t *testing.T) {
		h.tr.write("a.md", "%PDF-1.7\n1 0 obj\n")
		h.tr.setTime("a.md", oldTime.Add(time.Hour))
		cs := h.discover(def, cs1.Next)
		if got := tombstoneReasons(cs.Deleted); got["a.md"] != source.ReasonMIMEMismatch || len(cs.Modified) != 0 {
			t.Errorf("tombstones = %v, modified %v", got, keysOf(cs.Modified))
		}
		h.tr.write("a.md", "alpha")
	})

	t.Run("a directory that cannot be listed hides its items but does not delete them", func(t *testing.T) {
		h.watchListing("sub")
		defer func() { h.p.wrapFS = nil }()
		h.tr.remove("b.md")
		// Even if the item is in fact gone, a scan that could not list its
		// directory does not conclude that: it cannot see the directory.
		h.tr.remove("sub/c.md")
		cs := h.discover(def, cs1.Next)
		if len(cs.Failed) != 1 || cs.Failed[0].Key != "sub" || contracts.CodeOf(cs.Failed[0].Err) != contracts.ErrUnavailable {
			t.Errorf("failed = %+v", cs.Failed)
		}
		got := tombstoneReasons(cs.Deleted)
		if _, bad := got["sub/c.md"]; bad {
			t.Error("an unreadable directory produced a tombstone for what is in it")
		}
		if got["b.md"] != source.ReasonDeleted {
			t.Errorf("the readable part of the tree still reports deletions: %v", got)
		}
		kept := false
		for _, r := range cs.Next {
			kept = kept || r.Key == "sub/c.md"
		}
		if !kept {
			t.Error("sub/c.md lost its record")
		}
		h.tr.write("b.md", "bravo")
		h.tr.write("sub/c.md", "charlie")
	})

	t.Run("an unreadable file keeps its record", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs POSIX permissions and a non-root user")
		}
		p := h.tr.abs("a.md")
		h.tr.setTime("a.md", oldTime.Add(2*time.Hour))
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(p, 0o644)
		cs := h.discover(def, cs1.Next)
		if len(cs.Failed) != 1 || cs.Failed[0].Key != "a.md" || len(cs.Deleted) != 0 {
			t.Errorf("failed %+v deleted %+v", cs.Failed, cs.Deleted)
		}
		if k := recordKeys(cs.Next); len(k) != 4 {
			t.Errorf("next = %v", k)
		}
	})

	t.Run("DetectDeletions inspects only the previous keys and reads nothing", func(t *testing.T) {
		h.tr.remove("sub/c.md")
		h.resetOpens()
		listed := h.watchListing()
		defer func() { h.p.wrapFS = nil }()
		cs, err := h.p.DetectDeletions(h.ctx, scope(), def, cs1.Next)
		if err != nil {
			t.Fatal(err)
		}
		if got := tombstoneReasons(cs.Deleted); len(got) != 1 || got["sub/c.md"] != source.ReasonDeleted {
			t.Errorf("tombstones = %v", got)
		}
		if len(h.opened()) != 0 || len(*listed) != 0 {
			t.Errorf("opened %v, listed %v", h.opened(), *listed)
		}
		if cs.Counts.Tombstoned != 1 || cs.Counts.Unchanged != 3 || len(cs.Next) != 3 || len(cs.Added) != 0 {
			t.Errorf("result = %+v", cs)
		}
		h.tr.write("sub/c.md", "charlie")
	})

	t.Run("deletion at source removal", func(t *testing.T) {
		ts := source.TombstonesForRemoval(def, cs1.Next, h.clk.Now())
		if len(ts) != 4 || ts[0].Reason != source.ReasonSourceRemoved || !ts[0].PurgeAfter.Equal(h.clk.Now().Add(24*time.Hour)) {
			t.Errorf("tombstones = %+v", ts)
		}
	})
}

// SEC-017: an unreachable root is reported unavailable and never as checked,
// and it never turns into deletions.
func TestUnreachableRootReportedUnavailable(t *testing.T) {
	assertUnavailable := func(t *testing.T, h *harness, def source.Definition, label string) {
		t.Helper()
		hl, err := h.p.Probe(h.ctx, scope(), def)
		if err != nil {
			t.Fatalf("%s: Probe error = %v (an unreachable root is a Health, not an error)", label, err)
		}
		if hl.Availability != source.Unavailable || hl.Reachable() || !hl.CheckedAt.IsZero() || hl.Err == nil || hl.Err.Code != contracts.ErrUnavailable || !hl.Err.Retryable {
			t.Errorf("%s: health = %+v, want unavailable, not checked, with an unavailable error", label, hl)
		}
		cs, err := h.p.Discover(h.ctx, scope(), def, nil)
		if contracts.CodeOf(err) != contracts.ErrUnavailable || errors.Is(err, source.ErrNotFound) {
			t.Errorf("%s: Discover error = %v, want unavailable", label, err)
		}
		if cs.Changed() || len(cs.Next) != 0 {
			t.Errorf("%s: Discover returned changes: %+v", label, cs)
		}
		if _, err := h.p.DetectDeletions(h.ctx, scope(), def, nil); contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Errorf("%s: DetectDeletions error = %v", label, err)
		}
		_, err1 := h.p.Stat(h.ctx, scope(), def, "a.md")
		_, err2 := h.p.FetchVersion(h.ctx, scope(), def, "a.md")
		_, err3 := h.p.Open(h.ctx, scope(), def, "a.md")
		for _, err := range []error{err1, err2, err3} {
			if contracts.CodeOf(err) != contracts.ErrUnavailable || errors.Is(err, source.ErrNotFound) {
				t.Errorf("%s: per-item error = %v, want unavailable and not 'not found'", label, err)
			}
		}
	}

	t.Run("a root that does not exist", func(t *testing.T) {
		h := newHarness(t)
		def := h.tr.def()
		def.Root.Path = filepath.Join(h.tr.base, "never-existed")
		assertUnavailable(t, h, def, "missing root")
	})

	t.Run("a root that is a file", func(t *testing.T) {
		h := newHarness(t)
		f := h.tr.put(h.tr.base, "file.txt", "x")
		def := h.tr.def()
		def.Root.Path = f
		assertUnavailable(t, h, def, "file root")
	})

	t.Run("a relative root is not resolved against the working directory", func(t *testing.T) {
		h := newHarness(t)
		def := h.tr.def()
		def.Root.Path = "root"
		assertUnavailable(t, h, def, "relative root")
	})

	t.Run("a reachable root is checked", func(t *testing.T) {
		h := newHarness(t)
		h.tr.write("a.md", "a")
		hl, err := h.p.Probe(h.ctx, scope(), h.tr.def())
		if err != nil || !hl.Reachable() || hl.Availability != source.Available || !hl.CheckedAt.Equal(clockStart) || hl.Err != nil {
			t.Errorf("health = %+v, %v", hl, err)
		}
	})

	t.Run("a root removed between two syncs yields no tombstones", func(t *testing.T) {
		h := newHarness(t)
		h.tr.write("a.md", "alpha")
		h.tr.write("sub/b.md", "bravo")
		def := h.tr.def()
		cs1 := h.discover(def, nil)
		if len(cs1.Next) != 2 {
			t.Fatalf("setup: %+v", cs1)
		}
		if err := os.RemoveAll(h.tr.root); err != nil {
			t.Fatal(err)
		}
		cs, err := h.p.Discover(h.ctx, scope(), def, cs1.Next)
		if contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Fatalf("Discover error = %v, want unavailable", err)
		}
		if len(cs.Deleted) != 0 || cs.Changed() || len(cs.Next) != 0 || cs.Counts.Tombstoned != 0 {
			t.Errorf("the missing root was read as deletions: %+v", cs)
		}
		cs, err = h.p.DetectDeletions(h.ctx, scope(), def, cs1.Next)
		if contracts.CodeOf(err) != contracts.ErrUnavailable || len(cs.Deleted) != 0 {
			t.Errorf("DetectDeletions = %+v, %v", cs, err)
		}
		hl, _ := h.p.Probe(h.ctx, scope(), def)
		if hl.Reachable() || !hl.CheckedAt.IsZero() {
			t.Errorf("health = %+v", hl)
		}

		// The share comes back: the same snapshot still matches, nothing was
		// forgotten and nothing is tombstoned.
		h.tr.write("a.md", "alpha")
		h.tr.write("sub/b.md", "bravo")
		h.clk.Advance(time.Hour)
		back := h.discover(def, cs1.Next)
		if back.Changed() || back.Counts.Unchanged != 2 || back.Counts.Hashed != 0 {
			t.Errorf("after the root returned: %+v", back)
		}
	})

	t.Run("a root that vanishes during the scan is not read as a mass deletion", func(t *testing.T) {
		h := newHarness(t)
		h.tr.write("a.md", "alpha")
		h.tr.write("b.md", "bravo")
		def := h.tr.def()
		cs1 := h.discover(def, nil)
		h.tr.remove("b.md") // would be a tombstone in a healthy scan

		// The root resolves for the open at the start of the scan and fails
		// for the re-check at the end.
		real := h.p.evalSymlinks
		calls := 0
		h.p.evalSymlinks = func(p string) (string, error) {
			calls++
			if calls > 1 {
				return "", &iofs.PathError{Op: "lstat", Path: p, Err: iofs.ErrNotExist}
			}
			return real(p)
		}
		cs, err := h.p.Discover(h.ctx, scope(), def, cs1.Next)
		if contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Fatalf("error = %v, want unavailable", err)
		}
		if len(cs.Deleted) != 0 || cs.Changed() || len(cs.Next) != 0 {
			t.Errorf("a scan that lost its root returned changes: %+v", cs)
		}
	})

	t.Run("a cancelled context stops the scan with a typed error", func(t *testing.T) {
		h := newHarness(t)
		h.tr.write("a.md", "alpha")
		ctx, cancel := contextCancelled()
		defer cancel()
		_, err := h.p.Discover(ctx, scope(), h.tr.def(), nil)
		if contracts.CodeOf(err) != contracts.ErrTimeout {
			t.Errorf("error = %v, want timeout", err)
		}
	})
}

// A scope that is missing, malformed or another tenant's is refused before the
// root is touched.
func TestScopeIsCheckedBeforeAnything(t *testing.T) {
	h := newHarness(t)
	h.tr.write("a.md", "alpha")
	def := h.tr.def()
	other := scope()
	other.TenantID = "tenant-2"
	for name, sc := range map[string]contracts.Scope{
		"zero scope":   {},
		"other tenant": other,
		"wildcard":     func() contracts.Scope { s := scope(); s.WorkspaceID = "*"; return s }(),
	} {
		if _, err := h.p.Probe(h.ctx, sc, def); err == nil {
			t.Errorf("%s: Probe accepted it", name)
		}
		if _, err := h.p.Discover(h.ctx, sc, def, nil); err == nil {
			t.Errorf("%s: Discover accepted it", name)
		}
		if _, err := h.p.Stat(h.ctx, sc, def, "a.md"); err == nil {
			t.Errorf("%s: Stat accepted it", name)
		}
		if _, err := h.p.Open(h.ctx, sc, def, "a.md"); err == nil {
			t.Errorf("%s: Open accepted it", name)
		}
	}
	if got := h.opened(); len(got) != 0 {
		t.Errorf("a refused scope opened %v", got)
	}
}

func TestProviderServesHostRootsOnly(t *testing.T) {
	h := newHarness(t)
	def := h.tr.def()
	def.Root = source.Root{Kind: source.RootAPI}
	if _, err := h.p.Discover(h.ctx, scope(), def, nil); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("api root: %v", err)
	}
	def.Root = source.Root{Kind: source.RootPackage}
	if _, err := h.p.Probe(h.ctx, scope(), def); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("package root: %v", err)
	}
	if kinds := h.p.Kinds(); len(kinds) != 3 {
		t.Errorf("kinds = %v", kinds)
	}
}

func TestMountedVolumeSubPath(t *testing.T) {
	h := newHarness(t)
	h.tr.write("handbooks/2026/a.md", "alpha")
	h.tr.write("handbooks/other.md", "other")
	h.tr.write("elsewhere/x.md", "x")
	sub := ap.RelativePath("handbooks/2026")
	decl := baseDecl(h.tr.root)
	decl.Type = ap.RAGSourceTypeMountedVolume
	decl.Root = ap.RAGSourceRoot{Value: ap.RAGSourceRootMount{Kind: "mount", MountID: "nas-1", Path: &sub}}
	def, err := source.FromDeclaration(decl, source.Binding{
		SourceID: ids.New(source.IDPrefix), TenantID: "tenant-1", WorkspaceID: "ws-1",
		Mounts: func(id string) (string, bool) { return h.tr.root, id == "nas-1" },
	})
	if err != nil {
		t.Fatal(err)
	}
	cs := h.discover(def, nil)
	if got := keysOf(cs.Added); !equalStrings(got, []string{"a.md"}) {
		t.Errorf("added = %v: keys are relative to the sub-path and nothing above it is listed", got)
	}
	if _, err := h.p.Stat(h.ctx, scope(), def, "../other.md"); !errors.Is(err, source.ErrEscapesRoot) {
		t.Errorf("Stat(../other.md) = %v, want ErrEscapesRoot", err)
	}
	if _, err := h.p.Stat(h.ctx, scope(), def, "a.md"); err != nil {
		t.Errorf("Stat(a.md) = %v", err)
	}

	// A sub-path that does not exist inside the mount is unreachable.
	def.Root.SubPath = "nope"
	if hl, _ := h.p.Probe(h.ctx, scope(), def); hl.Reachable() || hl.Availability != source.Unavailable {
		t.Errorf("missing sub-path health = %+v", hl)
	}
	// A mount may be a whole volume or share (the operator chose it), unlike a
	// folder declared by a package: mount the volume root and point the
	// sub-path at the tree.
	vol := "/"
	if runtime.GOOS == "windows" {
		vol = filepath.VolumeName(h.tr.root) + `\`
	}
	rel, ok := h.p.paths.Rel(vol, filepath.Join(h.tr.root, "handbooks", "2026"))
	if !ok {
		t.Fatalf("%s is not under %s", h.tr.root, vol)
	}
	def.Root.Path, def.Root.SubPath = vol, rel
	if hl, err := h.p.Probe(h.ctx, scope(), def); err != nil || !hl.Reachable() {
		t.Errorf("a mount at a volume root: health %+v, %v", hl, err)
	}
	if cs := h.discover(def, nil); !equalStrings(keysOf(cs.Added), []string{"a.md"}) {
		t.Errorf("added = %v", keysOf(cs.Added))
	}
}
