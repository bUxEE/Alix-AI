// Package textchunk splits synthesis text at sentence boundaries for
// provider per-request character limits.
package textchunk

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Split cuts text into pieces of at most max runes: at sentence ends where it
// can, at spaces where it cannot, and mid-word only for a word longer than max.
// The pieces together are the trimmed text in order.
func Split(text string, max int) []string {
	text = strings.TrimSpace(text)
	if text == "" || max <= 0 {
		return nil
	}
	if utf8.RuneCountInString(text) <= max {
		return []string{text}
	}
	var sentences []string
	runes := []rune(text)
	start := 0
	for i, r := range runes {
		if (r == '.' || r == '!' || r == '?' || r == '\n') && (i+1 == len(runes) || unicode.IsSpace(runes[i+1])) {
			sentences = append(sentences, string(runes[start:i+1]))
			start = i + 1
		}
	}
	if start < len(runes) {
		sentences = append(sentences, string(runes[start:]))
	}
	var out []string
	var cur []rune
	flush := func() {
		if t := strings.TrimSpace(string(cur)); t != "" {
			out = append(out, t)
		}
		cur = cur[:0]
	}
	for _, sent := range sentences {
		sr := []rune(sent)
		for len(sr) > max {
			cut := max
			for j := max; j > max/2; j-- {
				if unicode.IsSpace(sr[j-1]) {
					cut = j
					break
				}
			}
			flush()
			out = append(out, strings.TrimSpace(string(sr[:cut])))
			sr = sr[cut:]
		}
		if len(cur)+len(sr) > max {
			flush()
		}
		cur = append(cur, sr...)
	}
	flush()
	return out
}

// SplitBytes is like Split but bounds each piece by UTF-8 byte length (Google
// Cloud TTS limits input.text to 5000 bytes). Pieces concatenate to the
// trimmed text with no added characters.
func SplitBytes(text string, maxBytes int) []string {
	text = strings.TrimSpace(text)
	if text == "" || maxBytes <= 0 {
		return nil
	}
	if len(text) <= maxBytes {
		return []string{text}
	}
	var sentences []string
	runes := []rune(text)
	start := 0
	for i, r := range runes {
		if (r == '.' || r == '!' || r == '?' || r == '\n') && (i+1 == len(runes) || unicode.IsSpace(runes[i+1])) {
			sentences = append(sentences, string(runes[start:i+1]))
			start = i + 1
		}
	}
	if start < len(runes) {
		sentences = append(sentences, string(runes[start:]))
	}
	var out []string
	var cur string
	flush := func() {
		if t := strings.TrimSpace(cur); t != "" {
			out = append(out, t)
		}
		cur = ""
	}
	emitOversize := func(s string) {
		for len(s) > maxBytes {
			cut := maxBytes
			for cut > 0 && !utf8.RuneStart(s[cut]) {
				cut--
			}
			if cut == 0 {
				_, n := utf8.DecodeRuneInString(s)
				cut = n
			}
			best := cut
			for j := cut; j > maxBytes/2; {
				r, size := utf8.DecodeLastRuneInString(s[:j])
				if size <= 0 {
					break
				}
				if unicode.IsSpace(r) {
					best = j
					break
				}
				j -= size
			}
			out = append(out, strings.TrimSpace(s[:best]))
			s = s[best:]
		}
		cur = s
	}
	for _, sent := range sentences {
		if len(cur)+len(sent) > maxBytes {
			flush()
		}
		if len(sent) > maxBytes {
			flush()
			emitOversize(sent)
			continue
		}
		cur += sent
	}
	flush()
	return out
}
