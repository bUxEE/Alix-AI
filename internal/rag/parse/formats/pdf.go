package formats

import (
	"errors"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// parsePDF extracts the text layer of a PDF in pure Go: it reads the file
// structure (classic and stream cross-reference tables, object streams,
// repair by scanning when they are damaged), walks the page tree, interprets
// each page's content stream for its text-showing operators, and decodes the
// strings through the fonts' encodings and ToUnicode maps.
//
// It runs nothing: no JavaScript, no embedded files, no launch actions, no
// fonts programs. It does not decrypt (an encrypted file is refused) and does
// not OCR: a page that is only an image is reported in NeedsOCR, never as an
// empty success.
//
// Hidden text is flagged from the operators themselves: text render mode 3 or
// 7 (invisible), fill colour (stroke colour for outlined text) that cannot be
// told from the painted background under it, text under a fill alpha of zero,
// text smaller than 2 points either way, text positioned outside the page, and
// text in an optional-content layer that is off by default.
func parsePDF(data []byte, lim wire.Limits) (*Output, error) {
	var w warnings
	bud := newBudget(lim)
	f, err := openPDF(data, lim, bud, &w)
	if err != nil {
		return nil, err
	}
	root := f.root()
	if root == nil {
		return nil, malformed("pdf: no document catalog")
	}
	pages, err := f.pageList(root, lim.MaxPages)
	if err != nil {
		return nil, err
	}
	ocOff := f.optionalContentOff(root)
	fonts := map[int]*pdfFont{}

	type pagePara struct {
		text   string
		hidden bool
		reason string
		size   float64
		lines  int
	}
	var (
		all     [][]pagePara
		needOCR []int
		sizeLen = map[float64]int{}
		ops     = new(int) // maxOperators is per document, not per page
	)
	for i, pg := range pages {
		pc := &pageCtx{
			f: f, box: pg.box, ocOff: ocOff, fonts: fonts, visiting: map[int]bool{},
			ops: ops, maxRun: int(min(lim.MaxOutputBytes, 1<<30)) * 4,
		}
		black := fillColour{rgb: gray(0), space: "DeviceGray", ncomp: 1}
		gs := gstate{ctm: identity, alpha: 1, th: 1, fill: black, stroke: black}
		// The streams of a page's /Contents array are one logical stream: graphics
		// state, and even a half-finished operator, carry from one to the next.
		var content []byte
		for _, st := range pg.contents {
			part, err := f.decode(st)
			if err != nil {
				if errors.Is(err, errUnsupportedFilter) {
					w.add("pdf_content_unsupported_filter")
					continue
				}
				return nil, err
			}
			content = append(content, part...)
			content = append(content, '\n')
		}
		if err := pc.exec(content, pg.res, gs, 0); err != nil {
			return nil, err
		}
		lines := buildLines(pc.runs)
		paras := groupParas(lines)
		var out []pagePara
		visible := 0
		for _, pa := range paras {
			text := joinLines(pa.lines)
			if strings.TrimSpace(text) == "" {
				continue
			}
			out = append(out, pagePara{text: text, hidden: pa.hidden, reason: pa.reason, size: pa.size, lines: len(pa.lines)})
			if !pa.hidden {
				visible++
				sizeLen[math.Round(pa.size*2)/2] += utf8.RuneCountInString(text)
			}
		}
		if visible == 0 && pc.hasImage {
			needOCR = append(needOCR, i+1)
		}
		all = append(all, out)
	}
	body := 0.0
	bestN := -1
	for sz, n := range sizeLen {
		if n > bestN || (n == bestN && sz < body) {
			body, bestN = sz, n
		}
	}
	b := newBuilder(lim)
	b.BeginPages()
	for i, paras := range all {
		if i > 0 {
			if err := b.NextPage(); err != nil {
				return nil, err
			}
		}
		for _, pa := range paras {
			seg := []Seg{{Text: pa.text, Hidden: pa.hidden, Reason: pa.reason}}
			if level := pdfHeadingLevel(pa.size, body, pa.lines, pa.text, pa.hidden); level > 0 {
				if err := b.Heading(level, seg); err != nil {
					return nil, err
				}
			} else if err := b.Paragraph(seg); err != nil {
				return nil, err
			}
		}
	}
	out := b.finish(&w)
	out.NeedsOCR = needOCR
	out.Expanded = bud.used
	return out, nil
}

// pdfHeadingLevel guesses a heading from type size alone: a short paragraph
// set noticeably larger than the body text of the document.
func pdfHeadingLevel(size, body float64, lines int, text string, hidden bool) int {
	if hidden || body <= 0 || lines > 2 || utf8.RuneCountInString(text) > 120 {
		return 0
	}
	if t := strings.TrimRight(text, " "); strings.HasSuffix(t, ",") || strings.HasSuffix(t, ";") {
		return 0
	}
	switch r := size / body; {
	case r >= 1.6:
		return 1
	case r >= 1.35:
		return 2
	case r >= 1.15:
		return 3
	}
	return 0
}

// ---- page tree ----

type pdfPage struct {
	res      pdfDict
	box      [4]float64
	contents []*pdfStream
}

func (f *pdfFile) pageList(root pdfDict, maxPages int) ([]pdfPage, error) {
	top := f.dict(root["Pages"])
	if top == nil {
		return nil, malformed("pdf: no page tree")
	}
	type frame struct {
		res pdfDict
		box []float64
	}
	var pages []pdfPage
	seenNum := map[int]bool{}
	var walk func(raw any, inh frame, depth int) error
	nodes := 0
	walk = func(raw any, inh frame, depth int) error {
		if depth > f.lim.MaxDepth {
			return fail(wire.ReasonNestingTooDeep, "the PDF page tree is nested deeper than %d", f.lim.MaxDepth)
		}
		if r, ok := raw.(pdfRef); ok {
			if seenNum[r.num] {
				return nil
			}
			seenNum[r.num] = true
		}
		if nodes++; nodes > maxPDFObjects {
			return malformed("pdf: page tree is too large")
		}
		node := f.dict(raw)
		if node == nil {
			return nil
		}
		cur := inh
		if r := f.dict(node["Resources"]); r != nil {
			cur.res = r
		}
		if b := f.floats(node["CropBox"], 4); b != nil {
			cur.box = b
		} else if b := f.floats(node["MediaBox"], 4); b != nil {
			cur.box = b
		}
		kids := f.array(node["Kids"])
		if nameOf(f.resolve(node["Type"])) == "Pages" || (kids != nil && node["Contents"] == nil) {
			for _, k := range kids {
				if err := walk(k, cur, depth+1); err != nil {
					return err
				}
			}
			return nil
		}
		if len(pages) >= maxPages {
			return fail(wire.ReasonTooManyPages, "the PDF has more than %d pages", maxPages)
		}
		pg := pdfPage{res: cur.res, box: [4]float64{0, 0, 612, 792}}
		if len(cur.box) == 4 {
			x0, x1 := math.Min(cur.box[0], cur.box[2]), math.Max(cur.box[0], cur.box[2])
			y0, y1 := math.Min(cur.box[1], cur.box[3]), math.Max(cur.box[1], cur.box[3])
			pg.box = [4]float64{x0, y0, x1, y1}
		}
		switch c := f.resolve(node["Contents"]).(type) {
		case *pdfStream:
			pg.contents = []*pdfStream{c}
		case pdfArray:
			for _, it := range c {
				if st := f.stream(it); st != nil {
					pg.contents = append(pg.contents, st)
				}
			}
		}
		pages = append(pages, pg)
		return nil
	}
	if err := walk(root["Pages"], frame{}, 0); err != nil {
		return nil, err
	}
	if len(pages) == 0 {
		return nil, malformed("pdf: no pages")
	}
	return pages, nil
}

// optionalContentOff returns the object numbers of the optional-content groups
// that are off in the default configuration.
func (f *pdfFile) optionalContentOff(root pdfDict) map[int]bool {
	off := map[int]bool{}
	oc := f.dict(root["OCProperties"])
	if oc == nil {
		return off
	}
	d := f.dict(oc["D"])
	if d == nil {
		return off
	}
	if nameOf(f.resolve(d["BaseState"])) == "OFF" {
		for _, g := range f.array(oc["OCGs"]) {
			if r, ok := g.(pdfRef); ok {
				off[r.num] = true
			}
		}
		for _, g := range f.array(d["ON"]) {
			if r, ok := g.(pdfRef); ok {
				delete(off, r.num)
			}
		}
	}
	for _, g := range f.array(d["OFF"]) {
		if r, ok := g.(pdfRef); ok {
			off[r.num] = true
		}
	}
	return off
}

// ---- layout ----

type pline struct {
	text   string
	sb     strings.Builder // the text while buildLines joins runs to it
	x0, x1 float64
	y      float64
	size   float64
	hidden bool
	reason string
}

// buildLines joins the runs of a page into lines, in content-stream order. A
// run joins the current line when it has the same visibility, sits on the same
// baseline and starts at or after the line's end; a gap wider than a fraction
// of the type size becomes a space.
func buildLines(runs []run) []*pline {
	var lines []*pline
	var cur *pline
	for _, r := range runs {
		if cur != nil && cur.hidden == r.hidden && cur.reason == r.reason &&
			math.Abs(r.y0-cur.y) <= 0.4*math.Max(r.size, 1) && r.x0 >= cur.x1-0.5*math.Max(r.size, 1) {
			gap := r.x0 - cur.x1
			if gap > 0.15*math.Max(math.Max(r.size, cur.size), 1) && !strings.HasSuffix(cur.sb.String(), " ") && !strings.HasPrefix(r.text, " ") {
				cur.sb.WriteByte(' ')
			}
			cur.sb.WriteString(r.text)
			cur.x1 = r.x1
			if r.size > cur.size {
				cur.size = r.size
			}
			continue
		}
		cur = &pline{x0: r.x0, x1: r.x1, y: r.y0, size: r.size, hidden: r.hidden, reason: r.reason}
		cur.sb.WriteString(r.text)
		lines = append(lines, cur)
	}
	for _, ln := range lines {
		ln.text = ln.sb.String()
	}
	return lines
}

type ppara struct {
	lines  []*pline
	hidden bool
	reason string
	size   float64
}

var bulletRE = func(s string) bool {
	s = strings.TrimLeft(s, " ")
	if s == "" {
		return false
	}
	r, n := utf8.DecodeRuneInString(s)
	switch r {
	case '•', '◦', '▪', '●', '○', '■', '–', '-', '*', '·':
		return n < len(s) && s[n] == ' '
	}
	i := 0
	for i < len(s) && i < 4 && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return i > 0 && i < len(s)-1 && (s[i] == '.' || s[i] == ')') && s[i+1] == ' '
}

// groupParas groups lines into paragraphs: a change of visibility, a vertical
// gap of more than about a line and a half, a jump back up the page, a
// noticeable change of type size, or a bullet starts a new paragraph.
func groupParas(lines []*pline) []*ppara {
	var paras []*ppara
	var cur *ppara
	for _, ln := range lines {
		start := cur == nil
		if !start {
			prev := cur.lines[len(cur.lines)-1]
			dy := prev.y - ln.y
			switch {
			case prev.hidden != ln.hidden || prev.reason != ln.reason:
				start = true
			case dy < -0.5*math.Max(prev.size, 1) || dy > 1.8*math.Max(prev.size, ln.size):
				start = true
			case math.Abs(prev.size-ln.size) > 0.12*math.Max(prev.size, 1):
				start = true
			case !ln.hidden && bulletRE(ln.text):
				start = true
			}
		}
		if start {
			cur = &ppara{hidden: ln.hidden, reason: ln.reason}
			paras = append(paras, cur)
		}
		cur.lines = append(cur.lines, ln)
		if ln.size > cur.size {
			cur.size = ln.size
		}
	}
	return paras
}

// joinLines joins the lines of a paragraph into running text: a line that
// ends in a hyphen before a lower-case continuation is joined without the
// hyphen's space, others with a single space.
func joinLines(lines []*pline) string {
	var sb strings.Builder
	for i, ln := range lines {
		t := strings.TrimSpace(ln.text)
		if t == "" {
			continue
		}
		if i > 0 && sb.Len() > 0 {
			prev := sb.String()
			if strings.HasSuffix(prev, "-") && len(prev) > 1 {
				if r, _ := utf8.DecodeRuneInString(t); unicode.IsLower(r) {
					// "exam-" + "ple": keep the hyphen of "well-" + "known", join
					// without a space either way.
					sb.WriteString(t)
					continue
				}
			}
			sb.WriteByte(' ')
		}
		sb.WriteString(t)
	}
	return sb.String()
}
