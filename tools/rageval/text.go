package main

import (
	"regexp"
	"strings"
	"unicode"
)

// Text helpers shared by the label checks and the extractive generator. Nothing
// here is linguistics: it is case, accent and whitespace folding, which is what
// a label has to survive to be matched against text a model or a chunker has
// reflowed.

var accentFold = strings.NewReplacer(
	"à", "a", "á", "a", "â", "a", "ä", "a", "ã", "a", "å", "a",
	"è", "e", "é", "e", "ê", "e", "ë", "e",
	"ì", "i", "í", "i", "î", "i", "ï", "i",
	"ò", "o", "ó", "o", "ô", "o", "ö", "o", "õ", "o",
	"ù", "u", "ú", "u", "û", "u", "ü", "u",
	"ç", "c", "ñ", "n", "ß", "ss",
	"’", "'", "‘", "'", "“", "\"", "”", "\"",
)

// foldText lower-cases s, removes accents and collapses every run of white
// space to one space.
func foldText(s string) string {
	s = accentFold.Replace(strings.ToLower(s))
	return strings.Join(strings.Fields(s), " ")
}

// containsFold reports whether hay holds needle after foldText.
func containsFold(hay, needle string) bool {
	n := foldText(needle)
	return n != "" && strings.Contains(foldText(hay), n)
}

var tokenPattern = regexp.MustCompile(`[\p{L}\p{N}](?:[\p{L}\p{N}\-_.,/:'’]*[\p{L}\p{N}])?`)

// tokenSet is the words and the exact tokens of a text. An exact token is one
// with a digit in it (a number, a code, an identifier, a date): it is compared
// whole, never stemmed. Words are the letter runs of the other tokens.
type tokenSet struct {
	words map[string]bool
	exact map[string]bool
}

func tokenize(s string) tokenSet {
	ts := tokenSet{words: map[string]bool{}, exact: map[string]bool{}}
	for _, tok := range tokenPattern.FindAllString(foldText(s), -1) {
		if strings.ContainsAny(tok, "0123456789") {
			ts.exact[tok] = true
			continue
		}
		for _, w := range strings.FieldsFunc(tok, func(r rune) bool { return !unicode.IsLetter(r) }) {
			if len(w) >= 3 {
				ts.words[w] = true
			}
		}
	}
	return ts
}

// stopWords are dropped from a question before it is matched against a
// sentence. They are the closed-class words of English and Italian that no
// question is about; the list is short on purpose.
var stopWords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`
		the and for are was were what which who whom whose when where why how does did can could would should
		may might must shall have has had been being with without from into onto over under about after before
		that this these those there their them they your you our any all each some such than then also not
		per via its it's how many much long
		il lo la le gli del dello della dei degli delle dal dallo dalla dai dagli dalle nel nello nella nei negli
		nelle sul sullo sulla sui sugli sulle con per tra fra che chi cui come dove quando quanto quanti quanta
		quante quale quali cosa fare sono era erano stato stati essere hanno ha non uno una dei degli anche
		sia siano entro dopo prima mentre qual dell dall nell sull all alla alle agli allo dalle
	`) {
		m[w] = true
	}
	return m
}()

// contentWords returns the words of s that can carry a question.
func contentWords(ts tokenSet) []string {
	var out []string
	for w := range ts.words {
		if !stopWords[w] {
			out = append(out, w)
		}
	}
	return out
}

// wordMatch reports whether two folded words are one word for the purpose of
// matching a question to a sentence: equal, or sharing a prefix of at least
// five letters (or of the whole shorter word when it is four letters). There is
// no dictionary: this is the cheapest thing that lets "warranty" meet
// "warranties" and "fattura" meet "fatture".
func wordMatch(a, b string) bool {
	if a == b {
		return true
	}
	short := min(len(a), len(b))
	if short < 4 {
		return false
	}
	need := min(5, short)
	return commonPrefix(a, b) >= need
}

func commonPrefix(a, b string) int {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

// unit is a piece of a passage a claim can be: a sentence, a list line or a
// table row. Context is what the row of a table is about and the row itself does
// not say: the header of its table.
type unit struct {
	text    string
	context string
}

// splitUnits cuts a passage into the units a claim can be: sentences, the rows
// of a table and the lines of a list. Markdown headings and the separator row of
// a table are dropped; the pipes of a table row are not part of its text; the
// header row of a table is not a unit (it is the context of the rows under it).
func splitUnits(passage string) []unit {
	var out []unit
	header := ""
	lastRow := -1 // index in out of the table row just before this line, if it was one
	for _, line := range strings.Split(passage, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			header, lastRow = "", -1
			continue
		case strings.HasPrefix(line, "#"):
			header, lastRow = "", -1
			continue
		case strings.HasPrefix(line, "|"):
			if isTableRule(line) {
				// The row just above is the header of this table.
				if lastRow >= 0 {
					header = out[lastRow].text
					out = append(out[:lastRow], out[lastRow+1:]...)
				}
				lastRow = -1
				continue
			}
			cells := strings.Split(strings.Trim(line, "|"), "|")
			for i := range cells {
				cells[i] = strings.TrimSpace(cells[i])
			}
			out = append(out, unit{text: strings.Join(cells, " | "), context: header})
			lastRow = len(out) - 1
			continue
		}
		header, lastRow = "", -1
		start := 0
		runes := []rune(line)
		for i, r := range runes {
			if (r == '.' || r == '!' || r == '?') && (i == len(runes)-1 || runes[i+1] == ' ') {
				if s := strings.TrimSpace(string(runes[start : i+1])); s != "" {
					out = append(out, unit{text: s})
				}
				start = i + 1
			}
		}
		if s := strings.TrimSpace(string(runes[start:])); s != "" {
			out = append(out, unit{text: s})
		}
	}
	return out
}

func isTableRule(line string) bool {
	for _, r := range line {
		switch r {
		case '|', '-', ':', ' ':
		default:
			return false
		}
	}
	return true
}

// clipRunes cuts s to at most n runes.
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
