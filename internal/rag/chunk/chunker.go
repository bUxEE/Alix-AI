package chunk

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// Kind is what a chunk is.
type Kind string

// The chunk kinds. KindSection is a parent; the others are children.
const (
	// KindSection is a parent: a heading with the blocks under it (or one
	// part of such a section when it is larger than the parent bound).
	KindSection Kind = "section"
	// KindParagraph is a child cut from a paragraph, or several small
	// neighbouring paragraphs merged.
	KindParagraph Kind = "paragraph"
	// KindCode is a child cut from a code block.
	KindCode Kind = "code"
	// KindTableRow is a child that is exactly one table row (or a piece of
	// one, when the row alone exceeds the child bound).
	KindTableRow Kind = "table_row"
	// KindHeading is a child cut from a heading that has nothing under it, so
	// the heading text is still indexed and citable.
	KindHeading Kind = "heading"
)

// Span is a half-open range of offsets into the extracted text.
type Span struct {
	Start int
	End   int
}

// Chunk is one parent or child.
type Chunk struct {
	// ID is deterministic: see the package comment.
	ID string
	// ParentID is the section a child belongs to. Empty on a parent.
	ParentID string
	Kind     Kind
	// Ordinal is the index of a parent in Result.Parents, or of a child
	// inside its parent.
	Ordinal int
	// Part is the zero-based part of a section that was cut into several
	// parents. Parents only.
	Part int
	// Text is the verbatim slice of the extracted text at ByteSpan.
	Text string
	// Span is the extent of Text in code points: the unit of a citation span.
	Span Span
	// ByteSpan is the extent of Text in UTF-8 bytes.
	ByteSpan Span
	// Page is the one-based page Text starts on, 0 when unknown. PageEnd is
	// the page it ends on; it differs from Page only for a long block or
	// parent that crosses a page break.
	Page    int
	PageEnd int
	// Section is the title of the nearest heading above (or of the heading of
	// this section), empty for text before the first heading.
	Section string
	// Path is the heading breadcrumb, outermost first, ending with Section.
	Path []string
	// Header is the header row of the table a table_row child belongs to.
	// Context for retrieval; not part of Text.
	Header string
	// ContentHash is the hex SHA-256 of Text, with no scope in it (see the
	// package comment on embedding reuse).
	ContentHash string
	// EstToks is EstimateTokens(Text): an upper bound.
	EstToks int
	// Hidden: on a child, the parser flagged its text hidden; on a parent,
	// some child of it is hidden. A retrieval step drops or fences hidden
	// text (SEC-012).
	Hidden bool
	// Split: the text is a piece of a larger block that was cut to meet the
	// child bound.
	Split bool
	// ChildIDs lists the children of a parent in order. Parents only.
	ChildIDs []string
}

// Result is the chunking of one document.
type Result struct {
	// Chunker is the component and version that produced the chunks; it is
	// recorded in the index generation (12.3).
	Chunker rag.ComponentVersion
	// Parents are in document order.
	Parents []Chunk
	// Children are in document order, grouped by parent.
	Children []Chunk
}

// Chunker chunks documents with fixed bounds. It holds no state between calls
// and is safe for concurrent use.
type Chunker struct {
	cfg Config
}

// New returns a Chunker, or contracts.ErrValidationFailed for unusable bounds.
func New(cfg Config) (*Chunker, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Chunker{cfg: cfg}, nil
}

// Config returns the bounds in force.
func (c *Chunker) Config() Config { return c.cfg }

// Component returns the chunker identity and version to record.
func (c *Chunker) Component() rag.ComponentVersion { return c.cfg.Component() }

const maxHeaderRunes = 512

// unit is a child in the making.
type unit struct {
	kind   Kind
	s, e   int // trimmed byte span
	est    int
	hidden bool
	split  bool
	header string
	table  int // ordinal of the table inside its section, -1 for none
}

// section is a parent in the making.
type section struct {
	hs, he int // trimmed heading span; hs is -1 before the first heading
	title  string
	path   []string
	hidden bool
	units  []unit
}

// build is the state of one Chunk call.
type build struct {
	cfg   Config
	doc   *Document
	runes *runeIndex
}

// Chunk splits doc into parents and children. It never edits the text, never
// drops non-white-space text that is inside a block, and fails closed on a
// malformed document (contracts.ErrValidationFailed).
func (c *Chunker) Chunk(ctx context.Context, doc Document) (Result, error) {
	res := Result{Chunker: c.Component()}
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("chunk: %w", err)
	}
	if err := doc.validate(); err != nil {
		return Result{}, err
	}
	b := &build{cfg: c.cfg, doc: &doc, runes: newRuneIndex(doc.Text)}

	secs := b.sections()
	seen := map[string]int{} // path key -> sections emitted with it
	for _, sec := range secs {
		if err := ctx.Err(); err != nil {
			return Result{}, fmt.Errorf("chunk: %w", err)
		}
		if len(sec.units) == 0 && sec.hs >= 0 && sec.title != "" {
			// A heading with nothing under it: its own text is the child, so
			// it can still be found and cited.
			b.addSpan(&sec, KindHeading, sec.hs, sec.he, sec.hidden, "", -1)
		}
		if len(sec.units) == 0 {
			continue
		}
		sec.units = b.merge(sec.units)
		key := pathKey(sec.path)
		occ := seen[key]
		seen[key]++
		b.emit(&res, sec, key, occ)
	}
	return res, nil
}

// sections walks the blocks and groups them under their headings.
func (b *build) sections() []section {
	var (
		secs   []section
		stack  []headingLevel
		tables int
	)
	cur := -1
	ensure := func() *section {
		if cur < 0 {
			secs = append(secs, section{hs: -1})
			cur = len(secs) - 1
		}
		return &secs[cur]
	}
	text := b.doc.Text
	for _, blk := range b.doc.Blocks {
		switch blk.Kind {
		case BlockHeading:
			title := truncateRunes(collapse(headingText(text, blk)), maxTitleRunes)
			for len(stack) > 0 && stack[len(stack)-1].level >= blk.Level {
				stack = stack[:len(stack)-1]
			}
			if title != "" {
				stack = append(stack, headingLevel{blk.Level, title})
			}
			hs, he := trimSpan(text, blk.Start, blk.End)
			path := make([]string, len(stack))
			for i, h := range stack {
				path[i] = h.title
			}
			secs = append(secs, section{hs: hs, he: he, title: title, path: path, hidden: blk.Hidden})
			cur = len(secs) - 1
			tables = 0
		case BlockParagraph:
			b.addSpan(ensure(), KindParagraph, blk.Start, blk.End, blk.Hidden, "", -1)
		case BlockCode:
			b.addSpan(ensure(), KindCode, blk.Start, blk.End, blk.Hidden, "", -1)
		case BlockTable:
			b.addTable(ensure(), blk, tables)
			tables++
		}
	}
	return secs
}

type headingLevel struct {
	level int
	title string
}

// headingText is the title of a heading block: its declared Title, else its
// own text.
func headingText(text string, blk Block) string {
	if blk.Title != "" {
		return blk.Title
	}
	return text[blk.Start:blk.End]
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for ; n > 0; n-- {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return strings.TrimSpace(s[:i])
}

// addSpan appends the child units of text[s:e], splitting it when it is over
// the child bound.
func (b *build) addSpan(sec *section, kind Kind, s, e int, hidden bool, header string, table int) {
	s, e = trimSpan(b.doc.Text, s, e)
	if s >= e {
		return
	}
	u := unit{kind: kind, hidden: hidden, header: header, table: table}
	if est := b.estimate(s, e); est <= b.cfg.ChildMaxToks {
		u.s, u.e, u.est = s, e, est
		sec.units = append(sec.units, u)
		return
	}
	u.split = true
	splitSpan(b.doc.Text, s, e, b.cfg.ChildMaxToks, func(a, z int) {
		p := u
		p.s, p.e, p.est = a, z, b.estimate(a, z)
		sec.units = append(sec.units, p)
	})
}

// addTable turns each data row into a child, carrying the header row along.
func (b *build) addTable(sec *section, blk Block, table int) {
	rows := blk.Rows
	header := ""
	if blk.HasHeader && len(rows) > 1 {
		hs, he := trimSpan(b.doc.Text, rows[0].Start, rows[0].End)
		header = truncateRunes(b.doc.Text[hs:he], maxHeaderRunes)
		rows = rows[1:]
	}
	for _, r := range rows {
		b.addSpan(sec, KindTableRow, r.Start, r.End, blk.Hidden, header, table)
	}
}

func (b *build) estimate(s, e int) int {
	var m meter
	m.addString(b.doc.Text[s:e])
	return m.toks()
}

// canMerge reports whether two neighbouring units may become one child:
// both plain paragraphs of the same visibility on the same page, with only
// white space between them.
func (b *build) canMerge(x, y unit) bool {
	return x.kind == KindParagraph && y.kind == KindParagraph &&
		!x.split && !y.split && x.hidden == y.hidden &&
		b.doc.pageAt(x.s) == b.doc.pageAt(y.e-1) &&
		isBlank(b.doc.Text, x.e, y.s)
}

// merge joins paragraphs smaller than ChildMinToks with a neighbour while the
// result stays within ChildMaxToks.
func (b *build) merge(units []unit) []unit {
	if b.cfg.ChildMinToks == 0 {
		return units
	}
	join := func(x, y unit) (unit, bool) {
		if !b.canMerge(x, y) {
			return x, false
		}
		m := x
		m.e = y.e
		m.est = b.estimate(m.s, m.e)
		return m, m.est <= b.cfg.ChildMaxToks
	}
	out := make([]unit, 0, len(units))
	for i := 0; i < len(units); {
		cur := units[i]
		i++
		for cur.est < b.cfg.ChildMinToks && i < len(units) {
			m, ok := join(cur, units[i])
			if !ok {
				break
			}
			cur = m
			i++
		}
		if cur.est < b.cfg.ChildMinToks && len(out) > 0 {
			if m, ok := join(out[len(out)-1], cur); ok {
				out[len(out)-1] = m
				continue
			}
		}
		out = append(out, cur)
	}
	return out
}

// part is a parent in the making: a run of whole children.
type part struct {
	start int // byte offset of the parent span
	units []unit
}

// parts packs a section's children into parents of at most ParentMaxToks. A
// table is one atomic group (so it stays under one parent) unless it alone is
// too big, in which case it is broken at row boundaries.
func (b *build) parts(sec section) []part {
	text := b.doc.Text
	var groups [][]unit
	for _, u := range sec.units {
		if n := len(groups); u.table >= 0 && n > 0 && groups[n-1][0].table == u.table {
			groups[n-1] = append(groups[n-1], u)
		} else {
			groups = append(groups, []unit{u})
		}
	}
	var (
		parts []part
		cur   *part
		m     meter // estimate of text[cur.start:end]
		end   int
	)
	maxToks := b.cfg.ParentMaxToks
	startOf := func(first unit) int {
		if len(parts) == 0 && sec.hs >= 0 {
			return sec.hs
		}
		return first.s
	}
	closeCur := func() {
		if cur != nil {
			parts = append(parts, *cur)
			cur = nil
		}
	}
	for gi := 0; gi < len(groups); gi++ {
		g := groups[gi]
		last := g[len(g)-1].e
		if cur != nil {
			ext := m
			ext.addString(text[end:last])
			if ext.toks() <= maxToks {
				m, end = ext, last
				cur.units = append(cur.units, g...)
				continue
			}
			closeCur()
		}
		// Start a new parent with this group.
		start := startOf(g[0])
		var fresh meter
		fresh.addString(text[start:last])
		if fresh.toks() > maxToks && start != g[0].s {
			// The heading and the first group do not fit together: the
			// parent starts at the first child (the heading stays in
			// Section and Path).
			start = g[0].s
			fresh = meter{}
			fresh.addString(text[start:last])
		}
		if fresh.toks() > maxToks {
			// An over-sized table: break it into single rows. A single child
			// always fits, because ParentMaxToks >= ChildMaxToks.
			singles := make([][]unit, len(g))
			for i, u := range g {
				singles[i] = []unit{u}
			}
			groups = slices.Concat(groups[:gi], singles, groups[gi+1:])
			gi--
			continue
		}
		cur = &part{start: start, units: slices.Clone(g)}
		m, end = fresh, last
	}
	closeCur()
	return parts
}

// emit turns a section into parents and children, with IDs.
func (b *build) emit(res *Result, sec section, key string, secOcc int) {
	srcID := b.doc.SourceID
	contentSeen := map[string]int{} // content hash + kind -> occurrences in this section
	for partNo, p := range b.parts(sec) {
		last := p.units[len(p.units)-1]
		parent := b.chunk(KindSection, p.start, last.e)
		parent.ID = derive("chp_", "parent", srcID, key, strconv.Itoa(secOcc), strconv.Itoa(partNo))
		parent.Ordinal = len(res.Parents)
		parent.Part = partNo
		parent.Section = sec.name()
		parent.Path = slices.Clone(sec.path)
		parent.Hidden = sec.hidden && p.start == sec.hs
		for i, u := range p.units {
			child := b.chunk(u.kind, u.s, u.e)
			ck := string(u.kind) + "\x00" + child.ContentHash
			occ := contentSeen[ck]
			contentSeen[ck]++
			child.ID = derive("chk_", "child", srcID, key, strconv.Itoa(secOcc),
				string(u.kind), child.ContentHash, strconv.Itoa(occ))
			child.ParentID = parent.ID
			child.Ordinal = i
			child.Section = sec.name()
			child.Path = slices.Clone(sec.path)
			child.Header = u.header
			child.Hidden = u.hidden
			child.Split = u.split
			parent.Hidden = parent.Hidden || u.hidden
			parent.ChildIDs = append(parent.ChildIDs, child.ID)
			res.Children = append(res.Children, child)
		}
		res.Parents = append(res.Parents, parent)
	}
}

// chunk fills the fields every chunk shares: text, spans, pages, hash, estimate.
func (b *build) chunk(kind Kind, s, e int) Chunk {
	text := b.doc.Text[s:e]
	sum := sha256.Sum256([]byte(text))
	return Chunk{
		Kind:        kind,
		Text:        text,
		ByteSpan:    Span{s, e},
		Span:        Span{b.runes.at(s), b.runes.at(e)},
		Page:        b.doc.pageAt(s),
		PageEnd:     b.doc.pageAt(e - 1),
		ContentHash: hex.EncodeToString(sum[:]),
		EstToks:     EstimateTokens(text),
	}
}

// idScheme versions the ID derivation. It is not the chunker version: see the
// package comment.
const idScheme = "alixai/chunk-id/v1"

// pathKey is an unambiguous encoding of a heading path.
func pathKey(path []string) string {
	var sb strings.Builder
	for _, p := range path {
		sb.WriteString(strconv.Itoa(len(p)))
		sb.WriteByte(':')
		sb.WriteString(p)
	}
	return sb.String()
}

// derive hashes length-prefixed fields, so no two field lists collide by
// moving a boundary, and returns prefix plus 128 bits in hex: at most 36
// characters, no whitespace, valid as a schemas/common identifier.
func derive(prefix string, fields ...string) string {
	h := sha256.New()
	var buf [binary.MaxVarintLen64]byte
	write := func(s string) {
		n := binary.PutUvarint(buf[:], uint64(len(s)))
		h.Write(buf[:n])
		_, _ = io.WriteString(h, s)
	}
	write(idScheme)
	for _, f := range fields {
		write(f)
	}
	return prefix + hex.EncodeToString(h.Sum(nil)[:16])
}

// name is the title of the nearest titled heading: the section's own, or that
// of the ancestor when its heading has no text.
func (s section) name() string {
	if len(s.path) == 0 {
		return ""
	}
	return s.path[len(s.path)-1]
}
