package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
)

var contentAddressRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// isContentAddress reports whether a listed version is a content address that
// can be verified against the bytes.
func isContentAddress(v string) bool { return contentAddressRE.MatchString(v) }

// normalizeVersion puts a listed version and an ETag header in one form: no
// surrounding blanks, no weak marker, no quotes.
func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "W/")
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = v[1 : len(v)-1]
	}
	return v
}

func versionChanged(why string) *contracts.Error {
	return contracts.New(contracts.ErrUnavailable, "the item changed between the listing and the fetch: "+why).
		WithDetails("reason", ReasonVersionChanged)
}

// itemResponse is the 200 answer of an item endpoint.
type itemResponse struct {
	*exchange
	etag         string
	contentType  string
	length       int64 // -1 when the API does not state it
	lastModified time.Time
}

// getItem requests one item (GET) or its metadata (HEAD). A 404 or 410 is a
// *goneError, which is a claim for the caller to settle; every other non-200
// status is a typed error.
func (s *session) getItem(ctx context.Context, method, key string, timeout time.Duration) (*itemResponse, error) {
	x, err := s.send(ctx, method, s.root.itemURL(key, s.query()), timeout)
	if err != nil {
		return nil, err
	}
	switch code := x.resp.StatusCode; {
	case code == http.StatusOK:
	case code == http.StatusNotFound || code == http.StatusGone:
		x.close()
		return nil, &goneError{status: code}
	default:
		x.close()
		return nil, statusError(code)
	}
	if err := checkEncoding(x.resp); err != nil {
		x.close()
		return nil, err
	}
	ir := &itemResponse{
		exchange:    x,
		etag:        x.resp.Header.Get("ETag"),
		contentType: x.resp.Header.Get("Content-Type"),
		length:      x.resp.ContentLength,
	}
	if lm := x.resp.Header.Get("Last-Modified"); lm != "" {
		if t, err := http.ParseTime(lm); err == nil {
			ir.lastModified = t.UTC()
		}
	}
	return ir, nil
}

// headItem asks for an item's metadata. An API that does not implement HEAD
// (405 or 501) is asked with a GET whose body is never read.
func (s *session) headItem(ctx context.Context, key string) (*itemResponse, error) {
	ir, err := s.getItem(ctx, http.MethodHead, key, s.p.cfg.RequestTimeout)
	var ce *contracts.Error
	if errors.As(err, &ce) {
		if st, _ := ce.Details["http_status"].(int); st == http.StatusMethodNotAllowed || st == http.StatusNotImplemented {
			return s.getItem(ctx, http.MethodGet, key, s.p.cfg.RequestTimeout)
		}
	}
	return ir, err
}

// confirmGone settles a 404 or 410 on an item. It is only believed when the
// list endpoint answers a well-formed page right now: a wrong base_path, a
// proxy that answers 404 for everything, or an API that is down says the same
// thing for every item and must never read as "all of it was deleted".
func (s *session) confirmGone(ctx context.Context, key string) error {
	if _, err := s.listPage(ctx, "", 1); err != nil {
		return err
	}
	return fmt.Errorf("%w: %q", source.ErrNotFound, key)
}

// settle turns the error of an item request into the error the caller of the
// provider gets: a claim of absence is checked first.
func (s *session) settle(ctx context.Context, key string, err error) error {
	var gone *goneError
	if errors.As(err, &gone) {
		return s.confirmGone(ctx, key)
	}
	return err
}

// readError classifies a failure to read a response body.
func readError(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return interrupted(cerr)
	}
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return ce
	}
	return unavailable("the item could not be read", egress.StripURL(err))
}

// readResult is the outcome of reading one item for its version.
type readResult struct {
	version source.Version
	// skip is set when the item was read far enough to know it is not
	// accepted.
	skip source.Reason
	// hashed is set when the content was read through to hash it.
	hashed bool
}

// readContent sniffs and hashes the body of an item response in one pass: the
// first bytes decide the MIME type and whether the item is accepted at all, and
// only an accepted item is read to the end. The read stops one byte past
// max_file_bytes, so an enormous or endless body costs at most that much and is
// skipped, never truncated and kept.
func (s *session) readContent(ctx context.Context, ir *itemResponse, key string) (readResult, error) {
	max := s.def.Limits.MaxFileBytes
	if ir.length > max {
		return readResult{skip: source.ReasonTooLarge}, nil
	}
	head, err := io.ReadAll(io.LimitReader(ir.resp.Body, min(int64(source.SniffLen), max+1)))
	if err != nil {
		return readResult{}, readError(ctx, err)
	}
	if int64(len(head)) > max {
		return readResult{skip: source.ReasonTooLarge}, nil
	}
	cls := s.p.classify(head, key, ir.contentType)
	switch {
	case cls.Mismatch:
		return readResult{skip: source.ReasonMIMEMismatch}, nil
	case !s.def.AcceptsFormat(cls.MIME):
		return readResult{skip: source.ReasonFormat}, nil
	}
	sum := sha256.New()
	sum.Write(head)
	rest, err := io.Copy(sum, io.LimitReader(ir.resp.Body, max+1-int64(len(head))))
	if err != nil {
		return readResult{hashed: true}, readError(ctx, err)
	}
	total := int64(len(head)) + rest
	switch {
	case total > max:
		return readResult{skip: source.ReasonTooLarge, hashed: true}, nil
	case ir.length >= 0 && total != ir.length:
		return readResult{hashed: true}, versionChanged("the body is not the length the API stated")
	}
	return readResult{hashed: true, version: source.Version{
		Hash: source.HashPrefix + hex.EncodeToString(sum.Sum(nil)),
		Size: total,
		MIME: cls.MIME,
	}}, nil
}

// fetchListed fetches one listed item for Discover and checks it against what
// the listing said about it: the ETag, a content-address version and the size.
// A mismatch means the item changed (or the API contradicts itself) and is a
// failure of this item for this scan, never a result.
func (s *session) fetchListed(ctx context.Context, e listEntry) (readResult, error) {
	ir, err := s.getItem(ctx, http.MethodGet, e.key, s.p.cfg.ItemTimeout)
	if err != nil {
		return readResult{}, err
	}
	defer ir.close()
	if ir.etag != "" && normalizeVersion(ir.etag) != normalizeVersion(e.version) {
		return readResult{}, versionChanged("the ETag is not the listed version")
	}
	res, err := s.readContent(ctx, ir, e.key)
	if err != nil || res.skip != "" {
		return res, err
	}
	if isContentAddress(e.version) && e.version != res.version.Hash {
		return res, versionChanged("the content does not match the listed content address")
	}
	if e.size >= 0 && e.size != res.version.Size {
		return res, versionChanged("the size is not the listed size")
	}
	// The record's time is the one the listing states, because the next
	// scan compares the next listing with it; the Last-Modified header is the
	// fallback for an API that lists no time.
	res.version.ModTime = e.modTime
	if res.version.ModTime.IsZero() {
		res.version.ModTime = ir.lastModified
	}
	return res, nil
}

// extForContentType maps a declared media type to the file extension
// source.Classify uses to choose among the types a sniffer cannot tell apart.
func extForContentType(ct string) string {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return ""
	}
	mt = strings.ToLower(mt)
	switch mt {
	case source.MIMEText:
		return ".txt"
	case source.MIMEMarkdown, "text/x-markdown":
		return ".md"
	case source.MIMECSV:
		return ".csv"
	case source.MIMETSV:
		return ".tsv"
	case source.MIMEHTML:
		return ".html"
	case source.MIMEJSON, "text/json":
		return ".json"
	case source.MIMEXML, "text/xml":
		return ".xml"
	case source.MIMEYAML, "text/yaml", "application/x-yaml", "text/x-yaml":
		return ".yaml"
	case source.MIMEPDF:
		return ".pdf"
	case source.MIMERTF, "text/rtf":
		return ".rtf"
	case source.MIMEZip:
		return ".zip"
	case source.MIMEDOCX:
		return ".docx"
	case source.MIMEXLSX:
		return ".xlsx"
	case source.MIMEPPTX:
		return ".pptx"
	case source.MIMEODT:
		return ".odt"
	case source.MIMEODS:
		return ".ods"
	case source.MIMEODP:
		return ".odp"
	}
	if strings.HasSuffix(mt, "+json") {
		return ".json"
	}
	if strings.HasSuffix(mt, "+xml") {
		return ".xml"
	}
	return ""
}

// classify sniffs the type of an item from its first bytes. The bytes decide.
// The key's extension, or when it has none the declared Content-Type, is
// consulted only to choose among types the bytes cannot tell apart (JSON and
// plain text, say), and a claim the bytes contradict is a mismatch.
func (p *Provider) classify(head []byte, key, contentType string) source.Class {
	c := source.Classify(head, key)
	if c.NameMIME != "" {
		return c
	}
	if ext := extForContentType(contentType); ext != "" {
		return source.Classify(head, key+ext)
	}
	return c
}

// Stat implements source.Provider.
func (p *Provider) Stat(ctx context.Context, scope contracts.Scope, def source.Definition, key string) (source.ItemInfo, error) {
	s, err := p.begin(ctx, scope, def)
	if err != nil {
		return source.ItemInfo{}, err
	}
	defer s.close()
	if err := s.eligible(key); err != nil {
		return source.ItemInfo{}, err
	}
	ir, err := s.headItem(ctx, key)
	if err != nil {
		return source.ItemInfo{}, s.settle(ctx, key, err)
	}
	defer ir.close()
	if ir.length > def.Limits.MaxFileBytes {
		return source.ItemInfo{}, fmt.Errorf("%w: %q is larger than the %d-byte limit", source.ErrNotAdmitted, key, def.Limits.MaxFileBytes)
	}
	return source.ItemInfo{Key: key, Size: ir.length, ModTime: ir.lastModified}, nil
}

// FetchVersion implements source.Provider.
func (p *Provider) FetchVersion(ctx context.Context, scope contracts.Scope, def source.Definition, key string) (source.Version, error) {
	s, err := p.begin(ctx, scope, def)
	if err != nil {
		return source.Version{}, err
	}
	defer s.close()
	if err := s.eligible(key); err != nil {
		return source.Version{}, err
	}
	ir, err := s.getItem(ctx, http.MethodGet, key, p.cfg.ItemTimeout)
	if err != nil {
		return source.Version{}, s.settle(ctx, key, err)
	}
	defer ir.close()
	res, err := s.readContent(ctx, ir, key)
	if err != nil {
		return source.Version{}, err
	}
	if res.skip != "" {
		return source.Version{}, fmt.Errorf("%w: %q (%s)", source.ErrNotAdmitted, key, res.skip)
	}
	v := res.version
	v.ModTime = ir.lastModified
	return v, nil
}

// Open implements source.Provider. The returned reader holds the connection
// until it is closed, delivers at most limits.max_file_bytes, and fails with
// resource_exhausted, instead of ending early, if the body is longer.
func (p *Provider) Open(ctx context.Context, scope contracts.Scope, def source.Definition, key string) (source.Content, error) {
	s, err := p.begin(ctx, scope, def)
	if err != nil {
		return source.Content{}, err
	}
	if err := s.eligible(key); err != nil {
		s.close()
		return source.Content{}, err
	}
	ir, err := s.getItem(ctx, http.MethodGet, key, p.cfg.ItemTimeout)
	if err != nil {
		defer s.close()
		return source.Content{}, s.settle(ctx, key, err)
	}
	fail := func(err error) (source.Content, error) {
		ir.close()
		s.close()
		return source.Content{}, err
	}
	max := def.Limits.MaxFileBytes
	if ir.length > max {
		return fail(fmt.Errorf("%w: %q is larger than the %d-byte limit", source.ErrNotAdmitted, key, max))
	}
	head, err := io.ReadAll(io.LimitReader(ir.resp.Body, min(int64(source.SniffLen), max+1)))
	if err != nil {
		return fail(readError(ctx, err))
	}
	if int64(len(head)) > max {
		return fail(fmt.Errorf("%w: %q is larger than the %d-byte limit", source.ErrNotAdmitted, key, max))
	}
	cls := p.classify(head, key, ir.contentType)
	if cls.Mismatch || !def.AcceptsFormat(cls.MIME) {
		return fail(fmt.Errorf("%w: %q has type %s, which the source does not accept", source.ErrNotAdmitted, key, cls.MIME))
	}
	return source.Content{
		Info: source.ItemInfo{Key: key, Size: ir.length, ModTime: ir.lastModified},
		MIME: cls.MIME,
		ReadCloser: &boundedBody{
			ctx:  ctx,
			r:    io.MultiReader(bytes.NewReader(head), ir.resp.Body),
			left: max,
			done: func() { ir.close(); s.close() },
		},
	}, nil
}

// boundedBody delivers at most max bytes and stops when the context ends. It
// reads from a response whose declared length passed the size check, so hitting
// the bound means the body is longer than the API said: an error, not a silent
// truncation.
type boundedBody struct {
	ctx  context.Context
	r    io.Reader
	left int64
	done func()
	once bool
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, interrupted(err)
	}
	if b.left <= 0 {
		var one [1]byte
		n, err := b.r.Read(one[:])
		if n > 0 {
			return 0, contracts.New(contracts.ErrResourceExhausted, "the item is longer than the source's size limit")
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, readError(b.ctx, err)
	}
	return n, err
}

func (b *boundedBody) Close() error {
	if !b.once {
		b.once = true
		b.done()
	}
	return nil
}
