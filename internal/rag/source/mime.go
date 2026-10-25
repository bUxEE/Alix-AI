package source

import (
	"bytes"
	"encoding/binary"
	"net/http"
	"path"
	"strings"
)

// SniffLen is how many bytes from the start of an item Classify needs. A
// provider reads this many bytes (or the whole item, if smaller) before it
// decides whether the item is accepted.
const SniffLen = 64 << 10

// The MIME types Classify can return, with the spelling a package declaration
// must use in `formats`.
const (
	MIMEText     = "text/plain"
	MIMEMarkdown = "text/markdown"
	MIMECSV      = "text/csv"
	MIMETSV      = "text/tab-separated-values"
	MIMEHTML     = "text/html"
	MIMEJSON     = "application/json"
	MIMEXML      = "application/xml"
	MIMEYAML     = "application/yaml"
	MIMEPDF      = "application/pdf"
	MIMERTF      = "application/rtf"
	MIMEZip      = "application/zip"
	MIMEOctet    = "application/octet-stream"
	MIMEDOCX     = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	MIMEXLSX     = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	MIMEPPTX     = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	MIMEODT      = "application/vnd.oasis.opendocument.text"
	MIMEODS      = "application/vnd.oasis.opendocument.spreadsheet"
	MIMEODP      = "application/vnd.oasis.opendocument.presentation"
	MIMEDOC      = "application/msword"
	MIMEXLS      = "application/vnd.ms-excel"
	MIMEPPT      = "application/vnd.ms-powerpoint"
	MIMEPNG      = "image/png"
	MIMEJPEG     = "image/jpeg"
	MIMEGIF      = "image/gif"
	MIMETIFF     = "image/tiff"
	MIMEWebP     = "image/webp"
	MIMEBMP      = "image/bmp"

	// mimeOLE and mimeOPC are internal family markers for containers whose
	// subtype the bytes cannot settle: an OLE2 compound file (legacy .doc,
	// .xls, .ppt) and an Open Packaging Convention zip (.docx, .xlsx, .pptx)
	// whose entry names are past the sniffed head.
	mimeOLE = "application/x-ole-storage"
	mimeOPC = "application/x-opc-package"
)

// Class is the outcome of Classify.
type Class struct {
	// MIME is the effective type: what the bytes are, refined by the file name
	// only inside a family the bytes cannot tell apart.
	MIME string
	// NameMIME is the type the file name suggests, "" when the extension is
	// unknown.
	NameMIME string
	// Mismatch is set when the name claims a type the bytes contradict: a
	// ".pdf" that is plain text, a ".md" that is a PDF, a ".docx" that is not
	// an Office package. A provider skips such an item instead of choosing
	// which of the two to believe.
	Mismatch bool
}

// Classify decides the MIME type of an item from its first bytes (at least
// min(size, SniffLen) of them) and its name.
//
// The bytes decide. PDF, RTF, TIFF, zip containers (including Office Open XML
// and OpenDocument, recognised from the entry names in the head), OLE2 files
// and the common image types are identified by magic numbers; anything that
// is valid text, has no NUL byte and no binary control bytes is text. The file
// name is never an input to that decision. It is consulted only to choose
// among types the bytes cannot distinguish: Markdown, CSV, JSON, YAML, HTML and
// XML are all "text" to a sniffer, and an OPC zip is a .docx, .xlsx or .pptx
// only by name. A name that contradicts the bytes sets Mismatch.
func Classify(head []byte, name string) Class {
	content, text := sniffContent(head)
	byName := mimeFromName(name)
	c := Class{NameMIME: byName, MIME: content}

	switch {
	case text:
		switch {
		case byName == "":
		case isTextMIME(byName):
			c.MIME = byName
		default:
			c.Mismatch = true
		}
	case content == mimeOPC:
		switch byName {
		case MIMEDOCX, MIMEXLSX, MIMEPPTX:
			c.MIME = byName
		case "":
			c.MIME = MIMEZip
		default:
			c.MIME, c.Mismatch = MIMEZip, true
		}
	case content == mimeOLE:
		switch byName {
		case MIMEDOC, MIMEXLS, MIMEPPT:
			c.MIME = byName
		case "":
			c.MIME = MIMEOctet
		default:
			c.MIME, c.Mismatch = MIMEOctet, true
		}
	default:
		c.Mismatch = byName != "" && byName != content
	}
	return c
}

func isTextMIME(m string) bool {
	switch m {
	case MIMEText, MIMEMarkdown, MIMECSV, MIMETSV, MIMEHTML, MIMEJSON, MIMEXML, MIMEYAML:
		return true
	}
	return false
}

var nameTypes = map[string]string{
	".txt": MIMEText, ".text": MIMEText, ".log": MIMEText,
	".md": MIMEMarkdown, ".markdown": MIMEMarkdown,
	".csv": MIMECSV, ".tsv": MIMETSV,
	".html": MIMEHTML, ".htm": MIMEHTML,
	".json": MIMEJSON, ".xml": MIMEXML, ".yaml": MIMEYAML, ".yml": MIMEYAML,
	".pdf": MIMEPDF, ".rtf": MIMERTF, ".zip": MIMEZip,
	".docx": MIMEDOCX, ".xlsx": MIMEXLSX, ".pptx": MIMEPPTX,
	".odt": MIMEODT, ".ods": MIMEODS, ".odp": MIMEODP,
	".doc": MIMEDOC, ".xls": MIMEXLS, ".ppt": MIMEPPT,
	".png": MIMEPNG, ".jpg": MIMEJPEG, ".jpeg": MIMEJPEG, ".gif": MIMEGIF,
	".tif": MIMETIFF, ".tiff": MIMETIFF, ".webp": MIMEWebP, ".bmp": MIMEBMP,
}

func mimeFromName(name string) string {
	return nameTypes[strings.ToLower(path.Ext(name))]
}

// sniffContent identifies the bytes alone. text is true for textual content,
// in which case the returned type is MIMEText.
func sniffContent(head []byte) (mime string, text bool) {
	if len(head) == 0 {
		return MIMEText, true
	}
	switch {
	case bytes.HasPrefix(head, []byte("%PDF-")):
		return MIMEPDF, false
	case bytes.HasPrefix(head, []byte(`{\rtf`)):
		return MIMERTF, false
	case bytes.HasPrefix(head, []byte("II*\x00")), bytes.HasPrefix(head, []byte("MM\x00*")):
		return MIMETIFF, false
	case bytes.HasPrefix(head, []byte("\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1")):
		return mimeOLE, false
	case bytes.HasPrefix(head, []byte("PK\x03\x04")), bytes.HasPrefix(head, []byte("PK\x05\x06")):
		return sniffZip(head), false
	}
	probe := head
	if len(probe) > 512 {
		probe = probe[:512]
	}
	detected := http.DetectContentType(probe)
	if i := strings.IndexByte(detected, ';'); i >= 0 {
		detected = detected[:i]
	}
	switch {
	case strings.HasPrefix(detected, "text/"):
		if looksBinary(head) {
			return MIMEOctet, false
		}
		return MIMEText, true
	case detected == "application/x-gzip":
		return "application/gzip", false
	}
	return detected, false
}

// looksBinary reports whether a head that http.DetectContentType called text
// holds a NUL or another binary control byte beyond the 512 bytes it inspected.
// Text with a UTF-16 byte order mark legitimately holds NULs and low bytes and
// is exempt. Legacy single-byte text (Latin-1, CP1252) is still text.
func looksBinary(head []byte) bool {
	if bytes.HasPrefix(head, []byte{0xfe, 0xff}) || bytes.HasPrefix(head, []byte{0xff, 0xfe}) {
		return false
	}
	for _, b := range head {
		if b <= 0x08 || b == 0x0b || (b >= 0x0e && b <= 0x1a) || (b >= 0x1c && b <= 0x1f) {
			return true
		}
	}
	return false
}

// sniffZip recognises OpenDocument (a stored "mimetype" first entry), Office
// Open XML (entry names under word/, xl/ or ppt/) and, when only the OPC
// marker [Content_Types].xml is in the head, the OPC family; anything else is
// a plain zip. It walks local file headers found in the head and trusts
// nothing past it, so a crafted archive can at worst be misclassified; the
// parser that reads it later has to survive that anyway.
func sniffZip(head []byte) string {
	const hdr = 30
	var names []string
	first := true
	for off := 0; off+hdr <= len(head) && len(names) < 64; {
		i := bytes.Index(head[off:], []byte("PK\x03\x04"))
		if i < 0 {
			break
		}
		off += i
		if off+hdr > len(head) {
			break
		}
		method := binary.LittleEndian.Uint16(head[off+8:])
		nameLen := int(binary.LittleEndian.Uint16(head[off+26:]))
		extraLen := int(binary.LittleEndian.Uint16(head[off+28:]))
		if nameLen == 0 || off+hdr+nameLen > len(head) {
			off++
			continue
		}
		name := string(head[off+hdr : off+hdr+nameLen])
		if first && name == "mimetype" && method == 0 {
			start := off + hdr + nameLen + extraLen
			if start <= len(head) {
				body := head[start:]
				if len(body) > 80 {
					body = body[:80]
				}
				for _, t := range []string{MIMEODT, MIMEODS, MIMEODP} {
					if bytes.HasPrefix(body, []byte(t)) {
						return t
					}
				}
			}
		}
		first = false
		names = append(names, name)
		off += hdr + nameLen
	}
	opc := false
	for _, n := range names {
		switch {
		case strings.HasPrefix(n, "word/"):
			return MIMEDOCX
		case strings.HasPrefix(n, "xl/"):
			return MIMEXLSX
		case strings.HasPrefix(n, "ppt/"):
			return MIMEPPTX
		case n == "[Content_Types].xml":
			opc = true
		}
	}
	if opc {
		return mimeOPC
	}
	return MIMEZip
}
