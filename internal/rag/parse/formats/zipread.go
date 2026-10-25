package formats

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"hash/crc32"
	"io"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// A small, strict reader for the zip container of a DOCX. It exists instead of
// archive/zip because that package reads the whole central directory into
// memory before any limit can apply (a few megabytes of forged directory is
// hundreds of megabytes of File structs) and trusts size fields. This reader:
//
//   - checks the entry count and the directory size from the end-of-central-
//     directory record before it reads a single directory entry;
//   - never inflates anything but the parts the caller asks for, so entries
//     that are not read (embedded media, nested packages) cost nothing, and a
//     zip inside the zip is never opened;
//   - counts the bytes the decompressor actually produces against the shared
//     budget, enforces the ratio on those, and compares them with the declared
//     size and CRC at the end, so a lying header is an error;
//   - rejects overlapping entries, duplicate names and unsafe names (absolute,
//     "..", backslashes, drive letters, NUL) as malformed instead of picking one
//     of two readings of the same archive. Entry names are only ever looked up,
//     never used as paths.
//
// It supports stored and deflated entries and refuses zip64, encryption and
// multi-disk archives.
type zipFile struct {
	data    []byte
	entries map[string]*zipEntry
	names   []string
	bud     *budget
}

type zipEntry struct {
	name           string
	method, flags  uint16
	crc            uint32
	csize, usize   uint32
	lho            uint32
	start, endData int64
}

const (
	sigEOCD  = 0x06054b50
	sigCDir  = 0x02014b50
	sigLocal = 0x04034b50
)

func openZip(data []byte, lim wire.Limits, bud *budget) (*zipFile, error) {
	// End of central directory: the last 22 bytes plus a comment of up to 64 KiB.
	lo := len(data) - 22 - 65535
	if lo < 0 {
		lo = 0
	}
	eocd := -1
	for i := len(data) - 22; i >= lo; i-- {
		if binary.LittleEndian.Uint32(data[i:]) == sigEOCD {
			eocd = i
			break
		}
	}
	if eocd < 0 {
		return nil, malformed("zip: no end of central directory")
	}
	rec := data[eocd:]
	disk, cdDisk := binary.LittleEndian.Uint16(rec[4:]), binary.LittleEndian.Uint16(rec[6:])
	onDisk, total := binary.LittleEndian.Uint16(rec[8:]), binary.LittleEndian.Uint16(rec[10:])
	cdSize, cdOff := binary.LittleEndian.Uint32(rec[12:]), binary.LittleEndian.Uint32(rec[16:])
	switch {
	case disk != 0 || cdDisk != 0 || onDisk != total:
		return nil, fail(wire.ReasonUnsupported, "zip: multi-disk archives are not supported")
	case total == 0xFFFF || cdSize == 0xFFFFFFFF || cdOff == 0xFFFFFFFF:
		return nil, fail(wire.ReasonUnsupported, "zip64 archives are not supported")
	case int(total) > lim.MaxEntries:
		return nil, fail(wire.ReasonArchiveBomb, "archive declares %d entries, over the limit of %d", total, lim.MaxEntries)
	case int64(cdSize) > int64(lim.MaxEntries)*4096:
		return nil, fail(wire.ReasonArchiveBomb, "central directory of %d bytes is over the limit", cdSize)
	case int64(cdOff)+int64(cdSize) > int64(eocd):
		return nil, malformed("zip: central directory is outside the file")
	}
	z := &zipFile{data: data, entries: make(map[string]*zipEntry, total), bud: bud}
	dir := data[cdOff : cdOff+cdSize]
	for i := 0; i < int(total); i++ {
		if len(dir) < 46 || binary.LittleEndian.Uint32(dir) != sigCDir {
			return nil, malformed("zip: bad central directory entry %d", i)
		}
		e := &zipEntry{
			flags:  binary.LittleEndian.Uint16(dir[8:]),
			method: binary.LittleEndian.Uint16(dir[10:]),
			crc:    binary.LittleEndian.Uint32(dir[16:]),
			csize:  binary.LittleEndian.Uint32(dir[20:]),
			usize:  binary.LittleEndian.Uint32(dir[24:]),
			lho:    binary.LittleEndian.Uint32(dir[42:]),
		}
		nameLen := int(binary.LittleEndian.Uint16(dir[28:]))
		extraLen := int(binary.LittleEndian.Uint16(dir[30:]))
		commentLen := int(binary.LittleEndian.Uint16(dir[32:]))
		if len(dir) < 46+nameLen+extraLen+commentLen {
			return nil, malformed("zip: truncated central directory entry %d", i)
		}
		if e.csize == 0xFFFFFFFF || e.usize == 0xFFFFFFFF || e.lho == 0xFFFFFFFF {
			return nil, fail(wire.ReasonUnsupported, "zip64 entries are not supported")
		}
		e.name = string(dir[46 : 46+nameLen])
		dir = dir[46+nameLen+extraLen+commentLen:]
		if err := checkEntryName(e.name); err != nil {
			return nil, err
		}
		if _, dup := z.entries[e.name]; dup {
			return nil, malformed("zip: duplicate entry name")
		}
		if int64(e.lho)+30 > int64(len(data)) || binary.LittleEndian.Uint32(data[e.lho:]) != sigLocal {
			return nil, malformed("zip: bad local header for entry %d", i)
		}
		ln := int64(binary.LittleEndian.Uint16(data[e.lho+26:]))
		le := int64(binary.LittleEndian.Uint16(data[e.lho+28:]))
		e.start = int64(e.lho) + 30 + ln + le
		e.endData = e.start + int64(e.csize)
		if e.endData > int64(len(data)) {
			return nil, malformed("zip: entry %d data is outside the file", i)
		}
		z.entries[e.name] = e
		z.names = append(z.names, e.name)
	}
	if err := z.checkOverlap(); err != nil {
		return nil, err
	}
	return z, nil
}

func checkEntryName(n string) error {
	bad := n == "" || len(n) > 512 || strings.ContainsAny(n, "\x00\\") || strings.HasPrefix(n, "/") ||
		(len(n) >= 2 && n[1] == ':')
	if !bad {
		for _, part := range strings.Split(n, "/") {
			if part == ".." {
				bad = true
				break
			}
		}
	}
	if bad {
		return malformed("zip: unsafe entry name")
	}
	return nil
}

// checkOverlap rejects entries whose compressed data overlaps another's.
func (z *zipFile) checkOverlap() error {
	list := make([]*zipEntry, 0, len(z.entries))
	for _, e := range z.entries {
		if e.csize > 0 {
			list = append(list, e)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].start < list[j].start })
	for i := 1; i < len(list); i++ {
		if list[i].start < list[i-1].endData {
			return fail(wire.ReasonArchiveBomb, "zip entries overlap")
		}
	}
	return nil
}

func (z *zipFile) has(name string) bool { _, ok := z.entries[name]; return ok }

// open returns a reader for the named entry, or nil when there is none. The
// reader enforces the budget, the ratio, the declared size and the CRC;
// call finish when done to drain it and surface a verification failure.
func (z *zipFile) open(name string) (*entryReader, error) {
	e, ok := z.entries[name]
	if !ok {
		return nil, nil
	}
	if e.flags&1 != 0 {
		return nil, fail(wire.ReasonEncrypted, "zip entry is encrypted")
	}
	raw := z.data[e.start:e.endData]
	r := &entryReader{z: z, e: e, hash: crc32.NewIEEE()}
	switch e.method {
	case 0:
		if e.csize != e.usize {
			return nil, malformed("zip: stored entry sizes disagree")
		}
		r.src = bytes.NewReader(raw)
	case 8:
		r.src = flate.NewReader(bytes.NewReader(raw))
	default:
		return nil, fail(wire.ReasonUnsupported, "zip: compression method %d is not supported", e.method)
	}
	return r, nil
}

// entryReader reads one entry, counting what it produces.
type entryReader struct {
	z    *zipFile
	e    *zipEntry
	src  io.Reader
	n    int64
	hash interface {
		Write([]byte) (int, error)
		Sum32() uint32
	}
	first bool
	done  bool
	err   error
}

func (r *entryReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	// Never produce more than the budget has room for, plus one byte so that
	// going over is noticed.
	if room := r.z.bud.room(); int64(len(p)) > room+1 {
		if room < 0 {
			room = 0
		}
		p = p[:room+1]
	}
	n, err := r.src.Read(p)
	if n > 0 {
		if !r.first {
			r.first = true
			if n >= 4 && (string(p[:4]) == "PK\x03\x04" || string(p[:4]) == "PK\x05\x06") {
				r.err = fail(wire.ReasonArchiveBomb, "nested archives are not opened")
				return 0, r.err
			}
		}
		r.n += int64(n)
		r.hash.Write(p[:n])
		if e := r.z.bud.take(int64(n)); e != nil {
			r.err = e
			return 0, e
		}
		if e := r.z.bud.checkRatio(r.n, int64(r.e.csize)); e != nil {
			r.err = e
			return 0, e
		}
		if r.n > int64(r.e.usize) {
			r.err = malformed("zip: entry is larger than its declared size")
			return 0, r.err
		}
	}
	if err == io.EOF {
		r.done = true
		switch {
		case r.n != int64(r.e.usize):
			r.err = malformed("zip: entry is smaller than its declared size")
		case r.hash.Sum32() != r.e.crc:
			r.err = malformed("zip: entry fails its checksum")
		}
		if r.err != nil {
			return n, r.err
		}
		return n, io.EOF
	}
	if err != nil {
		r.err = malformed("zip: %v", err)
		return n, r.err
	}
	return n, nil
}

// finish drains what is left of the entry (so size and CRC are verified) and
// returns the first failure.
func (r *entryReader) finish() error {
	if !r.done && r.err == nil {
		if _, err := io.Copy(io.Discard, r); err != nil {
			return err
		}
	}
	if r.err != nil && r.err != io.EOF {
		return r.err
	}
	return nil
}
