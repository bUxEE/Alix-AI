package main

import (
	"strings"
	"unicode"
)

// Normalize is the text normalisation of docs/evidence/m4-voice/protocol.md, the same for references and hypotheses: lower-case, every
// character that is not a Unicode letter or digit becomes a space (so apostrophes and hyphens separate words), runs of spaces collapse,
// the result is split into words. No numeral, spelling or accent mapping: an accent error is a word error.
func Normalize(s string) []string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	return strings.Fields(b.String())
}

// Edits is the word-level Levenshtein distance (substitution, deletion and insertion cost 1) between a reference and a hypothesis.
func Edits(ref, hyp []string) int {
	prev := make([]int, len(hyp)+1)
	cur := make([]int, len(hyp)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ref); i++ {
		cur[0] = i
		for j := 1; j <= len(hyp); j++ {
			cost := 1
			if ref[i-1] == hyp[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j-1]+cost, prev[j]+1, cur[j-1]+1)
		}
		prev, cur = cur, prev
	}
	return prev[len(hyp)]
}

// WER scores one hypothesis text against one reference text: the edits and the number of reference words.
func WER(ref, hyp string) (edits, refWords int) {
	r := Normalize(ref)
	return Edits(r, Normalize(hyp)), len(r)
}
