package formats

import (
	"errors"
	"math"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// maxOperators bounds the content-stream operators executed in one document.
const maxOperators = 50_000_000

// maxBackgrounds bounds the painted regions remembered per page for the
// background-colour test.
const maxBackgrounds = 512

type matrix [6]float64

var identity = matrix{1, 0, 0, 1, 0, 0}

// mul is a x b: apply a, then b (PDF row-vector convention).
func (a matrix) mul(b matrix) matrix {
	return matrix{
		a[0]*b[0] + a[1]*b[2], a[0]*b[1] + a[1]*b[3],
		a[2]*b[0] + a[3]*b[2], a[2]*b[1] + a[3]*b[3],
		a[4]*b[0] + a[5]*b[2] + b[4], a[4]*b[1] + a[5]*b[3] + b[5],
	}
}

func (a matrix) apply(x, y float64) (float64, float64) {
	return a[0]*x + a[2]*y + a[4], a[1]*x + a[3]*y + a[5]
}

type fillColour struct {
	rgb     *rgba // nil when unknown (a pattern, a Separation space, ...)
	space   string
	ncomp   int // components of the current colour space, 0 when unknown
	unknown bool
}

type gstate struct {
	ctm        matrix
	fill       fillColour
	stroke     fillColour
	alpha      float64
	font       *pdfFont
	size       float64
	tc, tw, th float64
	tl, rise   float64
	render     int
}

// run is one shown string with where it landed on the page.
type run struct {
	text           string
	x0, y0, x1, y1 float64
	size           float64
	width          float64 // the horizontal glyph scale, Tz included
	hidden         bool
	reason         string
}

type bgRect struct {
	x0, y0, x1, y1 float64
	rgb            *rgba // nil: painted with something unknown (an image, a shading)
}

type pageCtx struct {
	f        *pdfFile
	runs     []run
	runBytes int
	bgs      []bgRect
	overflow *bgRect
	hasImage bool
	box      [4]float64
	ocOff    map[int]bool
	ops      *int
	fonts    map[int]*pdfFont
	hideOC   int
	mc       []bool
	visiting map[int]bool
	maxRun   int
	// path being built, for painting
	pathMin, pathMax [2]float64
	pathHas          bool
}

func (p *pageCtx) fontOf(res pdfDict, name pdfName) *pdfFont {
	fonts := p.f.dict(res["Font"])
	if fonts == nil {
		return nil
	}
	raw := fonts[string(name)]
	if r, ok := raw.(pdfRef); ok {
		if fo, ok := p.fonts[r.num]; ok {
			return fo
		}
		fo := p.f.loadFont(p.f.dict(r))
		p.fonts[r.num] = fo
		return fo
	}
	return p.f.loadFont(p.f.dict(raw))
}

// exec runs one content stream. res is the resource dictionary in force.
func (p *pageCtx) exec(content []byte, res pdfDict, gs gstate, depth int) error {
	l := &lexer{b: content, maxDepth: p.f.lim.MaxDepth}
	var (
		stack []gstate
		tm    = identity
		tlm   = identity
		ops   []any
	)
	for {
		if l.eof() {
			return nil
		}
		o, err := l.readObject(0)
		if err != nil {
			var fe *Error
			if errors.As(err, &fe) {
				return err
			}
			continue
		}
		kw, isKw := o.(pdfKeyword)
		if !isKw {
			if len(ops) >= 64 {
				ops = ops[1:]
			}
			ops = append(ops, o)
			continue
		}
		if *p.ops++; *p.ops > maxOperators {
			return fail(wire.ReasonMalformed, "PDF content is too complex")
		}
		num := func(i int) float64 {
			if i >= 0 && i < len(ops) {
				switch t := ops[i].(type) {
				case int64:
					return float64(t)
				case float64:
					return t
				}
			}
			return 0
		}
		nargs := len(ops)
		switch kw {
		case "q":
			if len(stack) < 256 {
				stack = append(stack, gs)
			}
		case "Q":
			if n := len(stack); n > 0 {
				gs = stack[n-1]
				stack = stack[:n-1]
			}
		case "cm":
			if nargs >= 6 {
				gs.ctm = matrix{num(nargs - 6), num(nargs - 5), num(nargs - 4), num(nargs - 3), num(nargs - 2), num(nargs - 1)}.mul(gs.ctm)
			}
		case "gs":
			if nargs >= 1 {
				if n, ok := ops[nargs-1].(pdfName); ok {
					if eg := p.f.dict(p.f.dict(res["ExtGState"])[string(n)]); eg != nil {
						if a, ok := p.f.number(eg["ca"]); ok {
							gs.alpha = a
						}
					}
				}
			}
		case "g":
			gs.fill = fillColour{rgb: gray(num(nargs - 1)), space: "DeviceGray", ncomp: 1}
		case "rg":
			gs.fill = fillColour{rgb: rgbOf(num(nargs-3), num(nargs-2), num(nargs-1)), space: "DeviceRGB", ncomp: 3}
		case "k":
			gs.fill = fillColour{rgb: cmyk(num(nargs-4), num(nargs-3), num(nargs-2), num(nargs-1)), space: "DeviceCMYK", ncomp: 4}
		case "cs":
			if nargs >= 1 {
				if n, ok := ops[nargs-1].(pdfName); ok {
					gs.fill = p.colourSpace(res, string(n))
				}
			}
		case "sc", "scn":
			gs.fill = setColour(gs.fill, ops)
		case "G":
			gs.stroke = fillColour{rgb: gray(num(nargs - 1)), space: "DeviceGray", ncomp: 1}
		case "RG":
			gs.stroke = fillColour{rgb: rgbOf(num(nargs-3), num(nargs-2), num(nargs-1)), space: "DeviceRGB", ncomp: 3}
		case "K":
			gs.stroke = fillColour{rgb: cmyk(num(nargs-4), num(nargs-3), num(nargs-2), num(nargs-1)), space: "DeviceCMYK", ncomp: 4}
		case "CS":
			if nargs >= 1 {
				if n, ok := ops[nargs-1].(pdfName); ok {
					gs.stroke = p.colourSpace(res, string(n))
				}
			}
		case "SC", "SCN":
			gs.stroke = setColour(gs.stroke, ops)
		case "BT":
			tm, tlm = identity, identity
		case "ET":
		case "Tf":
			if nargs >= 2 {
				if n, ok := ops[nargs-2].(pdfName); ok {
					gs.font = p.fontOf(res, n)
				}
				gs.size = num(nargs - 1)
			}
		case "Tc":
			gs.tc = num(nargs - 1)
		case "Tw":
			gs.tw = num(nargs - 1)
		case "Tz":
			gs.th = num(nargs-1) / 100
		case "TL":
			gs.tl = num(nargs - 1)
		case "Ts":
			gs.rise = num(nargs - 1)
		case "Tr":
			gs.render = int(num(nargs - 1))
		case "Td", "TD":
			if nargs >= 2 {
				tx, ty := num(nargs-2), num(nargs-1)
				if kw == "TD" {
					gs.tl = -ty
				}
				tlm = matrix{1, 0, 0, 1, tx, ty}.mul(tlm)
				tm = tlm
			}
		case "T*":
			tlm = matrix{1, 0, 0, 1, 0, -gs.tl}.mul(tlm)
			tm = tlm
		case "Tm":
			if nargs >= 6 {
				tm = matrix{num(nargs - 6), num(nargs - 5), num(nargs - 4), num(nargs - 3), num(nargs - 2), num(nargs - 1)}
				tlm = tm
			}
		case "Tj":
			if nargs >= 1 {
				if s, ok := ops[nargs-1].(pdfStr); ok {
					if err := p.show(&gs, string(s), &tm); err != nil {
						return err
					}
				}
			}
		case "'", "\"":
			tlm = matrix{1, 0, 0, 1, 0, -gs.tl}.mul(tlm)
			tm = tlm
			if kw == "\"" && nargs >= 3 {
				gs.tw, gs.tc = num(nargs-3), num(nargs-2)
			}
			if nargs >= 1 {
				if s, ok := ops[nargs-1].(pdfStr); ok {
					if err := p.show(&gs, string(s), &tm); err != nil {
						return err
					}
				}
			}
		case "TJ":
			if nargs >= 1 {
				if arr, ok := ops[nargs-1].(pdfArray); ok {
					for _, it := range arr {
						switch t := it.(type) {
						case pdfStr:
							if err := p.show(&gs, string(t), &tm); err != nil {
								return err
							}
						case int64:
							tm = matrix{1, 0, 0, 1, -float64(t) / 1000 * gs.size * gs.th, 0}.mul(tm)
						case float64:
							tm = matrix{1, 0, 0, 1, -t / 1000 * gs.size * gs.th, 0}.mul(tm)
						}
					}
				}
			}
		case "m", "l":
			if nargs >= 2 {
				p.pathPoint(gs.ctm, num(nargs-2), num(nargs-1))
			}
		case "c":
			if nargs >= 6 {
				for i := 6; i >= 2; i -= 2 {
					p.pathPoint(gs.ctm, num(nargs-i), num(nargs-i+1))
				}
			}
		case "v", "y":
			if nargs >= 4 {
				p.pathPoint(gs.ctm, num(nargs-4), num(nargs-3))
				p.pathPoint(gs.ctm, num(nargs-2), num(nargs-1))
			}
		case "re":
			if nargs >= 4 {
				x, y, w, h := num(nargs-4), num(nargs-3), num(nargs-2), num(nargs-1)
				p.pathPoint(gs.ctm, x, y)
				p.pathPoint(gs.ctm, x+w, y+h)
			}
		case "f", "F", "f*", "B", "B*", "b", "b*":
			p.paint(gs.fill)
		case "S", "s", "n":
			p.pathHas = false
		case "sh":
			p.addBackground(bgRect{x0: p.box[0], y0: p.box[1], x1: p.box[2], y1: p.box[3]})
		case "BDC":
			hide := false
			if nargs >= 2 {
				if tag, ok := ops[nargs-2].(pdfName); ok && tag == "OC" {
					if pn, ok := ops[nargs-1].(pdfName); ok {
						if ref, ok := p.f.dict(res["Properties"])[string(pn)].(pdfRef); ok && p.ocOff[ref.num] {
							hide = true
						}
					}
				}
			}
			p.pushMC(hide)
		case "BMC":
			p.pushMC(false)
		case "EMC":
			if n := len(p.mc); n > 0 {
				if p.mc[n-1] {
					p.hideOC--
				}
				p.mc = p.mc[:n-1]
			}
		case "Do":
			if nargs >= 1 {
				if n, ok := ops[nargs-1].(pdfName); ok {
					if err := p.doXObject(res, n, gs, depth); err != nil {
						return err
					}
				}
			}
		case "BI":
			if !p.inlineImage(l, gs) {
				return nil
			}
		}
		ops = ops[:0]
	}
}

func (p *pageCtx) pushMC(hide bool) {
	if len(p.mc) >= 256 {
		return
	}
	p.mc = append(p.mc, hide)
	if hide {
		p.hideOC++
	}
}

// ---- colours ----

func gray(v float64) *rgba {
	c := uint8(math.Max(0, math.Min(1, v)) * 255)
	return &rgba{c, c, c, 1}
}

func rgbOf(r, g, b float64) *rgba {
	cl := func(v float64) uint8 { return uint8(math.Max(0, math.Min(1, v)) * 255) }
	return &rgba{cl(r), cl(g), cl(b), 1}
}

func cmyk(c, m, y, k float64) *rgba {
	cl := func(v float64) float64 { return math.Max(0, math.Min(1, v)) }
	c, m, y, k = cl(c), cl(m), cl(y), cl(k)
	return rgbOf((1-c)*(1-k), (1-m)*(1-k), (1-y)*(1-k))
}

// colourSpace resolves a colour space name to what sc/scn will mean.
func (p *pageCtx) colourSpace(res pdfDict, name string) fillColour {
	switch name {
	case "DeviceGray", "G", "CalGray":
		return fillColour{rgb: gray(0), space: "DeviceGray", ncomp: 1}
	case "DeviceRGB", "RGB", "CalRGB":
		return fillColour{rgb: rgbOf(0, 0, 0), space: "DeviceRGB", ncomp: 3}
	case "DeviceCMYK", "CMYK":
		return fillColour{rgb: cmyk(0, 0, 0, 1), space: "DeviceCMYK", ncomp: 4}
	}
	cs := p.f.resolve(p.f.dict(res["ColorSpace"])[name])
	if arr, ok := cs.(pdfArray); ok && len(arr) >= 2 && nameOf(p.f.resolve(arr[0])) == "ICCBased" {
		if st := p.f.stream(arr[1]); st != nil {
			if n, ok := p.f.number(st.dict["N"]); ok {
				switch int(n) {
				case 1:
					return fillColour{rgb: gray(0), space: "DeviceGray", ncomp: 1}
				case 3:
					return fillColour{rgb: rgbOf(0, 0, 0), space: "DeviceRGB", ncomp: 3}
				case 4:
					return fillColour{rgb: cmyk(0, 0, 0, 1), space: "DeviceCMYK", ncomp: 4}
				}
			}
		}
	}
	return fillColour{unknown: true, space: name}
}

func setColour(cur fillColour, ops []any) fillColour {
	if cur.unknown || cur.ncomp == 0 {
		return fillColour{unknown: true, space: cur.space}
	}
	var v []float64
	for _, o := range ops {
		switch t := o.(type) {
		case int64:
			v = append(v, float64(t))
		case float64:
			v = append(v, t)
		case pdfName:
			return fillColour{unknown: true, space: cur.space} // a pattern
		}
	}
	if len(v) < cur.ncomp {
		return fillColour{unknown: true, space: cur.space}
	}
	v = v[len(v)-cur.ncomp:]
	out := cur
	switch cur.ncomp {
	case 1:
		out.rgb = gray(v[0])
	case 3:
		out.rgb = rgbOf(v[0], v[1], v[2])
	case 4:
		out.rgb = cmyk(v[0], v[1], v[2], v[3])
	}
	return out
}

// ---- painting ----

func (p *pageCtx) pathPoint(ctm matrix, x, y float64) {
	dx, dy := ctm.apply(x, y)
	if !p.pathHas {
		p.pathMin, p.pathMax, p.pathHas = [2]float64{dx, dy}, [2]float64{dx, dy}, true
		return
	}
	p.pathMin[0], p.pathMin[1] = math.Min(p.pathMin[0], dx), math.Min(p.pathMin[1], dy)
	p.pathMax[0], p.pathMax[1] = math.Max(p.pathMax[0], dx), math.Max(p.pathMax[1], dy)
}

func (p *pageCtx) paint(fill fillColour) {
	if !p.pathHas {
		return
	}
	r := bgRect{x0: p.pathMin[0], y0: p.pathMin[1], x1: p.pathMax[0], y1: p.pathMax[1]}
	if !fill.unknown {
		r.rgb = fill.rgb
	}
	p.addBackground(r)
	p.pathHas = false
}

// addBackground remembers a painted region. Past the cap, further regions are
// folded into one bounding box with unknown colour: text inside it is not
// judged, which is the cost of the cap, stated in the docs.
func (p *pageCtx) addBackground(r bgRect) {
	if len(p.bgs) < maxBackgrounds {
		p.bgs = append(p.bgs, r)
		return
	}
	if p.overflow == nil {
		o := r
		o.rgb = nil
		p.overflow = &o
		return
	}
	p.overflow.x0, p.overflow.y0 = math.Min(p.overflow.x0, r.x0), math.Min(p.overflow.y0, r.y0)
	p.overflow.x1, p.overflow.y1 = math.Max(p.overflow.x1, r.x1), math.Max(p.overflow.y1, r.y1)
}

// backgroundFor is the colour the page shows behind a text run, judged at the
// run's baseline from its start to its end. The topmost painted region that
// contains both ends decides. A region that contains only one end (an icon
// beside a word, the edge of a panel) makes the answer unknown, because
// part of the run is on it and part is not; known is also false when the
// deciding region is an image or shading.
func (p *pageCtx) backgroundFor(xa, xb, y float64) (*rgba, bool) {
	if xa > xb {
		xa, xb = xb, xa
	}
	in := func(r bgRect, x float64) bool { return x >= r.x0 && x <= r.x1 && y >= r.y0 && y <= r.y1 }
	if o := p.overflow; o != nil && (in(*o, xa) || in(*o, xb)) {
		return nil, false
	}
	partial := false
	for i := len(p.bgs) - 1; i >= 0; i-- {
		r := p.bgs[i]
		a, b := in(r, xa), in(r, xb)
		switch {
		case a && b:
			if partial {
				return nil, false
			}
			return r.rgb, r.rgb != nil
		case a || b:
			partial = true
		}
	}
	if partial {
		return nil, false
	}
	return white, true
}

// ---- text ----

func (p *pageCtx) show(gs *gstate, s string, tm *matrix) error {
	font := gs.font
	if font == nil {
		font = &pdfFont{std: &helveticaWidths, defWidth: 0.5}
	}
	glyphs := font.decode(s)
	m := matrix{gs.size * gs.th, 0, 0, gs.size, 0, gs.rise}.mul(*tm).mul(gs.ctm)
	x0, y0 := m[4], m[5]
	var sb []byte
	for _, g := range glyphs {
		sb = append(sb, g.text...)
		tx := g.width*gs.size + gs.tc
		if g.space {
			tx += gs.tw
		}
		*tm = matrix{1, 0, 0, 1, tx * gs.th, 0}.mul(*tm)
	}
	end := matrix{1, 0, 0, 1, 0, 0}.mul(*tm).mul(gs.ctm)
	if len(sb) == 0 {
		return nil
	}
	size := gs.size * math.Hypot(tm[2]*gs.ctm[0]+tm[3]*gs.ctm[2], tm[2]*gs.ctm[1]+tm[3]*gs.ctm[3])
	width := gs.size * gs.th * math.Hypot(tm[0]*gs.ctm[0]+tm[1]*gs.ctm[2], tm[0]*gs.ctm[1]+tm[1]*gs.ctm[3])
	r := run{text: string(sb), x0: x0, y0: y0, x1: end[4], y1: end[5], size: size, width: width}
	r.hidden, r.reason = p.hiddenRun(gs, r)
	if p.runBytes += len(sb) + 64; int64(p.runBytes) > int64(p.maxRun) {
		return fail(wire.ReasonOutputTooLarge, "extracted text passes the limit of %d bytes", p.maxRun/4)
	}
	p.runs = append(p.runs, r)
	return nil
}

// hiddenRun applies the PDF hidden-text rules to one shown string.
func (p *pageCtx) hiddenRun(gs *gstate, r run) (bool, string) {
	switch {
	case gs.render == 3 || gs.render == 7:
		return true, wire.HideInvisibleRender
	case p.hideOC > 0:
		return true, wire.HideOptionalContent
	case gs.alpha <= 0.05:
		return true, wire.HideTransparent
	case math.Abs(r.size) < tinyFontPoints || math.Abs(r.width) < tinyFontPoints:
		return true, wire.HideTinyFont // zero included: a 0 Tf, a 0 Tz, a degenerate matrix
	}
	const margin = 20.0
	if r.x0 < p.box[0]-margin || r.x0 > p.box[2]+margin || r.y0 < p.box[1]-margin || r.y0 > p.box[3]+margin {
		return true, wire.HideOutsidePage
	}
	var colour *rgba
	switch gs.render {
	case 0, 2, 4, 6:
		colour = gs.fill.rgb
	case 1, 5: // stroked, not filled
		colour = gs.stroke.rgb
	}
	if colour != nil {
		if bg, known := p.backgroundFor(r.x0, r.x1, r.y0+r.size*0.3); known {
			if why := contrastReason(colour, bg); why != "" {
				return true, why
			}
		}
	}
	return false, ""
}

// ---- XObjects and inline images ----

func (p *pageCtx) doXObject(res pdfDict, name pdfName, gs gstate, depth int) error {
	raw := p.f.dict(res["XObject"])[string(name)]
	st := p.f.stream(raw)
	if st == nil {
		return nil
	}
	switch nameOf(p.f.resolve(st.dict["Subtype"])) {
	case "Image":
		p.hasImage = true
		x0, y0 := gs.ctm.apply(0, 0)
		x1, y1 := gs.ctm.apply(1, 1)
		p.addBackground(bgRect{x0: math.Min(x0, x1), y0: math.Min(y0, y1), x1: math.Max(x0, x1), y1: math.Max(y0, y1)})
	case "Form":
		if depth >= min(p.f.lim.MaxDepth, 12) {
			return fail(wire.ReasonNestingTooDeep, "PDF form XObjects are nested deeper than %d", min(p.f.lim.MaxDepth, 12))
		}
		if r, ok := raw.(pdfRef); ok {
			if p.visiting[r.num] {
				return nil // a form that draws itself
			}
			p.visiting[r.num] = true
			defer delete(p.visiting, r.num)
		}
		content, err := p.f.decode(st)
		if err != nil {
			if errors.Is(err, errUnsupportedFilter) {
				return nil
			}
			return err
		}
		sub := gs
		if m := p.f.floats(st.dict["Matrix"], 6); m != nil {
			sub.ctm = matrix{m[0], m[1], m[2], m[3], m[4], m[5]}.mul(gs.ctm)
		}
		subRes := res
		if r := p.f.dict(st.dict["Resources"]); r != nil {
			subRes = r
		}
		savedPath, savedHas := p.pathMin, p.pathHas
		p.pathHas = false
		err = p.exec(content, subRes, sub, depth+1)
		p.pathMin, p.pathHas = savedPath, savedHas
		return err
	}
	return nil
}

// inlineImage skips an inline image (BI ... ID data EI) the lexer has just
// seen the BI of. It records that the page has an image. It returns false when
// the content stream ends inside it.
func (p *pageCtx) inlineImage(l *lexer, gs gstate) bool {
	// the dictionary: key/value pairs up to ID
	for {
		o, err := l.readObject(0)
		if err != nil {
			if l.pos >= len(l.b) {
				return false
			}
			continue
		}
		if k, ok := o.(pdfKeyword); ok && k == "ID" {
			break
		}
		if l.pos >= len(l.b) {
			return false
		}
	}
	p.hasImage = true
	if l.pos < len(l.b) && isPDFSpace(l.b[l.pos]) {
		l.pos++
	}
	for i := l.pos; i+1 < len(l.b); i++ {
		if l.b[i] == 'E' && l.b[i+1] == 'I' && (i == l.pos || isPDFSpace(l.b[i-1])) &&
			(i+2 >= len(l.b) || isPDFSpace(l.b[i+2])) {
			l.pos = i + 2
			x0, y0 := gs.ctm.apply(0, 0)
			x1, y1 := gs.ctm.apply(1, 1)
			p.addBackground(bgRect{x0: math.Min(x0, x1), y0: math.Min(y0, y1), x1: math.Max(x0, x1), y1: math.Max(y0, y1)})
			return true
		}
	}
	l.pos = len(l.b)
	return false
}
