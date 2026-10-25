package formats

import (
	"encoding/xml"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// maxStyles bounds the styles read from styles.xml.
const maxStyles = 5000

// docxParts are the parts read after the body, in order. Headers, footers and
// comments are not text of the document and are not read; a part that is never
// read is never inflated either.
var docxNotes = []string{"word/footnotes.xml", "word/endnotes.xml"}

func parseDOCX(data []byte, lim wire.Limits) (*Output, error) {
	var w warnings
	bud := newBudget(lim)
	z, err := openZip(data, lim, bud)
	if err != nil {
		return nil, err
	}
	if !z.has("word/document.xml") {
		return nil, malformed("not a DOCX: word/document.xml is missing")
	}
	styles := &docxStyles{byID: map[string]*docxStyle{}}
	if z.has("word/styles.xml") {
		if err := readPart(z, "word/styles.xml", lim, func(d *xmlWalker) error { return styles.read(d) }); err != nil {
			return nil, err
		}
	}
	b := newBuilder(lim)
	dp := &docxParser{b: b, styles: styles, w: &w}
	if err := readPart(z, "word/document.xml", lim, func(d *xmlWalker) error { dp.x = d; return dp.document() }); err != nil {
		return nil, err
	}
	for _, name := range docxNotes {
		if !z.has(name) {
			continue
		}
		if err := readPart(z, name, lim, func(d *xmlWalker) error { dp.x = d; return dp.notes() }); err != nil {
			return nil, err
		}
	}
	return b.finish(&w), nil
}

func readPart(z *zipFile, name string, lim wire.Limits, fn func(d *xmlWalker) error) error {
	r, err := z.open(name)
	if err != nil {
		return err
	}
	d := newXMLWalker(r, lim.MaxDepth)
	if err := fn(d); err != nil {
		return asParseError(err)
	}
	return asParseError(r.finish())
}

// asParseError keeps a typed *Error and turns anything else (an XML syntax
// error) into a malformed-document failure.
func asParseError(err error) error {
	if err == nil {
		return nil
	}
	var fe *Error
	if errors.As(err, &fe) {
		return fe
	}
	return malformed("xml: %v", err)
}

// xmlWalker pulls XML tokens with a depth limit applied to every element,
// skipped or not, and refuses DTDs. encoding/xml does not expand entities, but
// a DTD has no place in a word-processing document, and nesting is unbounded
// without this.
type xmlWalker struct {
	d        *xml.Decoder
	depth    int
	maxDepth int
}

func newXMLWalker(r io.Reader, maxDepth int) *xmlWalker {
	d := xml.NewDecoder(r)
	d.Strict = true
	return &xmlWalker{d: d, maxDepth: maxDepth}
}

func (x *xmlWalker) next() (xml.Token, error) {
	tok, err := x.d.Token()
	if err != nil {
		return nil, err
	}
	switch tok.(type) {
	case xml.StartElement:
		x.depth++
		if x.depth > x.maxDepth {
			return nil, fail(wire.ReasonNestingTooDeep, "XML nesting is deeper than %d", x.maxDepth)
		}
	case xml.EndElement:
		x.depth--
	case xml.Directive:
		return nil, malformed("a DTD or other directive in an OOXML part")
	}
	return tok, nil
}

// children calls fn for every child element of the element just started. fn
// must consume the child completely (with children or skip). It returns when
// the parent's end element has been consumed.
func (x *xmlWalker) children(fn func(se xml.StartElement) error) error {
	start := x.depth
	for {
		tok, err := x.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return malformed("xml: unexpected end of document")
			}
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if err := fn(t); err != nil {
				return err
			}
		case xml.EndElement:
			if x.depth < start {
				return nil
			}
		}
	}
}

// skip consumes the element just started.
func (x *xmlWalker) skip() error {
	start := x.depth
	for x.depth >= start {
		if _, err := x.next(); err != nil {
			if errors.Is(err, io.EOF) {
				return malformed("xml: unexpected end of document")
			}
			return err
		}
	}
	return nil
}

// text returns the character data of the element just started, which is
// consumed. Child elements are skipped.
func (x *xmlWalker) text() (string, error) {
	start := x.depth
	var sb strings.Builder
	for x.depth >= start {
		tok, err := x.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", malformed("xml: unexpected end of document")
			}
			return "", err
		}
		if cd, ok := tok.(xml.CharData); ok {
			sb.Write(cd)
		}
	}
	return sb.String(), nil
}

// root reads up to the document element and returns it.
func (x *xmlWalker) root() (xml.StartElement, error) {
	for {
		tok, err := x.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return xml.StartElement{}, malformed("xml: no document element")
			}
			return xml.StartElement{}, err
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se, nil
		}
	}
}

func attrOf(se xml.StartElement, local string) string {
	for _, a := range se.Attr {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

// onOff reads an OOXML on/off property: present means on unless val says off.
func onOff(se xml.StartElement) bool {
	switch strings.ToLower(attrOf(se, "val")) {
	case "0", "false", "off":
		return false
	}
	return true
}

// ---- styles ----

// runProps are the character properties that decide whether text is visible.
// A nil field is unset and inherits.
type runProps struct {
	vanish, specVanish, webHidden *bool
	color                         *string // hex, or "auto"
	themeWhite                    bool
	sz                            *int // half-points
	fill                          *string
	highlight                     *string
	rstyle                        string
}

func (p *runProps) merge(o runProps) {
	if o.vanish != nil {
		p.vanish = o.vanish
	}
	if o.specVanish != nil {
		p.specVanish = o.specVanish
	}
	if o.webHidden != nil {
		p.webHidden = o.webHidden
	}
	if o.color != nil {
		p.color, p.themeWhite = o.color, o.themeWhite
	}
	if o.sz != nil {
		p.sz = o.sz
	}
	if o.fill != nil {
		p.fill = o.fill
	}
	if o.highlight != nil {
		p.highlight = o.highlight
	}
	if o.rstyle != "" {
		p.rstyle = o.rstyle
	}
}

type docxStyle struct {
	name    string
	basedOn string
	run     runProps
	outline int // -1 when unset
}

type docxStyles struct {
	byID     map[string]*docxStyle
	defaults runProps
}

// read parses styles.xml.
func (s *docxStyles) read(x *xmlWalker) error {
	if _, err := x.root(); err != nil {
		return err
	}
	return x.children(func(se xml.StartElement) error {
		switch se.Name.Local {
		case "docDefaults":
			return x.children(func(se xml.StartElement) error {
				if se.Name.Local != "rPrDefault" {
					return x.skip()
				}
				return x.children(func(se xml.StartElement) error {
					if se.Name.Local != "rPr" {
						return x.skip()
					}
					return readRPr(x, &s.defaults)
				})
			})
		case "style":
			if len(s.byID) >= maxStyles {
				return x.skip()
			}
			st := &docxStyle{outline: -1}
			id := attrOf(se, "styleId")
			err := x.children(func(se xml.StartElement) error {
				switch se.Name.Local {
				case "name":
					st.name = strings.ToLower(attrOf(se, "val"))
				case "basedOn":
					st.basedOn = attrOf(se, "val")
				case "rPr":
					return readRPr(x, &st.run)
				case "pPr":
					return x.children(func(se xml.StartElement) error {
						if se.Name.Local == "outlineLvl" {
							if n, err := strconv.Atoi(attrOf(se, "val")); err == nil {
								st.outline = n
							}
						}
						return x.skip()
					})
				default:
					return x.skip()
				}
				return x.skip()
			})
			if err != nil {
				return err
			}
			if id != "" {
				s.byID[id] = st
			}
			return nil
		}
		return x.skip()
	})
}

// chain returns the styles from the root of the basedOn chain down to id.
func (s *docxStyles) chain(id string) []*docxStyle {
	var rev []*docxStyle
	seen := map[string]bool{}
	for id != "" && !seen[id] && len(rev) < 20 {
		seen[id] = true
		st, ok := s.byID[id]
		if !ok {
			break
		}
		rev = append(rev, st)
		id = st.basedOn
	}
	out := make([]*docxStyle, len(rev))
	for i, st := range rev {
		out[len(rev)-1-i] = st
	}
	return out
}

// readRPr parses a run-properties element (rPr) just started.
func readRPr(x *xmlWalker, p *runProps) error {
	return x.children(func(se xml.StartElement) error {
		switch se.Name.Local {
		case "vanish":
			v := onOff(se)
			p.vanish = &v
		case "specVanish":
			v := onOff(se)
			p.specVanish = &v
		case "webHidden":
			v := onOff(se)
			p.webHidden = &v
		case "color":
			c := strings.ToLower(attrOf(se, "val"))
			p.color = &c
			switch strings.ToLower(attrOf(se, "themeColor")) {
			case "background1", "bg1", "light1":
				p.themeWhite = true
			default:
				p.themeWhite = false
			}
		case "sz":
			if n, err := strconv.Atoi(attrOf(se, "val")); err == nil {
				p.sz = &n
			}
		case "shd":
			if f := shadingFill(se); f != "" {
				p.fill = &f
			}
		case "highlight":
			h := strings.ToLower(attrOf(se, "val"))
			p.highlight = &h
		case "rStyle":
			p.rstyle = attrOf(se, "val")
		}
		return x.skip()
	})
}

// shadingFill is the background colour a w:shd sets, or "" for none or auto.
func shadingFill(se xml.StartElement) string {
	val := strings.ToLower(attrOf(se, "val"))
	var c string
	switch val {
	case "clear", "nil", "":
		c = attrOf(se, "fill")
	case "solid":
		c = attrOf(se, "color")
	default:
		return "" // patterned shading is not a flat colour
	}
	c = strings.ToLower(c)
	if c == "auto" {
		return ""
	}
	return c
}

var highlightColours = map[string]string{
	"black": "000000", "blue": "0000ff", "cyan": "00ffff", "darkblue": "00008b", "darkcyan": "008b8b",
	"darkgray": "a9a9a9", "darkgreen": "006400", "darkmagenta": "8b008b", "darkred": "8b0000",
	"darkyellow": "808000", "green": "00ff00", "lightgray": "d3d3d3", "magenta": "ff00ff", "red": "ff0000",
	"white": "ffffff", "yellow": "ffff00",
}

func hexColour(h string) *rgba {
	if len(h) != 6 {
		return nil
	}
	n, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return nil
	}
	return &rgba{uint8(n >> 16), uint8(n >> 8), uint8(n), 1}
}

// ---- document ----

type paraProps struct {
	style   string
	outline int // -1 unset
	fill    string
}

type docxParser struct {
	x      *xmlWalker
	b      *Builder
	styles *docxStyles
	w      *warnings

	// capture, when not nil, collects paragraphs instead of emitting them:
	// the text of a table cell or a text box.
	capture   *[]Seg
	cellFill  string
	afterPara [][]Seg // text-box paragraphs, emitted after the paragraph that holds the box
}

func (p *docxParser) document() error {
	if _, err := p.x.root(); err != nil {
		return err
	}
	return p.x.children(func(se xml.StartElement) error {
		if se.Name.Local == "body" {
			return p.blocks()
		}
		return p.x.skip()
	})
}

func (p *docxParser) notes() error {
	if _, err := p.x.root(); err != nil {
		return err
	}
	return p.x.children(func(se xml.StartElement) error {
		if se.Name.Local != "footnote" && se.Name.Local != "endnote" {
			return p.x.skip()
		}
		if t := attrOf(se, "type"); t == "separator" || t == "continuationSeparator" || t == "continuationNotice" {
			return p.x.skip()
		}
		return p.blocks()
	})
}

// blocks walks block-level content of the container just started.
func (p *docxParser) blocks() error {
	return p.x.children(func(se xml.StartElement) error {
		switch se.Name.Local {
		case "p":
			return p.paragraph()
		case "tbl":
			return p.table()
		case "sdt", "sdtContent", "customXml", "ins", "moveTo", "smartTag":
			return p.blocks()
		case "AlternateContent":
			return p.x.children(func(se xml.StartElement) error {
				if se.Name.Local == "Choice" {
					return p.blocks()
				}
				return p.x.skip()
			})
		}
		return p.x.skip()
	})
}

func (p *docxParser) paragraph() error {
	pp := paraProps{outline: -1}
	var segs []Seg
	err := p.x.children(func(se xml.StartElement) error {
		if se.Name.Local == "pPr" {
			return p.readPPr(&pp)
		}
		return p.inline(se, &pp, &segs, false)
	})
	if err != nil {
		return err
	}
	return p.emit(&pp, segs)
}

func (p *docxParser) readPPr(pp *paraProps) error {
	return p.x.children(func(se xml.StartElement) error {
		switch se.Name.Local {
		case "pStyle":
			pp.style = attrOf(se, "val")
		case "outlineLvl":
			if n, err := strconv.Atoi(attrOf(se, "val")); err == nil {
				pp.outline = n
			}
		case "shd":
			pp.fill = shadingFill(se)
		}
		return p.x.skip()
	})
}

// emit writes a finished paragraph, or hands it to the capture in progress.
func (p *docxParser) emit(pp *paraProps, segs []Seg) error {
	if p.capture != nil {
		if len(segs) > 0 {
			*p.capture = append(*p.capture, segs...)
			*p.capture = append(*p.capture, Seg{Text: " "})
		}
	} else {
		if level := p.headingLevel(pp); level > 0 {
			if err := p.b.Heading(level, segs); err != nil {
				return err
			}
		} else if err := p.b.Paragraph(segs); err != nil {
			return err
		}
	}
	if p.capture == nil && len(p.afterPara) > 0 {
		after := p.afterPara
		p.afterPara = nil
		for _, a := range after {
			if err := p.b.Paragraph(a); err != nil {
				return err
			}
		}
	}
	return nil
}

var headingNameRE = regexp.MustCompile(`^heading ?([1-9])$`)

// headingLevel is the heading level of a paragraph (1 to 6), or 0 for body
// text: from the outline level, direct or from the style chain, else from a
// built-in heading style name.
func (p *docxParser) headingLevel(pp *paraProps) int {
	level, styleOutline := 0, -1
	for _, st := range p.styles.chain(pp.style) { // from the root of the chain down
		if st.outline >= 0 {
			styleOutline = st.outline
		}
		if m := headingNameRE.FindStringSubmatch(st.name); m != nil {
			level, _ = strconv.Atoi(m[1])
		} else if st.name == "title" {
			level = 1
		}
	}
	outline := pp.outline
	if outline < 0 {
		outline = styleOutline
	}
	switch {
	case outline >= 0 && outline <= 8:
		level = outline + 1
	case outline == 9:
		level = 0 // an explicit "body text" outline level
	}
	if level == 0 && pp.style != "" && len(p.styles.byID) == 0 {
		// No styles part to consult: fall back to the built-in style IDs.
		if m := headingNameRE.FindStringSubmatch(strings.ToLower(pp.style)); m != nil {
			level, _ = strconv.Atoi(m[1])
		}
	}
	return min(level, 6)
}

// inline handles one child of a paragraph or of an inline container.
func (p *docxParser) inline(se xml.StartElement, pp *paraProps, segs *[]Seg, deleted bool) error {
	switch se.Name.Local {
	case "r":
		return p.run(pp, segs, deleted)
	case "hyperlink", "fldSimple", "smartTag", "ins", "moveTo", "sdt", "sdtContent", "customXml", "bdo", "dir":
		return p.x.children(func(se xml.StartElement) error { return p.inline(se, pp, segs, deleted) })
	case "del", "moveFrom":
		return p.x.children(func(se xml.StartElement) error { return p.inline(se, pp, segs, true) })
	case "AlternateContent":
		return p.x.children(func(se xml.StartElement) error {
			if se.Name.Local == "Choice" {
				return p.x.children(func(se xml.StartElement) error { return p.inline(se, pp, segs, deleted) })
			}
			return p.x.skip()
		})
	}
	return p.x.skip()
}

// run reads one w:r: its properties first, then its content.
func (p *docxParser) run(pp *paraProps, segs *[]Seg, deleted bool) error {
	var direct runProps
	var hidden bool
	var reason string
	ready := false
	resolve := func() {
		if ready {
			return
		}
		ready = true
		hidden, reason = p.runHidden(pp, direct, deleted)
	}
	add := func(s string) {
		resolve()
		*segs = append(*segs, Seg{Text: s, Hidden: hidden, Reason: reason})
	}
	return p.x.children(func(se xml.StartElement) error {
		switch se.Name.Local {
		case "rPr":
			return readRPr(p.x, &direct)
		case "t":
			s, err := p.x.text()
			add(s)
			return err
		case "delText":
			s, err := p.x.text()
			deleted = true
			ready = false
			add(s)
			return err
		case "tab":
			add("\t")
		case "br", "cr":
			add("\n")
		case "noBreakHyphen":
			add("-")
		case "drawing", "pict", "object", "AlternateContent":
			return p.deep()
		}
		return p.x.skip()
	})
}

// runHidden decides whether a run's text is hidden from a reader.
func (p *docxParser) runHidden(pp *paraProps, direct runProps, deleted bool) (bool, string) {
	var eff runProps
	eff.merge(p.styles.defaults)
	for _, st := range p.styles.chain(pp.style) {
		eff.merge(st.run)
	}
	rs := direct.rstyle
	for _, st := range p.styles.chain(rs) {
		eff.merge(st.run)
	}
	eff.merge(direct)
	switch {
	case deleted:
		return true, wire.HideDeletedText
	case isOn(eff.vanish) || isOn(eff.specVanish) || isOn(eff.webHidden):
		return true, wire.HideVanish
	case eff.sz != nil && *eff.sz > 0 && float64(*eff.sz) < tinyFontPoints*2:
		return true, wire.HideTinyFont
	}
	fg := (*rgba)(nil)
	switch {
	case eff.themeWhite:
		fg = white
	case eff.color != nil && *eff.color != "auto":
		fg = hexColour(*eff.color)
	}
	if fg == nil {
		return false, ""
	}
	bg := white
	switch {
	case eff.highlight != nil && *eff.highlight != "none":
		if h, ok := highlightColours[*eff.highlight]; ok {
			bg = hexColour(h)
		}
	case eff.fill != nil:
		if c := hexColour(*eff.fill); c != nil {
			bg = c
		}
	case pp.fill != "":
		if c := hexColour(pp.fill); c != nil {
			bg = c
		}
	case p.cellFill != "":
		if c := hexColour(p.cellFill); c != nil {
			bg = c
		}
	}
	if r := contrastReason(fg, bg); r != "" {
		return true, r
	}
	return false, ""
}

func isOn(b *bool) bool { return b != nil && *b }

// deep finds text boxes inside a drawing or picture and reads their paragraphs
// as separate paragraphs, emitted after the paragraph that holds the box. The
// non-chosen fallback of an AlternateContent repeats the same text and is
// skipped.
func (p *docxParser) deep() error {
	return p.x.children(func(se xml.StartElement) error {
		switch se.Name.Local {
		case "txbxContent":
			return p.textbox()
		case "Fallback":
			return p.x.skip()
		}
		return p.deep()
	})
}

func (p *docxParser) textbox() error {
	saved := p.capture
	var cap []Seg
	p.capture = &cap
	err := p.blocks()
	p.capture = saved
	if err != nil {
		return err
	}
	if len(cap) > 0 {
		p.afterPara = append(p.afterPara, cap)
	}
	return nil
}

func (p *docxParser) table() error {
	if p.capture != nil {
		// A table inside a cell or a text box is flattened into it.
		return p.x.children(func(se xml.StartElement) error {
			switch se.Name.Local {
			case "tr":
				return p.x.children(func(se xml.StartElement) error {
					if se.Name.Local == "tc" {
						return p.cell(nil)
					}
					return p.x.skip()
				})
			}
			return p.x.skip()
		})
	}
	tw := p.b.NewTable()
	header := false
	rowN := 0
	err := p.x.children(func(se xml.StartElement) error {
		if se.Name.Local != "tr" {
			return p.x.skip()
		}
		var row Row
		isHeader := false
		err := p.x.children(func(se xml.StartElement) error {
			switch se.Name.Local {
			case "trPr":
				return p.x.children(func(se xml.StartElement) error {
					if se.Name.Local == "tblHeader" {
						isHeader = onOff(se)
					}
					return p.x.skip()
				})
			case "tc":
				var cell Cell
				if err := p.cell(&cell); err != nil {
					return err
				}
				row = append(row, cell)
				return nil
			}
			return p.x.skip()
		})
		if err != nil {
			return err
		}
		if rowN == 0 {
			header = isHeader
		}
		rowN++
		return tw.AddRow(row)
	})
	if err != nil {
		return err
	}
	return tw.End(header)
}

// cell reads a w:tc, collecting its paragraphs as one cell of text. out is nil
// when the table is itself being flattened into an outer capture.
func (p *docxParser) cell(out *Cell) error {
	savedCap, savedFill := p.capture, p.cellFill
	var cap []Seg
	if out != nil {
		p.capture = &cap
	}
	defer func() { p.capture, p.cellFill = savedCap, savedFill }()
	err := p.x.children(func(se xml.StartElement) error {
		switch se.Name.Local {
		case "tcPr":
			return p.x.children(func(se xml.StartElement) error {
				if se.Name.Local == "shd" {
					if f := shadingFill(se); f != "" {
						p.cellFill = f
					}
				}
				return p.x.skip()
			})
		case "p":
			return p.paragraph()
		case "tbl":
			return p.table()
		case "sdt", "sdtContent", "customXml":
			return p.blocks()
		}
		return p.x.skip()
	})
	if err != nil {
		return err
	}
	if out != nil {
		*out = cap
	}
	return nil
}
