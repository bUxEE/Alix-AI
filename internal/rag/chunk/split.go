package chunk

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// trimSpan narrows [s,e) of text so it neither starts nor ends with white
// space. The result may be empty (s == e).
func trimSpan(text string, s, e int) (int, int) {
	for s < e {
		r, size := utf8.DecodeRuneInString(text[s:e])
		if !unicode.IsSpace(r) {
			break
		}
		s += size
	}
	for e > s {
		r, size := utf8.DecodeLastRuneInString(text[s:e])
		if !unicode.IsSpace(r) {
			break
		}
		e -= size
	}
	return s, e
}

// isBlank reports whether text[s:e] is only white space.
func isBlank(text string, s, e int) bool {
	return strings.TrimSpace(text[s:e]) == ""
}

// sentenceEnd classifies r as the end of a sentence. cjk marks the full-width
// stops, which end a sentence with no space after them.
func sentenceEnd(r rune) (end, cjk bool) {
	switch r {
	case '.', '!', '?', '…': // ... and the horizontal ellipsis
		return true, false
	case '。', '！', '？', '．', '؟', '।', '॥': // CJK and Arabic and Devanagari stops
		return true, true
	}
	return false, false
}

// splitSpan cuts text[s:e] into pieces of at most max estimated tokens and
// calls emit with the byte span of each, trimmed of white space. It prefers to
// cut after a line break or a sentence end that leaves at least half of max in
// the piece, then after any white space, and only then between code points.
// Pieces never overlap and, with the white space between them, cover [s,e).
// max must be at least minChildMaxToks, so one code point always fits.
func splitSpan(text string, s, e, max int, emit func(a, b int)) {
	pos, _ := trimSpan(text, s, e)
	for pos < e {
		var m meter
		strong, weak, strongToks := -1, -1, 0
		i := pos
		for i < e {
			r, size := utf8.DecodeRuneInString(text[i:e])
			m.add(r, size)
			if m.toks() > max {
				break
			}
			i += size
			if r == '\n' {
				strong, strongToks = i, m.toks()
			} else if end, cjk := sentenceEnd(r); end {
				if cjk || i == e || nextIsSpace(text, i, e) {
					strong, strongToks = i, m.toks()
				}
			} else if unicode.IsSpace(r) {
				weak = i
			}
		}
		if i >= e {
			if a, b := trimSpan(text, pos, e); a < b {
				emit(a, b)
			}
			return
		}
		cut := i // hard cut between code points
		switch {
		case strong > pos && strongToks*2 >= max:
			cut = strong
		case weak > pos:
			cut = weak
		case strong > pos:
			cut = strong
		}
		if a, b := trimSpan(text, pos, cut); a < b {
			emit(a, b)
		}
		pos, _ = trimSpan(text, cut, e)
	}
}

func nextIsSpace(text string, i, e int) bool {
	r, _ := utf8.DecodeRuneInString(text[i:e])
	return unicode.IsSpace(r)
}
