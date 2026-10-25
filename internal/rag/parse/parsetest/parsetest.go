// Package parsetest builds small, reproducible test documents for the parser
// tests: zip containers (including deliberately malformed and hostile ones),
// DOCX files, and PDF files. It is imported only from _test.go files. It builds
// bombs by streaming, so no multi-hundred-megabyte fixture is ever stored.
package parsetest

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"sort"
	"strings"
	"testing"
	"time"
)

// DocxNS is the namespace declaration of the WordprocessingML elements used by
// the helpers.
const DocxNS = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" ` +
	`xmlns:mc="http://schemas.openxmlformats.org/markup-compatibility/2006" ` +
	`xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing"`

// Zip writes a zip with the given parts in name order and a fixed timestamp, so
// the bytes are reproducible.
func Zip(t testing.TB, parts map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(parts))
	for n := range parts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: n, Method: zip.Deflate, Modified: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(parts[n])); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// DOCX builds a minimal DOCX from the XML of a document body and optional extra
// parts (which may replace the document part).
func DOCX(t testing.TB, body string, extra map[string]string) []byte {
	t.Helper()
	parts := map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`,
		"word/document.xml":   `<?xml version="1.0" encoding="UTF-8"?><w:document ` + DocxNS + `><w:body>` + body + `</w:body></w:document>`,
	}
	for k, v := range extra {
		parts[k] = v
	}
	return Zip(t, parts)
}

// Para is a paragraph with one run of text.
func Para(text string) string {
	return `<w:p><w:r><w:t xml:space="preserve">` + text + `</w:t></w:r></w:p>`
}

// ParaRPr is a paragraph with one run with the given run properties.
func ParaRPr(rpr, text string) string {
	return `<w:p><w:r><w:rPr>` + rpr + `</w:rPr><w:t xml:space="preserve">` + text + `</w:t></w:r></w:p>`
}

// RawEntry describes one zip entry whose header fields a test may falsify.
type RawEntry struct {
	Name string
	// Data is the uncompressed content; it is deflated unless Stored.
	Data   []byte
	Stored bool
	// Compressed, when set, is written as the entry's data instead of
	// compressing Data (a prepared bomb).
	Compressed []byte
	// DeclaredUSize and DeclaredCRC, when non-nil, replace the true values in the
	// headers: the entry then lies about itself.
	DeclaredUSize *uint32
	DeclaredCRC   *uint32
	// ShareDataWith, when > 0, makes this entry a directory record only, pointing
	// at the data of entry ShareDataWith-1 (an overlapping entry).
	ShareDataWith int
}

type rawRec struct {
	e            RawEntry
	method       uint16
	crc          uint32
	csize, usize uint32
	offset       uint32
}

// RawZip writes a zip from entries, local headers and central directory, with
// full control of the fields a test wants to falsify.
func RawZip(entries []RawEntry) []byte {
	var out bytes.Buffer
	le := binary.LittleEndian
	recs := make([]rawRec, len(entries))
	for i, e := range entries {
		r := rawRec{e: e, method: 8}
		if e.ShareDataWith > 0 {
			r = recs[e.ShareDataWith-1]
			r.e = e
			recs[i] = r
			continue
		}
		var comp []byte
		switch {
		case e.Compressed != nil:
			comp = e.Compressed
		case e.Stored:
			r.method = 0
			comp = e.Data
		default:
			var b bytes.Buffer
			fw, _ := flate.NewWriter(&b, flate.BestCompression)
			fw.Write(e.Data)
			fw.Close()
			comp = b.Bytes()
		}
		r.crc = crc32.ChecksumIEEE(e.Data)
		r.usize = uint32(len(e.Data))
		r.csize = uint32(len(comp))
		if e.DeclaredUSize != nil {
			r.usize = *e.DeclaredUSize
		}
		if e.DeclaredCRC != nil {
			r.crc = *e.DeclaredCRC
		}
		r.offset = uint32(out.Len())
		var h [30]byte
		le.PutUint32(h[0:], 0x04034b50)
		le.PutUint16(h[4:], 20)
		le.PutUint16(h[8:], r.method)
		le.PutUint32(h[14:], r.crc)
		le.PutUint32(h[18:], r.csize)
		le.PutUint32(h[22:], r.usize)
		le.PutUint16(h[26:], uint16(len(e.Name)))
		out.Write(h[:])
		out.WriteString(e.Name)
		out.Write(comp)
		recs[i] = r
	}
	cdStart := out.Len()
	for _, r := range recs {
		var h [46]byte
		le.PutUint32(h[0:], 0x02014b50)
		le.PutUint16(h[4:], 20)
		le.PutUint16(h[6:], 20)
		le.PutUint16(h[10:], r.method)
		le.PutUint32(h[16:], r.crc)
		le.PutUint32(h[20:], r.csize)
		le.PutUint32(h[24:], r.usize)
		le.PutUint16(h[28:], uint16(len(r.e.Name)))
		le.PutUint32(h[42:], r.offset)
		out.Write(h[:])
		out.WriteString(r.e.Name)
	}
	cdSize := out.Len() - cdStart
	var eocd [22]byte
	le.PutUint32(eocd[0:], 0x06054b50)
	le.PutUint16(eocd[8:], uint16(len(recs)))
	le.PutUint16(eocd[10:], uint16(len(recs)))
	le.PutUint32(eocd[12:], uint32(cdSize))
	le.PutUint32(eocd[16:], uint32(cdStart))
	out.Write(eocd[:])
	return out.Bytes()
}

// ZeroBomb returns the deflate stream of n zero bytes, built by streaming, and
// the CRC-32 of those bytes.
func ZeroBomb(n int) (deflated []byte, crc uint32) {
	var b bytes.Buffer
	fw, _ := flate.NewWriter(&b, flate.BestCompression)
	zeros := make([]byte, 1<<20)
	h := crc32.NewIEEE()
	for left := n; left > 0; {
		c := min(left, len(zeros))
		fw.Write(zeros[:c])
		h.Write(zeros[:c])
		left -= c
	}
	fw.Close()
	return b.Bytes(), h.Sum32()
}

// RepeatedXML returns a document.xml whose body is rep repeated n times: a
// highly compressible part that inflates to a known size.
func RepeatedXML(rep string, n int) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0"?><w:document ` + DocxNS + `><w:body>`)
	for i := 0; i < n; i++ {
		b.WriteString(rep)
	}
	b.WriteString(`</w:body></w:document>`)
	return b.Bytes()
}

// ---- PDF ----

// PDFDoc builds small, valid PDFs with a classic cross-reference table.
type PDFDoc struct {
	objs []string // object bodies, numbered from 1
}

// Add appends an object and returns its number.
func (d *PDFDoc) Add(body string) int {
	d.objs = append(d.objs, body)
	return len(d.objs)
}

// Len is the number of objects so far.
func (d *PDFDoc) Len() int { return len(d.objs) }

// Stream appends a stream object.
func (d *PDFDoc) Stream(dict, data string) int {
	return d.Add(fmt.Sprintf("<< %s /Length %d >>\nstream\n%s\nendstream", dict, len(data), data))
}

// Flate appends a Flate-compressed stream object.
func (d *PDFDoc) Flate(dict string, data []byte) int {
	return d.Stream(dict+" /Filter /FlateDecode", string(ZlibStream(data)))
}

// Bytes serialises the document; root is the catalog's object number.
func (d *PDFDoc) Bytes(root int) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n")
	offs := make([]int, len(d.objs))
	for i, o := range d.objs {
		offs[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(d.objs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root %d 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(d.objs)+1, root, xref)
	return b.Bytes()
}

// SimplePDF builds a document with one Helvetica font (/F1) and one page per
// content stream.
func SimplePDF(pages ...string) []byte {
	d := &PDFDoc{}
	font := d.Add("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	var kids []string
	pagesNum := d.Len() + 2*len(pages) + 1
	for _, c := range pages {
		cs := d.Stream("", c)
		pg := d.Add(fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 %d 0 R >> >> >>", pagesNum, cs, font))
		kids = append(kids, fmt.Sprintf("%d 0 R", pg))
	}
	pn := d.Add(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)))
	if pn != pagesNum {
		panic("pages object number")
	}
	root := d.Add(fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pn))
	return d.Bytes(root)
}

// ZlibStream compresses data as a zlib stream.
func ZlibStream(data []byte) []byte {
	var b bytes.Buffer
	zw := zlib.NewWriter(&b)
	zw.Write(data)
	zw.Close()
	return b.Bytes()
}
