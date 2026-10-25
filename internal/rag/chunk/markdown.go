package chunk

import "strings"

// FromMarkdown finds the structure of plain text or Markdown and returns it as
// a Document, for sources whose parser delivers text and no blocks (txt, md,
// and the text a PDF extractor emits). It is a line scanner, not a full
// CommonMark parser, and it recognises:
//
//   - ATX headings ("## Title") and one-line setext headings ("Title" over "===" or "---");
//   - fenced code blocks (``` or ~~~) and a leading "---" front-matter block, both as code;
//   - GFM pipe tables (a header row over a "|---|---|" delimiter row, then rows);
//   - paragraphs, ended by a blank line, a page break or the start of another block.
//
// A form feed (U+000C) is a page break: it ends the paragraph in progress and
// the next text is on the next page. If the text has none, pages are unknown.
// Thematic breaks ("---", "***", "___") are dropped. The Text is never edited,
// so every span stays a slice of the original.
func FromMarkdown(sourceID, text string) Document {
	doc := Document{SourceID: sourceID, Text: text}
	lines, pages := mdLines(text)
	doc.PageStarts = pages
	sc := mdScanner{text: text, lines: lines, doc: &doc}
	sc.run()
	return doc
}

type mdLine struct {
	s, e int  // line content, without the line terminator
	brk  bool // a page break follows this line
}

// mdLines splits text at "\n" and "\f". It also returns the page starts: the
// byte after every form feed, preceded by 0, or nil when there is none.
func mdLines(text string) ([]mdLine, []int) {
	var (
		lines []mdLine
		pages []int
	)
	start := 0
	end := func(i int, brk bool) {
		e := i
		if e > start && text[e-1] == '\r' {
			e--
		}
		lines = append(lines, mdLine{s: start, e: e, brk: brk})
		start = i + 1
	}
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\n':
			end(i, false)
		case '\f':
			end(i, true)
			if pages == nil {
				pages = []int{0}
			}
			pages = append(pages, i+1)
		}
	}
	if start < len(text) {
		lines = append(lines, mdLine{s: start, e: len(text)})
	}
	return lines, pages
}

type mdScanner struct {
	text  string
	lines []mdLine
	doc   *Document

	// the paragraph in progress: lines [pFrom, pTo)
	pFrom, pTo int
}

func (sc *mdScanner) content(i int) string { return sc.text[sc.lines[i].s:sc.lines[i].e] }

func (sc *mdScanner) flush() {
	if sc.pTo > sc.pFrom {
		sc.doc.Blocks = append(sc.doc.Blocks, Block{
			Kind:  BlockParagraph,
			Start: sc.lines[sc.pFrom].s,
			End:   sc.lines[sc.pTo-1].e,
		})
	}
	sc.pFrom, sc.pTo = 0, 0
}

func (sc *mdScanner) add(b Block) { sc.doc.Blocks = append(sc.doc.Blocks, b) }

func (sc *mdScanner) run() {
	n := len(sc.lines)
	i := 0
	if n > 1 && strings.TrimSpace(sc.content(0)) == "---" {
		for j := 1; j < n; j++ {
			if c := strings.TrimSpace(sc.content(j)); c == "---" || c == "..." {
				sc.add(Block{Kind: BlockCode, Start: sc.lines[0].s, End: sc.lines[j].e})
				i = j + 1
				break
			}
		}
	}
	for ; i < n; i++ {
		line := sc.content(i)
		trim := strings.TrimSpace(line)
		switch {
		case trim == "":
			sc.flush()
		case isFence(line):
			sc.flush()
			i = sc.fenced(i)
		case atxLevel(line) > 0:
			sc.flush()
			level, title := atxHeading(line)
			if title == "" {
				break // an empty heading says nothing
			}
			sc.add(Block{Kind: BlockHeading, Level: level, Title: title, Start: sc.lines[i].s, End: sc.lines[i].e})
		case sc.setext(i):
			// handled: the one-line paragraph became a heading
		case isThematicBreak(trim):
			sc.flush()
		case isTableHeader(line) && i+1 < n && isTableDelimiter(sc.content(i+1), line) && !sc.lines[i].brk:
			sc.flush()
			i = sc.table(i)
		default:
			if sc.pTo == sc.pFrom {
				sc.pFrom = i
			}
			sc.pTo = i + 1
		}
		if i < n && sc.lines[i].brk {
			sc.flush()
		}
	}
	sc.flush()
}

// setext turns the one-line paragraph above line i into a heading when line i
// is a "===" or "---" underline. It reports whether it did.
func (sc *mdScanner) setext(i int) bool {
	trim := strings.TrimSpace(sc.content(i))
	if sc.pTo-sc.pFrom != 1 || sc.pTo != i || trim == "" {
		return false
	}
	level := 0
	switch {
	case strings.Trim(trim, "=") == "":
		level = 1
	case strings.Trim(trim, "-") == "":
		level = 2
	default:
		return false
	}
	h := sc.lines[sc.pFrom]
	sc.add(Block{Kind: BlockHeading, Level: level, Start: h.s, End: h.e})
	sc.pFrom, sc.pTo = 0, 0
	return true
}

// fenced consumes a fenced code block opened at line i and returns the index
// of its last line (the closing fence, or the last line of the text when the
// fence is never closed).
func (sc *mdScanner) fenced(i int) int {
	open := strings.TrimLeft(sc.content(i), " ")
	ch := open[0]
	width := len(open) - len(strings.TrimLeft(open, string(ch)))
	last := len(sc.lines) - 1
	for j := i + 1; j < len(sc.lines); j++ {
		c := strings.TrimSpace(sc.content(j))
		if len(c) >= width && strings.Trim(c, string(ch)) == "" {
			last = j
			break
		}
	}
	sc.add(Block{Kind: BlockCode, Start: sc.lines[i].s, End: sc.lines[last].e})
	return last
}

// table consumes a pipe table whose header row is line i and returns its last
// line.
func (sc *mdScanner) table(i int) int {
	blk := Block{Kind: BlockTable, HasHeader: true, Start: sc.lines[i].s}
	blk.Rows = append(blk.Rows, Row{Start: sc.lines[i].s, End: sc.lines[i].e})
	last := i + 1 // the delimiter row
	for j := i + 2; j < len(sc.lines); j++ {
		c := sc.content(j)
		if strings.TrimSpace(c) == "" || !strings.Contains(c, "|") || sc.lines[j-1].brk {
			break
		}
		blk.Rows = append(blk.Rows, Row{Start: sc.lines[j].s, End: sc.lines[j].e})
		last = j
	}
	blk.End = sc.lines[last].e
	sc.add(blk)
	return last
}

// isFence reports whether line opens or closes a fenced code block: up to
// three spaces, then at least three backticks or tildes.
func isFence(line string) bool {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 || len(t) < 3 {
		return false
	}
	ch := t[0]
	if ch != '`' && ch != '~' {
		return false
	}
	return strings.HasPrefix(t, strings.Repeat(string(ch), 3))
}

func atxLevel(line string) int {
	l, _ := atxHeading(line)
	return l
}

// atxHeading parses "## Title ##" (up to three leading spaces) and returns
// the level and the title, or level 0 when line is not an ATX heading.
func atxHeading(line string) (int, string) {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 {
		return 0, ""
	}
	level := len(t) - len(strings.TrimLeft(t, "#"))
	if level < 1 || level > 6 {
		return 0, ""
	}
	rest := t[level:]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return 0, ""
	}
	rest = strings.TrimSpace(rest)
	if trimmed := strings.TrimRight(rest, "#"); trimmed != rest && (trimmed == "" || trimmed[len(trimmed)-1] == ' ' || trimmed[len(trimmed)-1] == '\t') {
		rest = strings.TrimSpace(trimmed)
	}
	return level, rest
}

// isThematicBreak: three or more of the same of - * _, spaces allowed.
func isThematicBreak(trim string) bool {
	if len(trim) < 3 {
		return false
	}
	ch := trim[0]
	if ch != '-' && ch != '*' && ch != '_' {
		return false
	}
	count := 0
	for i := 0; i < len(trim); i++ {
		switch trim[i] {
		case ch:
			count++
		case ' ', '\t':
		default:
			return false
		}
	}
	return count >= 3
}

func isTableHeader(line string) bool { return strings.Contains(line, "|") }

// tableCells counts the cells of a pipe row, ignoring one leading and one
// trailing pipe.
func tableCells(line string) int {
	t := strings.TrimSpace(line)
	t = strings.TrimPrefix(t, "|")
	t = strings.TrimSuffix(t, "|")
	return strings.Count(t, "|") + 1
}

// isTableDelimiter reports whether line is a GFM delimiter row ("|---|:-:|")
// with as many cells as the header row.
func isTableDelimiter(line, header string) bool {
	t := strings.TrimSpace(line)
	if !strings.Contains(t, "|") {
		return false
	}
	t = strings.TrimSuffix(strings.TrimPrefix(t, "|"), "|")
	cells := strings.Split(t, "|")
	if len(cells) != tableCells(header) {
		return false
	}
	for _, c := range cells {
		c = strings.TrimSpace(c)
		c = strings.TrimPrefix(c, ":")
		c = strings.TrimSuffix(c, ":")
		if c == "" || strings.Trim(c, "-") != "" {
			return false
		}
	}
	return true
}
