package answer

import (
	"strings"
	"unicode/utf8"
)

// A claim can be fully supported by a passage and still not be an answer: a model
// that is handed the renewals passage for "capital of France" and states what the
// passage says has written supported claims, and none of them says anything about
// France. Support is provenance; this is relevance, the cheapest check of it there
// is: an answer must share at least one thing with the question it answers (a content
// term, a number, an identifier). It is checked for the answer as a whole, not claim
// by claim: a claim that supports the one that answers ("renewals are refused when
// the book is reserved", for a question about renewing) does not have to repeat the
// question's words itself.
//
// The check is lexical, like the claim check, and has the same limit: a question and a
// source in different languages share no terms, and a question with no content term
// at all ("what is it?") cannot be compared; the check does not apply to such a
// question, and the answer is judged by provenance alone, as before.

// metaStems are the stems of words that talk about asking and answering and not about
// a subject: a question that carries "permitted to answer any question" shares
// "answer" and "question" with a sentence that says the same, whatever it is about,
// and "tell me everything" names no subject at all. They are not what makes a claim
// relevant, and a question made of nothing else is not compared (see above).
var metaStems = func() map[string]struct{} {
	m := map[string]struct{}{}
	for _, w := range []string{
		"answer", "answers", "question", "questions", "reply", "respond", "ask", "tell", "know", "want", "need", "please",
		"everything", "anything", "something", "summary", "summarise", "summarize", "explain", "describe", "overview",
		"information", "info", "detail", "details",
		"risposta", "risposte", "rispondere", "rispondi", "domanda", "domande", "chiedere", "sapere", "volere", "vorrei",
		"dimmi", "dire", "parlami", "raccontami", "tutto", "niente", "qualcosa", "riassunto", "riassumi", "spiega", "descrivi", "panoramica", "informazioni", "dettagli",
	} {
		m[stem(w)] = struct{}{}
	}
	return m
}()

// topic is what a question is about: its content stems (without the meta words),
// numbers and identifiers.
type topic struct {
	stems  []string
	nums   [][]string
	idents []string
}

// newTopic reads a question. It reports false when the question holds nothing to
// compare a claim with.
func newTopic(question string) (topic, bool) {
	a := analyze(question)
	var t topic
	for _, s := range a.stems {
		if _, meta := metaStems[s]; !meta {
			t.stems = append(t.stems, s)
		}
	}
	t.nums, t.idents = a.nums, a.idents
	return t, len(t.stems)+len(t.nums)+len(t.idents) > 0
}

// sameStem reports whether two stems are one word: equal, or one at least four runes
// long and a prefix of the other ("renew" and "renewal", "rinnov" and "rinnovabil").
func sameStem(a, b string) bool {
	if a == b {
		return true
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return utf8.RuneCountInString(a) >= 4 && strings.HasPrefix(b, a)
}

// shares reports whether the claim has a content term, number or identifier in
// common with the topic.
func (t topic) shares(c analysis) bool {
	for _, qs := range t.stems {
		for _, cs := range c.stems {
			if sameStem(qs, cs) {
				return true
			}
		}
	}
	for _, qi := range t.idents {
		for _, ci := range c.idents {
			if qi == ci {
				return true
			}
		}
	}
	for _, qn := range t.nums {
		for _, cn := range c.nums {
			for _, qr := range qn {
				for _, cr := range cn {
					if qr == cr {
						return true
					}
				}
			}
		}
	}
	return false
}

// answersQuestion reports whether any of the claims has something in common with the
// question. A question there is nothing to compare with is answered by anything that
// passed verification.
func answersQuestion(question string, kept []keptClaim) bool {
	t, ok := newTopic(question)
	if !ok {
		return true
	}
	for _, k := range kept {
		if t.shares(k.terms) {
			return true
		}
	}
	return false
}
