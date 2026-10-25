package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// listEntry is one validated entry of a list page.
type listEntry struct {
	key     string
	version string
	// size is the listed length in bytes, -1 when the API did not list it.
	size int64
	// modTime is the listed modification time (UTC), zero when not listed.
	modTime time.Time
	deleted bool
}

// page is one validated list page.
type page struct {
	entries []listEntry
	next    string // "" on the last page
}

// wirePage and wireEntry are the JSON shape of the list response
// (testdata/list-page.schema.json). Unknown members are ignored so an
// application can add its own; every member the provider uses is checked.
type wirePage struct {
	Items      *[]wireEntry `json:"items"`
	NextCursor *string      `json:"next_cursor"`
}

type wireEntry struct {
	Key        *string `json:"key"`
	Version    *string `json:"version"`
	Size       *int64  `json:"size"`
	ModifiedAt *string `json:"modified_at"`
	Deleted    *bool   `json:"deleted"`
}

func malformedPage(format string, args ...any) *contracts.Error {
	return contracts.Newf(contracts.ErrSchemaInvalid, "the list response does not follow the API contract: "+format, args...).
		WithDetails("reason", ReasonMalformedPage)
}

func pageTooLarge(limit int64) *contracts.Error {
	return contracts.New(contracts.ErrSchemaInvalid, "the list response is larger than the size limit").
		WithDetails("reason", ReasonResponseTooLarge).WithDetails("limit_bytes", limit)
}

// validVersion mirrors the opaque-version rule: 1 to 256 characters, none of
// them whitespace or a control character.
func validVersion(v string) bool {
	n := utf8.RuneCountInString(v)
	if n < 1 || n > maxVersionLen || !utf8.ValidString(v) {
		return false
	}
	return strings.IndexFunc(v, func(r rune) bool { return r <= 0x20 || r == 0x7f }) < 0
}

func validCursor(c string) bool {
	return len(c) <= maxCursorBytes && utf8.ValidString(c) &&
		strings.IndexFunc(c, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
}

// decodePage parses and validates one list response body. It never copies a
// value of the body into an error: a reply is untrusted text.
func decodePage(body []byte) (page, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var w wirePage
	if err := dec.Decode(&w); err != nil {
		return page{}, malformedPage("the body is not the expected JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return page{}, malformedPage("the body has data after the JSON object")
	}
	if w.Items == nil {
		return page{}, malformedPage("\"items\" is missing or null")
	}
	items := *w.Items
	if len(items) > maxPageEntries {
		return page{}, malformedPage("a page has more than %d entries", maxPageEntries)
	}
	out := page{entries: make([]listEntry, 0, len(items))}
	for i, it := range items {
		if it.Key == nil || *it.Key == "" {
			return page{}, malformedPage("entry %d has no key", i)
		}
		if utf8.RuneCountInString(*it.Key) > maxKeyChars {
			return page{}, malformedPage("entry %d has a key longer than %d characters", i, maxKeyChars)
		}
		if it.Version == nil || !validVersion(*it.Version) {
			return page{}, malformedPage("entry %d has no valid version", i)
		}
		e := listEntry{key: *it.Key, version: *it.Version, size: -1}
		if it.Size != nil {
			if *it.Size < 0 {
				return page{}, malformedPage("entry %d has a negative size", i)
			}
			e.size = *it.Size
		}
		if it.ModifiedAt != nil {
			t, err := time.Parse(time.RFC3339, *it.ModifiedAt)
			if err != nil {
				return page{}, malformedPage("entry %d has a modified_at that is not RFC 3339", i)
			}
			e.modTime = t.UTC()
		}
		if it.Deleted != nil {
			e.deleted = *it.Deleted
		}
		out.entries = append(out.entries, e)
	}
	if w.NextCursor != nil {
		if !validCursor(*w.NextCursor) {
			return page{}, malformedPage("\"next_cursor\" is too long or holds control characters")
		}
		out.next = *w.NextCursor
	}
	return out, nil
}

// listPage requests one page. limit is the page size asked for.
func (s *session) listPage(ctx context.Context, cursor string, limit int) (page, error) {
	q := s.query()
	n := s.p.cfg.Params
	q.Set(n.Limit, strconv.Itoa(limit))
	if cursor != "" {
		q.Set(n.Cursor, cursor)
	}
	x, err := s.send(ctx, http.MethodGet, s.root.listURL(q), s.p.cfg.RequestTimeout)
	if err != nil {
		return page{}, err
	}
	defer x.close()
	if x.resp.StatusCode != http.StatusOK {
		return page{}, statusError(x.resp.StatusCode)
	}
	if err := checkEncoding(x.resp); err != nil {
		return page{}, err
	}
	max := s.p.cfg.MaxPageBytes
	if x.resp.ContentLength > max {
		return page{}, pageTooLarge(max)
	}
	body, err := io.ReadAll(io.LimitReader(x.resp.Body, max+1))
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return page{}, interrupted(cerr)
		}
		return page{}, unavailable("the list response could not be read", err)
	}
	if int64(len(body)) > max {
		return page{}, pageTooLarge(max)
	}
	return decodePage(body)
}

// listAll reads the whole listing. It returns only a complete listing: every
// page answered, well formed and within the bounds, with a cursor that always
// moved on. Anything else is an error and no partial listing, because a
// partial listing must never be read as "the rest was deleted".
func (s *session) listAll(ctx context.Context, maxEntries int64) ([]listEntry, error) {
	var all []listEntry
	// Cursors are remembered by digest, so a long cursor costs 32 bytes.
	seen := map[[sha256.Size]byte]bool{sha256.Sum256(nil): true}
	budget, retained := maxEntries*listBytesPerEntry, int64(0)
	cursor := ""
	for pages := 0; ; pages++ {
		if pages >= s.p.cfg.MaxPages {
			return nil, contracts.Newf(contracts.ErrResourceExhausted, "the listing has more than %d pages", s.p.cfg.MaxPages).
				WithDetails("limit", "max_pages")
		}
		pg, err := s.listPage(ctx, cursor, s.p.cfg.PageSize)
		if err != nil {
			return nil, err
		}
		if int64(len(all))+int64(len(pg.entries)) > maxEntries {
			return nil, contracts.Newf(contracts.ErrResourceExhausted, "the API lists more than %d entries, far beyond the source's declared size", maxEntries).
				WithDetails("limit", "max_files")
		}
		for _, e := range pg.entries {
			retained += int64(len(e.key) + len(e.version))
		}
		if retained > budget {
			return nil, contracts.New(contracts.ErrResourceExhausted, "the listing holds far more key data than the source's declared size justifies").
				WithDetails("limit", "listing_bytes")
		}
		all = append(all, pg.entries...)
		if pg.next == "" {
			return all, nil
		}
		digest := sha256.Sum256([]byte(pg.next))
		if seen[digest] {
			return nil, contracts.New(contracts.ErrSchemaInvalid, "the pagination does not advance: a cursor came back twice").
				WithDetails("reason", ReasonPagination)
		}
		seen[digest] = true
		cursor = pg.next
	}
}

// listBound is the most entries a listing may hold for a source declared with
// maxFiles items: room for deletion markers and items the rules do not admit,
// and no more.
func listBound(maxFiles int64) int64 {
	if maxFiles > (1<<62)/4 {
		return maxFiles
	}
	return maxFiles*2 + 10_000
}
