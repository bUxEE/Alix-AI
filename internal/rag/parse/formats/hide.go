package formats

import (
	"math"
	"strconv"
	"strings"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// tinyFontPoints is the size under which text is treated as hidden: nobody
// reads 1 point text, and it is a standard way to smuggle instructions to a
// model. 2 pt is a deliberate round number far below any real text.
const tinyFontPoints = 2.0

// minContrast is the WCAG contrast ratio under which foreground and background
// are treated as the same colour. 1.1:1 is white against #f2f2f2 or closer;
// light grey text a person can read (#ccc on white is 1.6:1) is not flagged.
const minContrast = 1.1

// offscreenPx is how far off the page a positioned box or an indent must be to
// count as hidden (negative, in CSS pixels).
const offscreenPx = -900.0

// attrMap holds the attributes of one tag, names lower-cased.
type attrMap map[string]string

// parseAttrs parses the attribute text of a tag: name="v", name='v', name=v and
// bare names. It is deliberately forgiving and linear.
func parseAttrs(s string) attrMap {
	a := attrMap{}
	i := 0
	for i < len(s) {
		for i < len(s) && (isSpace(s[i]) || s[i] == '/') {
			i++
		}
		j := i
		for j < len(s) && !isSpace(s[j]) && s[j] != '=' && s[j] != '/' && s[j] != '>' {
			j++
		}
		if j == i {
			i++
			continue
		}
		name := strings.ToLower(s[i:j])
		i = j
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		val := ""
		if i < len(s) && s[i] == '=' {
			i++
			for i < len(s) && isSpace(s[i]) {
				i++
			}
			if i < len(s) && (s[i] == '"' || s[i] == '\'') {
				q := s[i]
				i++
				k := strings.IndexByte(s[i:], q)
				if k < 0 {
					val, i = s[i:], len(s)
				} else {
					val, i = s[i:i+k], i+k+1
				}
			} else {
				k := i
				for k < len(s) && !isSpace(s[k]) {
					k++
				}
				val, i = s[i:k], k
			}
		}
		if _, dup := a[name]; !dup {
			a[name] = val
		}
	}
	return a
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' }

// hiddenByFlagAttrs is the reason the "hidden" and "aria-hidden" attributes
// hide a tag, or "".
func hiddenByFlagAttrs(a attrMap) string {
	if _, ok := a["hidden"]; ok {
		return wire.HideHiddenAttr
	}
	if strings.EqualFold(strings.TrimSpace(a["aria-hidden"]), "true") {
		return wire.HideAria
	}
	return ""
}

// hiddenByAttrs says why a tag with these attributes is hidden by its own
// attributes (not by inheritance), or "". Colour is judged against white, the
// page default, because there is no tree to inherit a background from; the
// HTML parser does its own, better, judgement.
func hiddenByAttrs(a attrMap) string {
	if r := hiddenByFlagAttrs(a); r != "" {
		return r
	}
	if st, ok := a["style"]; ok {
		d := parseDecls(st)
		if r := declsHidden(d); r != "" {
			return r
		}
		if fg := parseColor(d["color"]); fg != nil {
			bg := parseColor(firstNonEmpty(d["background-color"], d["background"]))
			if bg == nil || bg.a <= 0.05 {
				bg = white
			}
			if r := contrastReason(fg, bg); r != "" {
				return r
			}
		}
	}
	return ""
}

// contrastReason is why text of colour fg cannot be read on bg, or "".
func contrastReason(fg, bg *rgba) string {
	switch {
	case fg.a <= 0.05:
		return wire.HideTransparent
	case !fg.visibleOn(bg):
		return wire.HideSameColour
	}
	return ""
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// parseDecls parses a CSS declaration list ("a: b; c: d !important") into a map
// of lower-cased property to lower-cased value, last one winning.
func parseDecls(s string) map[string]string {
	d := map[string]string{}
	for _, part := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(part, ":")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.ToLower(strings.TrimSpace(v))
		if i := strings.Index(v, "!important"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		if k != "" && v != "" {
			d[k] = v
		}
	}
	return d
}

// declsHidden is the reason a declaration set hides its element's own content,
// not counting colour contrast (which needs the background in force).
func declsHidden(d map[string]string) string {
	if d["display"] == "none" {
		return wire.HideDisplayNone
	}
	if v := d["visibility"]; v == "hidden" || v == "collapse" {
		return wire.HideVisibility
	}
	if v, ok := d["opacity"]; ok {
		if f, err := strconv.ParseFloat(strings.TrimSuffix(v, "%"), 64); err == nil {
			if strings.HasSuffix(v, "%") {
				f /= 100
			}
			if f <= 0.01 {
				return wire.HideZeroOpacity
			}
		}
	}
	if v, ok := d["font-size"]; ok {
		if px, ok := cssPx(v); ok && px < tinyFontPoints*96/72 {
			return wire.HideTinyFont
		}
	}
	if v, ok := d["text-indent"]; ok {
		if px, ok := cssPx(v); ok && px <= offscreenPx {
			return wire.HideOffscreen
		}
	}
	pos := d["position"]
	if pos == "absolute" || pos == "fixed" || pos == "relative" {
		for _, k := range []string{"left", "top", "right", "bottom"} {
			if px, ok := cssPx(d[k]); ok && px <= offscreenPx {
				return wire.HideOffscreen
			}
		}
	}
	if px, ok := cssPx(d["margin-left"]); ok && px <= offscreenPx {
		return wire.HideOffscreen
	}
	if v := d["clip"]; strings.HasPrefix(v, "rect(") && onlyZeros(v) {
		return wire.HideClipped
	}
	if v := d["clip-path"]; strings.HasPrefix(v, "inset(") && (strings.Contains(v, "100%") || strings.Contains(v, "50%")) {
		return wire.HideClipped
	}
	if strings.Contains(d["overflow"], "hidden") || strings.Contains(d["overflow-x"], "hidden") || strings.Contains(d["overflow-y"], "hidden") {
		for _, k := range []string{"height", "max-height", "width", "max-width"} {
			if px, ok := cssPx(d[k]); ok && px == 0 {
				return wire.HideClipped
			}
		}
	}
	return ""
}

// onlyZeros reports whether a rect(...) value has only zero lengths.
func onlyZeros(v string) bool {
	inner := strings.TrimSuffix(strings.TrimPrefix(v, "rect("), ")")
	for _, f := range strings.FieldsFunc(inner, func(r rune) bool { return r == ',' || r == ' ' }) {
		if px, ok := cssPx(f); !ok || px != 0 {
			return false
		}
	}
	return inner != ""
}

// cssPx converts a CSS length to pixels. Relative units use a 16 px base; a
// bare number is taken as pixels. ok is false for anything else (auto,
// calc(), var()).
func cssPx(v string) (float64, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	units := []struct {
		suffix string
		mul    float64
	}{
		{"px", 1}, {"pt", 96.0 / 72}, {"pc", 16}, {"in", 96}, {"cm", 96 / 2.54}, {"mm", 96 / 25.4},
		{"rem", 16}, {"em", 16}, {"ex", 8}, {"ch", 8}, {"vw", 12}, {"vh", 8}, {"%", 0.16},
	}
	for _, u := range units {
		if strings.HasSuffix(v, u.suffix) {
			f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(v, u.suffix)), 64)
			if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
				return 0, false
			}
			return f * u.mul, true
		}
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
		return f, true
	}
	return 0, false
}

// rgba is a colour; a is 0 to 1.
type rgba struct {
	r, g, b uint8
	a       float64
}

var white = &rgba{255, 255, 255, 1}

var namedColours = map[string]rgba{
	"white": {255, 255, 255, 1}, "black": {0, 0, 0, 1}, "transparent": {0, 0, 0, 0},
	"red": {255, 0, 0, 1}, "green": {0, 128, 0, 1}, "blue": {0, 0, 255, 1}, "yellow": {255, 255, 0, 1},
	"gray": {128, 128, 128, 1}, "grey": {128, 128, 128, 1}, "silver": {192, 192, 192, 1},
	"snow": {255, 250, 250, 1}, "ivory": {255, 255, 240, 1}, "whitesmoke": {245, 245, 245, 1},
	"ghostwhite": {248, 248, 255, 1}, "floralwhite": {255, 250, 240, 1}, "aliceblue": {240, 248, 255, 1},
	"azure": {240, 255, 255, 1}, "mintcream": {245, 255, 250, 1}, "seashell": {255, 245, 238, 1},
	"honeydew": {240, 255, 240, 1}, "linen": {250, 240, 230, 1}, "oldlace": {253, 245, 230, 1},
	"navy": {0, 0, 128, 1}, "maroon": {128, 0, 0, 1}, "orange": {255, 165, 0, 1}, "purple": {128, 0, 128, 1},
	"lightgray": {211, 211, 211, 1}, "lightgrey": {211, 211, 211, 1}, "gainsboro": {220, 220, 220, 1},
}

// parseColor parses a CSS colour: hex (3, 4, 6 or 8 digits), rgb()/rgba(),
// hsl()/hsla() and a few names. nil means unknown, which callers treat as
// "not provably hidden".
func parseColor(v string) *rgba {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" {
		return nil
	}
	if c, ok := namedColours[v]; ok {
		return &c
	}
	if strings.HasPrefix(v, "#") {
		h := v[1:]
		switch len(h) {
		case 3, 4:
			var e []byte
			for i := 0; i < len(h); i++ {
				e = append(e, h[i], h[i])
			}
			h = string(e)
		case 6, 8:
		default:
			return nil
		}
		n, err := strconv.ParseUint(h, 16, 64)
		if err != nil {
			return nil
		}
		if len(h) == 6 {
			return &rgba{uint8(n >> 16), uint8(n >> 8), uint8(n), 1}
		}
		return &rgba{uint8(n >> 24), uint8(n >> 16), uint8(n >> 8), float64(uint8(n)) / 255}
	}
	fn, args, ok := strings.Cut(v, "(")
	if !ok || !strings.HasSuffix(args, ")") {
		return nil
	}
	args = strings.TrimSuffix(args, ")")
	parts := strings.FieldsFunc(args, func(r rune) bool { return r == ',' || r == ' ' || r == '/' })
	num := func(s string, scale float64) (float64, bool) {
		pct := strings.HasSuffix(s, "%")
		f, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
		if err != nil {
			return 0, false
		}
		if pct {
			f = f / 100 * scale
		}
		return f, true
	}
	switch fn {
	case "rgb", "rgba":
		if len(parts) < 3 {
			return nil
		}
		var c [3]float64
		for i := 0; i < 3; i++ {
			f, ok := num(parts[i], 255)
			if !ok {
				return nil
			}
			c[i] = math.Max(0, math.Min(255, f))
		}
		a := 1.0
		if len(parts) >= 4 {
			f, ok := num(parts[3], 1)
			if !ok {
				return nil
			}
			a = math.Max(0, math.Min(1, f))
		}
		return &rgba{uint8(c[0]), uint8(c[1]), uint8(c[2]), a}
	case "hsl", "hsla":
		if len(parts) < 3 {
			return nil
		}
		h, ok1 := num(strings.TrimSuffix(parts[0], "deg"), 360)
		s, ok2 := num(parts[1], 1)
		l, ok3 := num(parts[2], 1)
		if !ok1 || !ok2 || !ok3 {
			return nil
		}
		r, g, b := hslToRGB(math.Mod(math.Mod(h, 360)+360, 360)/360, clamp01(s), clamp01(l))
		a := 1.0
		if len(parts) >= 4 {
			if f, ok := num(parts[3], 1); ok {
				a = clamp01(f)
			}
		}
		return &rgba{r, g, b, a}
	}
	return nil
}

func clamp01(f float64) float64 { return math.Max(0, math.Min(1, f)) }

func hslToRGB(h, s, l float64) (uint8, uint8, uint8) {
	if s == 0 {
		v := uint8(math.Round(l * 255))
		return v, v, v
	}
	q := l + s - l*s
	if l < 0.5 {
		q = l * (1 + s)
	}
	p := 2*l - q
	hue := func(t float64) float64 {
		if t < 0 {
			t++
		}
		if t > 1 {
			t--
		}
		switch {
		case t < 1.0/6:
			return p + (q-p)*6*t
		case t < 0.5:
			return q
		case t < 2.0/3:
			return p + (q-p)*(2.0/3-t)*6
		}
		return p
	}
	return uint8(math.Round(hue(h+1.0/3) * 255)), uint8(math.Round(hue(h) * 255)), uint8(math.Round(hue(h-1.0/3) * 255))
}

func (c *rgba) luminance() float64 {
	lin := func(v uint8) float64 {
		f := float64(v) / 255
		if f <= 0.03928 {
			return f / 12.92
		}
		return math.Pow((f+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c.r) + 0.7152*lin(c.g) + 0.0722*lin(c.b)
}

// visibleOn reports whether text of colour c can be told apart from bg.
func (c *rgba) visibleOn(bg *rgba) bool {
	if c.a <= 0.05 {
		return false
	}
	l1, l2 := c.luminance(), bg.luminance()
	if l1 < l2 {
		l1, l2 = l2, l1
	}
	return (l1+0.05)/(l2+0.05) >= minContrast
}
