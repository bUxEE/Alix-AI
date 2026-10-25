package formats

import (
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// hiddenRegion is a span of the text found to be hidden from a reader.
type hiddenRegion struct {
	start, end int
	reason     string
	// raw is true for regions found by looking at Markdown source (comments,
	// raw HTML), which do not apply inside a code block: there the markup is
	// literal text a reader can see.
	raw bool
}

var (
	// An HTML comment is not rendered by Markdown, but it is in the source, so
	// an extractor that keeps the source keeps the comment. Unclosed, it runs to
	// the end of the text, as a browser reads it.
	commentRE = regexp.MustCompile(`(?s)<!--.*?(?:-->|$)`)
	// An opening tag, for finding raw HTML that hides its content.
	openTagRE = regexp.MustCompile(`(?i)<([a-z][a-z0-9]*)(\s[^<>]*)?>`)
	// An opening or closing tag, for finding where a hidden element ends.
	anyTagRE = regexp.MustCompile(`(?i)<(/?)([a-z][a-z0-9]*)(?:\s[^<>]*)?>`)
)

// parseText parses plain text and Markdown. The structure (headings, fenced
// code, pipe tables, paragraphs, page breaks) comes from the chunker's own
// scanner, so the blocks the chunker would find in the text are the blocks
// recorded here, and the hidden-text pass then flags or carves them.
func parseText(data []byte, markdown bool, lim wire.Limits) (*Output, error) {
	var w warnings
	text := cleanText(decodeText(data, &w))
	if int64(len(text)) > lim.MaxOutputBytes {
		return nil, fail(wire.ReasonOutputTooLarge, "extracted text is %d bytes, over the limit of %d", len(text), lim.MaxOutputBytes)
	}
	doc := chunk.FromMarkdown("parse", text)

	regions := textHiddenRegions(text, markdown)
	blocks := make([]wire.Block, 0, len(doc.Blocks))
	structural := 0
	for _, cb := range doc.Blocks {
		blk := wire.Block{Kind: wire.BlockKind(cb.Kind), Level: cb.Level, Title: cb.Title, Start: cb.Start, End: cb.End, HasHeader: cb.HasHeader}
		for _, r := range cb.Rows {
			blk.Rows = append(blk.Rows, wire.Span{Start: r.Start, End: r.End})
		}
		blocks = append(blocks, applyHidden(text, blk, regions)...)
	}
	for _, b := range blocks {
		structural += 1 + len(b.Rows)
	}
	if lim.MaxBlocks > 0 && structural > lim.MaxBlocks {
		return nil, fail(wire.ReasonOutputTooLarge, "more than %d blocks and rows", lim.MaxBlocks)
	}
	o := &Output{Text: []byte(text), Blocks: blocks, Warnings: w.list, PageStarts: doc.PageStarts}
	if len(doc.PageStarts) > 0 {
		o.Pages = len(doc.PageStarts)
	}
	return o, nil
}

// textHiddenRegions finds hidden spans in plain text or Markdown source:
// invisible Unicode runs always, and for Markdown also HTML comments and raw
// HTML elements that hide themselves.
func textHiddenRegions(text string, markdown bool) []hiddenRegion {
	var regions []hiddenRegion
	for _, s := range invisibleRuns(text) {
		regions = append(regions, hiddenRegion{start: s.Start, end: s.End, reason: wire.HideInvisibleChars})
	}
	if markdown {
		for _, m := range commentRE.FindAllStringIndex(text, -1) {
			regions = append(regions, hiddenRegion{start: m[0], end: m[1], reason: wire.HideComment, raw: true})
		}
		regions = append(regions, rawHTMLHidden(text)...)
	}
	return mergeRegions(regions)
}

// mergeRegions sorts the regions and merges overlapping ones into a disjoint,
// ascending list. A merged region is raw only when every part of it is.
func mergeRegions(regions []hiddenRegion) []hiddenRegion {
	sort.Slice(regions, func(i, j int) bool { return regions[i].start < regions[j].start })
	var out []hiddenRegion
	for _, r := range regions {
		if n := len(out); n > 0 && r.start < out[n-1].end {
			if r.end > out[n-1].end {
				out[n-1].end = r.end
			}
			out[n-1].raw = out[n-1].raw && r.raw
			continue
		}
		out = append(out, r)
	}
	return out
}

// rawHTMLHidden finds raw HTML elements in Markdown whose own attributes hide
// them, and returns the span from the opening tag to the matching close tag
// (or the end of the line when there is none).
func rawHTMLHidden(text string) []hiddenRegion {
	var out []hiddenRegion
	covered := 0
	opens := openTagRE.FindAllStringSubmatchIndex(text, -1)
	var closers []int
	for i, m := range opens {
		if m[0] < covered {
			continue
		}
		attrs := ""
		if m[4] >= 0 {
			attrs = text[m[4]:m[5]]
		}
		reason := hiddenByAttrs(parseAttrs(attrs))
		if reason == "" {
			continue
		}
		if closers == nil {
			closers = tagClosers(text, len(opens))
		}
		end := closers[i]
		if end < 0 {
			end = m[1]
			if nl := strings.IndexByte(text[end:], '\n'); nl >= 0 {
				end += nl
			} else {
				end = len(text)
			}
		}
		out = append(out, hiddenRegion{start: m[0], end: end, reason: wire.HideRawHTML, raw: true})
		covered = end
	}
	return out
}

// tagClosers returns, for the i-th of the n opening tags in text, the end
// offset of the </name> that closes it, counting nested elements of the same
// name, or -1. It matches every tag in one pass with a stack per name, so the
// cost is linear in the text however many hidden elements are left unclosed.
func tagClosers(text string, n int) []int {
	closers := make([]int, n)
	for i := range closers {
		closers[i] = -1
	}
	open := map[string][]int{}
	i := 0
	for pos := 0; pos < len(text); {
		m := anyTagRE.FindStringSubmatchIndex(text[pos:])
		if m == nil {
			break
		}
		// Names fold as a (?i) pattern does, the long s and Kelvin sign included.
		name := strings.ToLower(strings.ToUpper(text[pos+m[4] : pos+m[5]]))
		if m[2] == m[3] { // no slash: an opening tag, the i-th
			if i < n {
				open[name] = append(open[name], i)
			}
			i++
		} else if s := open[name]; len(s) > 0 {
			closers[s[len(s)-1]] = pos + m[1]
			open[name] = s[:len(s)-1]
		}
		pos += m[1]
	}
	return closers
}

// applyHidden applies the hidden regions to one block. A paragraph with hidden
// text inside is split so the hidden text is its own block; a heading, code
// block or table that merely overlaps a region is hidden as a whole; and a
// block of any kind that lies entirely inside a region is reduced to a hidden
// paragraph, so hidden text can never start a section or a table.
func applyHidden(text string, blk wire.Block, regions []hiddenRegion) []wire.Block {
	// Regions overlapping [Start, End), clipped to it. A raw region does not
	// apply inside a code block.
	var hit []hiddenRegion
	i := sort.Search(len(regions), func(i int) bool { return regions[i].end > blk.Start })
	for ; i < len(regions) && regions[i].start < blk.End; i++ {
		if r := regions[i]; !(r.raw && blk.Kind == wire.KindCode) {
			hit = append(hit, r)
		}
	}
	if len(hit) == 0 {
		return []wire.Block{blk}
	}
	// Entirely inside one region: a hidden paragraph.
	for _, r := range hit {
		if r.start <= blk.Start && r.end >= blk.End {
			return []wire.Block{{Kind: wire.KindParagraph, Start: blk.Start, End: blk.End, Hidden: true, HiddenReason: r.reason}}
		}
	}
	if blk.Kind != wire.KindParagraph {
		blk.Hidden, blk.HiddenReason = true, hit[0].reason
		return []wire.Block{blk}
	}
	var out []wire.Block
	emit := func(s, e int, hidden bool, reason string) {
		s, e = trimSpan(text, s, e)
		if s >= e {
			return
		}
		b := wire.Block{Kind: wire.KindParagraph, Start: s, End: e}
		if hidden {
			b.Hidden, b.HiddenReason = true, reason
		}
		out = append(out, b)
	}
	pos := blk.Start
	for _, r := range hit {
		s, e := r.start, r.end
		if s < pos {
			s = pos
		}
		if e > blk.End {
			e = blk.End
		}
		if e <= s {
			continue
		}
		emit(pos, s, false, "")
		emit(s, e, true, r.reason)
		pos = e
	}
	emit(pos, blk.End, false, "")
	return out
}

// trimSpan trims white space off both ends of text[s:e].
func trimSpan(text string, s, e int) (int, int) {
	sub := text[s:e]
	left := strings.TrimLeftFunc(sub, unicode.IsSpace)
	s += len(sub) - len(left)
	right := strings.TrimRightFunc(left, unicode.IsSpace)
	return s, s + len(right)
}
