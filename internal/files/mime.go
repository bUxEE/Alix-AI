package files

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"io"
	"mime"
	"net/http"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// sniffLen is how much of a file DetectContentType looks at.
const sniffLen = 512

// maxZipEntries bounds the central directory an OOXML check will list.
const maxZipEntries = 20000

// kind is the coarse class of a type, what a model can be given.
type kind string

const (
	kindImage    kind = "image"
	kindText     kind = "text"
	kindDocument kind = "document"
)

// previewMode is how a safe previewer shows a type, if one exists.
type previewMode int

const (
	previewNone previewMode = iota
	// previewImage: a raster image, served as its own type.
	previewImage
	// previewText: plain text, served as text/plain whatever it was declared as.
	previewText
)

// typeSpec is one media type the core can store: what the bytes must look like
// for the declared type to be believed (SEC-019). A declared type that is not
// in the table is refused, so nothing active (HTML, SVG, XML, script) is ever
// stored, however a policy is written.
type typeSpec struct {
	mime string
	kind kind
	// sniffed lists the types http.DetectContentType may report for the bytes
	// (parameters removed). Formats that sniff as something broader (Markdown,
	// CSV and JSON are text/plain; OOXML is application/zip) list that, and are
	// narrowed by headCheck or zipDir.
	sniffed []string
	preview previewMode
	// headCheck narrows a broad sniff using the first bytes.
	headCheck func(head []byte) bool
	// zipDir, when set, requires the file to be a ZIP container holding
	// [Content_Types].xml and a part below this directory (an OOXML package).
	zipDir string
}

var typeTable = func() map[string]typeSpec {
	list := []typeSpec{
		{mime: "image/png", kind: kindImage, sniffed: []string{"image/png"}, preview: previewImage},
		{mime: "image/jpeg", kind: kindImage, sniffed: []string{"image/jpeg"}, preview: previewImage},
		{mime: "image/gif", kind: kindImage, sniffed: []string{"image/gif"}, preview: previewImage},
		{mime: "image/webp", kind: kindImage, sniffed: []string{"image/webp"}, preview: previewImage},
		{mime: "application/pdf", kind: kindDocument, sniffed: []string{"application/pdf"}},
		{mime: "text/plain", kind: kindText, sniffed: []string{"text/plain"}, preview: previewText},
		{mime: "text/markdown", kind: kindText, sniffed: []string{"text/plain"}, preview: previewText},
		{mime: "text/csv", kind: kindText, sniffed: []string{"text/plain"}, preview: previewText},
		{mime: "application/json", kind: kindText, sniffed: []string{"text/plain"}, preview: previewText, headCheck: looksLikeJSON},
		{mime: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", kind: kindDocument, sniffed: []string{"application/zip"}, zipDir: "word/"},
		{mime: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", kind: kindDocument, sniffed: []string{"application/zip"}, zipDir: "xl/"},
		{mime: "application/vnd.openxmlformats-officedocument.presentationml.presentation", kind: kindDocument, sniffed: []string{"application/zip"}, zipDir: "ppt/"},
	}
	m := make(map[string]typeSpec, len(list))
	for _, s := range list {
		m[s.mime] = s
	}
	return m
}()

// KnownTypes lists the media types the core can verify from their bytes, the
// only ones a policy can allow.
func KnownTypes() []string {
	out := make([]string, 0, len(typeTable))
	for m := range typeTable {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// knownPattern reports whether a policy entry is an exact known type or one of
// the families image/* and text/*.
func knownPattern(p string) bool {
	p = lowerTrim(p)
	if p == "image/*" || p == "text/*" {
		return true
	}
	_, ok := typeTable[p]
	return ok
}

// declaredType parses the declared Content-Type of an upload into its lower-case
// media type, parameters dropped.
func declaredType(raw string) (string, error) {
	mt, _, err := mime.ParseMediaType(strings.TrimSpace(raw))
	if err != nil || mt == "" {
		return "", invalid("mime_declared", "the file's content type is missing or malformed")
	}
	return strings.ToLower(mt), nil
}

// sniffedType is the media type of the bytes, parameters removed.
func sniffedType(head []byte) string {
	if len(head) > sniffLen {
		head = head[:sniffLen]
	}
	t := http.DetectContentType(head)
	if i := strings.IndexByte(t, ';'); i >= 0 {
		t = t[:i]
	}
	return strings.TrimSpace(t)
}

// verifyHead checks the first bytes of a file against its declared type: the
// sniffed type must be one the table lists for it, and a broad sniff must pass
// the type's own check. A mismatch is refused, never reconciled.
func verifyHead(spec typeSpec, head []byte) error {
	got := sniffedType(head)
	ok := false
	for _, s := range spec.sniffed {
		if got == s {
			ok = true
			break
		}
	}
	if ok && spec.headCheck != nil && !spec.headCheck(head) {
		ok = false
	}
	if !ok {
		return invalid("mime_mismatch", "the file's content is not of the type it was declared as").
			WithDetails("declared", spec.mime).WithDetails("detected", got)
	}
	return nil
}

// verifyContainer checks the container of a type that has one (OOXML): the ZIP
// directory is read, never a member, so a decompression bomb cannot be
// triggered.
func verifyContainer(spec typeSpec, r io.ReaderAt, size int64) error {
	if spec.zipDir == "" {
		return nil
	}
	mismatch := func() error {
		return invalid("mime_mismatch", "the file's content is not of the type it was declared as").
			WithDetails("declared", spec.mime).WithDetails("detected", "application/zip")
	}
	// The entry count the end of the central directory declares is checked before archive/zip reads the directory,
	// as internal/rag/parse/formats does: a declared count over the bound is refused without listing anything.
	if n, ok := zipDeclaredEntries(r, size); !ok || n > maxZipEntries {
		return mismatch()
	}
	zr, err := zip.NewReader(r, size)
	if err != nil || len(zr.File) > maxZipEntries {
		return mismatch()
	}
	var haveTypes, havePart bool
	for _, f := range zr.File {
		switch {
		case f.Name == "[Content_Types].xml":
			haveTypes = true
		case strings.HasPrefix(f.Name, spec.zipDir):
			havePart = true
		}
	}
	if !haveTypes || !havePart {
		return mismatch()
	}
	return nil
}

// zipEOCDSig is the signature of the end of central directory record.
const zipEOCDSig = 0x06054b50

// zipDeclaredEntries reads the total entry count of the end of central directory record (the last 22 bytes plus a
// comment of up to 64 KiB). It reports false when there is no record, for a multi-disk archive, and for a ZIP64
// marker count, which an OOXML file under the bound never needs.
func zipDeclaredEntries(r io.ReaderAt, size int64) (int, bool) {
	const eocdLen, maxComment = 22, 65535
	if size < eocdLen {
		return 0, false
	}
	tail := min(size, eocdLen+maxComment)
	buf := make([]byte, tail)
	if _, err := r.ReadAt(buf, size-tail); err != nil && err != io.EOF {
		return 0, false
	}
	for i := len(buf) - eocdLen; i >= 0; i-- {
		if binary.LittleEndian.Uint32(buf[i:]) != zipEOCDSig {
			continue
		}
		rec := buf[i:]
		disk, cdDisk := binary.LittleEndian.Uint16(rec[4:]), binary.LittleEndian.Uint16(rec[6:])
		onDisk, total := binary.LittleEndian.Uint16(rec[8:]), binary.LittleEndian.Uint16(rec[10:])
		if disk != 0 || cdDisk != 0 || onDisk != total || total == 0xFFFF {
			return 0, false
		}
		return int(total), true
	}
	return 0, false
}

func looksLikeJSON(head []byte) bool {
	b := bytes.TrimLeft(head, " \t\r\n")
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	b = bytes.TrimLeft(b, " \t\r\n")
	return len(b) > 0 && (b[0] == '{' || b[0] == '[')
}

// previewable reports whether a safe previewer exists for the type: raster
// images and plain text only. PDF, Office files and anything scriptable have
// none in a headless core.
func previewable(mimeType string) bool {
	return typeTable[mimeType].preview != previewNone
}

// cleanName reduces a client-supplied file name to the bare, printable name
// the FileRef schema allows: no directory, no separator, no control or format
// character (so no bidirectional-override spoofing), at most 200 characters. It
// is a label only: no path is ever built from it.
func cleanName(raw string) string {
	if i := strings.LastIndexAny(raw, `/\`); i >= 0 {
		raw = raw[i+1:]
	}
	var b strings.Builder
	n := 0
	for _, r := range raw {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		b.WriteRune(r)
		if n++; n >= 200 {
			break
		}
	}
	s := strings.Trim(b.String(), " .")
	if s == "" {
		return "file"
	}
	return s
}

// trimToRune cuts b back to the last complete UTF-8 sequence, so a text that
// was truncated on a byte boundary never ends in half a character.
func trimToRune(b []byte) []byte {
	for i := 0; i < utf8.UTFMax && len(b) > 0; i++ {
		if r, size := utf8.DecodeLastRune(b); r == utf8.RuneError && size <= 1 {
			b = b[:len(b)-1]
			continue
		}
		break
	}
	return b
}

// invalid is a validation_failed error with a machine-readable reason.
func invalid(reason, msg string) *contracts.Error {
	return contracts.New(contracts.ErrValidationFailed, msg).WithDetails("reason", reason)
}
