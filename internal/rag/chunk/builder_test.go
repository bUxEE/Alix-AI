package chunk

import "strings"

// builder assembles a Document with explicit blocks, so a test controls the
// structure and offsets exactly instead of going through FromMarkdown.
type builder struct {
	sb  strings.Builder
	doc Document
}

func newBuilder(sourceID string) *builder {
	return &builder{doc: Document{SourceID: sourceID}}
}

// raw appends text that belongs to no block.
func (b *builder) raw(s string) *builder {
	b.sb.WriteString(s)
	return b
}

func (b *builder) add(blk Block, s string) *builder {
	blk.Start = b.sb.Len()
	b.sb.WriteString(s)
	blk.End = b.sb.Len()
	b.doc.Blocks = append(b.doc.Blocks, blk)
	return b
}

func (b *builder) heading(level int, title string) *builder {
	return b.add(Block{Kind: BlockHeading, Level: level, Title: title}, strings.Repeat("#", level)+" "+title).raw("\n\n")
}

func (b *builder) para(s string) *builder {
	return b.add(Block{Kind: BlockParagraph}, s).raw("\n\n")
}

func (b *builder) hidden(s string) *builder {
	return b.add(Block{Kind: BlockParagraph, Hidden: true}, s).raw("\n\n")
}

func (b *builder) code(s string) *builder {
	return b.add(Block{Kind: BlockCode}, s).raw("\n\n")
}

// table appends a table whose rows are the given lines.
func (b *builder) table(header bool, rows ...string) *builder {
	blk := Block{Kind: BlockTable, HasHeader: header, Start: b.sb.Len()}
	for i, r := range rows {
		if i > 0 {
			b.sb.WriteString("\n")
		}
		s := b.sb.Len()
		b.sb.WriteString(r)
		blk.Rows = append(blk.Rows, Row{Start: s, End: b.sb.Len()})
	}
	blk.End = b.sb.Len()
	b.doc.Blocks = append(b.doc.Blocks, blk)
	return b.raw("\n\n")
}

// page starts a new page at the current offset.
func (b *builder) page() *builder {
	b.doc.PageStarts = append(b.doc.PageStarts, b.sb.Len())
	return b
}

func (b *builder) build() Document {
	d := b.doc
	d.Text = b.sb.String()
	return d
}
