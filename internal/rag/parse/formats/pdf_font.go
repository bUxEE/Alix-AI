package formats

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"
)

// glyph is one decoded character of a shown string.
type glyph struct {
	text  string  // its Unicode text; "" when it cannot be mapped
	width float64 // advance in em units (1.0 = the font size)
	space bool    // a single-byte code 32, to which word spacing applies
}

type pdfFont struct {
	composite bool // two-byte codes (Type0 with an Identity-style encoding)
	toUni     *cmap
	enc       [256]rune // simple fonts: code to Unicode, 0 for none
	widths    map[int]float64
	defWidth  float64
	std       *[95]uint16 // fallback widths for ASCII 32..126
	unmapped  bool
	// unicodeCodes: a Type0 font whose codes are UTF-16 code units (Uni*-UCS2/UTF16 CMaps).
	unicodeCodes bool
}

func (fo *pdfFile) loadFont(d pdfDict) *pdfFont {
	if d == nil {
		return nil
	}
	fn := &pdfFont{defWidth: 0.5}
	sub := nameOf(fo.resolve(d["Subtype"]))
	base := nameOf(fo.resolve(d["BaseFont"]))
	if ts := fo.stream(d["ToUnicode"]); ts != nil {
		if data, err := fo.decode(ts); err == nil {
			fn.toUni = parseCMap(data, fo.lim.MaxDepth)
		}
	}
	if sub == "Type0" {
		fn.composite = true
		enc := nameOf(fo.resolve(d["Encoding"]))
		if strings.HasPrefix(enc, "Uni") && (strings.Contains(enc, "UCS2") || strings.Contains(enc, "UTF16")) {
			fn.unicodeCodes = true
		}
		fn.defWidth = 1.0
		if desc := fo.array(d["DescendantFonts"]); len(desc) > 0 {
			if cf := fo.dict(desc[0]); cf != nil {
				if dw, ok := fo.number(cf["DW"]); ok {
					fn.defWidth = dw / 1000
				}
				fn.widths = fo.cidWidths(cf["W"])
			}
		}
		return fn
	}
	// Simple font: base encoding, Differences, widths.
	baseEnc := "WinAnsiEncoding"
	var diffs pdfArray
	switch e := fo.resolve(d["Encoding"]).(type) {
	case pdfName:
		baseEnc = string(e)
	case pdfDict:
		if b := nameOf(fo.resolve(e["BaseEncoding"])); b != "" {
			baseEnc = b
		}
		diffs = fo.array(e["Differences"])
	}
	for c := 0; c < 256; c++ {
		fn.enc[c] = baseRune(baseEnc, c)
	}
	code := 0
	for _, it := range diffs {
		switch t := fo.resolve(it).(type) {
		case int64:
			code = int(t)
		case float64:
			code = int(t)
		case pdfName:
			if code >= 0 && code < 256 {
				if r, ok := glyphRune(string(t)); ok {
					fn.enc[code] = r
				} else {
					fn.enc[code] = 0
				}
			}
			code++
		}
	}
	first, _ := fo.number(d["FirstChar"])
	if ws := fo.array(d["Widths"]); len(ws) > 0 {
		fn.widths = make(map[int]float64, len(ws))
		for i, w := range ws {
			if v, ok := fo.number(w); ok {
				fn.widths[int(first)+i] = v / 1000
			}
		}
		fn.defWidth = 0
		if fd := fo.dict(d["FontDescriptor"]); fd != nil {
			if mw, ok := fo.number(fd["MissingWidth"]); ok {
				fn.defWidth = mw / 1000
			}
		}
		if sub == "Type3" {
			// glyph space to text space: FontMatrix scales the widths
			if m := fo.floats(d["FontMatrix"], 6); m != nil {
				for k, v := range fn.widths {
					fn.widths[k] = v * 1000 * m[0]
				}
			}
		}
	} else {
		fn.std = standardWidths(base)
	}
	return fn
}

// cidWidths parses a CIDFont /W array: "c [w1 w2 ...]" and "cfirst clast w".
func (fo *pdfFile) cidWidths(o any) map[int]float64 {
	a := fo.array(o)
	m := map[int]float64{}
	for i := 0; i < len(a) && len(m) < 1<<20; {
		c, ok := fo.number(a[i])
		if !ok || i+1 >= len(a) {
			break
		}
		if arr := fo.array(a[i+1]); arr != nil {
			for j, w := range arr {
				if v, ok := fo.number(w); ok {
					m[int(c)+j] = v / 1000
				}
			}
			i += 2
			continue
		}
		if i+2 >= len(a) {
			break
		}
		last, ok1 := fo.number(a[i+1])
		w, ok2 := fo.number(a[i+2])
		if ok1 && ok2 && last >= c && last-c < 70000 {
			for k := int(c); k <= int(last); k++ {
				m[k] = w / 1000
			}
		}
		i += 3
	}
	return m
}

// decode turns the bytes of a shown string into glyphs.
func (f *pdfFont) decode(s string) []glyph {
	out := make([]glyph, 0, len(s))
	if f.composite {
		for i := 0; i+1 < len(s); i += 2 {
			code := int(s[i])<<8 | int(s[i+1])
			g := glyph{width: f.defWidth}
			if w, ok := f.widths[code]; ok {
				g.width = w
			}
			if f.toUni != nil {
				g.text, _ = f.toUni.lookup(uint32(code))
			}
			if g.text == "" && f.unicodeCodes {
				g.text = string(rune(code))
			}
			if g.text == "" {
				f.unmapped = true
			}
			out = append(out, g)
		}
		return out
	}
	for i := 0; i < len(s); i++ {
		c := int(s[i])
		g := glyph{space: c == 32}
		switch {
		case f.toUni != nil:
			g.text, _ = f.toUni.lookup(uint32(c))
			if g.text == "" && f.enc[c] != 0 {
				g.text = string(f.enc[c])
			}
		case f.enc[c] != 0:
			g.text = string(f.enc[c])
		}
		if g.text == "" && c >= 0x20 && c < 0x7f {
			g.text = string(rune(c))
		}
		if g.text == "" && c >= 0x20 {
			f.unmapped = true
		}
		if w, ok := f.widths[c]; ok {
			g.width = w
		} else if f.widths == nil && f.std != nil && c >= 32 && c <= 126 {
			g.width = float64(f.std[c-32]) / 1000
		} else {
			g.width = f.defWidth
			if f.widths == nil && f.std != nil {
				g.width = 0.5
			}
		}
		out = append(out, g)
	}
	return out
}

// ---- encodings ----

var macRoman = []rune("ÄÅÇÉÑÖÜáàâäãåçéèêëíìîïñóòôöõúùûü†°¢£§•¶ß®©™´¨≠ÆØ∞±≤≥¥µ∂∑∏π∫ªºΩæø¿¡¬√ƒ≈∆«»… ÀÃÕŒœ–—“”‘’÷◊ÿŸ⁄€‹›ﬁﬂ‡·‚„‰ÂÊÁËÈÍÎÏÌÓÔ\u0000ÒÚÛÙıˆ˜¯˘˙˚¸˝˛ˇ")

var standardUpper = map[int]rune{
	0xA1: '¡', 0xA2: '¢', 0xA3: '£', 0xA4: '⁄', 0xA5: '¥', 0xA6: 'ƒ', 0xA7: '§', 0xA8: '¤', 0xA9: '\'',
	0xAA: '“', 0xAB: '«', 0xAC: '‹', 0xAD: '›', 0xAE: 'ﬁ', 0xAF: 'ﬂ', 0xB1: '–', 0xB2: '†', 0xB3: '‡',
	0xB4: '·', 0xB6: '¶', 0xB7: '•', 0xB8: '‚', 0xB9: '„', 0xBA: '”', 0xBB: '»', 0xBC: '…', 0xBD: '‰',
	0xBF: '¿', 0xC1: '`', 0xC2: '´', 0xC3: 'ˆ', 0xC4: '˜', 0xC5: '¯', 0xC6: '˘', 0xC7: '˙', 0xC8: '¨',
	0xCA: '˚', 0xCB: '¸', 0xCD: '˝', 0xCE: '˛', 0xCF: 'ˇ', 0xD0: '—', 0xE1: 'Æ', 0xE3: 'ª', 0xE8: 'Ł',
	0xE9: 'Ø', 0xEA: 'Œ', 0xEB: 'º', 0xF1: 'æ', 0xF5: 'ı', 0xF8: 'ł', 0xF9: 'ø', 0xFA: 'œ', 0xFB: 'ß',
}

// baseRune maps a code to a rune in a named base encoding.
func baseRune(enc string, c int) rune {
	switch {
	case c < 0x20:
		return 0
	case c < 0x7f:
		if enc == "StandardEncoding" {
			switch c {
			case 0x27:
				return '’'
			case 0x60:
				return '‘'
			}
		}
		return rune(c)
	case c == 0x7f:
		return 0
	}
	switch enc {
	case "MacRomanEncoding":
		if c >= 0x80 {
			return macRoman[c-0x80]
		}
	case "StandardEncoding":
		return standardUpper[c]
	}
	// WinAnsi, and the fallback for anything else.
	switch {
	case c < 0xA0:
		r := windows1252[c-0x80]
		if r < 0xA0 {
			return 0xB7 // undefined in WinAnsi: a bullet per the PDF reference
		}
		return r
	case c == 0xA0:
		return ' '
	case c == 0xAD:
		return '-'
	}
	return rune(c)
}

var (
	glyphOnce sync.Once
	glyphMap  map[string]rune
)

const glyphTable = "space 20 exclam 21 quotedbl 22 numbersign 23 dollar 24 percent 25 ampersand 26 quotesingle 27 " +
	"quoteright 2019 quoteleft 2018 parenleft 28 parenright 29 asterisk 2A plus 2B comma 2C hyphen 2D minus 2212 " +
	"period 2E slash 2F zero 30 one 31 two 32 three 33 four 34 five 35 six 36 seven 37 eight 38 nine 39 " +
	"colon 3A semicolon 3B less 3C equal 3D greater 3E question 3F at 40 bracketleft 5B backslash 5C " +
	"bracketright 5D asciicircum 5E underscore 5F grave 60 braceleft 7B bar 7C braceright 7D asciitilde 7E " +
	"nbspace A0 exclamdown A1 cent A2 sterling A3 currency A4 yen A5 brokenbar A6 section A7 dieresis A8 " +
	"copyright A9 ordfeminine AA guillemotleft AB logicalnot AC registered AE macron AF degree B0 plusminus B1 " +
	"twosuperior B2 threesuperior B3 acute B4 mu B5 paragraph B6 periodcentered B7 cedilla B8 onesuperior B9 " +
	"ordmasculine BA guillemotright BB onequarter BC onehalf BD threequarters BE questiondown BF " +
	"Agrave C0 Aacute C1 Acircumflex C2 Atilde C3 Adieresis C4 Aring C5 AE C6 Ccedilla C7 Egrave C8 Eacute C9 " +
	"Ecircumflex CA Edieresis CB Igrave CC Iacute CD Icircumflex CE Idieresis CF Eth D0 Ntilde D1 Ograve D2 " +
	"Oacute D3 Ocircumflex D4 Otilde D5 Odieresis D6 multiply D7 Oslash D8 Ugrave D9 Uacute DA Ucircumflex DB " +
	"Udieresis DC Yacute DD Thorn DE germandbls DF agrave E0 aacute E1 acircumflex E2 atilde E3 adieresis E4 " +
	"aring E5 ae E6 ccedilla E7 egrave E8 eacute E9 ecircumflex EA edieresis EB igrave EC iacute ED " +
	"icircumflex EE idieresis EF eth F0 ntilde F1 ograve F2 oacute F3 ocircumflex F4 otilde F5 odieresis F6 " +
	"divide F7 oslash F8 ugrave F9 uacute FA ucircumflex FB udieresis FC yacute FD thorn FE ydieresis FF " +
	"bullet 2022 endash 2013 emdash 2014 quotedblleft 201C quotedblright 201D quotesinglbase 201A " +
	"quotedblbase 201E ellipsis 2026 dagger 2020 daggerdbl 2021 perthousand 2030 guilsinglleft 2039 " +
	"guilsinglright 203A trademark 2122 fi FB01 fl FB02 ff FB00 ffi FB03 ffl FB04 Euro 20AC OE 152 oe 153 " +
	"Scaron 160 scaron 161 Zcaron 17D zcaron 17E Ydieresis 178 florin 192 circumflex 2C6 tilde 2DC " +
	"dotlessi 131 Lslash 141 lslash 142 fraction 2044 summation 2211 radical 221A infinity 221E " +
	"notequal 2260 lessequal 2264 greaterequal 2265 approxequal 2248 partialdiff 2202 integral 222B " +
	"Omega 3A9 pi 3C0 alpha 3B1 beta 3B2 gamma 3B3 delta 3B4 epsilon 3B5 theta 3B8 lambda 3BB sigma 3C3 " +
	"tau 3C4 phi 3C6 omega 3C9 Delta 394 Sigma 3A3 Pi 3A0 arrowright 2192 arrowleft 2190 arrowup 2191 " +
	"arrowdown 2193 checkmark 2713 periodcentered B7 middot B7 nbspace A0 nonbreakingspace A0 sfthyphen AD softhyphen AD"

// glyphRune maps a glyph name to a rune: the Adobe glyph names that occur in
// real documents, uniXXXX and uXXXX[XX] forms, and single letters. Names like
// g123 or cid45, which carry no Unicode value, are unmapped.
func glyphRune(name string) (rune, bool) {
	glyphOnce.Do(func() {
		f := strings.Fields(glyphTable)
		glyphMap = make(map[string]rune, len(f)/2)
		for i := 0; i+1 < len(f); i += 2 {
			if v, err := strconv.ParseUint(f[i+1], 16, 32); err == nil {
				glyphMap[f[i]] = rune(v)
			}
		}
	})
	if i := strings.IndexByte(name, '.'); i > 0 { // a.alt, f_i.liga: the base name
		name = name[:i]
	}
	if r, ok := glyphMap[name]; ok {
		return r, true
	}
	if len(name) == 1 && name[0] < 0x80 {
		return rune(name[0]), true
	}
	if strings.HasPrefix(name, "uni") && len(name) >= 7 {
		if v, err := strconv.ParseUint(name[3:7], 16, 32); err == nil {
			return rune(v), true
		}
	}
	if strings.HasPrefix(name, "u") && len(name) >= 5 && len(name) <= 7 {
		if v, err := strconv.ParseUint(name[1:], 16, 32); err == nil && v <= 0x10FFFF {
			return rune(v), true
		}
	}
	if i := strings.IndexByte(name, '_'); i > 0 { // ligature: f_i
		var sb strings.Builder
		for _, part := range strings.Split(name, "_") {
			r, ok := glyphRune(part)
			if !ok {
				return 0, false
			}
			sb.WriteRune(r)
		}
		if rs := []rune(sb.String()); len(rs) == 1 {
			return rs[0], true
		}
	}
	return 0, false
}

// ---- ToUnicode CMaps ----

type cmapRange struct {
	lo, hi uint32
	dst    string
	arr    []string
}

type cmap struct {
	single map[uint32]string
	ranges []cmapRange
}

// parseCMap reads the bfchar and bfrange sections of a ToUnicode CMap. It
// tolerates what real files do and keeps a hard cap on the number of mappings.
func parseCMap(data []byte, maxDepth int) *cmap {
	const maxEntries = 1 << 18
	m := &cmap{single: map[uint32]string{}}
	l := &lexer{b: data, maxDepth: maxDepth}
	var operands []any
	count := 0
	for !l.eof() && count < maxEntries {
		o, err := l.readObject(0)
		if err != nil {
			continue
		}
		k, isKw := o.(pdfKeyword)
		if !isKw {
			operands = append(operands, o)
			if len(operands) > 4096 {
				operands = operands[len(operands)-1024:]
			}
			continue
		}
		switch k {
		case "endbfchar":
			for i := 0; i+1 < len(operands); i += 2 {
				src, ok1 := operands[i].(pdfStr)
				dst, ok2 := operands[i+1].(pdfStr)
				if ok1 && ok2 {
					m.single[codeOf(string(src))] = utf16BE(string(dst))
					count++
				}
			}
		case "endbfrange":
			for i := 0; i+2 < len(operands); i += 3 {
				lo, ok1 := operands[i].(pdfStr)
				hi, ok2 := operands[i+1].(pdfStr)
				if !ok1 || !ok2 {
					continue
				}
				r := cmapRange{lo: codeOf(string(lo)), hi: codeOf(string(hi))}
				switch d := operands[i+2].(type) {
				case pdfStr:
					r.dst = utf16BE(string(d))
				case pdfArray:
					for _, e := range d {
						if s, ok := e.(pdfStr); ok {
							r.arr = append(r.arr, utf16BE(string(s)))
						} else {
							r.arr = append(r.arr, "")
						}
					}
				default:
					continue
				}
				if r.hi >= r.lo {
					m.ranges = append(m.ranges, r)
					count++
				}
			}
		}
		if strings.HasPrefix(string(k), "end") || strings.HasPrefix(string(k), "begin") {
			operands = operands[:0]
		}
	}
	sort.SliceStable(m.ranges, func(i, j int) bool { return m.ranges[i].lo < m.ranges[j].lo })
	return m
}

func codeOf(s string) uint32 {
	var v uint32
	for i := 0; i < len(s) && i < 4; i++ {
		v = v<<8 | uint32(s[i])
	}
	return v
}

func utf16BE(s string) string {
	u := make([]uint16, 0, len(s)/2)
	for i := 0; i+1 < len(s); i += 2 {
		u = append(u, uint16(s[i])<<8|uint16(s[i+1]))
	}
	if len(s) == 1 { // a one-byte destination is a Latin-1 code point
		return string(rune(s[0]))
	}
	return string(utf16.Decode(u))
}

func (m *cmap) lookup(code uint32) (string, bool) {
	if s, ok := m.single[code]; ok {
		return s, true
	}
	// Ranges are sorted by lo; the last one starting at or before code that
	// also covers it wins. Scan back a few, which covers overlapping ranges.
	i := sort.Search(len(m.ranges), func(i int) bool { return m.ranges[i].lo > code })
	for j := i - 1; j >= 0 && j >= i-8; j-- {
		r := m.ranges[j]
		if code > r.hi {
			continue
		}
		off := code - r.lo
		if r.arr != nil {
			if int(off) < len(r.arr) {
				return r.arr[off], r.arr[off] != ""
			}
			continue
		}
		rs := []rune(r.dst)
		if len(rs) == 0 {
			continue
		}
		rs[len(rs)-1] += rune(off)
		return string(rs), true
	}
	return "", false
}

// ---- fallback widths for the standard fonts ----

var helveticaWidths = [95]uint16{
	278, 278, 355, 556, 556, 889, 667, 191, 333, 333, 389, 584, 278, 333, 278, 278,
	556, 556, 556, 556, 556, 556, 556, 556, 556, 556, 278, 278, 584, 584, 584, 556,
	1015, 667, 667, 722, 722, 667, 611, 778, 722, 278, 500, 667, 556, 833, 722, 778,
	667, 778, 722, 667, 611, 722, 667, 944, 667, 667, 611, 278, 278, 278, 469, 556,
	333, 556, 556, 500, 556, 556, 278, 556, 556, 222, 222, 500, 222, 833, 556, 556,
	556, 556, 333, 500, 278, 556, 500, 722, 500, 500, 500, 334, 260, 334, 584,
}

var timesWidths = [95]uint16{
	250, 333, 408, 500, 500, 833, 778, 180, 333, 333, 500, 564, 250, 333, 250, 278,
	500, 500, 500, 500, 500, 500, 500, 500, 500, 500, 278, 278, 564, 564, 564, 444,
	921, 722, 667, 667, 722, 611, 556, 722, 722, 333, 389, 722, 611, 889, 722, 722,
	556, 722, 667, 556, 611, 722, 722, 944, 722, 722, 611, 333, 278, 333, 469, 500,
	333, 444, 500, 444, 500, 444, 333, 500, 500, 278, 278, 500, 278, 778, 500, 500,
	500, 500, 333, 389, 278, 500, 500, 722, 500, 500, 444, 480, 200, 480, 541,
}

var courierWidths = func() [95]uint16 {
	var w [95]uint16
	for i := range w {
		w[i] = 600
	}
	return w
}()

// standardWidths picks fallback widths for a font that has no /Widths: the
// widths of the standard font its name suggests. They only drive word-gap
// detection, so near enough is enough.
func standardWidths(base string) *[95]uint16 {
	n := strings.ToLower(base)
	switch {
	case strings.Contains(n, "courier") || strings.Contains(n, "mono") || strings.Contains(n, "consolas"):
		return &courierWidths
	case strings.Contains(n, "times") || strings.Contains(n, "serif") && !strings.Contains(n, "sans") ||
		strings.Contains(n, "georgia") || strings.Contains(n, "garamond") || strings.Contains(n, "palatino") || strings.Contains(n, "minion"):
		return &timesWidths
	}
	return &helveticaWidths
}
