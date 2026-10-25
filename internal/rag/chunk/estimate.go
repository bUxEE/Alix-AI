package chunk

import "unicode/utf8"

// Token estimation without a model.
//
// The estimate is the larger of two bounds, both additive over text, in
// milli-tokens (1000 = one token):
//
//   - a script-weighted count: every rune costs a fixed amount that depends on
//     its script, calibrated high for the byte-pair tokenizers the supported
//     models use (a Latin letter 0.34 token, an ASCII digit or punctuation
//     mark a whole token, a Han, kana or Hangul character 1.25, an emoji 2.5);
//   - a byte floor of one token per three UTF-8 bytes, which is what a
//     tokenizer that falls back to bytes can cost at worst for an unfamiliar
//     script.
//
// Both terms are sums over runes or bytes, so the estimate of a concatenation
// never exceeds the sum of the estimates of its parts: children can be
// budgeted without the parent exceeding their total. A text is estimated at
// ceil(total/1000) tokens; the empty text costs 0.
const (
	milli = 1000

	wLatin      = 340  // ASCII letter
	wSpace      = 100  // ' '
	wControlWS  = 250  // \n \r \t \f \v
	wDigit      = 1000 // digits are split one by one by many tokenizers
	wASCIIOther = 1000 // punctuation, symbols, controls
	wLatinExt   = 500  // Latin-1 supplement and Latin Extended letters
	wCombining  = 500
	wGreekCyr   = 600
	wHebrewArab = 700
	wIndicSEA   = 1500 // Indic and South-East Asian scripts
	wCJK        = 1250 // Han, kana, Hangul
	wCJKExt     = 2000 // CJK extension planes
	wSymbol     = 1000
	wEmoji      = 2500
	wAstral     = 2000 // every other code point above U+FFFF
	wOther      = 1000 // any other non-ASCII rune
	wInvalid    = 1000 // one stray byte of invalid UTF-8
)

// runeWeight is the milli-token cost of r. It is a switch of range checks, no
// table lookups, so a pass over a megabyte stays in the low milliseconds.
func runeWeight(r rune) int64 {
	if r < 0x80 {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			return wLatin
		case r >= '0' && r <= '9':
			return wDigit
		case r == ' ':
			return wSpace
		case r == '\n' || r == '\r' || r == '\t' || r == '\f' || r == '\v':
			return wControlWS
		}
		return wASCIIOther
	}
	switch {
	case r <= 0x024F:
		return wLatinExt
	case r <= 0x036F:
		return wCombining
	case r <= 0x052F:
		return wGreekCyr
	case r >= 0x0590 && r <= 0x06FF, r >= 0x0750 && r <= 0x077F,
		r >= 0xFB1D && r <= 0xFDFF, r >= 0xFE70 && r <= 0xFEFF:
		return wHebrewArab
	case r >= 0x0900 && r <= 0x0FFF, r >= 0x1000 && r <= 0x109F,
		r >= 0x1780 && r <= 0x17FF:
		return wIndicSEA
	case r >= 0x2000 && r <= 0x2BFF:
		return wSymbol
	case r >= 0x3040 && r <= 0x30FF, r >= 0x3400 && r <= 0x4DBF,
		r >= 0x4E00 && r <= 0x9FFF, r >= 0xAC00 && r <= 0xD7AF,
		r >= 0xF900 && r <= 0xFAFF:
		return wCJK
	case r >= 0x1F000 && r <= 0x1FFFF, r >= 0xFE00 && r <= 0xFE0F:
		return wEmoji
	case r >= 0x20000 && r <= 0x3FFFF:
		return wCJKExt
	case r > 0xFFFF:
		return wAstral
	}
	return wOther
}

// meter accumulates the estimate of a text one rune at a time, so a splitter
// and a packer can ask "how many tokens so far" without rescanning. The zero
// value is an empty text.
type meter struct {
	w int64 // script-weighted milli-tokens
	b int64 // UTF-8 bytes
}

func (m *meter) add(r rune, size int) {
	if r == utf8.RuneError && size == 1 {
		m.w += wInvalid
	} else {
		m.w += runeWeight(r)
	}
	m.b += int64(size)
}

func (m *meter) addString(s string) {
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			m.w += runeWeight(rune(c))
			m.b++
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		m.add(r, size)
		i += size
	}
}

// toks is the estimate of everything added so far.
func (m meter) toks() int {
	floor := (m.b*milli + 2) / 3 // ceil(b/3) tokens, in milli-tokens
	total := m.w
	if floor > total {
		total = floor
	}
	return int((total + milli - 1) / milli)
}

// EstimateTokens estimates the model tokens text costs in a prompt, with no
// model. It errs high on purpose: a budget measured with it is an upper bound.
// Deterministic, allocation-free and linear in len(text). Invalid UTF-8 is
// counted one token per stray byte.
func EstimateTokens(text string) int {
	var m meter
	m.addString(text)
	return m.toks()
}
