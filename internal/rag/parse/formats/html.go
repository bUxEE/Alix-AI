package formats

import (
	"errors"
	"io"
	"strings"

	"golang.org/x/net/html"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// maxHTMLToken bounds one HTML token (a comment, a text node, a tag). Beyond it
// the document is refused instead of letting the tokenizer buffer without limit.
const maxHTMLToken = 16 << 20

// maxCSSRules bounds the style rules kept from <style> blocks. Every element
// is matched against every rule, so the bound is also a bound on parse time.
const maxCSSRules = 300

// htmlSkip lists elements whose content is never extracted: it is not text a
// reader sees (scripts, styles, templates, embedded objects, non-rendered
// fallbacks), or it is not document text (form controls' options).
var htmlSkip = map[string]bool{
	"script": true, "style": true, "template": true, "noscript": true, "head": true, "title": true,
	"svg": true, "math": true, "iframe": true, "object": true, "embed": true, "canvas": true,
	"audio": true, "video": true, "select": true, "textarea": true, "noembed": true, "noframes": true,
}

var htmlVoid = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true, "img": true,
	"input": true, "link": true, "meta": true, "param": true, "source": true, "track": true, "wbr": true,
}

// htmlBlock lists elements that start and end a paragraph.
var htmlBlock = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true, "details": true, "dialog": true,
	"dd": true, "div": true, "dl": true, "dt": true, "fieldset": true, "figcaption": true, "figure": true,
	"footer": true, "form": true, "header": true, "hgroup": true, "hr": true, "li": true, "main": true,
	"nav": true, "ol": true, "p": true, "section": true, "summary": true, "ul": true, "br": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "caption": true, "center": true,
}

type htmlFrame struct {
	tag string
	// Structural hiding is inherited and sticky: display:none and friends hide
	// the whole subtree whatever its descendants say.
	hidden bool
	reason string
	// Colours are inherited too, but contrast is re-evaluated at every
	// element: white text on a black panel inside a white page is readable.
	fg, bg     *rgba
	bgUnknown  bool
	skip       bool
	classes    []string
	id         string
	headingLvl int
}

type cssRule struct {
	tag     string
	classes []string
	id      string
	decls   map[string]string
}

type htmlParser struct {
	lim wire.Limits
	b   *Builder
	w   *warnings

	stack []htmlFrame
	skipN int
	rules []cssRule

	cur []Seg // the paragraph or heading in progress

	pre   []Seg
	inPre int
	table *htmlTable
}

type htmlTable struct {
	depth   int
	rows    []Row
	row     Row
	cell    Cell
	inCell  bool
	allTH   bool
	hasHead bool
	anyRow  bool
}

func parseHTML(data []byte, lim wire.Limits) (*Output, error) {
	var w warnings
	text := decodeText(data, &w)
	p := &htmlParser{lim: lim, b: newBuilder(lim), w: &w}
	p.collectCSS(text)
	z := html.NewTokenizer(strings.NewReader(text))
	z.SetMaxBuf(maxHTMLToken)
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			err := z.Err()
			if errors.Is(err, io.EOF) {
				break
			}
			if errors.Is(err, html.ErrBufferExceeded) {
				return nil, fail(wire.ReasonMalformed, "an HTML token is longer than %d bytes", maxHTMLToken)
			}
			return nil, malformed("html tokenizer: %v", err)
		}
		switch tt {
		case html.TextToken:
			if err := p.text(string(z.Text())); err != nil {
				return nil, err
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			attrs := attrMap{}
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				if _, dup := attrs[string(k)]; !dup {
					attrs[strings.ToLower(string(k))] = string(v)
				}
			}
			if err := p.start(strings.ToLower(string(name)), attrs); err != nil {
				return nil, err
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			if err := p.end(strings.ToLower(string(name))); err != nil {
				return nil, err
			}
		}
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	return p.b.finish(&w), nil
}

// collectCSS reads the rules of every <style> element before the document is
// walked: a rule applies to the whole document, the elements before its
// <style> included. The tokenizer reads a <style> as raw text up to its end
// tag, so its CSS is the text between the two. At most 1 MiB of CSS is read.
// A tokenizer error ends the pass; the main pass reports it.
func (p *htmlParser) collectCSS(text string) {
	z := html.NewTokenizer(strings.NewReader(text))
	z.SetMaxBuf(maxHTMLToken)
	var css *strings.Builder
	size := 0
	for {
		switch z.Next() {
		case html.ErrorToken:
			if css != nil {
				p.addCSS(css.String())
			}
			return
		case html.StartTagToken, html.SelfClosingTagToken:
			if name, _ := z.TagName(); strings.ToLower(string(name)) == "style" {
				css = &strings.Builder{}
			}
		case html.TextToken:
			if t := z.Text(); css != nil {
				if size += len(t); size <= 1<<20 {
					css.Write(t)
				}
			}
		case html.EndTagToken:
			if name, _ := z.TagName(); css != nil && strings.ToLower(string(name)) == "style" {
				p.addCSS(css.String())
				css = nil
			}
		}
	}
}

// top returns the innermost open frame, or a root frame.
func (p *htmlParser) top() htmlFrame {
	if n := len(p.stack); n > 0 {
		return p.stack[n-1]
	}
	return htmlFrame{}
}

func (f htmlFrame) textHidden() (bool, string) {
	if f.hidden {
		return true, f.reason
	}
	fg := f.fg
	if fg == nil {
		return false, "" // default colour is black on white
	}
	if fg.a <= 0.05 {
		return true, wire.HideTransparent // whatever is behind it
	}
	if f.bgUnknown {
		return false, ""
	}
	bg := f.bg
	if bg == nil {
		bg = white
	}
	if r := contrastReason(fg, bg); r != "" {
		return true, r
	}
	return false, ""
}

func (p *htmlParser) text(s string) error {
	if p.skipN > 0 {
		return nil
	}
	f := p.top()
	hidden, reason := f.textHidden()
	if p.inPre == 0 {
		// HTML collapses white space; keep one space at each end so words of
		// neighbouring inline elements do not run together.
		lead := s != "" && isSpace(s[0])
		trail := s != "" && isSpace(s[len(s)-1])
		s = collapse(s)
		if s == "" {
			if lead {
				s = " "
			} else {
				return nil
			}
		} else {
			if lead {
				s = " " + s
			}
			if trail {
				s += " "
			}
		}
	}
	seg := Seg{Text: s, Hidden: hidden, Reason: reason}
	switch {
	case p.inPre > 0:
		p.pre = append(p.pre, seg)
	case p.table != nil && p.table.depth > 0 && !p.inCaption():
		if p.table.inCell {
			p.table.cell = append(p.table.cell, seg)
		}
	default:
		p.cur = append(p.cur, seg)
	}
	return nil
}

func (p *htmlParser) inCaption() bool {
	for i := len(p.stack) - 1; i >= 0; i-- {
		switch p.stack[i].tag {
		case "caption":
			return true
		case "table":
			return false
		}
	}
	return false
}

func (p *htmlParser) flush() error {
	if len(p.cur) == 0 {
		return nil
	}
	segs := p.cur
	p.cur = nil
	// A heading is flushed by its own end tag; this path is for paragraphs.
	return p.b.Paragraph(segs)
}

// pop closes the innermost frame.
func (p *htmlParser) pop() error {
	n := len(p.stack)
	f := p.stack[n-1]
	p.stack = p.stack[:n-1]
	if f.skip {
		p.skipN--
	}
	switch {
	case f.headingLvl > 0:
		segs := p.cur
		p.cur = nil
		return p.b.Heading(f.headingLvl, segs)
	case f.tag == "pre":
		p.inPre--
		segs := p.pre
		p.pre = nil
		return p.b.Code(segs)
	case f.tag == "td" || f.tag == "th":
		if p.table != nil && p.table.depth == 1 && p.table.inCell {
			p.table.row = append(p.table.row, p.table.cell)
			p.table.cell, p.table.inCell = nil, false
		}
	case f.tag == "tr":
		if p.table != nil && p.table.depth == 1 {
			p.endRow()
		}
	case f.tag == "table":
		return p.endTable()
	}
	if htmlBlock[f.tag] {
		return p.endBlock()
	}
	return nil
}

func (p *htmlParser) endBlock() error {
	if p.table != nil && p.table.depth > 0 && !p.inCaption() {
		if p.table.inCell {
			p.table.cell = append(p.table.cell, Seg{Text: " "})
		}
		return nil
	}
	return p.flush()
}

func (p *htmlParser) endRow() {
	t := p.table
	if t.inCell {
		t.row = append(t.row, t.cell)
		t.cell, t.inCell = nil, false
	}
	if len(t.row) > 0 {
		if !t.anyRow {
			t.hasHead = t.allTH
		}
		t.anyRow = true
		t.rows = append(t.rows, t.row)
	}
	t.row, t.allTH = nil, true
}

func (p *htmlParser) endTable() error {
	t := p.table
	if t == nil {
		return nil
	}
	t.depth--
	if t.depth > 0 {
		return nil
	}
	if len(t.row) > 0 || t.inCell {
		p.endRow()
	}
	p.table = nil
	return p.b.Table(t.rows, t.hasHead)
}

// closeImplied pops elements the HTML parser would close on seeing a new open
// tag, so that unclosed paragraphs and list items do not pile up as nesting.
func (p *htmlParser) closeImplied(tag string) error {
	closes := func(open string) bool {
		switch tag {
		case "li":
			return open == "li"
		case "dt", "dd":
			return open == "dt" || open == "dd"
		case "td", "th":
			return open == "td" || open == "th"
		case "tr":
			return open == "td" || open == "th" || open == "tr"
		case "body":
			return open == "head"
		case "thead", "tbody", "tfoot":
			return open == "thead" || open == "tbody" || open == "tfoot" || open == "tr" || open == "td" || open == "th"
		}
		if open == "p" {
			return htmlBlock[tag] || tag == "table" || tag == "pre"
		}
		return false
	}
	boundary := func(open string) bool {
		switch open {
		case "table", "ul", "ol", "dl", "td", "th", "caption", "button", "html", "body", "div", "section", "article", "blockquote", "li", "dd", "form":
			return true
		}
		return false
	}
	// Find the nearest open element this tag closes, not crossing a boundary
	// (the boundary element itself may be the one that is closed: a new <li>
	// closes an open <li>, and <li> is also a boundary for <p>).
	for i := len(p.stack) - 1; i >= 0; i-- {
		open := p.stack[i].tag
		if closes(open) {
			for len(p.stack) > i {
				if err := p.pop(); err != nil {
					return err
				}
			}
			return nil
		}
		if boundary(open) {
			return nil
		}
	}
	return nil
}

func (p *htmlParser) start(tag string, attrs attrMap) error {
	if tag == "" {
		return nil
	}
	if err := p.closeImplied(tag); err != nil {
		return err
	}
	if htmlBlock[tag] {
		if err := p.startBlock(); err != nil {
			return err
		}
	}
	if htmlVoid[tag] {
		return nil
	}
	if len(p.stack) >= p.lim.MaxDepth {
		return fail(wire.ReasonNestingTooDeep, "HTML nesting is deeper than %d", p.lim.MaxDepth)
	}
	parent := p.top()
	f := htmlFrame{tag: tag, hidden: parent.hidden, reason: parent.reason, fg: parent.fg, bg: parent.bg, bgUnknown: parent.bgUnknown}
	if cls := strings.Fields(attrs["class"]); len(cls) > 0 {
		f.classes = cls
	}
	f.id = attrs["id"]
	if htmlSkip[tag] {
		f.skip = true
		p.skipN++
	}
	if !f.hidden {
		if r := hiddenByFlagAttrs(attrs); r != "" {
			f.hidden, f.reason = true, r
		}
	}
	// The legacy colour attributes, which any CSS below overrides.
	switch tag {
	case "font":
		if c := legacyColour(attrs["color"]); c != nil {
			f.fg = c
		}
	case "body", "table", "thead", "tbody", "tfoot", "tr", "td", "th":
		if attrs["background"] != "" { // an image
			f.bgUnknown = true
		} else if c := legacyColour(attrs["bgcolor"]); c != nil && c.a > 0.05 {
			f.bg, f.bgUnknown = c, false
		}
	}
	// CSS from <style> rules, then the inline style on top.
	decls := p.matchRules(tag, f.classes, f.id)
	if st, ok := attrs["style"]; ok {
		if decls == nil {
			decls = map[string]string{}
		}
		for k, v := range parseDecls(st) {
			decls[k] = v
		}
	}
	if len(decls) > 0 {
		if !f.hidden {
			if r := declsHidden(decls); r != "" {
				f.hidden, f.reason = true, r
			}
		}
		if c := parseColor(decls["color"]); c != nil {
			f.fg = c
		}
		bgv := firstNonEmpty(decls["background-color"], decls["background"])
		if bgv != "" {
			switch c, unknown := backgroundColour(bgv); {
			case unknown:
				f.bgUnknown = true
			case c != nil && c.a > 0.05:
				f.bg, f.bgUnknown = c, false
			}
		}
	}
	switch tag {
	case "table":
		if p.table == nil {
			p.table = &htmlTable{allTH: true}
		}
		p.table.depth++
	case "tr":
		if p.table != nil && p.table.depth == 1 {
			p.table.allTH = true
		}
	case "td", "th":
		if p.table != nil && p.table.depth == 1 {
			if tag == "td" {
				p.table.allTH = false
			}
			p.table.inCell = true
			p.table.cell = nil
		}
	case "pre":
		p.inPre++
	case "h1", "h2", "h3", "h4", "h5", "h6":
		f.headingLvl = int(tag[1] - '0')
	}
	p.stack = append(p.stack, f)
	return nil
}

func (p *htmlParser) startBlock() error {
	if p.table != nil && p.table.depth > 0 && !p.inCaption() {
		if p.table.inCell {
			p.table.cell = append(p.table.cell, Seg{Text: " "})
		}
		return nil
	}
	// A heading in progress is closed by its own end tag, but a block that
	// starts inside one ends the run of text first.
	return p.flush()
}

func (p *htmlParser) end(tag string) error {
	if tag == "" || htmlVoid[tag] {
		if tag == "br" {
			return p.startBlock()
		}
		return nil
	}
	for i := len(p.stack) - 1; i >= 0; i-- {
		if p.stack[i].tag == tag {
			for len(p.stack) > i {
				if err := p.pop(); err != nil {
					return err
				}
			}
			return nil
		}
	}
	return nil // an end tag with no open element is ignored, as browsers do
}

func (p *htmlParser) finish() error {
	for len(p.stack) > 0 {
		if err := p.pop(); err != nil {
			return err
		}
	}
	if p.table != nil {
		p.table.depth = 1
		if err := p.endTable(); err != nil {
			return err
		}
	}
	return p.flush()
}

// backgroundColour extracts the colour from a background or background-color
// value. unknown is true when the background is an image or gradient (or
// anything else that is not provably a flat colour).
func backgroundColour(v string) (c *rgba, unknown bool) {
	if strings.Contains(v, "url(") || strings.Contains(v, "gradient(") {
		return nil, true
	}
	for _, tok := range splitCSSTokens(v) {
		if c := parseColor(tok); c != nil {
			return c, false
		}
	}
	return nil, false
}

// legacyColour parses the colour of a presentational attribute (<font color>,
// bgcolor): a CSS colour, else hex digits as HTML's rules for legacy colour
// values read them, which browsers apply without a "#": "ffffff" is white and
// "fff" is three one-digit components, nearly black. Anything else (an
// unknown name) is nil, as for CSS.
func legacyColour(v string) *rgba {
	if c := parseColor(v); c != nil {
		return c
	}
	h := strings.TrimSpace(v)
	if len(h) > 128 {
		h = h[:128]
	}
	h = strings.TrimPrefix(h, "#")
	if h == "" {
		return nil
	}
	for i := 0; i < len(h); i++ {
		if _, ok := hexVal(h[i]); !ok {
			return nil
		}
	}
	for len(h)%3 != 0 {
		h += "0"
	}
	n := len(h) / 3
	comp := [3]string{h[:n], h[n : 2*n], h[2*n:]}
	if n > 8 {
		for i := range comp {
			comp[i] = comp[i][n-8:]
		}
		n = 8
	}
	for n > 2 && comp[0][0] == '0' && comp[1][0] == '0' && comp[2][0] == '0' {
		for i := range comp {
			comp[i] = comp[i][1:]
		}
		n--
	}
	var rgb [3]uint8
	for i, s := range comp {
		for j := 0; j < min(n, 2); j++ {
			d, _ := hexVal(s[j])
			rgb[i] = rgb[i]<<4 | d
		}
	}
	return &rgba{rgb[0], rgb[1], rgb[2], 1}
}

// splitCSSTokens splits a CSS value at spaces that are not inside parentheses.
func splitCSSTokens(v string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ' ':
			if depth == 0 {
				if i > start {
					out = append(out, v[start:i])
				}
				start = i + 1
			}
		}
	}
	if start < len(v) {
		out = append(out, v[start:])
	}
	return out
}

// addCSS reads the simple rules of a <style> block: selectors made of a tag,
// classes and an id, no combinators or pseudo-classes. Rules inside at-rules
// are skipped. This catches the usual ".hidden { display: none }" and
// ".sr-only"-style hiding; the cascade is not modelled, so a hiding rule that a
// more specific rule undoes still hides: the failure is toward flagging.
func (p *htmlParser) addCSS(css string) {
	css = stripCSSComments(css)
	for len(css) > 0 && len(p.rules) < maxCSSRules {
		open := strings.IndexByte(css, '{')
		if open < 0 {
			return
		}
		sel := strings.TrimSpace(css[:open])
		depth, end := 1, open+1
		for end < len(css) && depth > 0 {
			switch css[end] {
			case '{':
				depth++
			case '}':
				depth--
			}
			end++
		}
		body := css[open+1 : max(open+1, end-1)]
		css = css[min(end, len(css)):]
		if strings.HasPrefix(sel, "@") {
			continue
		}
		decls := parseDecls(body)
		if len(decls) == 0 {
			continue
		}
		for _, one := range strings.Split(sel, ",") {
			if r, ok := parseSimpleSelector(strings.TrimSpace(one)); ok {
				r.decls = decls
				p.rules = append(p.rules, r)
			}
		}
	}
}

// stripCSSComments replaces each comment with a space, in one pass.
func stripCSSComments(s string) string {
	var sb strings.Builder
	for {
		i := strings.Index(s, "/*")
		if i < 0 {
			sb.WriteString(s)
			return sb.String()
		}
		sb.WriteString(s[:i])
		j := strings.Index(s[i+2:], "*/")
		if j < 0 {
			return sb.String()
		}
		sb.WriteByte(' ')
		s = s[i+2+j+2:]
	}
}

func parseSimpleSelector(s string) (cssRule, bool) {
	var r cssRule
	if s == "" || strings.ContainsAny(s, " >+~[:*()") {
		return r, false
	}
	i := 0
	for i < len(s) && s[i] != '.' && s[i] != '#' {
		i++
	}
	r.tag = strings.ToLower(s[:i])
	for i < len(s) {
		kind := s[i]
		j := i + 1
		for j < len(s) && s[j] != '.' && s[j] != '#' {
			j++
		}
		name := s[i+1 : j]
		if name == "" {
			return r, false
		}
		if kind == '.' {
			r.classes = append(r.classes, name)
		} else {
			r.id = name
		}
		i = j
	}
	return r, r.tag != "" || len(r.classes) > 0 || r.id != ""
}

// matchRules merges the declarations of every rule that matches the element,
// in source order.
func (p *htmlParser) matchRules(tag string, classes []string, id string) map[string]string {
	if len(p.rules) == 0 {
		return nil
	}
	var out map[string]string
	for _, r := range p.rules {
		if r.tag != "" && r.tag != tag {
			continue
		}
		if r.id != "" && r.id != id {
			continue
		}
		ok := true
		for _, c := range r.classes {
			found := false
			for _, ec := range classes {
				if ec == c {
					found = true
					break
				}
			}
			if !found {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		for k, v := range r.decls {
			out[k] = v
		}
	}
	return out
}
