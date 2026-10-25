package httpapi

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// endless writes an unbounded chunked body until the client goes away (or a
// safety cap), and reports how much it managed to write.
func endless(written *atomic.Int64, prefix string) func(w http.ResponseWriter, r *http.Request) {
	const chunk, cap = 64 << 10, 256 << 20
	return func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		buf := bytes.Repeat([]byte("x"), chunk)
		if prefix != "" {
			n, _ := io.WriteString(w, prefix)
			written.Add(int64(n))
		}
		for written.Load() < cap {
			n, err := w.Write(buf)
			written.Add(int64(n))
			if err != nil || r.Context().Err() != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
	}
}

// TestOversizeResponseRejected (SEC-013): nothing the API sends is read past
// its limit. An oversized list page fails the scan and applies nothing; an
// oversized item is skipped (and leaves retrieval if it was indexed), never
// truncated and kept; a body of unbounded length costs at most the limit; and
// the declared limits on items and bytes end a scan with resource_exhausted.
// SEC-013
func TestOversizeResponseRejected(t *testing.T) {
	t.Run("a list page larger than the limit, by Content-Length", func(t *testing.T) {
		h := newHarness(t, "/api/docs", func(c *Config) { c.MaxPageBytes = 1024 })
		h.api.put("a.md", "a\n")
		h.api.listHook = func(w http.ResponseWriter, r *http.Request, page int) bool {
			pad := strings.Repeat(" ", 4096)
			w.Header().Set("Content-Length", strconv.Itoa(len(pad)+len(`{"items":[]}`)))
			_, _ = io.WriteString(w, `{"items":[]}`+pad)
			return true
		}
		prev := []source.Record{{Key: "a.md", Hash: hashOf("a\n"), MIME: source.MIMEText, SeenAt: clockStart}}
		cs, err := h.p.Discover(h.ctx, scope(), h.def(), prev)
		var ce *contracts.Error
		if !errors.As(err, &ce) || ce.Code != contracts.ErrSchemaInvalid || ce.Details["reason"] != ReasonResponseTooLarge {
			t.Errorf("err = %v, want schema_invalid / response_too_large", err)
		}
		noChange(t, cs) // in particular: an oversized page must not read as "everything was deleted"
	})

	t.Run("a list page of unbounded length, without Content-Length", func(t *testing.T) {
		h := newHarness(t, "/api/docs", func(c *Config) { c.MaxPageBytes = 8 << 10 })
		var written atomic.Int64
		h.api.listHook = func(w http.ResponseWriter, r *http.Request, page int) bool {
			endless(&written, `{"items":[`)(w, r)
			return true
		}
		start := time.Now()
		cs, err := h.p.Discover(h.ctx, scope(), h.def(), nil)
		var ce *contracts.Error
		if !errors.As(err, &ce) || ce.Details["reason"] != ReasonResponseTooLarge {
			t.Errorf("err = %v, want response_too_large", err)
		}
		noChange(t, cs)
		if w := written.Load(); w >= 64<<20 {
			t.Errorf("the server wrote %d bytes before the client stopped it", w)
		}
		if d := time.Since(start); d > 20*time.Second {
			t.Errorf("took %v", d)
		}
	})

	t.Run("entries with huge keys cannot add up to unbounded memory", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		h.api.curDec = func(string) (int, bool) { return 0, true }
		pages := 0
		h.api.listHook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
			pages++
			k := 0
			if c := r.URL.Query().Get("cursor"); c != "" {
				k, _ = strconv.Atoi(strings.TrimPrefix(c, "n"))
			}
			var sb strings.Builder
			sb.WriteString(`{"items":[`)
			for i := range 20 {
				if i > 0 {
					sb.WriteByte(',')
				}
				fmt.Fprintf(&sb, `{"key":"%05d-%s","version":"1"}`, k*20+i, strings.Repeat("k", 4000))
			}
			fmt.Fprintf(&sb, `],"next_cursor":"n%d"}`, k+1)
			_, _ = io.WriteString(w, sb.String())
			return true
		}
		// A source declared for one item may hold at most about 10 MiB of keys.
		def := h.def(func(d *ap.RAGSource) { d.Limits.MaxFiles = 1 })
		cs, err := h.p.Discover(h.ctx, scope(), def, nil)
		var ce *contracts.Error
		if !errors.As(err, &ce) || ce.Code != contracts.ErrResourceExhausted || ce.Details["limit"] != "listing_bytes" {
			t.Errorf("err = %v, want resource_exhausted on listing_bytes", err)
		}
		noChange(t, cs)
		if pages > 200 {
			t.Errorf("the provider read %d pages of huge keys before it gave up", pages)
		}
		if n := h.api.count(http.MethodGet, true); n != 0 {
			t.Errorf("%d items were fetched", n)
		}
	})

	t.Run("a key longer than the contract allows is a malformed page", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		h.api.listHook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
			_, _ = fmt.Fprintf(w, `{"items":[{"key":"%s","version":"1"}]}`, strings.Repeat("k", maxKeyChars+1))
			return true
		}
		cs, err := h.p.Discover(h.ctx, scope(), h.def(), nil)
		var ce *contracts.Error
		if !errors.As(err, &ce) || ce.Code != contracts.ErrSchemaInvalid || ce.Details["reason"] != ReasonMalformedPage {
			t.Errorf("err = %v, want schema_invalid / malformed_page", err)
		}
		noChange(t, cs)
	})

	t.Run("an item larger than the limit by Content-Length is skipped, not read", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		h.api.put("small.md", "fits\n")
		big := h.api.put("big.md", strings.Repeat("y", 4096))
		big.hideSize = true // the listing does not warn; the response does
		def := h.def(func(d *ap.RAGSource) { d.Limits.MaxFileBytes = 1024 })
		cs := h.discover(def, nil)
		if got := keysOf(cs.Added); !equalStrings(got, []string{"small.md"}) {
			t.Errorf("Added = %v", got)
		}
		if got := skipReasons(cs.Skipped)["big.md"]; got != source.ReasonTooLarge {
			t.Errorf("skipped = %v", cs.Skipped)
		}
		if _, err := h.p.Open(h.ctx, scope(), def, "big.md"); !errors.Is(err, source.ErrNotAdmitted) {
			t.Errorf("Open = %v, want ErrNotAdmitted", err)
		}
		if _, err := h.p.FetchVersion(h.ctx, scope(), def, "big.md"); !errors.Is(err, source.ErrNotAdmitted) {
			t.Errorf("FetchVersion = %v, want ErrNotAdmitted", err)
		}
		if _, err := h.p.Stat(h.ctx, scope(), def, "big.md"); !errors.Is(err, source.ErrNotAdmitted) {
			t.Errorf("Stat = %v, want ErrNotAdmitted", err)
		}
	})

	t.Run("an item whose listed size is over the limit is not even requested", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		h.api.put("big.md", strings.Repeat("y", 4096))
		def := h.def(func(d *ap.RAGSource) { d.Limits.MaxFileBytes = 1024 })
		cs := h.discover(def, nil)
		if got := skipReasons(cs.Skipped)["big.md"]; got != source.ReasonTooLarge || len(cs.Added) != 0 {
			t.Errorf("cs = %+v", cs)
		}
		if n := h.api.count(http.MethodGet, true); n != 0 {
			t.Errorf("%d item requests for an item listed as too large", n)
		}
	})

	t.Run("an item of unbounded length is cut off at the limit and skipped", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		h.api.put("ok.md", "fine\n")
		h.api.put("huge.md", "listed small\n")
		var written atomic.Int64
		h.api.itemHook = func(w http.ResponseWriter, r *http.Request, key string) bool {
			if key != "huge.md" {
				return false
			}
			endless(&written, "# a markdown file that never ends\n")(w, r)
			return true
		}
		def := h.def(func(d *ap.RAGSource) { d.Limits.MaxFileBytes = 100 << 10 })
		// huge.md was indexed before: it must leave retrieval now.
		prev := []source.Record{{Key: "huge.md", Size: 13, ModTime: oldTime, Hash: hashOf("listed small\n"), MIME: source.MIMEMarkdown, SeenAt: clockStart.Add(-time.Hour)}}
		h.api.update("huge.md", "listed small\n", oldTime.Add(time.Hour)) // changed since: it is fetched
		cs := h.discover(def, prev)
		if got := skipReasons(cs.Skipped)["huge.md"]; got != source.ReasonTooLarge {
			t.Errorf("skipped = %v", cs.Skipped)
		}
		if got := tombstoneReasons(cs.Deleted)["huge.md"]; got != source.ReasonTooLarge {
			t.Errorf("tombstones = %v: an indexed item that became too large must leave retrieval", cs.Deleted)
		}
		if got := keysOf(cs.Added); !equalStrings(got, []string{"ok.md"}) {
			t.Errorf("Added = %v", got)
		}
		if w := written.Load(); w >= 64<<20 {
			t.Errorf("the server wrote %d bytes before the client stopped it", w)
		}
	})

	t.Run("Open delivers at most the limit and then fails", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		h.api.put("huge.md", "x\n")
		var written atomic.Int64
		h.api.itemHook = func(w http.ResponseWriter, r *http.Request, key string) bool {
			endless(&written, "# text\n")(w, r)
			return true
		}
		const limit = 200 << 10 // beyond the sniffed head
		def := h.def(func(d *ap.RAGSource) { d.Limits.MaxFileBytes = limit })
		c, err := h.p.Open(h.ctx, scope(), def, "huge.md")
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		data, err := io.ReadAll(c)
		_ = c.Close()
		if contracts.CodeOf(err) != contracts.ErrResourceExhausted {
			t.Errorf("reading past the limit: %v, want resource_exhausted (never a quiet truncation)", err)
		}
		if len(data) != limit {
			t.Errorf("delivered %d bytes, want exactly the limit %d", len(data), limit)
		}
		// With a limit inside the sniffed head the item is refused outright.
		tiny := h.def(func(d *ap.RAGSource) { d.Limits.MaxFileBytes = 1024 })
		if _, err := h.p.Open(h.ctx, scope(), tiny, "huge.md"); !errors.Is(err, source.ErrNotAdmitted) {
			t.Errorf("Open = %v, want ErrNotAdmitted", err)
		}
		if w := written.Load(); w >= 64<<20 {
			t.Errorf("the server wrote %d bytes", w)
		}
	})

	t.Run("an encoded response is refused", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		h.api.put("a.md", "a\n")
		h.api.itemHook = func(w http.ResponseWriter, r *http.Request, key string) bool {
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = io.WriteString(w, "\x1f\x8b not really gzip")
			return true
		}
		cs := h.discover(h.def(), nil)
		if len(cs.Failed) != 1 || cs.Failed[0].Err.Details["reason"] != ReasonEncoded {
			t.Errorf("cs = %+v", cs)
		}
		h.api.itemHook = nil
		h.api.listHook = func(w http.ResponseWriter, r *http.Request, page int) bool {
			w.Header().Set("Content-Encoding", "br")
			_, _ = io.WriteString(w, `{"items":[]}`)
			return true
		}
		if _, err := h.p.Discover(h.ctx, scope(), h.def(), nil); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
			t.Errorf("an encoded list: %v", err)
		}
	})

	t.Run("a request that never answers times out", func(t *testing.T) {
		h := newHarness(t, "/api/docs", func(c *Config) { c.RequestTimeout = 150 * time.Millisecond })
		h.api.listHook = func(w http.ResponseWriter, r *http.Request, page int) bool {
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
			return true
		}
		cs, err := h.p.Discover(h.ctx, scope(), h.def(), nil)
		if contracts.CodeOf(err) != contracts.ErrTimeout {
			t.Errorf("err = %v, want timeout", err)
		}
		noChange(t, cs)
	})
}
