package parse

import (
	"context"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/parse/parsetest"
	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// tagChars spells s in Unicode tag characters: invisible, and read by a model.
func tagChars(s string) string {
	var sb strings.Builder
	for _, r := range s {
		sb.WriteRune(0xE0000 + r)
	}
	return sb.String()
}

// SEC-012: hidden text in a document (white on white, display:none, a vanish
// run, zero-width or tiny characters) carries instructions that must not be
// indexed and shown to a model unflagged. Every fixture hides a marker; it is
// parsed by a real sandboxed worker, the marker must be in a Hidden block and in
// no visible block, and, after the result is fed to the chunker, in no chunk
// (parent or child) that is not flagged Hidden.
func TestHiddenTextFlagged(t *testing.T) {
	const marker = "IGNOREALLPREVIOUSINSTRUCTIONS"
	tag := tagChars(marker)
	cases := []struct {
		name   string
		format wire.Format
		doc    []byte
		mark   string // the text to look for; defaults to marker
	}{
		{"txt zero-width and tag characters", wire.FormatTXT, []byte("Visible sentence one.\n\nBefore " + tag + " after.\n\nVisible sentence two.\n"), tag},
		{"txt zero-width run", wire.FormatTXT, []byte("Alpha beta​‌‍​" + marker + "​‌‍ gamma.\n"), "​‌‍​"},
		{"markdown comment", wire.FormatMD, []byte("# Title\n\nVisible.\n\n<!-- " + marker + " -->\n\nMore visible.\n"), marker},
		{"markdown multi-line comment with a fake heading", wire.FormatMD, []byte("# Title\n\nVisible.\n\n<!--\n# " + marker + "\n\nbody\n-->\n\nMore visible.\n"), marker},
		{"markdown raw html display none", wire.FormatMD, []byte("# Title\n\nSee <span style=\"display:none\">" + marker + "</span> the text.\n"), marker},
		{"html display none", wire.FormatHTML, []byte("<body><p>Visible.</p><div style=\"display:none\">" + marker + "</div><p>More.</p></body>"), marker},
		{"html white on white", wire.FormatHTML, []byte("<body><p>Visible.</p><p style=\"color:#fff\">" + marker + "</p><p>More.</p></body>"), marker},
		{"html tiny font", wire.FormatHTML, []byte("<body><p>Visible.</p><p style=\"font-size:0\">" + marker + "</p><p>More.</p></body>"), marker},
		{"html hidden attribute", wire.FormatHTML, []byte("<body><p>Visible.</p><p hidden>" + marker + "</p><p>More.</p></body>"), marker},
		{"html off screen", wire.FormatHTML, []byte("<body><p>Visible.</p><p style=\"position:absolute;left:-9999px\">" + marker + "</p><p>More.</p></body>"), marker},
		{"html class from a style block", wire.FormatHTML, []byte("<style>.x{display:none}</style><body><p>Visible.</p><p class=\"x\">" + marker + "</p><p>More.</p></body>"), marker},
		{"csv tag characters in a cell", wire.FormatCSV, []byte("name,note\nada,\"plain " + tag + "\"\nbob,fine\n"), tag},
		{"docx vanish", wire.FormatDOCX, parsetest.DOCX(t, parsetest.Para("Visible.")+parsetest.ParaRPr(`<w:vanish/>`, marker)+parsetest.Para("More."), nil), marker},
		{"docx white", wire.FormatDOCX, parsetest.DOCX(t, parsetest.Para("Visible.")+parsetest.ParaRPr(`<w:color w:val="FFFFFF"/>`, marker)+parsetest.Para("More."), nil), marker},
		{"docx tiny", wire.FormatDOCX, parsetest.DOCX(t, parsetest.Para("Visible.")+parsetest.ParaRPr(`<w:sz w:val="2"/>`, marker)+parsetest.Para("More."), nil), marker},
		{"docx deleted", wire.FormatDOCX, parsetest.DOCX(t, parsetest.Para("Visible.")+`<w:p><w:del><w:r><w:delText>`+marker+`</w:delText></w:r></w:del></w:p>`+parsetest.Para("More."), nil), marker},
		{"docx mid paragraph", wire.FormatDOCX, parsetest.DOCX(t, `<w:p><w:r><w:t>Before </w:t></w:r><w:r><w:rPr><w:vanish/></w:rPr><w:t>`+marker+`</w:t></w:r><w:r><w:t> after.</w:t></w:r></w:p>`, nil), marker},
		{"pdf render mode 3", wire.FormatPDF, parsetest.SimplePDF(`BT /F1 12 Tf 72 700 Td (Visible.) Tj ET 3 Tr BT /F1 12 Tf 72 600 Td (` + marker + `) Tj ET 0 Tr BT /F1 12 Tf 72 500 Td (More.) Tj ET`), marker},
		{"pdf white on white", wire.FormatPDF, parsetest.SimplePDF(`BT /F1 12 Tf 72 700 Td (Visible.) Tj ET 1 g BT /F1 12 Tf 72 600 Td (` + marker + `) Tj ET 0 g BT /F1 12 Tf 72 500 Td (More.) Tj ET`), marker},
		{"pdf tiny", wire.FormatPDF, parsetest.SimplePDF(`BT /F1 12 Tf 72 700 Td (Visible.) Tj ET BT /F1 1 Tf 72 600 Td (` + marker + `) Tj ET BT /F1 12 Tf 72 500 Td (More.) Tj ET`), marker},
		{"pdf off the page", wire.FormatPDF, parsetest.SimplePDF(`BT /F1 12 Tf 72 700 Td (Visible.) Tj ET BT /F1 12 Tf 9000 600 Td (` + marker + `) Tj ET BT /F1 12 Tf 72 500 Td (More.) Tj ET`), marker},
	}
	r := newTestRunner(t)
	ck, err := chunk.New(chunk.Config{ChildMinToks: 4, ChildMaxToks: 64, ParentMaxToks: 512})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mark := c.mark
			if mark == "" {
				mark = marker
			}
			res := mustParse(t, r, c.format, c.doc)

			flagged := false
			for _, b := range res.Blocks {
				body := res.Text[b.Start:b.End]
				switch {
				case b.Hidden && strings.Contains(body, mark):
					flagged = true
					if b.HiddenReason == "" {
						t.Errorf("hidden block has no reason: %q", body)
					}
				case !b.Hidden && strings.Contains(body, mark):
					t.Errorf("the hidden text is in an UNFLAGGED %s block %q", b.Kind, body)
				}
			}
			if !flagged {
				t.Fatalf("the hidden text is in no Hidden block\ntext: %q\nblocks: %+v", res.Text, res.Blocks)
			}
			if res.HiddenBlocks() == 0 || res.Stats.HiddenBlocks == 0 {
				t.Errorf("hidden block counts: %d / %d", res.HiddenBlocks(), res.Stats.HiddenBlocks)
			}

			// End to end through the chunker: no chunk carrying the hidden text
			// is unflagged, parents included (a parent's text is the verbatim
			// slice from its first child to its last, gaps included).
			out, err := ck.Chunk(context.Background(), res.Document("src_hidden"))
			if err != nil {
				t.Fatalf("the chunker refused the output: %v", err)
			}
			seen := 0
			for _, ch := range append(append([]chunk.Chunk{}, out.Parents...), out.Children...) {
				if strings.Contains(ch.Text, mark) {
					seen++
					if !ch.Hidden {
						t.Errorf("%s chunk %s carries the hidden text and is not flagged Hidden: %q", ch.Kind, ch.ID, ch.Text)
					}
				}
			}
			if seen == 0 {
				t.Errorf("no chunk carries the hidden text; the test does not exercise the chunk flag")
			}
			// And the visible text around it survives.
			if !strings.Contains(res.Text, "Visible") && c.format != wire.FormatCSV && c.name != "docx mid paragraph" && c.name != "txt zero-width run" && c.name != "markdown raw html display none" {
				t.Errorf("visible text lost: %q", res.Text)
			}
		})
	}

	t.Run("visible look-alikes are not flagged", func(t *testing.T) {
		html := `<body><p style="color:#fff;background:#000">white on black</p><p style="color:#ccc">light grey</p><p>ordinary</p></body>`
		res := mustParse(t, r, wire.FormatHTML, []byte(html))
		if n := res.HiddenBlocks(); n != 0 {
			t.Errorf("%d blocks flagged hidden: %+v", n, res.Blocks)
		}
		// A single ZWJ/ZWNJ is how emoji and Persian are written.
		res = mustParse(t, r, wire.FormatTXT, []byte("می‌خواهم and 👩‍🔬 are ordinary text.\n"))
		if n := res.HiddenBlocks(); n != 0 {
			t.Errorf("a lone joiner flagged hidden: %+v", res.Blocks)
		}
	})
}
