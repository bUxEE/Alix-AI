package formats

import (
	"errors"
	"math"
	"strconv"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// PDF object model. A PDF is parsed lazily: the cross-reference table says
// where each object is, objects are read when asked for, and every read is
// bounded (depth, size, count) because the file is hostile.

type (
	pdfName    string
	pdfStr     string // string bytes, not necessarily text
	pdfKeyword string // an operator or structural keyword (obj, endobj, stream, BT, ...)
	pdfArray   []any
	pdfDict    map[string]any
	pdfRef     struct{ num, gen int }
)

type pdfStream struct {
	dict pdfDict
	raw  []byte // the encoded bytes, a slice of the file
}

var errPDFSyntax = errors.New("pdf syntax")

type lexer struct {
	b        []byte
	pos      int
	maxDepth int
}

func isPDFSpace(c byte) bool {
	return c == 0 || c == 9 || c == 10 || c == 12 || c == 13 || c == 32
}

func isPDFDelim(c byte) bool {
	switch c {
	case '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}

func isPDFRegular(c byte) bool { return !isPDFSpace(c) && !isPDFDelim(c) }

// skipWS skips white space and comments.
func (l *lexer) skipWS() {
	for l.pos < len(l.b) {
		c := l.b[l.pos]
		switch {
		case isPDFSpace(c):
			l.pos++
		case c == '%':
			for l.pos < len(l.b) && l.b[l.pos] != '\n' && l.b[l.pos] != '\r' {
				l.pos++
			}
		default:
			return
		}
	}
}

func (l *lexer) eof() bool { l.skipWS(); return l.pos >= len(l.b) }

// readObject reads one object. A keyword (an operator, or obj, endobj, stream)
// comes back as a pdfKeyword; the stray closers ] and >> come back as the
// keywords "]" and ">>" so that the container loops can end on them.
func (l *lexer) readObject(depth int) (any, error) {
	if depth > l.maxDepth {
		return nil, fail(wire.ReasonNestingTooDeep, "PDF objects are nested deeper than %d", l.maxDepth)
	}
	l.skipWS()
	if l.pos >= len(l.b) {
		return nil, errPDFSyntax
	}
	c := l.b[l.pos]
	switch {
	case c == '/':
		return l.name(), nil
	case c == '(':
		return l.literalString(), nil
	case c == '<':
		if l.pos+1 < len(l.b) && l.b[l.pos+1] == '<' {
			l.pos += 2
			return l.dict(depth)
		}
		return l.hexString(), nil
	case c == '>':
		if l.pos+1 < len(l.b) && l.b[l.pos+1] == '>' {
			l.pos += 2
			return pdfKeyword(">>"), nil
		}
		l.pos++
		return pdfKeyword(">"), nil
	case c == '[':
		l.pos++
		return l.array(depth)
	case c == ']':
		l.pos++
		return pdfKeyword("]"), nil
	case c == '{' || c == '}' || c == ')':
		l.pos++
		return pdfKeyword(string(c)), nil
	case c == '+' || c == '-' || c == '.' || (c >= '0' && c <= '9'):
		return l.number(), nil
	}
	start := l.pos
	for l.pos < len(l.b) && isPDFRegular(l.b[l.pos]) {
		l.pos++
	}
	if l.pos == start {
		l.pos++
		return nil, errPDFSyntax
	}
	switch w := string(l.b[start:l.pos]); w {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null":
		return nil, nil
	default:
		return pdfKeyword(w), nil
	}
}

func (l *lexer) name() pdfName {
	l.pos++ // the slash
	start := l.pos
	for l.pos < len(l.b) && isPDFRegular(l.b[l.pos]) {
		l.pos++
	}
	raw := l.b[start:l.pos]
	hasEsc := false
	for _, c := range raw {
		if c == '#' {
			hasEsc = true
			break
		}
	}
	if !hasEsc {
		return pdfName(raw)
	}
	out := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] == '#' && i+2 < len(raw) {
			h, ok1 := hexVal(raw[i+1])
			lo, ok2 := hexVal(raw[i+2])
			if ok1 && ok2 {
				out = append(out, h<<4|lo)
				i += 2
				continue
			}
		}
		out = append(out, raw[i])
	}
	return pdfName(out)
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func (l *lexer) literalString() pdfStr {
	l.pos++ // the opening paren
	var out []byte
	depth := 1
	for l.pos < len(l.b) {
		c := l.b[l.pos]
		l.pos++
		switch c {
		case '(':
			depth++
			out = append(out, c)
		case ')':
			depth--
			if depth == 0 {
				return pdfStr(out)
			}
			out = append(out, c)
		case '\\':
			if l.pos >= len(l.b) {
				return pdfStr(out)
			}
			e := l.b[l.pos]
			l.pos++
			switch e {
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case '\r':
				if l.pos < len(l.b) && l.b[l.pos] == '\n' {
					l.pos++
				}
			case '\n':
			case '0', '1', '2', '3', '4', '5', '6', '7':
				v := int(e - '0')
				for i := 0; i < 2 && l.pos < len(l.b) && l.b[l.pos] >= '0' && l.b[l.pos] <= '7'; i++ {
					v = v*8 + int(l.b[l.pos]-'0')
					l.pos++
				}
				out = append(out, byte(v))
			default:
				out = append(out, e)
			}
		default:
			out = append(out, c)
		}
	}
	return pdfStr(out)
}

func (l *lexer) hexString() pdfStr {
	l.pos++ // <
	var out []byte
	var hi byte
	have := false
	for l.pos < len(l.b) {
		c := l.b[l.pos]
		l.pos++
		if c == '>' {
			break
		}
		v, ok := hexVal(c)
		if !ok {
			continue
		}
		if have {
			out = append(out, hi<<4|v)
			have = false
		} else {
			hi, have = v, true
		}
	}
	if have {
		out = append(out, hi<<4)
	}
	return pdfStr(out)
}

// number reads an integer or real. Something that starts like a number but is
// not one (a lone sign or dot) is returned as 0 after consuming it, so the
// lexer always advances.
func (l *lexer) number() any {
	start := l.pos
	if c := l.b[l.pos]; c == '+' || c == '-' {
		l.pos++
		// tolerate "--5" as some writers emit
		for l.pos < len(l.b) && (l.b[l.pos] == '-' || l.b[l.pos] == '+') {
			l.pos++
		}
	}
	isReal := false
	digits := 0
	for l.pos < len(l.b) {
		c := l.b[l.pos]
		if c >= '0' && c <= '9' {
			digits++
			l.pos++
		} else if c == '.' && !isReal {
			isReal = true
			l.pos++
		} else {
			break
		}
	}
	// A number glued to letters ("1abc") is a keyword in some writers' output;
	// consume the rest as a keyword and give 0.
	for l.pos < len(l.b) && isPDFRegular(l.b[l.pos]) {
		l.pos++
		digits = 0
	}
	if digits == 0 {
		return 0
	}
	s := string(l.b[start:l.pos])
	neg := false
	for len(s) > 0 && (s[0] == '-' || s[0] == '+') {
		neg = neg || s[0] == '-'
		s = s[1:]
	}
	if !isReal {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			f, _ := strconv.ParseFloat(s, 64)
			if neg {
				f = -f
			}
			return f
		}
		if neg {
			n = -n
		}
		return n
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0.0
	}
	if neg {
		f = -f
	}
	return f
}

func (l *lexer) array(depth int) (any, error) {
	arr := pdfArray{}
	for {
		l.skipWS()
		if l.pos >= len(l.b) {
			return arr, nil // unterminated: take what there is
		}
		o, err := l.readObject(depth + 1)
		if err != nil {
			if err == errPDFSyntax {
				continue
			}
			return nil, err
		}
		if k, ok := o.(pdfKeyword); ok {
			if k == "]" {
				return arr, nil
			}
			if k == ">>" || k == "endobj" || k == "stream" {
				l.pos -= len(k) // leave it for the caller
				return arr, nil
			}
			continue // an operator inside an array makes no sense; ignore it
		}
		if n, ok := o.(int64); ok {
			if r, ok := l.tryRef(n); ok {
				arr = append(arr, r)
				continue
			}
		}
		arr = append(arr, o)
		if len(arr) > 1<<22 {
			return nil, fail(wire.ReasonMalformed, "PDF array is too long")
		}
	}
}

func (l *lexer) dict(depth int) (any, error) {
	d := pdfDict{}
	for {
		l.skipWS()
		if l.pos >= len(l.b) {
			return d, nil
		}
		k, err := l.readObject(depth + 1)
		if err != nil {
			if err == errPDFSyntax {
				continue
			}
			return nil, err
		}
		switch kv := k.(type) {
		case pdfKeyword:
			if kv == ">>" {
				return d, nil
			}
			if kv == "endobj" || kv == "stream" {
				l.pos -= len(kv)
				return d, nil
			}
			continue
		case pdfName:
			l.skipWS()
			if l.pos >= len(l.b) {
				return d, nil
			}
			v, err := l.readObject(depth + 1)
			if err != nil {
				if err == errPDFSyntax {
					continue
				}
				return nil, err
			}
			if vk, ok := v.(pdfKeyword); ok {
				if vk == ">>" {
					return d, nil
				}
				if vk == "endobj" || vk == "stream" {
					l.pos -= len(vk)
					return d, nil
				}
				continue
			}
			if n, ok := v.(int64); ok {
				if r, ok := l.tryRef(n); ok {
					v = r
				}
			}
			if _, dup := d[string(kv)]; !dup {
				d[string(kv)] = v
			}
			if len(d) > 1<<20 {
				return nil, fail(wire.ReasonMalformed, "PDF dictionary is too large")
			}
		}
	}
}

// tryRef checks whether the integer just read is the object number of an
// indirect reference ("12 0 R") and, if so, consumes the rest of it.
func (l *lexer) tryRef(num int64) (pdfRef, bool) {
	save := l.pos
	l.skipWS()
	start := l.pos
	for l.pos < len(l.b) && l.b[l.pos] >= '0' && l.b[l.pos] <= '9' {
		l.pos++
	}
	if l.pos == start || l.pos-start > 10 {
		l.pos = save
		return pdfRef{}, false
	}
	gen, _ := strconv.Atoi(string(l.b[start:l.pos]))
	l.skipWS()
	if l.pos < len(l.b) && l.b[l.pos] == 'R' && (l.pos+1 >= len(l.b) || !isPDFRegular(l.b[l.pos+1])) &&
		num >= 0 && num <= math.MaxInt32 {
		l.pos++
		return pdfRef{int(num), gen}, true
	}
	l.pos = save
	return pdfRef{}, false
}
