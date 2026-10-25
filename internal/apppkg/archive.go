package apppkg

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// FromArchive reads a package from a gzip-compressed tar stream (.tar.gz).
//
// The archive is read into memory and never extracted to disk, so nothing an
// archive says can write outside any directory: there is no destination to
// escape. What the entries may be is fixed:
//
//   - regular files and directories only; symbolic links, hard links, devices,
//     FIFOs, sparse files and anything else are refused;
//   - names must pass the package path grammar after one leading "./" is
//     removed (so "tar czf x.tar.gz -C dir ." works); an absolute path, a ".."
//     segment, a backslash, a hidden name or a name outside the grammar is
//     refused, never repaired;
//   - a name that appears twice, differs from another only by letter case, or
//     is both a file and a directory is refused (the last-entry-wins tricks of
//     tar do not exist here);
//   - the archive is one gzip member holding one tar stream: trailing data is
//     refused;
//   - sizes are enforced by counting bytes actually read, never by trusting a
//     header, and an archive that expands by more than Limits.MaxRatio is
//     refused as soon as that is evident;
//   - the first refused entry refuses the archive: nothing after it, its own
//     data included, is decompressed, and it is the finding reported.
//
// The archive root is the package root: alixai.yaml is at the top level.
func FromArchive(r io.Reader) Source { return &archiveSource{r: r} }

// FromArchiveFile is FromArchive for a file path. The path must name a regular
// file, not a link.
func FromArchiveFile(path string) Source { return &archiveSource{path: path} }

type archiveSource struct {
	r    io.Reader
	path string
}

func (a *archiveSource) describe() string {
	if a.path != "" {
		return "archive " + clipPath(a.path)
	}
	return "archive"
}

// errRatio marks a decompression bomb.
var errRatio = errors.New("archive expands beyond the allowed ratio")

// countReader counts the bytes read through it.
type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// ratioReader counts uncompressed bytes and fails with errRatio when they
// exceed ratio times the compressed bytes consumed so far.
type ratioReader struct {
	r          io.Reader
	compressed *countReader
	ratio      int64
	n          int64
}

func (rr *ratioReader) Read(p []byte) (int, error) {
	n, err := rr.r.Read(p)
	rr.n += int64(n)
	if rr.n > RatioFloor {
		c := rr.compressed.n
		if c < 1 {
			c = 1
		}
		if rr.n/c > rr.ratio {
			return n, errRatio
		}
	}
	return n, err
}

func (a *archiveSource) snapshot(ctx context.Context, lim Limits) (*Snapshot, []problem, error) {
	r := a.r
	if a.path != "" {
		st, err := os.Lstat(a.path)
		if err != nil {
			return nil, nil, contracts.Wrap(contracts.ErrValidationFailed, err, "package archive cannot be read")
		}
		if !st.Mode().IsRegular() {
			return nil, []problem{{chkArchiveInvalid, a.path, "archive path is not a regular file"}}, nil
		}
		if st.Size() > lim.MaxArchiveBytes {
			return nil, []problem{{chkLimit, a.path, fmt.Sprintf("archive is larger than the limit of %d bytes", lim.MaxArchiveBytes)}}, nil
		}
		f, err := os.Open(a.path)
		if err != nil {
			return nil, nil, contracts.Wrap(contracts.ErrValidationFailed, err, "package archive cannot be opened")
		}
		defer f.Close()
		r = f
	}
	if r == nil {
		return nil, nil, contracts.New(contracts.ErrValidationFailed, "no archive to read")
	}

	raw := &countReader{r: io.LimitReader(r, lim.MaxArchiveBytes+1)}
	b := newBuilder(lim)
	fail := func(c *check, format string, args ...any) (*Snapshot, []problem, error) {
		return nil, []problem{{c, ".", fmt.Sprintf(format, args...)}}, nil
	}

	// br is the one reader of the compressed bytes, so that trailing data is
	// seen by the check below instead of being swallowed by read-ahead.
	br := bufio.NewReader(raw)
	zr, err := gzip.NewReader(br)
	if err != nil {
		return fail(chkArchiveInvalid, "not a gzip stream")
	}
	zr.Multistream(false)
	ratio := &ratioReader{r: zr, compressed: raw, ratio: lim.MaxRatio}
	tr := tar.NewReader(ratio)

	entries := 0
	maxEntries := lim.MaxFiles*4 + 16
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if len(b.problems) > 0 {
			// An entry was refused, so the archive is: reading on would only
			// decompress the refused entry's data and what follows it.
			return nil, b.problems, nil
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return a.readFailure(raw, lim, err, fail)
		}
		entries++
		if entries > maxEntries {
			return fail(chkLimit, "archive has more than %d entries", maxEntries)
		}
		if raw.n > lim.MaxArchiveBytes {
			return fail(chkLimit, "archive is larger than the limit of %d bytes", lim.MaxArchiveBytes)
		}

		name, skip, ok := normalizeEntryName(hdr.Name)
		if !ok {
			b.problem(chkUnsafeEntry, hdr.Name, "entry name is absolute or malformed")
			continue
		}
		if skip {
			if hdr.Typeflag == tar.TypeDir || hdr.Typeflag == tar.TypeXGlobalHeader {
				continue
			}
		}
		sparse := false
		for k := range hdr.PAXRecords {
			if strings.HasPrefix(k, "GNU.sparse") {
				sparse = true
			}
		}
		if sparse {
			b.problem(chkUnsafeEntry, name, "sparse files are never accepted")
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeXGlobalHeader:
			continue // carries archive metadata (a comment), no file
		case tar.TypeDir:
			if b.checkPath(name) && b.fold(name) {
				b.dirsSeen[name] = true
			}
		case tar.TypeReg:
			if !b.checkPath(name) {
				continue
			}
			if !b.reserve(name, hdr.Size) {
				return nil, b.problems, nil
			}
			if !b.admit(name) {
				continue
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				return a.readFailure(raw, lim, err, fail)
			}
			if int64(len(data)) != hdr.Size {
				return fail(chkArchiveInvalid, "entry %s is shorter than its header says", clipPath(name))
			}
			b.put(name, data)
		default:
			b.problem(chkUnsafeEntry, name, "is %s; only regular files and directories are accepted", describeTarType(hdr.Typeflag))
		}
	}

	// The tar stream ended. Nothing but padding may follow it, in the gzip
	// stream or after it.
	if err := drainZeros(ratio); err != nil {
		if errors.Is(err, errRatio) {
			return fail(chkArchiveRatio, "archive expands by more than %d to 1", lim.MaxRatio)
		}
		return fail(chkArchiveInvalid, "unexpected data after the end of the tar stream")
	}
	if _, err := br.ReadByte(); err == nil {
		return fail(chkArchiveInvalid, "data follows the gzip stream; an archive is exactly one gzip member")
	}
	if raw.n > lim.MaxArchiveBytes {
		return fail(chkLimit, "archive is larger than the limit of %d bytes", lim.MaxArchiveBytes)
	}

	snap, probs := b.finish()
	return snap, probs, nil
}

// readFailure classifies an error from reading the compressed stream.
func (a *archiveSource) readFailure(raw *countReader, lim Limits, err error, fail func(*check, string, ...any) (*Snapshot, []problem, error)) (*Snapshot, []problem, error) {
	switch {
	case errors.Is(err, errRatio):
		return fail(chkArchiveRatio, "archive expands by more than %d to 1", lim.MaxRatio)
	case raw.n > lim.MaxArchiveBytes:
		return fail(chkLimit, "archive is larger than the limit of %d bytes", lim.MaxArchiveBytes)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return nil, nil, err
	}
	return fail(chkArchiveInvalid, "archive is truncated or corrupt")
}

// drainZeros reads to the end of r, accepting only zero bytes (tar blocks are
// padded with them).
func drainZeros(r io.Reader) error {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		for _, b := range buf[:n] {
			if b != 0 {
				return errors.New("non-zero data after the tar end marker")
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// normalizeEntryName removes one leading "./" and a trailing "/" from a tar
// name. skip is true for the archive root itself ("." or "./"). ok is false
// for a name that must be refused outright: empty, absolute or with a
// backslash or NUL.
func normalizeEntryName(raw string) (name string, skip, ok bool) {
	n := raw
	if n == "" || strings.HasPrefix(n, "/") || strings.ContainsAny(n, "\\\x00") {
		return raw, false, false
	}
	n = strings.TrimPrefix(n, "./")
	n = strings.TrimSuffix(n, "/")
	if n == "" || n == "." {
		return ".", true, true
	}
	return n, false, true
}

func describeTarType(t byte) string {
	switch t {
	case tar.TypeSymlink:
		return "a symbolic link"
	case tar.TypeLink:
		return "a hard link"
	case tar.TypeChar, tar.TypeBlock:
		return "a device"
	case tar.TypeFifo:
		return "a FIFO"
	case tar.TypeGNUSparse:
		return "a sparse file"
	}
	return fmt.Sprintf("of type %q", string(rune(t)))
}
