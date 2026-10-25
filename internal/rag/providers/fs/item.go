package fs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"

	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// resolved is an item that passed every check short of reading it.
type resolved struct {
	h    *rootHandle
	key  string // canonical, on-disk spelling
	info iofs.FileInfo
}

// resolve takes an item key from the caller through every check of the
// confinement (see the package comment): key syntax, rules, a no-follow walk of
// the path, the canonical-path check and the size limit. It returns the open
// root; the caller closes it. Nothing here opens the item.
func (p *Provider) resolve(ctx context.Context, scope contracts.Scope, def source.Definition, key string) (*resolved, error) {
	if err := p.begin(ctx, scope, def); err != nil {
		return nil, err
	}
	if err := p.paths.ValidKey(key); err != nil {
		return nil, err
	}
	m, err := p.matcher(def)
	if err != nil {
		return nil, err
	}
	if !m.Eligible(key) {
		return nil, fmt.Errorf("%w: %q", source.ErrNotAdmitted, key)
	}
	h, err := p.openRoot(def)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*resolved, error) {
		h.close()
		return nil, err
	}
	info, kind, _, err := h.lstatKey(key)
	if err != nil {
		return fail(err)
	}
	switch kind {
	case kindMissing:
		return fail(fmt.Errorf("%w: %q", source.ErrNotFound, key))
	case kindSymlink, kindIrregular:
		return fail(refuse(key, kind))
	case kindDir:
		return fail(fmt.Errorf("%w: %q is a directory", source.ErrNotAdmitted, key))
	}
	canon, err := p.canonicalKey(h, key)
	if err != nil {
		return fail(err)
	}
	if canon != key && !m.Eligible(canon) {
		return fail(fmt.Errorf("%w: %q", source.ErrNotAdmitted, canon))
	}
	if info.Size() > def.Limits.MaxFileBytes {
		return fail(fmt.Errorf("%w: %q is larger than the %d-byte limit", source.ErrNotAdmitted, canon, def.Limits.MaxFileBytes))
	}
	return &resolved{h: h, key: canon, info: info}, nil
}

// Stat implements source.Provider.
func (p *Provider) Stat(ctx context.Context, scope contracts.Scope, def source.Definition, key string) (source.ItemInfo, error) {
	r, err := p.resolve(ctx, scope, def, key)
	if err != nil {
		return source.ItemInfo{}, err
	}
	defer r.h.close()
	return source.ItemInfo{Key: r.key, Size: r.info.Size(), ModTime: r.info.ModTime().UTC()}, nil
}

// FetchVersion implements source.Provider.
func (p *Provider) FetchVersion(ctx context.Context, scope contracts.Scope, def source.Definition, key string) (source.Version, error) {
	r, err := p.resolve(ctx, scope, def, key)
	if err != nil {
		return source.Version{}, err
	}
	defer r.h.close()
	f, err := p.openFile(r.h, r.key)
	if err != nil {
		return source.Version{}, itemError(r.key, err)
	}
	defer f.Close()
	res, err := p.readItem(ctx, f, r.key, def)
	if err != nil {
		return source.Version{}, err
	}
	if res.skip != "" {
		return source.Version{}, fmt.Errorf("%w: %q (%s)", source.ErrNotAdmitted, r.key, res.skip)
	}
	return res.version, nil
}

// Open implements source.Provider.
func (p *Provider) Open(ctx context.Context, scope contracts.Scope, def source.Definition, key string) (source.Content, error) {
	r, err := p.resolve(ctx, scope, def, key)
	if err != nil {
		return source.Content{}, err
	}
	defer r.h.close()
	f, err := p.openFile(r.h, r.key)
	if err != nil {
		return source.Content{}, itemError(r.key, err)
	}
	fail := func(err error) (source.Content, error) {
		_ = f.Close()
		return source.Content{}, err
	}
	fi, err := f.Stat()
	if err != nil {
		return fail(itemError(r.key, err))
	}
	if !fi.Mode().IsRegular() {
		return fail(fmt.Errorf("%w: %q is not a regular file", source.ErrNotAdmitted, r.key))
	}
	if fi.Size() > def.Limits.MaxFileBytes {
		return fail(fmt.Errorf("%w: %q is larger than the %d-byte limit", source.ErrNotAdmitted, r.key, def.Limits.MaxFileBytes))
	}
	head := make([]byte, min(fi.Size(), int64(source.SniffLen)))
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return fail(itemError(r.key, err))
	}
	cls := source.Classify(head[:n], r.key)
	if cls.Mismatch || !def.AcceptsFormat(cls.MIME) {
		return fail(fmt.Errorf("%w: %q has type %s, which the source does not accept", source.ErrNotAdmitted, r.key, cls.MIME))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fail(itemError(r.key, err))
	}
	return source.Content{
		Info:       source.ItemInfo{Key: r.key, Size: fi.Size(), ModTime: fi.ModTime().UTC()},
		MIME:       cls.MIME,
		ReadCloser: &boundedFile{ctx: ctx, f: f, left: def.Limits.MaxFileBytes},
	}, nil
}

// openFile is the only place the provider opens a file's content. It inspects
// the key again without following anything, tells the test hook the canonical
// absolute path, opens it, and keeps the file only if it is the one inspected:
// os.Root follows a link that stays inside the root, so a key swapped for one
// (into an excluded folder, say) after the checks would otherwise be read.
func (p *Provider) openFile(h *rootHandle, key string) (*os.File, error) {
	info, kind, _, err := h.lstatKey(key)
	if err != nil {
		return nil, err
	}
	switch kind {
	case kindRegular:
	case kindMissing:
		return nil, &iofs.PathError{Op: "open", Path: key, Err: iofs.ErrNotExist}
	case kindDir:
		return nil, fmt.Errorf("%w: %q is a directory", source.ErrNotAdmitted, key)
	default:
		return nil, refuse(key, kind)
	}
	if p.onOpen != nil {
		p.onOpen(p.paths.Join(h.canon, key))
	}
	f, err := h.r.OpenFile(key, openFlags, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !os.SameFile(info, fi) {
		_ = f.Close()
		return nil, contracts.Newf(contracts.ErrUnavailable, "item %q changed while it was opened", key)
	}
	return f, nil
}

// boundedFile delivers at most max bytes and stops when the context ends. It
// reads from a file that passed a size check, so hitting the bound means the
// file grew after that check; it is an error, not a silent truncation.
type boundedFile struct {
	ctx  context.Context
	f    *os.File
	left int64
}

func (b *boundedFile) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, interrupted(err)
	}
	if b.left <= 0 {
		// Distinguish "exactly at the limit" from "more to come".
		var one [1]byte
		n, err := b.f.Read(one[:])
		if n > 0 {
			return 0, contracts.New(contracts.ErrResourceExhausted, "the item grew past the source's size limit while it was read")
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.f.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *boundedFile) Close() error { return b.f.Close() }

// readResult is the outcome of reading one item for its version.
type readResult struct {
	version source.Version
	// skip is set when the item was read far enough to know it is not
	// accepted.
	skip source.Reason
	// hashed is set when the content was read through to hash it.
	hashed bool
}

// readItem sniffs and hashes an open file in one pass: the first bytes decide
// the MIME type and whether the item is accepted at all, and only an accepted
// item is read to the end. The read is bounded by max_file_bytes in case the
// file grew after the size check, and a file whose size changed while it was
// read is an error (the version would not describe any state the file was in),
// not a result.
func (p *Provider) readItem(ctx context.Context, f *os.File, key string, def source.Definition) (readResult, error) {
	fi, err := f.Stat()
	if err != nil {
		return readResult{}, itemError(key, err)
	}
	if !fi.Mode().IsRegular() {
		return readResult{skip: source.ReasonIrregular}, nil
	}
	max := def.Limits.MaxFileBytes
	if fi.Size() > max {
		return readResult{skip: source.ReasonTooLarge}, nil
	}
	head := make([]byte, min(fi.Size(), int64(source.SniffLen)))
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return readResult{}, itemError(key, err)
	}
	head = head[:n]
	cls := source.Classify(head, key)
	switch {
	case cls.Mismatch:
		return readResult{skip: source.ReasonMIMEMismatch}, nil
	case !def.AcceptsFormat(cls.MIME):
		return readResult{skip: source.ReasonFormat}, nil
	}
	sum := sha256.New()
	sum.Write(head)
	rest, err := io.Copy(sum, io.LimitReader(&ctxReader{ctx: ctx, r: f}, max+1-int64(n)))
	if err != nil {
		if ctx.Err() != nil {
			return readResult{}, interrupted(ctx.Err())
		}
		return readResult{hashed: true}, itemError(key, err)
	}
	total := int64(n) + rest
	switch {
	case total > max:
		return readResult{skip: source.ReasonTooLarge, hashed: true}, nil
	case total != fi.Size():
		return readResult{hashed: true}, contracts.Newf(contracts.ErrUnavailable, "item %q changed while it was read", key)
	}
	return readResult{
		hashed: true,
		version: source.Version{
			Hash:    source.HashPrefix + hex.EncodeToString(sum.Sum(nil)),
			Size:    total,
			ModTime: fi.ModTime().UTC(),
			MIME:    cls.MIME,
		},
	}, nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
