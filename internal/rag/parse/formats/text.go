package formats

import (
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// cleanText makes s safe to index: valid UTF-8, no NUL or other C0/C1 control
// characters (a NUL in a document can truncate a C string in some index or
// log path), "\r\n" and "\r" and the Unicode line and paragraph separators
// become "\n". Tab and newline survive. It returns s itself when nothing
// needs to change, which is the common case.
func cleanText(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x80 || c < 0x20 && c != '\n' && c != '\t' || c == 0x7f {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		switch {
		case r == '\r':
			if i < len(s) && s[i] == '\n' {
				i++
			}
			b.WriteByte('\n')
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == 0x85 || r == 0x2028 || r == 0x2029:
			b.WriteByte('\n')
		case r == '\f' || r == 0x0b:
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0:
			// dropped
		case r == 0xfffe || r == 0xffff:
			// noncharacters, dropped
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// collapse folds every run of white space to one space and trims the ends.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// invisibleClass classifies a rune for hidden-text detection by invisible
// characters. Strong characters are never legitimate in running text and flag
// the run they are in; weak ones (zero-width joiners and friends) are used by
// emoji and by Persian, Indic and other scripts, so they flag a run only when
// several of them sit together, which is how a zero-width encoded payload
// looks.
func invisibleClass(r rune) (strong, weak bool) {
	switch {
	case r >= 0xE0000 && r <= 0xE007F: // Unicode tag characters: ASCII smuggling
		return true, false
	case r >= 0xE0100 && r <= 0xE01EF: // variation selectors supplement (not the emoji FE00-FE0F)
		return true, false
	case r >= 0x202A && r <= 0x202E: // bidi embeddings and overrides
		return true, false
	case r >= 0x2066 && r <= 0x2069: // bidi isolates
		return true, false
	case r == 0x200B || r == 0x200C || r == 0x200D || r == 0x2060 || (r >= 0x2061 && r <= 0x2064) ||
		r == 0xFEFF || r == 0x180E || r == 0x034F || r == 0x00AD:
		return false, true
	}
	return false, false
}

// invisibleRuns returns the byte spans of s that hold hidden characters: runs
// of invisible code points that contain a strong one, or at least three weak
// ones. Spans are disjoint and in order.
func invisibleRuns(s string) []wire.Span {
	var out []wire.Span
	start, count := -1, 0
	strongSeen := false
	flush := func(end int) {
		if start >= 0 && (strongSeen || count >= 3) {
			out = append(out, wire.Span{Start: start, End: end})
		}
		start, count, strongSeen = -1, 0, false
	}
	for i := 0; i < len(s); {
		c := s[i]
		if c < 0x80 { // ASCII is never invisible here; skip the decode
			flush(i)
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		strong, weak := invisibleClass(r)
		if strong || weak {
			if start < 0 {
				start = i
			}
			count++
			strongSeen = strongSeen || strong
		} else {
			flush(i)
		}
		i += n
	}
	flush(len(s))
	return out
}

// hasInvisible reports whether s holds a hidden run (see invisibleRuns).
func hasInvisible(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return len(invisibleRuns(s)) > 0
		}
	}
	return false
}

// windows1252 maps bytes 0x80 to 0x9F of Windows-1252 to code points. The five
// bytes the code page leaves undefined map to the C1 control of the same value,
// as WHATWG does, and cleanText then drops them.
var windows1252 = [32]rune{
	0x20AC, 0x81, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021,
	0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0x8D, 0x017D, 0x8F,
	0x90, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014,
	0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0x9D, 0x017E, 0x0178,
}

// decodeText turns document bytes into UTF-8 text: a UTF-8 or UTF-16 byte order
// mark is honoured, valid UTF-8 is taken as is, and anything else is read as
// Windows-1252 (a superset of Latin-1, the usual legacy encoding), with a
// warning, rather than guessed at further.
func decodeText(data []byte, w *warnings) string {
	switch {
	case len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF:
		return string(data[3:])
	case len(data) >= 2 && data[0] == 0xFF && data[1] == 0xFE:
		return decodeUTF16(data[2:], false)
	case len(data) >= 2 && data[0] == 0xFE && data[1] == 0xFF:
		return decodeUTF16(data[2:], true)
	}
	if utf8.Valid(data) {
		return string(data)
	}
	w.add("charset_fallback_windows1252")
	var b strings.Builder
	b.Grow(len(data) + len(data)/4)
	for _, c := range data {
		switch {
		case c < 0x80:
			b.WriteByte(c)
		case c >= 0x80 && c < 0xA0:
			b.WriteRune(windows1252[c-0x80])
		default:
			b.WriteRune(rune(c))
		}
	}
	return b.String()
}

func decodeUTF16(b []byte, bigEndian bool) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if bigEndian {
			u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
		} else {
			u = append(u, uint16(b[i+1])<<8|uint16(b[i]))
		}
	}
	return string(utf16.Decode(u))
}

// warnings collects stable warning codes, once each, in first-seen order.
type warnings struct {
	list []string
}

func (w *warnings) add(code string) {
	for _, c := range w.list {
		if c == code {
			return
		}
	}
	w.list = append(w.list, code)
}
