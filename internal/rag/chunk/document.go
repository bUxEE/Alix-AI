package chunk

import (
	"sort"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// BlockKind is the structural kind of an input block.
type BlockKind string

// The block kinds a parser or FromMarkdown can hand the chunker.
const (
	// BlockHeading starts a new section; Level is its depth, 1 to 6.
	BlockHeading BlockKind = "heading"
	// BlockParagraph is running text (a list, a quote and a paragraph are all
	// paragraphs here).
	BlockParagraph BlockKind = "paragraph"
	// BlockCode is preformatted text. It is chunked like a paragraph but never
	// merged with its neighbours.
	BlockCode BlockKind = "code"
	// BlockTable is a table: Rows holds one span per row.
	BlockTable BlockKind = "table"
)

// Block is one structural unit of the extracted text. Start and End are UTF-8
// byte offsets into Document.Text, End exclusive, on code point boundaries.
type Block struct {
	Kind BlockKind
	// Level is the heading depth, 1 (outermost) to 6. Zero for other kinds.
	Level int
	// Title is the heading title without markup ("Installation", not
	// "## Installation"). Optional: when empty the title is the heading's own
	// text with whitespace collapsed.
	Title string
	Start int
	End   int
	// Rows are the rows of a table, in order, inside [Start, End). A table
	// needs at least one. Other kinds have none.
	Rows []Row
	// HasHeader says Rows[0] is the table's header row. The header row is not
	// a child of its own; it is carried with every other row as Chunk.Header.
	HasHeader bool
	// Hidden marks text the parser found hidden (white on white, display:none,
	// zero-width, tiny; SEC-012). The flag is carried to every chunk cut from
	// the block, and hidden and visible text are never merged into one child.
	// A parent is flagged when any child of it is. Parser contract: hidden
	// text must arrive as a Hidden block, never as an unflagged gap between
	// blocks, or a parent-expanded passage would carry it unflagged.
	Hidden bool
}

// Row is one table row, as byte offsets like Block.
type Row struct {
	Start int
	End   int
}

// Document is what the chunker consumes: the extracted text of one source
// version and its structure.
type Document struct {
	// SourceID identifies the source. It is part of every chunk ID, so the
	// same text in two sources never shares an ID. Required.
	SourceID string
	// Text is the extracted text, valid UTF-8. Chunk spans index into it.
	Text string
	// PageStarts holds, in strictly ascending order, the byte offset at which
	// each page begins: page n starts at PageStarts[n-1]. Text before the
	// first entry is on no known page. Empty when the source has no pages.
	PageStarts []int
	// Blocks are the structural units in document order, not overlapping.
	// Text outside every block (whitespace, rules, page furniture) is never
	// part of a child. It can still sit inside a parent, because a parent is
	// the verbatim slice from its first child to its last, gaps included. So
	// a parser must never leave text it wants flagged or dropped in a gap:
	// hidden text is its own block with Hidden set, and text that must not
	// reach a model is not in Text at all.
	Blocks []Block
}

const (
	maxSourceIDLen = 128
	maxPages       = 1000000 // maxLength of a locator page
)

func invalid(format string, args ...any) error {
	return contracts.Newf(contracts.ErrValidationFailed, "chunk document: "+format, args...)
}

func onBoundary(text string, i int) bool {
	return i == 0 || i == len(text) || utf8.RuneStart(text[i])
}

// validate refuses a malformed document instead of repairing it: a span that
// does not point into the text would produce a citation that does not.
func (d *Document) validate() error {
	if d.SourceID == "" {
		return invalid("source id is empty")
	}
	if len(d.SourceID) > maxSourceIDLen {
		return invalid("source id is longer than %d bytes", maxSourceIDLen)
	}
	if len(d.Text) > MaxDocumentBytes {
		return contracts.Newf(contracts.ErrResourceExhausted,
			"chunk document: text is %d bytes, over the limit of %d", len(d.Text), MaxDocumentBytes)
	}
	if !utf8.ValidString(d.Text) {
		return invalid("text is not valid UTF-8")
	}
	if len(d.PageStarts) > maxPages {
		return invalid("%d pages is over the limit of %d", len(d.PageStarts), maxPages)
	}
	for i, p := range d.PageStarts {
		switch {
		case p < 0 || p > len(d.Text):
			return invalid("page start %d (page %d) is outside the text", p, i+1)
		case !onBoundary(d.Text, p):
			return invalid("page start %d (page %d) is inside a code point", p, i+1)
		case i > 0 && p <= d.PageStarts[i-1]:
			return invalid("page starts are not strictly ascending at page %d", i+1)
		}
	}
	prevEnd := 0
	for i, b := range d.Blocks {
		if err := d.validateBlock(i, b, prevEnd); err != nil {
			return err
		}
		prevEnd = b.End
	}
	return nil
}

func (d *Document) validateBlock(i int, b Block, prevEnd int) error {
	switch {
	case b.Start < prevEnd:
		return invalid("block %d starts at %d, before the previous block ends at %d", i, b.Start, prevEnd)
	case b.Start >= b.End:
		return invalid("block %d has an empty or reversed span [%d,%d)", i, b.Start, b.End)
	case b.End > len(d.Text):
		return invalid("block %d ends at %d, past the %d-byte text", i, b.End, len(d.Text))
	case !onBoundary(d.Text, b.Start) || !onBoundary(d.Text, b.End):
		return invalid("block %d span [%d,%d) is inside a code point", i, b.Start, b.End)
	}
	switch b.Kind {
	case BlockHeading:
		if b.Level < 1 || b.Level > 6 {
			return invalid("block %d is a heading of level %d, want 1 to 6", i, b.Level)
		}
	case BlockParagraph, BlockCode, BlockTable:
		if b.Level != 0 || b.Title != "" {
			return invalid("block %d (%s) carries a heading level or title", i, b.Kind)
		}
	default:
		return invalid("block %d has unknown kind %q", i, b.Kind)
	}
	if b.Kind != BlockTable {
		if len(b.Rows) != 0 || b.HasHeader {
			return invalid("block %d (%s) carries table rows", i, b.Kind)
		}
		return nil
	}
	if len(b.Rows) == 0 {
		return invalid("block %d is a table without rows", i)
	}
	prev := b.Start
	for j, r := range b.Rows {
		switch {
		case r.Start < prev || r.End > b.End:
			return invalid("block %d row %d [%d,%d) is outside the table or overlaps the previous row", i, j, r.Start, r.End)
		case r.Start >= r.End:
			return invalid("block %d row %d has an empty or reversed span", i, j)
		case !onBoundary(d.Text, r.Start) || !onBoundary(d.Text, r.End):
			return invalid("block %d row %d is inside a code point", i, j)
		}
		prev = r.End
	}
	return nil
}

// pageAt is the one-based page of byte offset b, or 0 when the text there is
// on no known page.
func (d *Document) pageAt(b int) int {
	return sort.SearchInts(d.PageStarts, b+1) // pages starting at or before b
}

// runeIndex converts byte offsets into code point offsets in O(256) each,
// with len/256 words of memory.
type runeIndex struct {
	text string
	cp   []int // cp[k]: code points starting before byte k*256
}

func newRuneIndex(text string) *runeIndex {
	x := &runeIndex{text: text, cp: make([]int, len(text)>>8+1)}
	n := 0
	for i := 0; i < len(text); i++ {
		if i&0xFF == 0 {
			x.cp[i>>8] = n
		}
		if text[i]&0xC0 != 0x80 {
			n++
		}
	}
	if len(text)&0xFF == 0 {
		x.cp[len(text)>>8] = n
	}
	return x
}

// at is the code point offset of byte offset b, which must be on a boundary.
func (x *runeIndex) at(b int) int {
	n := x.cp[b>>8]
	for i := b &^ 0xFF; i < b; i++ {
		if x.text[i]&0xC0 != 0x80 {
			n++
		}
	}
	return n
}
