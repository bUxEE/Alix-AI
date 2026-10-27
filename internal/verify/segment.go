package verify

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// SplitSegments takes a growing model buffer and returns complete segments that are
// safe to verify and emit, plus the unfinished remainder. Language-aware for en and it:
// abbreviations, decimals, lists, fenced code and Markdown tables stay whole (ADR 0046).
func SplitSegments(buf string, lang Language) (ready []string, rest string) {
	if buf == "" {
		return nil, ""
	}
	var out []string
	for {
		n, ok := nextSegmentEnd(buf, lang)
		if !ok {
			return out, buf
		}
		seg := buf[:n]
		buf = buf[n:]
		if strings.TrimSpace(seg) != "" {
			out = append(out, seg)
		}
	}
}

// FlushSegments returns every ready segment plus a non-empty remainder as a final segment.
// Used when the model stream ends: leftover text is verified as one last piece.
func FlushSegments(buf string, lang Language) []string {
	ready, rest := SplitSegments(buf, lang)
	if strings.TrimSpace(rest) != "" {
		ready = append(ready, rest)
	}
	return ready
}

func nextSegmentEnd(s string, lang Language) (int, bool) {
	if s == "" {
		return 0, false
	}
	if end, holding := holdFence(s); holding {
		if end == 0 {
			return 0, false // incomplete fence
		}
		return end, true
	}
	if end, holding := holdTable(s); holding {
		if end == 0 {
			return 0, false
		}
		return end, true
	}
	if end, holding := holdList(s); holding {
		if end == 0 {
			return 0, false
		}
		return end, true
	}
	return sentenceEnd(s, lang)
}

// holdFence keeps a fenced code block whole. The second return is true when this buffer is
// inside (or is) a fence path: incomplete → hold all (caller gets no ready segment); complete →
// cut at the closing fence.
func holdFence(s string) (int, bool) {
	start := strings.Index(s, "```")
	if start < 0 {
		return 0, false
	}
	rest := s[start+3:]
	closeAt := strings.Index(rest, "```")
	if closeAt < 0 {
		return 0, true // incomplete: SplitSegments keeps the whole buffer as rest
	}
	end := start + 3 + closeAt + 3
	for end < len(s) && (s[end] == '\n' || s[end] == '\r') {
		end++
	}
	return end, true
}

// holdTable keeps a GFM table (header + separator + rows) whole once the separator line is seen.
func holdTable(s string) (int, bool) {
	lines := strings.SplitAfter(s, "\n")
	if len(lines) < 2 {
		return 0, false
	}
	// Find a pipe header followed by a separator.
	for i := 0; i+1 < len(lines); i++ {
		h := strings.TrimSpace(lines[i])
		sep := strings.TrimSpace(lines[i+1])
		if !strings.Contains(h, "|") || !isTableSep(sep) {
			continue
		}
		end := 0
		for j := 0; j <= i+1; j++ {
			end += len(lines[j])
		}
		incomplete := true
		for j := i + 2; j < len(lines); j++ {
			row := strings.TrimSpace(lines[j])
			if row == "" {
				incomplete = false
				break
			}
			if !strings.Contains(row, "|") {
				incomplete = false
				break
			}
			end += len(lines[j])
		}
		if incomplete && !strings.HasSuffix(s, "\n\n") {
			return 0, true // hold: table still growing
		}
		return end, true
	}
	return 0, false
}

func isTableSep(line string) bool {
	if line == "" || !strings.Contains(line, "-") {
		return false
	}
	for _, r := range line {
		if r != '-' && r != '|' && r != ':' && r != ' ' {
			return false
		}
	}
	return strings.Contains(line, "|")
}

// holdList keeps a contiguous Markdown list as one segment once a blank line (or non-list) ends it.
func holdList(s string) (int, bool) {
	if !isListLine(firstLine(s)) {
		return 0, false
	}
	end := 0
	for end < len(s) {
		line, next := readLine(s, end)
		trim := strings.TrimSpace(line)
		if trim == "" {
			if next > end {
				return next, true
			}
			return 0, true // trailing open list
		}
		if !isListLine(trim) && end > 0 {
			return end, true
		}
		if !isListLine(trim) {
			return 0, false
		}
		end = next
	}
	return 0, true // still open
}

func isListLine(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") || strings.HasPrefix(line, "+ ") {
		return true
	}
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	return i > 0 && i < len(line) && (line[i] == '.' || line[i] == ')') && i+1 < len(line) && line[i+1] == ' '
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func readLine(s string, from int) (line string, next int) {
	rest := s[from:]
	if i := strings.IndexByte(rest, '\n'); i >= 0 {
		return rest[:i+1], from + i + 1
	}
	return rest, len(s)
}

func sentenceEnd(s string, lang Language) (int, bool) {
	for i := 0; i < len(s); {
		r, w := utf8.DecodeRuneInString(s[i:])
		if r == '.' || r == '!' || r == '?' || r == '…' {
			if r == '.' && (isDecimalDot(s, i) || isAbbreviation(s, i, lang)) {
				i += w
				continue
			}
			j := i + w
			for j < len(s) {
				rr, ww := utf8.DecodeRuneInString(s[j:])
				if rr == '"' || rr == '\'' || rr == '”' || rr == '’' || rr == ')' || rr == ']' {
					j += ww
					continue
				}
				break
			}
			if j >= len(s) {
				return j, true
			}
			rr, ww := utf8.DecodeRuneInString(s[j:])
			if unicode.IsSpace(rr) {
				return j + ww, true
			}
			_ = ww
		}
		i += w
	}
	return 0, false
}

func isDecimalDot(s string, dot int) bool {
	if dot == 0 || dot+1 >= len(s) {
		return false
	}
	prev, _ := utf8.DecodeLastRuneInString(s[:dot])
	next, _ := utf8.DecodeRuneInString(s[dot+1:])
	return unicode.IsDigit(prev) && unicode.IsDigit(next)
}

var (
	abbrEN = []string{"mr", "mrs", "ms", "dr", "prof", "sr", "jr", "st", "vs", "etc", "e.g", "i.e", "approx", "no", "vol", "pp", "fig"}
	abbrIT = []string{"sig", "sig.ra", "sig.na", "dott", "dott.ssa", "prof", "prof.ssa", "ecc", "es", "n", "pag", "tel", "avv", "geom", "ing", "rag"}
)

func isAbbreviation(s string, dot int, lang Language) bool {
	// Word immediately before the dot.
	start := dot
	for start > 0 {
		r, w := utf8.DecodeLastRuneInString(s[:start])
		if unicode.IsLetter(r) || r == '.' {
			start -= w
			continue
		}
		break
	}
	word := strings.ToLower(s[start:dot])
	list := abbrEN
	if lang == LangIT {
		list = append(append([]string{}, abbrIT...), abbrEN...)
	}
	for _, a := range list {
		if word == a || strings.HasSuffix(word, "."+a) {
			return true
		}
	}
	// Single capital letter initials: "A. Rossi"
	if utf8.RuneCountInString(word) == 1 {
		r, _ := utf8.DecodeRuneInString(word)
		if unicode.IsUpper(r) {
			return true
		}
	}
	return false
}
