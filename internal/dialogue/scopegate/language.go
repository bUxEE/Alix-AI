package scopegate

import (
	"sort"
	"strings"
)

// Language reading. The gate refuses only what it can read: an utterance in a language that
// the catalogue's own wording (or its application-supplied terms) does not cover shares no
// words with the catalogue whether or not it is related, so "no overlap" says nothing about
// it and the gate abstains (ReasonUnreadableLanguage), leaving the turn to the controller.
//
// The detector is a closed-class one: it counts function words that exist in exactly one of
// the two languages the product declares (English and Italian). A word that is a function
// word in both ("a", "in", "no", "so", "me", "i", "come", "per", "con") is in neither list,
// and so is any word a name could be. It is a counting rule and never reads a model.

const (
	// LangEN and LangIT are the languages the gate can read.
	LangEN = "en"
	LangIT = "it"
)

var enMarkers = words(`
	the and of to is are was were been you your my what who whom whose when where why how which that this these those
	with from for into about can could would should will shall does did have has had not it its we they he she our their
	any some there here please an by but or if at on up out than then them
`)

var itMarkers = words(`
	il lo la le gli un una uno di del dello della dei delle dell al alla alle allo nel nella nelle sul sulla dal dalla
	che chi cosa dove quando quanto quanti quale quali perche sono sei ho hai hanno mi ti ci vi si mio mia miei mie
	tuo tua suo sua nostro vostro questo questa questi queste quello quella anche ma ed ne non piu poi tra fra da se su
	voglio vorrei posso puoi devo puo ecco avete
`)

func words(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

// detectLanguage returns LangEN or LangIT when the tokens carry strictly more function
// words of one language than of the other, and "" when there is no evidence or a tie.
func detectLanguage(tokens []string) string {
	en, it := 0, 0
	for _, t := range tokens {
		if enMarkers[t] {
			en++
		}
		if itMarkers[t] {
			it++
		}
	}
	switch {
	case en > it:
		return LangEN
	case it > en:
		return LangIT
	}
	return ""
}

// primaryTag reduces a language tag to the languages the gate knows: "en-GB" and "EN" are
// LangEN, "it_IT" is LangIT, anything else is "" (not readable).
func primaryTag(tag string) string {
	tag = strings.ToLower(strings.TrimSpace(tag))
	if i := strings.IndexAny(tag, "-_"); i >= 0 {
		tag = tag[:i]
	}
	if tag == LangEN || tag == LangIT {
		return tag
	}
	return ""
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
