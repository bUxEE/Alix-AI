package chunk

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// describe renders the blocks of a Document compactly: "h2:Title", "p:text",
// "code:text", "table:rows".
func describe(doc Document) []string {
	var out []string
	for _, b := range doc.Blocks {
		switch b.Kind {
		case BlockHeading:
			out = append(out, fmt.Sprintf("h%d:%s", b.Level, headingText(doc.Text, b)))
		case BlockParagraph:
			out = append(out, "p:"+doc.Text[b.Start:b.End])
		case BlockCode:
			out = append(out, "code:"+doc.Text[b.Start:b.End])
		case BlockTable:
			out = append(out, fmt.Sprintf("table:%d", len(b.Rows)))
		}
	}
	return out
}

func TestFromMarkdownBlocks(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"atx headings", "# One\n\n## Two ##\n\n###### Six\n", []string{"h1:One", "h2:Two", "h6:Six"}},
		{"closing hashes only count after a space", "## C#\n\n## F# ##\n", []string{"h2:C#", "h2:F#"}},
		{"not headings", "#hashtag\n\n####### seven\n\n    # indented code\n", []string{"p:#hashtag", "p:####### seven", "p:    # indented code"}},
		{"paragraph lines join, blank lines split", "line one\nline two\n\nsecond\n", []string{"p:line one\nline two", "p:second"}},
		{"setext needs a one-line paragraph", "Title\n===\n\nsome\ntext\n---\n\nSub\n---\n", []string{"h1:Title", "p:some\ntext", "h2:Sub"}},
		{"thematic breaks are dropped", "above\n\n***\n\n___\n\n- - -\n\nbelow\n", []string{"p:above", "p:below"}},
		{"fenced code keeps hashes inside", "intro\n```sh\n# a comment\n\nls\n```\nafter\n", []string{"p:intro", "code:```sh\n# a comment\n\nls\n```", "p:after"}},
		{"tilde fence and unclosed fence", "~~~\nx\n~~~\n\n```\nnever closed\n# still code\n", []string{"code:~~~\nx\n~~~", "code:```\nnever closed\n# still code"}},
		{"front matter", "---\ntitle: x\n---\n\n# Real\n\nbody\n", []string{"code:---\ntitle: x\n---", "h1:Real", "p:body"}},
		{"crlf", "# H\r\n\r\npara one\r\nstill\r\n\r\n| a | b |\r\n|---|---|\r\n| 1 | 2 |\r\n", []string{"h1:H", "p:para one\r\nstill", "table:2"}},
		{"gfm table with alignment and no outer pipes", "a | b\n:-- | --:\n1 | 2\n3 | 4\n\nafter\n", []string{"table:3", "p:after"}},
		{"table ends at a line without a pipe", "| a |\n|---|\n| 1 |\nplain\n", []string{"table:2", "p:plain"}},
		{"a pipe is not a table without a delimiter row", "a | b\nc | d\n", []string{"p:a | b\nc | d"}},
		{"delimiter must match the header's cell count", "| a | b |\n|---|\n| 1 | 2 |\n", []string{"p:| a | b |\n|---|\n| 1 | 2 |"}},
		{"table interrupts a paragraph", "intro\n| a | b |\n|---|---|\n| 1 | 2 |\n", []string{"p:intro", "table:2"}},
		{"empty headings say nothing", "#\n\n##   \n\nbody\n", []string{"p:body"}},
		{"page break ends a paragraph", "end of page one\fstart of page two\n", []string{"p:end of page one", "p:start of page two"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := FromMarkdown("src_md", tc.in)
			if err := doc.validate(); err != nil {
				t.Fatalf("FromMarkdown produced an invalid document: %v", err)
			}
			if diff := cmp.Diff(tc.want, describe(doc)); diff != "" {
				t.Errorf("blocks (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFromMarkdownTableRows(t *testing.T) {
	doc := FromMarkdown("src_md", "| h1 | h2 |\n|:-:|--:|\n| a | b |\n| c | d |\n")
	if len(doc.Blocks) != 1 || !doc.Blocks[0].HasHeader {
		t.Fatalf("blocks = %+v", doc.Blocks)
	}
	var rows []string
	for _, r := range doc.Blocks[0].Rows {
		rows = append(rows, doc.Text[r.Start:r.End])
	}
	// The delimiter row is inside the block but is no row.
	if want := []string{"| h1 | h2 |", "| a | b |", "| c | d |"}; !cmp.Equal(rows, want) {
		t.Errorf("rows = %q, want %q", rows, want)
	}
}

func TestFromMarkdownPageStarts(t *testing.T) {
	if got := FromMarkdown("s", "no page breaks here\n").PageStarts; got != nil {
		t.Errorf("a text without form feeds has pages %v", got)
	}
	doc := FromMarkdown("s", "p1\fp2\n\fp3")
	if want := []int{0, 3, 7}; !cmp.Equal(doc.PageStarts, want) {
		t.Errorf("page starts = %v, want %v", doc.PageStarts, want)
	}
}
