package source

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// zipEntry builds a local file header followed by data, enough for the head
// sniffer: it reads names, the stored "mimetype" entry and nothing else.
func zipEntry(name string, method uint16, data string) []byte {
	var b bytes.Buffer
	b.WriteString("PK\x03\x04")
	h := make([]byte, 26)
	binary.LittleEndian.PutUint16(h[4:], method)
	binary.LittleEndian.PutUint16(h[22:], uint16(len(name)))
	b.Write(h)
	b.WriteString(name)
	b.WriteString(data)
	return b.Bytes()
}

func zipOf(entries ...[]byte) []byte { return bytes.Join(entries, nil) }

func TestClassify(t *testing.T) {
	pdf := []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n1 0 obj\n")
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	jpg := []byte("\xff\xd8\xff\xe0\x00\x10JFIF")
	tiff := []byte("II*\x00\x08\x00\x00\x00")
	docx := zipOf(zipEntry("[Content_Types].xml", 8, "xxxxxxxx"), zipEntry("_rels/.rels", 8, "yy"), zipEntry("word/document.xml", 8, "zz"))
	xlsx := zipOf(zipEntry("[Content_Types].xml", 8, "x"), zipEntry("xl/workbook.xml", 8, "y"))
	pptx := zipOf(zipEntry("[Content_Types].xml", 8, "x"), zipEntry("ppt/presentation.xml", 8, "y"))
	opcOnly := zipOf(zipEntry("[Content_Types].xml", 8, "x"), zipEntry("customXml/item1.xml", 8, "y"))
	odt := zipOf(zipEntry("mimetype", 0, "application/vnd.oasis.opendocument.text"), zipEntry("content.xml", 8, "x"))
	plainZip := zipOf(zipEntry("a.txt", 8, "hello"), zipEntry("b.txt", 8, "world"))
	ole := []byte("\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1\x00\x00\x00\x00")
	elf := []byte("\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00")
	text := []byte("# Heading\n\nSome text with ünïcode and a [link](x).\n")
	htmlish := []byte("<!-- a comment that starts a markdown file -->\n# Title\n")
	latin1 := []byte("Caf\xe9 au lait, na\xefve r\xe9sum\xe9\n")
	utf16 := append([]byte{0xff, 0xfe}, []byte("h\x00i\x00\n\x00")...)
	nulText := []byte("looks like text\x00but has a NUL byte")
	late := append(bytes.Repeat([]byte("plain text line\n"), 100), 0x00, 0x01)

	tests := []struct {
		name     string
		head     []byte
		file     string
		want     string
		mismatch bool
	}{
		{"pdf by content", pdf, "a.pdf", MIMEPDF, false},
		{"pdf without extension", pdf, "README", MIMEPDF, false},
		{"pdf named md is a mismatch", pdf, "evil.md", MIMEPDF, true},
		{"pdf named txt is a mismatch", pdf, "notes.txt", MIMEPDF, true},
		{"text named pdf is a mismatch", text, "report.pdf", MIMEText, true},
		{"text named docx is a mismatch", text, "report.docx", MIMEText, true},
		{"text named png is a mismatch", text, "x.png", MIMEText, true},
		{"md by name refines text", text, "a.md", MIMEMarkdown, false},
		{"markdown extension", text, "a.markdown", MIMEMarkdown, false},
		{"csv by name", []byte("a,b\n1,2\n"), "t.csv", MIMECSV, false},
		{"json by name", []byte(`{"a":1}`), "t.json", MIMEJSON, false},
		{"yaml by name", []byte("a: 1\n"), "t.yml", MIMEYAML, false},
		{"txt", text, "a.txt", MIMEText, false},
		{"text without extension", text, "LICENSE", MIMEText, false},
		{"text with unknown extension", text, "a.weird", MIMEText, false},
		{"markdown that starts like html", htmlish, "a.md", MIMEMarkdown, false},
		{"html by name", []byte("<!DOCTYPE html><html></html>"), "a.html", MIMEHTML, false},
		{"empty file is text", nil, "a.md", MIMEMarkdown, false},
		{"empty unnamed", nil, "x", MIMEText, false},
		{"latin-1 text is text", latin1, "a.txt", MIMEText, false},
		{"utf-16 with BOM is text", utf16, "a.txt", MIMEText, false},
		{"NUL makes it binary", nulText, "a.txt", MIMEOctet, true},
		{"binary control byte past 512 bytes", late, "a.txt", MIMEOctet, true},
		{"png", png, "a.png", MIMEPNG, false},
		{"png named jpg", png, "a.jpg", MIMEPNG, true},
		{"jpeg", jpg, "a.jpeg", MIMEJPEG, false},
		{"tiff", tiff, "scan.tif", MIMETIFF, false},
		{"docx", docx, "a.docx", MIMEDOCX, false},
		{"docx without extension", docx, "a", MIMEDOCX, false},
		{"docx named xlsx is a mismatch", docx, "a.xlsx", MIMEDOCX, true},
		{"xlsx", xlsx, "a.xlsx", MIMEXLSX, false},
		{"pptx", pptx, "a.pptx", MIMEPPTX, false},
		{"opc package refined by name", opcOnly, "a.docx", MIMEDOCX, false},
		{"opc package without name", opcOnly, "a", MIMEZip, false},
		{"opc package named pdf", opcOnly, "a.pdf", MIMEZip, true},
		{"odt", odt, "a.odt", MIMEODT, false},
		{"plain zip", plainZip, "a.zip", MIMEZip, false},
		{"plain zip named docx is a mismatch", plainZip, "a.docx", MIMEZip, true},
		{"plain zip named txt is a mismatch", plainZip, "a.txt", MIMEZip, true},
		{"ole2 named doc", ole, "a.doc", MIMEDOC, false},
		{"ole2 named xls", ole, "a.xls", MIMEXLS, false},
		{"ole2 unnamed", ole, "a", MIMEOctet, false},
		{"ole2 named pdf", ole, "a.pdf", MIMEOctet, true},
		{"elf", elf, "a.txt", "application/octet-stream", true},
		{"elf unnamed", elf, "tool", "application/octet-stream", false},
		{"rtf", []byte(`{\rtf1\ansi hello}`), "a.rtf", MIMERTF, false},
		{"extension case is ignored", text, "A.MD", MIMEMarkdown, false},
	}
	for _, tc := range tests {
		got := Classify(tc.head, tc.file)
		if got.MIME != tc.want || got.Mismatch != tc.mismatch {
			t.Errorf("%s: Classify(%q) = %+v, want MIME %q mismatch %v", tc.name, tc.file, got, tc.want, tc.mismatch)
		}
	}
}

// The extension is never an input to the decision about what the bytes are.
func TestClassifyIgnoresTheNameForTheBytes(t *testing.T) {
	pdf := []byte("%PDF-1.4\n")
	for _, name := range []string{"a.pdf", "a.md", "a.txt", "a", "a.docx", "a.PDF", ""} {
		if got := Classify(pdf, name); got.MIME != MIMEPDF {
			t.Errorf("Classify(pdf bytes, %q) = %q; the bytes decide", name, got.MIME)
		}
	}
	text := []byte("plain words\n")
	for _, name := range []string{"a.md", "a.txt", "a", "a.csv"} {
		if got := Classify(text, name); got.Mismatch {
			t.Errorf("Classify(text, %q) is a mismatch", name)
		}
	}
}

func TestSniffZipIsBoundedByTheHead(t *testing.T) {
	// A header that claims a name longer than the head must not read past it.
	b := zipEntry("word/document.xml", 8, "")
	cut := b[:len(b)-5]
	if got := sniffZip(cut); got != MIMEZip {
		t.Errorf("a truncated header sniffed as %q", got)
	}
	if got := sniffZip([]byte("PK\x03\x04")); got != MIMEZip {
		t.Errorf("a bare signature sniffed as %q", got)
	}
}
