package formats

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"github.com/buxee/alix-ai/internal/rag/parse/parsetest"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

func TestPDFText(t *testing.T) {
	page1 := `BT /F1 24 Tf 72 720 Td (Annual Report) Tj ET
BT /F1 11 Tf 72 680 Td 14 TL (First paragraph line one) Tj T* (continues on line two.) Tj ET
BT /F1 11 Tf 72 620 Td [(Kerned) -30 ( text) -250 (in an array)] TJ ET
BT /F1 11 Tf 72 560 Td (Second paragraph after a gap.) Tj ET`
	page2 := `BT /F1 11 Tf 72 700 Td (Page two text with \(parens\) and \\ escapes.) Tj ET`
	o := mustParse(t, wire.FormatPDF, parsetest.SimplePDF(page1, page2))
	got := dump(o)
	for _, want := range []string{
		`H1 "Annual Report"`,
		`P "First paragraph line one continues on line two."`,
		`P "Kerned text in an array"`,
		`P "Second paragraph after a gap."`,
		`P "Page two text with (parens) and \\ escapes."`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in\n%s", want, got)
		}
	}
	if len(o.PageStarts) != 2 || o.PageStarts[0] != 0 || o.Text[o.PageStarts[1]-1] != '\f' {
		t.Errorf("page starts = %v\n%s", o.PageStarts, got)
	}
	if hiddenText(o) != "" {
		t.Errorf("ordinary text flagged hidden:\n%s", got)
	}
}

func TestPDFHiddenText(t *testing.T) {
	cases := []struct{ name, content, reason string }{
		{"render mode 3", `BT /F1 12 Tf 72 500 Td 3 Tr (MARK) Tj ET`, wire.HideInvisibleRender},
		{"render mode 7", `BT /F1 12 Tf 72 500 Td 7 Tr (MARK) Tj ET`, wire.HideInvisibleRender},
		{"white on white", `BT /F1 12 Tf 72 500 Td 1 g (MARK) Tj ET`, wire.HideSameColour},
		{"white rgb", `BT /F1 12 Tf 72 500 Td 1 1 1 rg (MARK) Tj ET`, wire.HideSameColour},
		{"white cmyk", `BT /F1 12 Tf 72 500 Td 0 0 0 0 k (MARK) Tj ET`, wire.HideSameColour},
		{"tiny font", `BT /F1 1 Tf 72 500 Td (MARK) Tj ET`, wire.HideTinyFont},
		{"tiny by matrix", `BT /F1 10 Tf 0.1 0 0 0.1 72 500 Tm (MARK) Tj ET`, wire.HideTinyFont},
		{"zero font size", `BT /F1 0 Tf 72 500 Td (MARK) Tj ET`, wire.HideTinyFont},
		{"zero text matrix", `BT /F1 12 Tf 0 0 0 0 72 500 Tm (MARK) Tj ET`, wire.HideTinyFont},
		{"zero transformation matrix", `q 0 0 0 0 72 500 cm BT /F1 12 Tf (MARK) Tj ET Q`, wire.HideTinyFont},
		{"zero horizontal scaling", `q BT /F1 12 Tf 0 Tz 72 500 Td (MARK) Tj ET Q`, wire.HideTinyFont},
		{"squeezed by matrix", `BT /F1 10 Tf 0.1 0 0 1 72 500 Tm (MARK) Tj ET`, wire.HideTinyFont},
		{"white stroke", `q BT /F1 12 Tf 72 500 Td 1 Tr 1 G (MARK) Tj ET Q`, wire.HideSameColour},
		{"white stroke and clip", `q BT /F1 12 Tf 72 500 Td 5 Tr 1 1 1 RG (MARK) Tj ET Q`, wire.HideSameColour},
		{"off the page", `BT /F1 12 Tf 5000 500 Td (MARK) Tj ET`, wire.HideOutsidePage},
		{"negative position", `BT /F1 12 Tf -4000 500 Td (MARK) Tj ET`, wire.HideOutsidePage},
		{"same colour as its rectangle", `0.2 0.4 0.6 rg 50 480 300 40 re f BT /F1 12 Tf 72 500 Td (MARK) Tj ET`, wire.HideSameColour},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			content := `BT /F1 12 Tf 72 700 Td (Visible before.) Tj ET ` + c.content + ` 0 Tr 0 g BT /F1 12 Tf 72 400 Td (Visible after.) Tj ET`
			o := mustParse(t, wire.FormatPDF, parsetest.SimplePDF(content))
			if !strings.Contains(hiddenText(o), "MARK") {
				t.Fatalf("MARK not flagged hidden:\n%s", dump(o))
			}
			var reason string
			for _, b := range o.Blocks {
				if b.Hidden {
					reason = b.HiddenReason
				}
			}
			if reason != c.reason {
				t.Errorf("reason = %q, want %q\n%s", reason, c.reason, dump(o))
			}
			if strings.Contains(visibleText(o), "MARK") {
				t.Errorf("MARK leaked into visible text:\n%s", dump(o))
			}
			if v := visibleText(o); !strings.Contains(v, "Visible before.") || !strings.Contains(v, "Visible after.") {
				t.Errorf("visible text lost:\n%s", dump(o))
			}
		})
	}
}

func TestPDFVisibleLookalikes(t *testing.T) {
	// White text on a dark rectangle, and on an image, is readable.
	content := `0 0 0 rg 50 480 300 40 re f BT /F1 12 Tf 72 500 Td 1 g (white on black) Tj ET
0 g BT /F1 12 Tf 72 300 Td (plain black) Tj ET
q BT /F1 12 Tf 72 250 Td 1 Tr (black outline) Tj ET Q
q 0 0 0 rg 50 180 300 40 re f BT /F1 12 Tf 72 200 Td 1 Tr 1 G (white outline on black) Tj ET Q
q BT /F1 12 Tf 80 Tz 72 150 Td (condensed type) Tj ET Q
q BT /F1 -12 Tf 400 100 Td (mirrored type) Tj ET Q`
	o := mustParse(t, wire.FormatPDF, parsetest.SimplePDF(content))
	if h := hiddenText(o); h != "" {
		t.Errorf("readable text flagged hidden: %q\n%s", h, dump(o))
	}
}

func TestPDFImageOnlyPageNeedsOCR(t *testing.T) {
	d := &parsetest.PDFDoc{}
	img := d.Stream("/Type /XObject /Subtype /Image /Width 1 /Height 1 /ColorSpace /DeviceGray /BitsPerComponent 8", "\x80")
	cs := d.Stream("", "q 612 0 0 792 0 0 cm /Im1 Do Q")
	pagesNum := d.Len() + 2
	pg := d.Add(fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /XObject << /Im1 %d 0 R >> >> >>", pagesNum, cs, img))
	pn := d.Add(fmt.Sprintf("<< /Type /Pages /Kids [%d 0 R] /Count 1 >>", pg))
	root := d.Add(fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pn))
	o := mustParse(t, wire.FormatPDF, d.Bytes(root))
	if len(o.NeedsOCR) != 1 || o.NeedsOCR[0] != 1 {
		t.Errorf("NeedsOCR = %v, want [1]\n%s", o.NeedsOCR, dump(o))
	}
	if strings.TrimSpace(string(o.Text)) != "" {
		t.Errorf("text = %q, want none", o.Text)
	}
}

func TestPDFFlateAndRecovery(t *testing.T) {
	d := &parsetest.PDFDoc{}
	font := d.Add("<< /Type /Font /Subtype /Type1 /BaseFont /Times-Roman >>")
	cs := d.Flate("", []byte("BT /F1 12 Tf 72 700 Td (compressed content) Tj ET"))
	pg := d.Add(fmt.Sprintf("<< /Type /Page /Parent 4 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 %d 0 R >> >> >>", cs, font))
	pn := d.Add(fmt.Sprintf("<< /Type /Pages /Kids [%d 0 R] /Count 1 >>", pg))
	if pn != 4 {
		t.Fatalf("object numbering moved: pages is %d", pn)
	}
	root := d.Add(fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pn))
	good := d.Bytes(root)
	o := mustParse(t, wire.FormatPDF, good)
	if !strings.Contains(string(o.Text), "compressed content") {
		t.Fatalf("flate content lost:\n%s", dump(o))
	}
	// Wreck the cross-reference table: the parser must rebuild it by scanning.
	bad := bytes.Replace(good, []byte("startxref\n"), []byte("startxref\n99999"), 1)
	o = mustParse(t, wire.FormatPDF, bad)
	if !strings.Contains(string(o.Text), "compressed content") {
		t.Fatalf("recovery lost the content:\n%s", dump(o))
	}
	if !containsStr(o.Warnings, "pdf_xref_recovered") {
		t.Errorf("recovery not reported: %v", o.Warnings)
	}
	// And with no trailer or xref at all.
	i := bytes.Index(good, []byte("xref\n0 "))
	o = mustParse(t, wire.FormatPDF, good[:i])
	if !strings.Contains(string(o.Text), "compressed content") {
		t.Fatalf("recovery without a table lost the content:\n%s", dump(o))
	}
}

func TestPDFRecoveryTrailerScanIsLinear(t *testing.T) {
	// Every "trailer(" starts a string that runs to the end of the file:
	// reading each one in turn is quadratic.
	bad := []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\n" + strings.Repeat("trailer(", 10000))
	if n := allocated(func() { Parse(wire.FormatPDF, bad, testLimits()) }); n > 32<<20 {
		t.Errorf("recovering a %d-byte file allocated %d bytes", len(bad), n)
	}
	// The newest trailer still wins: an older one naming a missing root is
	// overridden.
	good := parsetest.SimplePDF(`BT /F1 12 Tf 72 700 Td (recovered root) Tj ET`)
	good = bytes.Replace(good, []byte("startxref\n"), []byte("startxref\n99999"), 1)
	good = bytes.Replace(good, []byte("xref\n0 "), []byte("trailer\n<< /Root 999 0 R /Size 1 >>\nxref\n0 "), 1)
	o := mustParse(t, wire.FormatPDF, good)
	if !strings.Contains(string(o.Text), "recovered root") {
		t.Fatalf("recovery lost the content:\n%s", dump(o))
	}
}

func TestPDFTryRefLeavesNonReferences(t *testing.T) {
	// "N G R" with N outside the object numbers is not a reference, and its
	// generation and R must be left to be read, as for any other lookalike.
	for _, c := range []struct{ in, want string }{
		{"[1 0 R]", "[{1 0}]"},
		{"[-1 0 R 5]", "[-1 0 5]"},
		{"[4294967296 0 R 5]", "[4294967296 0 5]"},
		{"[1 0 Q]", "[1 0]"},
		{"[1 0 obj 2]", "[1 0 2]"},
		{"[1 0 Rx 2]", "[1 0 2]"},
	} {
		l := &lexer{b: []byte(c.in), maxDepth: 10}
		o, err := l.readObject(0)
		if got := fmt.Sprint(o); err != nil || got != c.want {
			t.Errorf("%s = %s (%v), want %s", c.in, got, err, c.want)
		}
	}
	l := &lexer{b: []byte("-1 0 R"), pos: 2, maxDepth: 10}
	if _, ok := l.tryRef(-1); ok || l.pos != 2 {
		t.Errorf("tryRef(-1) = %v, moved to %d", ok, l.pos)
	}
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestPDFToUnicodeAndType0(t *testing.T) {
	cmap := `/CIDInit /ProcSet findresource begin 12 dict begin begincmap
1 begincodespacerange <0000> <FFFF> endcodespacerange
2 beginbfchar <0001> <0048> <0002> <0069> endbfchar
1 beginbfrange <0010> <0012> <0041> endbfrange
endcmap end end`
	d := &parsetest.PDFDoc{}
	tu := d.Stream("", cmap)
	cidfont := d.Add("<< /Type /Font /Subtype /CIDFontType2 /BaseFont /Test /DW 600 /CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> >>")
	font := d.Add(fmt.Sprintf("<< /Type /Font /Subtype /Type0 /BaseFont /Test /Encoding /Identity-H /DescendantFonts [%d 0 R] /ToUnicode %d 0 R >>", cidfont, tu))
	cs := d.Stream("", "BT /F1 12 Tf 72 700 Td <00010002001000110012> Tj ET")
	pagesNum := d.Len() + 2
	pg := d.Add(fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 %d 0 R >> >> >>", pagesNum, cs, font))
	pn := d.Add(fmt.Sprintf("<< /Type /Pages /Kids [%d 0 R] /Count 1 >>", pg))
	root := d.Add(fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pn))
	o := mustParse(t, wire.FormatPDF, d.Bytes(root))
	if !strings.Contains(string(o.Text), "HiABC") {
		t.Fatalf("ToUnicode mapping wrong:\n%s", dump(o))
	}
}

func TestPDFEncodingDifferences(t *testing.T) {
	d := &parsetest.PDFDoc{}
	font := d.Add("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding << /Type /Encoding /BaseEncoding /WinAnsiEncoding /Differences [65 /fi /ffi /uni00E9 /bullet] >> >>")
	cs := d.Stream("", "BT /F1 12 Tf 72 700 Td (ABCD \\351) Tj ET")
	pagesNum := d.Len() + 2
	pg := d.Add(fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 %d 0 R >> >> >>", pagesNum, cs, font))
	pn := d.Add(fmt.Sprintf("<< /Type /Pages /Kids [%d 0 R] /Count 1 >>", pg))
	root := d.Add(fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pn))
	o := mustParse(t, wire.FormatPDF, d.Bytes(root))
	if !strings.Contains(string(o.Text), "ﬁﬃé• é") {
		t.Fatalf("encoding wrong:\n%s", dump(o))
	}
}

func TestPDFObjectStreamsAndXrefStream(t *testing.T) {
	// A PDF whose catalog, pages and page objects live in an object stream and
	// whose cross-reference data is a stream: the modern layout.
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 5 0 R /Resources << /Font << /F1 4 0 R >> >> >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var head, body bytes.Buffer
	for i, o := range objs {
		fmt.Fprintf(&head, "%d %d ", i+1, body.Len())
		body.WriteString(o + "\n")
	}
	stm := head.String() + body.String()
	var out bytes.Buffer
	out.WriteString("%PDF-1.5\n")
	off := map[int]int{}
	off[6] = out.Len()
	var zb bytes.Buffer
	zw := zlib.NewWriter(&zb)
	zw.Write([]byte(stm))
	zw.Close()
	fmt.Fprintf(&out, "6 0 obj\n<< /Type /ObjStm /N %d /First %d /Filter /FlateDecode /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(objs), head.Len(), zb.Len(), zb.String())
	content := "BT /F1 12 Tf 72 700 Td (from an object stream) Tj ET"
	off[5] = out.Len()
	fmt.Fprintf(&out, "5 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(content), content)
	xrefOff := out.Len()
	// W [1 3 1]: type, offset or stream number, generation or index
	var x bytes.Buffer
	row := func(typ, a, b int) { x.Write([]byte{byte(typ), byte(a >> 16), byte(a >> 8), byte(a), byte(b)}) }
	row(0, 0, 255)
	for i := 1; i <= 4; i++ {
		row(2, 6, i-1)
	}
	row(1, off[5], 0)
	row(1, off[6], 0)
	row(1, xrefOff, 0)
	fmt.Fprintf(&out, "7 0 obj\n<< /Type /XRef /Size 8 /W [1 3 1] /Root 1 0 R /Length %d >>\nstream\n%s\nendstream\nendobj\nstartxref\n%d\n%%%%EOF\n", x.Len(), x.String(), xrefOff)
	o := mustParse(t, wire.FormatPDF, out.Bytes())
	if !strings.Contains(string(o.Text), "from an object stream") {
		t.Fatalf("object stream content lost:\n%s\nwarnings=%v", dump(o), o.Warnings)
	}
	if containsStr(o.Warnings, "pdf_xref_recovered") {
		t.Errorf("a valid xref stream needed recovery")
	}
}

func TestPDFHugeStreamLength(t *testing.T) {
	// A /Length near the int64 limit is not trustworthy: the stream ends at
	// its endstream, and the length must not wrap the offset arithmetic.
	content := "BT /F1 12 Tf 72 700 Td (despite the length) Tj ET"
	for _, n := range []string{"9223372036854775807", "9223372036854775000", "4611686018427387904"} {
		d := &parsetest.PDFDoc{}
		font := d.Add("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
		cs := d.Add(fmt.Sprintf("<< /Length %s >>\nstream\n%s\nendstream", n, content))
		pg := d.Add(fmt.Sprintf("<< /Type /Page /Parent 4 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 %d 0 R >> >> >>", cs, font))
		pn := d.Add(fmt.Sprintf("<< /Type /Pages /Kids [%d 0 R] /Count 1 >>", pg))
		root := d.Add(fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pn))
		o := mustParse(t, wire.FormatPDF, d.Bytes(root))
		if !strings.Contains(string(o.Text), "despite the length") {
			t.Errorf("/Length %s: content lost:\n%s", n, dump(o))
		}
	}
}

func TestPDFLinearLinesAndPredictor(t *testing.T) {
	// One baseline shown a character at a time is one line, built in memory
	// linear in its length.
	runs := make([]run, 50000)
	for i := range runs {
		x := float64(i) * 10
		runs[i] = run{text: "a", x0: x, x1: x + 5, y0: 700, y1: 700, size: 10}
	}
	var lines []*pline
	if n := allocated(func() { lines = buildLines(runs) }); n > 32<<20 {
		t.Errorf("a line of %d runs allocated %d bytes", len(runs), n)
	}
	if want := strings.TrimSpace(strings.Repeat("a ", len(runs))); len(lines) != 1 || lines[0].text != want {
		t.Errorf("got %d lines", len(lines))
	}

	// A predictor whose rows are far longer than the data holds no row: it
	// must not allocate two rows' worth of buffers to find that out.
	dp := pdfDict{"Predictor": int64(12), "Colors": int64(32), "BitsPerComponent": int64(16), "Columns": int64(1 << 20)}
	var out []byte
	if n := allocated(func() { out = (&pdfFile{}).predict([]byte{2, 1, 2, 3}, dp) }); n > 1<<20 {
		t.Errorf("predicting 4 bytes allocated %d bytes", n)
	}
	if out == nil || len(out) != 0 {
		t.Errorf("predict = %v, want an empty slice", out)
	}
}

func TestPDFRefusals(t *testing.T) {
	good := parsetest.SimplePDF(`BT /F1 12 Tf 72 700 Td (x) Tj ET`)
	enc := bytes.Replace(good, []byte("/Root "), []byte("/Encrypt << /Filter /Standard >> /Root "), 1)
	if _, err := Parse(wire.FormatPDF, enc, testLimits()); err == nil || err.(*Error).Reason != wire.ReasonEncrypted {
		t.Errorf("encrypted PDF: %v", err)
	}
	if _, err := Parse(wire.FormatPDF, []byte("%PDF-1.4\nthis is not a pdf at all"), testLimits()); err == nil || err.(*Error).Reason != wire.ReasonMalformed {
		t.Errorf("garbage PDF: %v", err)
	}
	if _, err := Parse(wire.FormatPDF, []byte("hello"), testLimits()); err == nil || err.(*Error).Reason != wire.ReasonFormatMismatch {
		t.Errorf("not a PDF: %v", err)
	}
	lim := testLimits()
	lim.MaxPages = 2
	pages := make([]string, 5)
	for i := range pages {
		pages[i] = `BT /F1 12 Tf 72 700 Td (page) Tj ET`
	}
	if _, err := Parse(wire.FormatPDF, parsetest.SimplePDF(pages...), lim); err == nil || err.(*Error).Reason != wire.ReasonTooManyPages {
		t.Errorf("page cap: %v", err)
	}
}

func TestPDFBombs(t *testing.T) {
	lim := testLimits()
	lim.MaxExpandedBytes = 32 << 20
	d := &parsetest.PDFDoc{}
	font := d.Add("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	big := d.Flate("", bytes.Repeat([]byte("q Q "), 40<<20))
	pagesNum := d.Len() + 2
	pg := d.Add(fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 %d 0 R >> >> >>", pagesNum, big, font))
	pn := d.Add(fmt.Sprintf("<< /Type /Pages /Kids [%d 0 R] /Count 1 >>", pg))
	root := d.Add(fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pn))
	_, err := Parse(wire.FormatPDF, d.Bytes(root), lim)
	if err == nil || err.(*Error).Reason != wire.ReasonArchiveBomb {
		t.Fatalf("a content stream that inflates past the budget: %v", err)
	}
}

func TestPDFRepeatedStreamsCountAgainstBudget(t *testing.T) {
	// One small stream listed thousands of times in /Contents must not be
	// multiplied past the decode budget, whatever its filter; nor may an
	// ASCII85 "z" (four zero bytes from one) inflate past it uncounted.
	lim := testLimits()
	lim.MaxExpandedBytes = 4 << 20
	comment := "%" + strings.Repeat("x", 1022) + "\n" // no operators: only the budget can stop it
	refs := func(n, obj int) string { return strings.TrimSpace(strings.Repeat(fmt.Sprintf("%d 0 R ", obj), n)) }
	cases := []struct {
		name, dict, data string
		n                int
	}{
		{"unfiltered", "", comment, 20000},
		{"ASCIIHex", "/Filter /ASCIIHexDecode", fmt.Sprintf("%x>", comment), 20000},
		{"ASCII85", "/Filter /A85", "<~" + strings.Repeat("z", 256) + "~>", 20000},
		{"ASCII85 z once", "/Filter /ASCII85Decode", strings.Repeat("z", 2<<20) + "~>", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &parsetest.PDFDoc{}
			cs := d.Stream(c.dict, c.data)
			pg := d.Add(fmt.Sprintf("<< /Type /Page /Parent 3 0 R /MediaBox [0 0 612 792] /Contents [%s] >>", refs(c.n, cs)))
			pn := d.Add(fmt.Sprintf("<< /Type /Pages /Kids [%d 0 R] /Count 1 >>", pg))
			root := d.Add(fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pn))
			_, err := Parse(wire.FormatPDF, d.Bytes(root), lim)
			if err == nil || err.(*Error).Reason != wire.ReasonArchiveBomb {
				t.Fatalf("err = %v, want the decode budget", err)
			}
		})
	}
	// A stream used once is not charged for its raw bytes, and a form drawn
	// twice still parses.
	d := &parsetest.PDFDoc{}
	font := d.Add("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	form := d.Stream("/Type /XObject /Subtype /Form /BBox [0 0 612 792] /Filter /AHx", fmt.Sprintf("%x>", "BT /F1 12 Tf 72 700 Td (from a form) Tj ET"))
	cs := d.Stream("", comment+"/Fm Do 1 0 0 1 0 -100 cm /Fm Do")
	pg := d.Add(fmt.Sprintf("<< /Type /Page /Parent 5 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 %d 0 R >> /XObject << /Fm %d 0 R >> >> >>", cs, font, form))
	pn := d.Add(fmt.Sprintf("<< /Type /Pages /Kids [%d 0 R] /Count 1 >>", pg))
	root := d.Add(fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pn))
	o := mustParse(t, wire.FormatPDF, d.Bytes(root))
	if strings.Count(string(o.Text), "from a form") != 2 || o.Expanded >= int64(len(comment)) {
		t.Errorf("text %q, expanded %d", o.Text, o.Expanded)
	}
}

func TestPDFOverlappingStreamsCountAgainstBudget(t *testing.T) {
	// Streams with no /Length run to the next endstream: two thousand
	// stream headers before one endstream are two thousand distinct streams
	// that overlap, each used once, together many times the file.
	lim := testLimits()
	lim.MaxExpandedBytes = 4 << 20
	var b strings.Builder
	b.WriteString("%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	b.WriteString("3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents [")
	for n := 10; n < 2010; n++ {
		fmt.Fprintf(&b, "%d 0 R ", n)
	}
	b.WriteString("] >>\nendobj\n")
	for n := 10; n < 2010; n++ {
		fmt.Fprintf(&b, "%d 0 obj << >> stream\n", n)
	}
	b.WriteString("%" + strings.Repeat("x", 1024) + "\nendstream\nendobj\n")
	_, err := Parse(wire.FormatPDF, []byte(b.String()), lim)
	if err == nil || err.(*Error).Reason != wire.ReasonArchiveBomb {
		t.Fatalf("err = %v, want the decode budget", err)
	}
}

func TestPDFOperatorCapIsPerDocument(t *testing.T) {
	// Every page runs under the operator cap, the document does not.
	lim := testLimits()
	lim.MaxExpandedBytes, lim.MaxExpansionRatio = 1<<30, 1<<20
	perPage := maxOperators/40 + 1
	d := &parsetest.PDFDoc{}
	cs := d.Flate("", bytes.Repeat([]byte("n "), perPage))
	var kids []string
	pagesNum := d.Len() + 41
	for i := 0; i < 40; i++ {
		pg := d.Add(fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 612 792] /Contents %d 0 R >>", pagesNum, cs))
		kids = append(kids, fmt.Sprintf("%d 0 R", pg))
	}
	pn := d.Add(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count 40 >>", strings.Join(kids, " ")))
	root := d.Add(fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pn))
	_, err := Parse(wire.FormatPDF, d.Bytes(root), lim)
	if err == nil || err.(*Error).Reason != wire.ReasonMalformed || !strings.Contains(err.Error(), "too complex") {
		t.Fatalf("%d operators over 40 pages: err = %v, want the operator cap", 40*perPage, err)
	}
}

func TestPDFOutputCap(t *testing.T) {
	lim := testLimits()
	lim.MaxOutputBytes = 1000
	var sb strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&sb, "BT /F1 12 Tf 72 %d Td (line number %d of a long page of text) Tj ET\n", 700-i*5, i)
	}
	_, err := Parse(wire.FormatPDF, parsetest.SimplePDF(sb.String()), lim)
	if err == nil || err.(*Error).Reason != wire.ReasonOutputTooLarge {
		t.Fatalf("err = %v, want output_too_large", err)
	}
}

func TestPDFFormNestingAndCycles(t *testing.T) {
	d := &parsetest.PDFDoc{}
	font := d.Add("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	// the form draws itself; its content must still come through once
	form := d.Stream("/Type /XObject /Subtype /Form /BBox [0 0 100 100] /Resources << /Font << /F1 1 0 R >> /XObject << /Fm 2 0 R >> >>",
		"BT /F1 12 Tf 72 700 Td (inside a form) Tj ET /Fm Do /Fm Do")
	if form != 2 {
		t.Fatalf("form is object %d", form)
	}
	cs := d.Stream("", "/Fm Do")
	pagesNum := d.Len() + 2
	pg := d.Add(fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 %d 0 R >> /XObject << /Fm %d 0 R >> >> >>", pagesNum, cs, font, form))
	pn := d.Add(fmt.Sprintf("<< /Type /Pages /Kids [%d 0 R] /Count 1 >>", pg))
	root := d.Add(fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pn))
	o, err := Parse(wire.FormatPDF, d.Bytes(root), testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(o.Text), "inside a form") != 1 {
		t.Errorf("self-referencing form: %q", o.Text)
	}
}

// nextEndstream answers what a scan from each position would find, from one
// pass over the file.
func TestPDFNextEndstreamMatchesAScan(t *testing.T) {
	data := []byte("x endstream yy endstreamendstream z endstrea")
	f := &pdfFile{data: data}
	for p := 0; p <= len(data); p++ {
		want := -1
		if i := bytes.Index(data[p:], []byte("endstream")); i >= 0 {
			want = p + i
		}
		if got := f.nextEndstream(p); got != want {
			t.Errorf("nextEndstream(%d) = %d, want %d", p, got, want)
		}
	}
}
