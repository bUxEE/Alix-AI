package formats

import (
	"github.com/buxee/alix-ai/internal/rag/parse/parsetest"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

const testStyles = `<?xml version="1.0"?><w:styles ` + parsetest.DocxNS + `>
<w:docDefaults><w:rPrDefault><w:rPr><w:sz w:val="22"/></w:rPr></w:rPrDefault></w:docDefaults>
<w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="heading 1"/><w:pPr><w:outlineLvl w:val="0"/></w:pPr></w:style>
<w:style w:type="paragraph" w:styleId="Heading2"><w:name w:val="heading 2"/><w:basedOn w:val="Heading1"/><w:pPr><w:outlineLvl w:val="1"/></w:pPr></w:style>
<w:style w:type="paragraph" w:styleId="Ghost"><w:name w:val="Ghost"/><w:rPr><w:vanish/></w:rPr></w:style>
<w:style w:type="character" w:styleId="Faint"><w:name w:val="Faint"/><w:rPr><w:color w:val="FFFFFF"/></w:rPr></w:style>
</w:styles>`

func TestDOCXStructure(t *testing.T) {
	body := `<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>Report</w:t></w:r></w:p>` +
		parsetest.Para("Intro paragraph.") +
		`<w:p><w:pPr><w:pStyle w:val="Heading2"/></w:pPr><w:r><w:t>Details</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>Split </w:t></w:r><w:r><w:rPr><w:b/></w:rPr><w:t>across</w:t></w:r><w:r><w:t xml:space="preserve"> runs</w:t></w:r><w:r><w:tab/><w:t>tabbed</w:t></w:r></w:p>` +
		`<w:tbl><w:tr><w:trPr><w:tblHeader/></w:trPr><w:tc><w:p><w:r><w:t>Item</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>Cost</w:t></w:r></w:p></w:tc></w:tr>` +
		`<w:tr><w:tc><w:p><w:r><w:t>Pen</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>2</w:t></w:r></w:p></w:tc></w:tr></w:tbl>` +
		`<w:p><w:r><w:t>Hello</w:t></w:r><mc:AlternateContent><mc:Choice><w:r><w:drawing><w:txbxContent>` +
		`<w:p><w:r><w:t>Boxed text</w:t></w:r></w:p></w:txbxContent></w:drawing></w:r></mc:Choice>` +
		`<mc:Fallback><w:r><w:pict><w:txbxContent><w:p><w:r><w:t>Boxed text</w:t></w:r></w:p></w:txbxContent></w:pict></w:r></mc:Fallback></mc:AlternateContent></w:p>`
	notes := `<?xml version="1.0"?><w:footnotes ` + parsetest.DocxNS + `><w:footnote w:type="separator" w:id="-1"><w:p><w:r><w:t>SEPARATOR</w:t></w:r></w:p></w:footnote>` +
		`<w:footnote w:id="1"><w:p><w:r><w:t>A footnote.</w:t></w:r></w:p></w:footnote></w:footnotes>`
	o := mustParse(t, wire.FormatDOCX, parsetest.DOCX(t, body, map[string]string{"word/styles.xml": testStyles, "word/footnotes.xml": notes}))
	got := dump(o)
	for _, want := range []string{
		`H1 "Report"`, `P "Intro paragraph."`, `H2 "Details"`, `P "Split across runs\ttabbed"`,
		`TABLE header=true rows=2`, `"Item | Cost"`, `"Pen | 2"`, `P "Hello"`, `P "Boxed text"`, `P "A footnote."`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in\n%s", want, got)
		}
	}
	if strings.Count(string(o.Text), "Boxed text") != 1 {
		t.Errorf("the AlternateContent fallback repeats the text box:\n%s", got)
	}
	if strings.Contains(string(o.Text), "SEPARATOR") {
		t.Errorf("footnote separator extracted:\n%s", got)
	}
}

func TestDOCXHiddenText(t *testing.T) {
	cases := []struct{ name, body, reason string }{
		{"vanish", parsetest.ParaRPr(`<w:vanish/>`, "MARK"), wire.HideVanish},
		{"vanish val true", parsetest.ParaRPr(`<w:vanish w:val="1"/>`, "MARK"), wire.HideVanish},
		{"spec vanish", parsetest.ParaRPr(`<w:specVanish/>`, "MARK"), wire.HideVanish},
		{"web hidden", parsetest.ParaRPr(`<w:webHidden/>`, "MARK"), wire.HideVanish},
		{"white", parsetest.ParaRPr(`<w:color w:val="FFFFFF"/>`, "MARK"), wire.HideSameColour},
		{"theme background", parsetest.ParaRPr(`<w:color w:val="000000" w:themeColor="background1"/>`, "MARK"), wire.HideSameColour},
		{"tiny", parsetest.ParaRPr(`<w:sz w:val="2"/>`, "MARK"), wire.HideTinyFont},
		{"white on white shading", parsetest.ParaRPr(`<w:color w:val="FFFFFF"/><w:shd w:val="clear" w:fill="FFFFFF"/>`, "MARK"), wire.HideSameColour},
		{"style vanish", `<w:p><w:pPr><w:pStyle w:val="Ghost"/></w:pPr><w:r><w:t>MARK</w:t></w:r></w:p>`, wire.HideVanish},
		{"character style white", `<w:p><w:r><w:rPr><w:rStyle w:val="Faint"/></w:rPr><w:t>MARK</w:t></w:r></w:p>`, wire.HideSameColour},
		{"deleted text", `<w:p><w:del><w:r><w:delText>MARK</w:delText></w:r></w:del></w:p>`, wire.HideDeletedText},
		{"mid paragraph", `<w:p><w:r><w:t>before </w:t></w:r><w:r><w:rPr><w:vanish/></w:rPr><w:t>MARK</w:t></w:r><w:r><w:t> after</w:t></w:r></w:p>`, wire.HideVanish},
		{"in a table cell", `<w:tbl><w:tr><w:tc><w:p><w:r><w:t>ok</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:rPr><w:vanish/></w:rPr><w:t>MARK</w:t></w:r></w:p></w:tc></w:tr></w:tbl>`, wire.HideVanish},
		{"in a heading", `<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>Shown </w:t></w:r><w:r><w:rPr><w:vanish/></w:rPr><w:t>MARK</w:t></w:r></w:p>`, wire.HideVanish},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := mustParse(t, wire.FormatDOCX, parsetest.DOCX(t, parsetest.Para("Visible one.")+c.body+parsetest.Para("Visible two."), map[string]string{"word/styles.xml": testStyles}))
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
			for _, b := range o.Blocks {
				if b.Kind == wire.KindParagraph && !b.Hidden && strings.Contains(string(o.Text[b.Start:b.End]), "MARK") {
					t.Errorf("MARK in an unflagged paragraph:\n%s", dump(o))
				}
			}
			if !strings.Contains(string(o.Text), "Visible one.") || !strings.Contains(string(o.Text), "Visible two.") {
				t.Errorf("visible text lost:\n%s", dump(o))
			}
		})
	}
}

func TestDOCXVisibleLookalikes(t *testing.T) {
	// White on a dark shading, a normal size, and plain text are readable.
	body := parsetest.ParaRPr(`<w:color w:val="FFFFFF"/><w:shd w:val="clear" w:fill="1F3864"/>`, "white on navy") +
		parsetest.ParaRPr(`<w:highlight w:val="black"/><w:color w:val="FFFFFF"/>`, "white on black highlight") +
		parsetest.ParaRPr(`<w:sz w:val="20"/>`, "ten point") +
		`<w:tbl><w:tr><w:tc><w:tcPr><w:shd w:val="clear" w:fill="000000"/></w:tcPr><w:p><w:r><w:rPr><w:color w:val="FFFFFF"/></w:rPr><w:t>white in a black cell</w:t></w:r></w:p></w:tc></w:tr></w:tbl>`
	o := mustParse(t, wire.FormatDOCX, parsetest.DOCX(t, body, nil))
	if h := hiddenText(o); h != "" {
		t.Errorf("readable text flagged hidden: %q\n%s", h, dump(o))
	}
}

func TestDOCXRejectsBadPackages(t *testing.T) {
	good := parsetest.DOCX(t, parsetest.Para("x"), nil)
	if _, err := Parse(wire.FormatDOCX, good, testLimits()); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		data  []byte
		check func(*Error) bool
	}{
		{"not a zip", []byte("PK but not really a zip file at all"), func(e *Error) bool { return e.Reason == wire.ReasonMalformed }},
		{"missing document part", parsetest.Zip(t, map[string]string{"x.txt": "hi"}), func(e *Error) bool { return e.Reason == wire.ReasonMalformed }},
		{"dtd", parsetest.DOCX(t, parsetest.Para("x"), map[string]string{"word/document.xml": `<?xml version="1.0"?><!DOCTYPE d [<!ENTITY a "aaaa">]><w:document ` + parsetest.DocxNS + `><w:body>` + parsetest.Para("&a;") + `</w:body></w:document>`}),
			func(e *Error) bool { return e.Reason == wire.ReasonMalformed }},
		{"zip slip name", parsetest.Zip(t, map[string]string{"word/document.xml": "<a/>", "../evil": "x"}), func(e *Error) bool { return e.Reason == wire.ReasonMalformed }},
		{"nested archive as a part", parsetest.Zip(t, map[string]string{"word/document.xml": string(good)}), func(e *Error) bool { return e.Reason == wire.ReasonArchiveBomb }},
		{"deep xml", parsetest.DOCX(t, strings.Repeat("<w:sdt><w:sdtContent>", 400)+parsetest.Para("x")+strings.Repeat("</w:sdtContent></w:sdt>", 400), nil),
			func(e *Error) bool { return e.Reason == wire.ReasonNestingTooDeep }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(wire.FormatDOCX, c.data, testLimits())
			fe, ok := err.(*Error)
			if !ok || !c.check(fe) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestDOCXBombs(t *testing.T) {
	lim := testLimits()
	lim.MaxExpandedBytes = 64 << 20
	lim.MaxEntries = 100

	// A part that inflates far past the ratio.
	big := parsetest.DOCX(t, "", map[string]string{"word/document.xml": `<?xml version="1.0"?><w:document ` + parsetest.DocxNS + `><w:body>` + strings.Repeat("<w:p/>", 5<<20) + `</w:body></w:document>`})
	if _, err := Parse(wire.FormatDOCX, big, lim); err == nil {
		t.Error("a ratio bomb parsed")
	} else if fe := err.(*Error); fe.Reason != wire.ReasonArchiveBomb {
		t.Errorf("ratio bomb: %v", err)
	}

	// More entries than allowed, declared in the end-of-central-directory record.
	parts := map[string]string{"word/document.xml": `<?xml version="1.0"?><w:document ` + parsetest.DocxNS + `><w:body/></w:document>`}
	for i := 0; i < 150; i++ {
		parts[strings.Repeat("a", 3)+string(rune('A'+i%26))+strings.Repeat("b", i/26+1)] = "x"
	}
	if _, err := Parse(wire.FormatDOCX, parsetest.Zip(t, parts), lim); err == nil {
		t.Error("an entry flood parsed")
	} else if fe := err.(*Error); fe.Reason != wire.ReasonArchiveBomb {
		t.Errorf("entry flood: %v", err)
	}
}
