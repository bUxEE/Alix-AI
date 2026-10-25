package fuzzsec

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/rag/parse/formats"
	"github.com/buxee/alix-ai/internal/rag/parse/parsetest"
	"github.com/buxee/alix-ai/internal/rag/parse/wire"
	"github.com/buxee/alix-ai/tests/security/secutil"
)

// parseFormats is the order in which the first fuzz byte selects a format.
var parseFormats = wire.Formats()

// parseLimits are small, so that a bomb is refused quickly and a run covers many
// inputs: the production limits are bigger, the logic is the same.
var parseLimits = wire.Limits{
	MaxInputBytes:     1 << 20,
	MaxOutputBytes:    1 << 20,
	MaxExpandedBytes:  2 << 20,
	MaxExpansionRatio: 100,
	MaxEntries:        200,
	MaxDepth:          50,
	MaxPages:          50,
	MaxBlocks:         20000,
}

var reWarningCode = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// pathLike reports an archive entry name that is, or could be turned into, a
// place on a file system outside the one the parser works in.
func pathLike(name string) bool {
	return len(name) >= 3 && (strings.ContainsAny(name, "/\\\x00") || strings.Contains(name, "..") || strings.Contains(name, ":"))
}

// zipNames lists the entry names of a zip, best effort (a hostile archive may
// not be one).
func zipNames(data []byte) []string {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || zr == nil {
		return nil
	}
	var out []string
	for _, f := range zr.File {
		out = append(out, f.Name)
	}
	return out
}

// checkParse is every property of one parse of data as format. It returns the
// first violation, "" when none. entryNames are the archive entry names of the
// input, for the path check.
func checkParse(format wire.Format, data []byte, entryNames []string) (msg string, out *formats.Output, err error) {
	start := time.Now()
	out, err = formats.Parse(format, data, parseLimits)
	if took := time.Since(start); took > 20*time.Second {
		return fmt.Sprintf("%s: the parse took %s", format, took), out, err
	}
	var seen []string
	if err != nil {
		var fe *formats.Error
		if !errors.As(err, &fe) {
			return fmt.Sprintf("%s: an error that is not a parser error: %v", format, err), out, err
		}
		if fe.Reason == wire.ReasonInternal {
			return fmt.Sprintf("%s: an internal failure (a panic in the parser): %v", format, err), out, err
		}
		seen = append(seen, fe.Message)
	} else {
		r := &wire.Result{TextSize: int64(len(out.Text)), PageStarts: out.PageStarts, Blocks: out.Blocks}
		if verr := r.Validate(out.Text, parseLimits.MaxBlocks); verr != nil {
			return fmt.Sprintf("%s: the output is not valid: %v", format, verr), out, err
		}
		if int64(len(out.Text)) > parseLimits.MaxOutputBytes {
			return fmt.Sprintf("%s: %d bytes of text over the output limit %d", format, len(out.Text), parseLimits.MaxOutputBytes), out, err
		}
		if out.Expanded > parseLimits.MaxExpandedBytes {
			return fmt.Sprintf("%s: %d bytes expanded over the budget %d", format, out.Expanded, parseLimits.MaxExpandedBytes), out, err
		}
		for _, w := range out.Warnings {
			if !reWarningCode.MatchString(w) {
				return fmt.Sprintf("%s: a warning that is not a stable code: %q", format, w), out, err
			}
		}
		seen = append(seen, out.Warnings...)
	}
	return leaksEntryName(entryNames, seen...), out, err
}

// leaksEntryName returns a description when one of the strings (a message or a
// warning, which are what a caller logs and returns) holds an archive entry name
// that is path-like.
func leaksEntryName(names []string, strs ...string) string {
	for _, n := range names {
		if !pathLike(n) {
			continue
		}
		for _, s := range strs {
			if strings.Contains(s, n) {
				return fmt.Sprintf("an archive entry name, %q, is in %q", n, s)
			}
		}
	}
	return ""
}

// markerState says where a marker ended up: in a hidden block, in a visible one.
func markerState(out *formats.Output, marker string) (hidden, visible bool) {
	for _, b := range out.Blocks {
		if b.Start < 0 || b.End > len(out.Text) || b.Start >= b.End {
			continue
		}
		if strings.Contains(string(out.Text[b.Start:b.End]), marker) {
			if b.Hidden {
				hidden = true
			} else {
				visible = true
			}
		}
	}
	// Text that belongs to no block counts as visible: nothing flags it.
	if strings.Count(string(out.Text), marker) > 0 && !hidden && !visible {
		visible = true
	}
	return hidden, visible
}

// hiddenDoc is one way to hide a marker in one format. build wraps the marker in
// the construct and puts visible text (already escaped for the format) around it.
type hiddenDoc struct {
	name   string
	format wire.Format
	build  func(tb testing.TB, filler, marker string) []byte
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xmlEscapeTo(&b, s)
	return b.String()
}

func xmlEscapeTo(b *bytes.Buffer, s string) error {
	for _, r := range s {
		switch r {
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '&':
			b.WriteString("&amp;")
		case '"':
			b.WriteString("&quot;")
		case '\t', '\n', '\r':
			b.WriteRune(' ')
		default:
			if r < 0x20 || r == 0xFFFE || r == 0xFFFF || (r >= 0xD800 && r <= 0xDFFF) {
				continue
			}
			b.WriteRune(r)
		}
	}
	return nil
}

func htmlHidden(construct string) func(tb testing.TB, filler, marker string) []byte {
	return func(_ testing.TB, filler, marker string) []byte {
		f := html.EscapeString(strings.ToValidUTF8(filler, ""))
		return []byte("<body><p>Visible " + f + "</p>" + strings.ReplaceAll(construct, "MARK", marker) + "<p>More visible " + f + "</p></body>")
	}
}

func docxHidden(rpr string) func(tb testing.TB, filler, marker string) []byte {
	return func(tb testing.TB, filler, marker string) []byte {
		f := xmlEscape(strings.ToValidUTF8(filler, ""))
		return parsetest.DOCX(tb, parsetest.Para("Visible "+f)+parsetest.ParaRPr(rpr, marker)+parsetest.Para("More visible "+f), nil)
	}
}

var hiddenDocs = []hiddenDoc{
	{"html display none", wire.FormatHTML, htmlHidden(`<div style="display:none">MARK</div>`)},
	{"html hidden attribute", wire.FormatHTML, htmlHidden(`<p hidden>MARK</p>`)},
	{"html aria-hidden", wire.FormatHTML, htmlHidden(`<p aria-hidden="true">MARK</p>`)},
	{"html opacity zero", wire.FormatHTML, htmlHidden(`<p style="opacity:0">MARK</p>`)},
	{"html font size zero", wire.FormatHTML, htmlHidden(`<p style="font-size:0">MARK</p>`)},
	{"html white on white", wire.FormatHTML, htmlHidden(`<p style="color:#fff">MARK</p>`)},
	{"html off screen", wire.FormatHTML, htmlHidden(`<p style="position:absolute;left:-9999px">MARK</p>`)},
	{"docx vanish", wire.FormatDOCX, docxHidden(`<w:vanish/>`)},
	{"docx web hidden", wire.FormatDOCX, docxHidden(`<w:webHidden/>`)},
}

// checkHidden builds the document with a random marker in the hiding construct
// and visible, hostile filler around it, and requires the marker in a hidden
// block and nowhere visible.
func checkHidden(tb testing.TB, h hiddenDoc, filler string) string {
	marker := "hm" + randWord(tb, 14)
	doc := h.build(tb, filler, marker)
	msg, out, err := checkParse(h.format, doc, zipNames(doc))
	if msg != "" {
		return h.name + ": " + msg
	}
	if err != nil {
		return fmt.Sprintf("%s: the document was refused: %v", h.name, err)
	}
	hidden, visible := markerState(out, marker)
	switch {
	case !hidden:
		return h.name + ": the hidden marker was not flagged hidden"
	case visible:
		return h.name + ": the hidden marker is also in visible text"
	}
	return ""
}

// parseSeeds are the small valid documents and the hostile archives, built the
// way the parser's own tests build them, plus the sample documents of the parser.
func parseSeeds(f *testing.F) (seeds []struct {
	which uint8
	data  []byte
}) {
	add := func(format wire.Format, data []byte) {
		for i, ff := range parseFormats {
			if ff == format {
				seeds = append(seeds, struct {
					which uint8
					data  []byte
				}{uint8(i), data})
			}
		}
	}
	root := secutil.RepoRoot(f)
	for _, s := range []struct {
		format wire.Format
		file   string
	}{
		{wire.FormatTXT, "sample.txt"}, {wire.FormatTXT, "latin1.txt"}, {wire.FormatMD, "sample.md"}, {wire.FormatHTML, "sample.html"},
		{wire.FormatCSV, "sample.csv"}, {wire.FormatPDF, "sample.pdf"}, {wire.FormatDOCX, "sample.docx"},
	} {
		b, err := os.ReadFile(filepath.Join(root, "internal", "rag", "parse", "testdata", s.file))
		if err != nil {
			f.Fatal(err)
		}
		add(s.format, b)
	}
	add(wire.FormatPDF, parsetest.SimplePDF("page one", "page two"))
	add(wire.FormatMD, []byte("# Title\n\ntext <!-- hidden comment -->\n\n<div hidden>hidden</div>\n\n```\ncode\n```\n"))
	add(wire.FormatHTML, []byte("<html><body><h1>T</h1><p style=\"display:none\">x</p><table><tr><td>a</td></tr></table></body></html>"))
	add(wire.FormatCSV, []byte("a,b\n1,\"2\n3\"\n"))
	add(wire.FormatDOCX, parsetest.DOCX(f, parsetest.Para("hello")+parsetest.ParaRPr("<w:vanish/>", "secret"), nil))
	// Zip-slip entry names next to a valid document part.
	body := []byte(`<?xml version="1.0"?><w:document ` + parsetest.DocxNS + `><w:body>` + parsetest.Para("hello") + `</w:body></w:document>`)
	for _, name := range []string{"../../escape.txt", "/etc/cron.d/x", "C:\\Windows\\x", "word\\..\\..\\x", "a/../../b", "x\x00y.xml"} {
		add(wire.FormatDOCX, parsetest.RawZip([]parsetest.RawEntry{
			{Name: "[Content_Types].xml", Data: []byte(`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`)},
			{Name: "word/document.xml", Data: body},
			{Name: name, Data: []byte("payload")},
		}))
	}
	// Bombs: a part that inflates to far more than the budget, a part whose header
	// lies about its size, overlapping entries, duplicate names.
	bomb, crc := parsetest.ZeroBomb(8 << 20)
	usize := uint32(10)
	add(wire.FormatDOCX, parsetest.RawZip([]parsetest.RawEntry{
		{Name: "[Content_Types].xml", Data: []byte(`<Types/>`)},
		{Name: "word/document.xml", Data: make([]byte, 8<<20), Compressed: bomb, DeclaredCRC: &crc},
	}))
	add(wire.FormatDOCX, parsetest.RawZip([]parsetest.RawEntry{
		{Name: "[Content_Types].xml", Data: []byte(`<Types/>`)},
		{Name: "word/document.xml", Data: make([]byte, 8<<20), Compressed: bomb, DeclaredUSize: &usize, DeclaredCRC: &crc},
	}))
	add(wire.FormatDOCX, parsetest.RawZip([]parsetest.RawEntry{
		{Name: "word/document.xml", Data: body},
		{Name: "word/document.xml", Data: body},
	}))
	add(wire.FormatDOCX, parsetest.RawZip([]parsetest.RawEntry{
		{Name: "word/document.xml", Data: body},
		{Name: "word/other.xml", ShareDataWith: 1},
	}))
	var many []parsetest.RawEntry
	for i := 0; i < parseLimits.MaxEntries+20; i++ {
		many = append(many, parsetest.RawEntry{Name: fmt.Sprintf("word/p%d.xml", i), Data: []byte("x")})
	}
	add(wire.FormatDOCX, parsetest.RawZip(many))
	add(wire.FormatDOCX, parsetest.DOCX(f, "", map[string]string{"word/document.xml": string(parsetest.RepeatedXML("<w:p><w:r><w:t>a</w:t></w:r></w:p>", 200000))}))
	// Deep nesting and a million-cell table.
	add(wire.FormatHTML, []byte(strings.Repeat("<div>", 5000)+"x"+strings.Repeat("</div>", 5000)))
	add(wire.FormatHTML, []byte("<table>"+strings.Repeat("<tr><td>a</td></tr>", 5000)+"</table>"))
	add(wire.FormatMD, []byte(strings.Repeat("> ", 5000)+"x"))
	add(wire.FormatCSV, []byte(strings.Repeat("a,", 50000)+"\n"))
	return seeds
}

// SEC-016
// FuzzParseDocument: the in-process document parsers (internal/rag/parse/
// formats, the code the sandboxed worker runs; the sandbox itself is covered by
// the parser's own tests) survive arbitrary bytes as every supported format. For
// any input: no panic (a panic comes back as an internal failure, which is the
// failure here), a bounded time, a refusal that is a typed parser error, and on
// success a result that is valid under the host's own check (wire.Result.Validate),
// within the output and expansion caps, whose warnings are stable codes. No
// archive entry name that is a path (zip-slip, absolute, drive letter, NUL) is
// echoed in an error or a warning.
//
// The first byte selects the format; with its high bit set the target instead
// builds a document that hides a random marker (HTML and DOCX constructs) in
// hostile visible text and requires the marker flagged hidden and never visible.
//
// Controls (before the fuzzing): the same marker in plain text is found visible
// and not hidden, a planted path in a message is flagged, a planted invalid
// result is rejected, and the hostile archives are refused.
//
//	go test -run '^$' -fuzz '^FuzzParseDocument$' -fuzztime 5s ./tests/security/fuzz/
func FuzzParseDocument(f *testing.F) {
	// Hidden-text semantics on the seeds, with a positive control for each format.
	for _, h := range hiddenDocs {
		if msg := checkHidden(f, h, "plain filler"); msg != "" {
			f.Fatal(msg)
		}
	}
	for _, c := range []struct {
		format wire.Format
		doc    func(marker string) []byte
	}{
		{wire.FormatHTML, func(m string) []byte { return []byte("<body><p>Visible " + m + "</p></body>") }},
		{wire.FormatDOCX, func(m string) []byte { return parsetest.DOCX(f, parsetest.Para(m), nil) }},
	} {
		marker := "hm" + randWord(f, 14)
		msg, out, err := checkParse(c.format, c.doc(marker), nil)
		if msg != "" || err != nil {
			f.Fatalf("control: a plain %s document: %s %v", c.format, msg, err)
		}
		if hidden, visible := markerState(out, marker); hidden || !visible {
			f.Fatalf("control: a plain %s marker is hidden=%v visible=%v, want visible only", c.format, hidden, visible)
		}
	}
	if leaksEntryName([]string{"../../x"}, "bad entry ../../x in the archive") == "" {
		f.Fatal("control: a path in a message is not flagged")
	}
	if leaksEntryName([]string{"word/document.xml"}, "fine") != "" || leaksEntryName([]string{"ab"}, "ab") != "" {
		f.Fatal("control: a harmless name is flagged")
	}
	bad := &wire.Result{TextSize: 3, Blocks: []wire.Block{{Kind: wire.KindParagraph, Start: 0, End: 9}}}
	if bad.Validate([]byte("abc"), 0) == nil {
		f.Fatal("control: Result.Validate accepted a block past the end of the text")
	}

	seeds := parseSeeds(f)
	for _, s := range seeds {
		f.Add(s.which, s.data)
	}
	for i := range hiddenDocs {
		f.Add(uint8(0x80|i), []byte("hostile <b>&amp; filler\u202e\u200b \x00 text"))
	}
	// The hostile archives are refused, not parsed: the seeds themselves are the
	// control that the checks have something to refuse.
	refused := 0
	for _, s := range seeds {
		if parseFormats[int(s.which)%len(parseFormats)] != wire.FormatDOCX {
			continue
		}
		if _, err := formats.Parse(wire.FormatDOCX, s.data, parseLimits); err != nil {
			refused++
		}
	}
	if refused < 8 {
		f.Fatalf("control: only %d of the hostile archives were refused", refused)
	}

	f.Fuzz(func(t *testing.T, which uint8, data []byte) {
		var msg string
		within(t, 60*time.Second, "the parser", func() {
			if which&0x80 != 0 {
				// checkHidden uses randWord and parsetest, which call tb.Fatal on error; both
				// only fail on a broken environment. The filler is bounded.
				h := hiddenDocs[int(which&0x7f)%len(hiddenDocs)]
				filler := string(data)
				if len(filler) > 4096 {
					filler = filler[:4096]
				}
				msg = checkHidden(t, h, filler)
				return
			}
			format := parseFormats[int(which)%len(parseFormats)]
			msg, _, _ = checkParse(format, data, zipNames(data))
		})
		if msg != "" {
			t.Fatalf("input %s: %s", quoteShort(data), msg)
		}
	})
}
