package formats

import (
	"strings"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// Seg is a piece of inline text with its visibility, as a parser found it.
type Seg struct {
	Text   string
	Hidden bool
	// Reason is the wire.Hide* reason when Hidden.
	Reason string
}

// Cell is the text of one table cell.
type Cell []Seg

// Row is one table row.
type Row []Cell

// Builder assembles extracted text and its blocks. It owns two rules every
// format shares:
//
//   - Hidden text arrives as its own Hidden block. A paragraph with hidden text
//     in the middle is split into visible, hidden and visible blocks, so a
//     parent that spans them (the chunker's parent is the verbatim slice from
//     its first child to its last) cannot carry hidden text as an unflagged
//     gap. A heading, a code block or a table that holds hidden text is hidden
//     as a whole.
//   - The output is capped as it is written. Text over the cap is an error, not
//     a truncation, so a half-parsed document never looks like a whole one.
type Builder struct {
	max       int64
	maxBlocks int
	text      []byte
	blocks    []wire.Block
	pages     []int
	paged     bool
	structure int // blocks plus table rows so far
}

func newBuilder(l wire.Limits) *Builder {
	return &Builder{max: l.MaxOutputBytes, maxBlocks: l.MaxBlocks}
}

func (b *Builder) room(n int) error {
	if int64(len(b.text))+int64(n) > b.max {
		return fail(wire.ReasonOutputTooLarge, "extracted text passes the limit of %d bytes", b.max)
	}
	return nil
}

func (b *Builder) count(n int) error {
	b.structure += n
	if b.maxBlocks > 0 && b.structure > b.maxBlocks {
		return fail(wire.ReasonOutputTooLarge, "more than %d blocks and rows", b.maxBlocks)
	}
	return nil
}

// begin writes the separator before a block and returns its start offset.
func (b *Builder) begin() (int, error) {
	if len(b.text) > 0 && b.text[len(b.text)-1] != '\f' {
		if err := b.room(2); err != nil {
			return 0, err
		}
		b.text = append(b.text, '\n', '\n')
	}
	return len(b.text), nil
}

func (b *Builder) put(s string) error {
	if err := b.room(len(s)); err != nil {
		return err
	}
	b.text = append(b.text, s...)
	return nil
}

// BeginPages declares that the format has pages: page one starts at the
// current offset. Call it once, before the first block.
func (b *Builder) BeginPages() {
	if !b.paged {
		b.paged = true
		b.pages = append(b.pages, len(b.text))
	}
}

// NextPage ends the current page with a form feed and starts the next one.
func (b *Builder) NextPage() error {
	b.BeginPages()
	if err := b.put("\f"); err != nil {
		return err
	}
	b.pages = append(b.pages, len(b.text))
	return nil
}

// prep cleans the segments, carves hidden invisible-character runs out of
// visible ones, and merges neighbours that share a visibility.
func prep(segs []Seg) []Seg {
	out := make([]Seg, 0, len(segs))
	// merged collects the text of the last segment while neighbours join it,
	// so a run of many small pieces is copied once, not once per piece.
	var merged strings.Builder
	flush := func() {
		if merged.Len() > 0 {
			out[len(out)-1].Text = merged.String()
			merged = strings.Builder{}
		}
	}
	add := func(s Seg) {
		if s.Text == "" {
			return
		}
		if n := len(out); n > 0 && out[n-1].Hidden == s.Hidden && out[n-1].Reason == s.Reason {
			if merged.Len() == 0 {
				merged.WriteString(out[n-1].Text)
			}
			merged.WriteString(s.Text)
			return
		}
		flush()
		out = append(out, s)
	}
	for _, s := range segs {
		s.Text = cleanText(s.Text)
		if s.Hidden || !hasInvisible(s.Text) {
			add(s)
			continue
		}
		pos := 0
		for _, r := range invisibleRuns(s.Text) {
			add(Seg{Text: s.Text[pos:r.Start]})
			add(Seg{Text: s.Text[r.Start:r.End], Hidden: true, Reason: wire.HideInvisibleChars})
			pos = r.End
		}
		add(Seg{Text: s.Text[pos:]})
	}
	flush()
	return out
}

func anyHidden(segs []Seg) (bool, string) {
	for _, s := range segs {
		if s.Hidden {
			return true, s.Reason
		}
	}
	return false, ""
}

func joinSegs(segs []Seg, visibleOnly bool) string {
	var sb strings.Builder
	for _, s := range segs {
		if visibleOnly && s.Hidden {
			continue
		}
		sb.WriteString(s.Text)
	}
	return sb.String()
}

// Paragraph adds running text. Hidden segments become their own blocks.
func (b *Builder) Paragraph(segs []Seg) error {
	for _, s := range prep(segs) {
		t := strings.TrimSpace(s.Text)
		if t == "" {
			continue
		}
		if err := b.add(wire.KindParagraph, t, s.Hidden, s.Reason); err != nil {
			return err
		}
	}
	return nil
}

func (b *Builder) add(kind wire.BlockKind, text string, hidden bool, reason string) error {
	if err := b.count(1); err != nil {
		return err
	}
	start, err := b.begin()
	if err != nil {
		return err
	}
	if err := b.put(text); err != nil {
		return err
	}
	blk := wire.Block{Kind: kind, Start: start, End: len(b.text), Hidden: hidden}
	if hidden {
		blk.HiddenReason = reason
	}
	b.blocks = append(b.blocks, blk)
	return nil
}

// Heading adds a heading of the given level (1 to 6). Text that is entirely
// hidden is added as a hidden paragraph instead: hidden text never opens a
// section and so never becomes a citation's section title.
func (b *Builder) Heading(level int, segs []Seg) error {
	segs = prep(segs)
	if level < 1 {
		level = 1
	}
	if level > 6 {
		level = 6
	}
	hidden, reason := anyHidden(segs)
	visible := collapse(joinSegs(segs, true))
	if visible == "" {
		return b.Paragraph(segs)
	}
	text := collapse(joinSegs(segs, false))
	if err := b.count(1); err != nil {
		return err
	}
	start, err := b.begin()
	if err != nil {
		return err
	}
	if err := b.put(text); err != nil {
		return err
	}
	blk := wire.Block{Kind: wire.KindHeading, Level: level, Title: visible, Start: start, End: len(b.text)}
	if hidden {
		blk.Hidden, blk.HiddenReason = true, reason
	}
	b.blocks = append(b.blocks, blk)
	return nil
}

// Code adds preformatted text, kept verbatim except for cleaning. Hidden text
// inside makes the whole block hidden.
func (b *Builder) Code(segs []Seg) error {
	segs = prep(segs)
	hidden, reason := anyHidden(segs)
	text := strings.Trim(joinSegs(segs, false), "\n")
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return b.add(wire.KindCode, text, hidden, reason)
}

// Table adds a table, one line per row, cells joined by " | ". Rows that are
// empty are dropped. Hidden text in any cell makes the whole table hidden.
func (b *Builder) Table(rows []Row, header bool) error {
	t := b.NewTable()
	for i, r := range rows {
		n := len(t.rows)
		if err := t.AddRow(r); err != nil {
			return err
		}
		if i == 0 && len(t.rows) == n {
			header = false // the header row was empty and dropped
		}
	}
	return t.End(header)
}

// TableWriter writes one table straight into the output, a row at a time, so a
// large table costs no more than its text.
type TableWriter struct {
	b      *Builder
	start  int
	rows   []wire.Span
	hidden bool
	reason string
	began  bool
}

// NewTable starts a table. Nothing is written until the first row.
func (b *Builder) NewTable() *TableWriter { return &TableWriter{b: b} }

func (t *TableWriter) open() error {
	if t.began {
		return nil
	}
	if err := t.b.count(1); err != nil {
		return err
	}
	start, err := t.b.begin()
	if err != nil {
		return err
	}
	t.start, t.began = start, true
	return nil
}

func (t *TableWriter) note(hidden bool, reason string) {
	if hidden && !t.hidden {
		t.hidden, t.reason = true, reason
	}
}

// AddRow adds a row of cells that may carry hidden segments. An empty row is
// dropped.
func (t *TableWriter) AddRow(r Row) error {
	cells := make([]string, len(r))
	empty := true
	for j, c := range r {
		segs := prep(c)
		h, why := anyHidden(segs)
		t.note(h, why)
		cells[j] = collapse(joinSegs(segs, false))
		if cells[j] != "" {
			empty = false
		}
	}
	if empty {
		return nil
	}
	return t.writeRow(cells)
}

// AddStrings adds a row of plain cells: the fast path for CSV.
func (t *TableWriter) AddStrings(cells []string) error {
	empty := true
	for j, c := range cells {
		c = cleanText(c)
		if hasInvisible(c) {
			t.note(true, wire.HideInvisibleChars)
		}
		cells[j] = collapse(c)
		if cells[j] != "" {
			empty = false
		}
	}
	if empty {
		return nil
	}
	return t.writeRow(cells)
}

func (t *TableWriter) writeRow(cells []string) error {
	if err := t.open(); err != nil {
		return err
	}
	if err := t.b.count(1); err != nil {
		return err
	}
	if len(t.rows) > 0 {
		if err := t.b.put("\n"); err != nil {
			return err
		}
	}
	rs := len(t.b.text)
	for i, c := range cells {
		if i > 0 {
			if err := t.b.put(" | "); err != nil {
				return err
			}
		}
		if err := t.b.put(c); err != nil {
			return err
		}
	}
	t.rows = append(t.rows, wire.Span{Start: rs, End: len(t.b.text)})
	return nil
}

// End finishes the table. header says the first row is a header row; it is
// ignored for a table with a single row.
func (t *TableWriter) End(header bool) error {
	if !t.began {
		return nil
	}
	blk := wire.Block{Kind: wire.KindTable, Start: t.start, End: len(t.b.text), Rows: t.rows,
		HasHeader: header && len(t.rows) > 1, Hidden: t.hidden, HiddenReason: t.reason}
	t.b.blocks = append(t.b.blocks, blk)
	return nil
}
