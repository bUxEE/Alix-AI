package answer

import (
	"math"
	"strings"
)

// termSet is the searchable form of one sentence of a passage.
type termSet struct {
	stems  map[string]struct{}
	reads  map[string]struct{}
	idents map[string]struct{}
	neg    bool
}

// passage is an evidence item's text, sanitised once, split into sentences, each
// read once. Claims are checked against windows of its sentences.
type passage struct {
	sents []string
	sets  []termSet
}

// newPassage reads text, which must already be sanitised: the text the model was
// shown is the text a claim is checked against.
func newPassage(text string) *passage {
	p := &passage{sents: sentences(text)}
	p.sets = make([]termSet, len(p.sents))
	for i, s := range p.sents {
		if looksLikeInstruction(s) {
			// An instruction aimed at the assistant is not something the passage
			// states: it cannot be what supports a claim (master plan 12.6).
			p.sets[i] = termSet{}
			continue
		}
		a := analyze(s)
		ts := termSet{
			stems:  make(map[string]struct{}, len(a.stems)),
			reads:  make(map[string]struct{}, len(a.nums)),
			idents: make(map[string]struct{}, len(a.idents)),
			neg:    a.neg,
		}
		for _, st := range a.stems {
			ts.stems[st] = struct{}{}
		}
		for _, rs := range a.nums {
			for _, r := range rs {
				ts.reads[r] = struct{}{}
			}
		}
		for _, id := range a.idents {
			ts.idents[id] = struct{}{}
		}
		p.sets[i] = ts
	}
	return p
}

// claimCheck is a claim, read once, with the number of its content terms a
// passage must contain.
type claimCheck struct {
	a    analysis
	need int
}

// newClaimCheck reads a claim. It reports false when the claim is too thin to
// check: no content word at all, or fewer terms than minTerms. Such a claim is
// not a statement a passage can support, and is dropped.
func newClaimCheck(text string, cfg Config) (claimCheck, bool) {
	a := analyze(text)
	if len(a.stems) == 0 || a.weight() < cfg.MinClaimTerms {
		return claimCheck{}, false
	}
	need := int(math.Ceil(cfg.MinCoverage*float64(len(a.stems)) - 1e-9))
	if need < 1 {
		need = 1
	}
	return claimCheck{a: a, need: need}, true
}

// support looks for a window of at most window consecutive sentences of the
// passage that supports the claim, and returns that window's text. A window
// supports a claim when
//
//   - every number, date, time and amount of the claim is in it, as one of the
//     readings of a number there (numberReadings);
//   - every identifier of the claim (a code, an address, a URL) is in it, exactly;
//   - at least MinCoverage of the claim's content terms are in it, after stemming;
//   - claim and window agree on whether there is a negation, so "refunds are not
//     available" is never supported by "refunds are available" or the reverse.
//
// The check reads lexically: it establishes that the claim is made of what the
// passage says, not that it means what the passage means. It is the cheap
// deterministic first line of master plan 12.4's claim/source verification; the
// limits are in doc.go. A claim supported only by the union of two sentences more
// than window apart is unsupported.
func (p *passage) support(c claimCheck, window int) (string, bool) {
	n := len(p.sets)
	if window < 1 {
		window = 1
	}
	covered := make([]bool, len(c.a.stems))
	numOK := make([]bool, len(c.a.nums))
	idOK := make([]bool, len(c.a.idents))
	// supports reports whether the window of sentences holds what the claim needs.
	supports := func(win []termSet) bool {
		for k := range covered {
			covered[k] = false
		}
		for k := range numOK {
			numOK[k] = false
		}
		for k := range idOK {
			idOK[k] = false
		}
		cnt, nums, ids, neg := 0, 0, 0, false
		for j := range win {
			s := &win[j]
			neg = neg || s.neg
			for k, st := range c.a.stems {
				if !covered[k] {
					if _, ok := s.stems[st]; ok {
						covered[k] = true
						cnt++
					}
				}
			}
			for k, reads := range c.a.nums {
				if numOK[k] {
					continue
				}
				for _, r := range reads {
					if _, ok := s.reads[r]; ok {
						numOK[k] = true
						nums++
						break
					}
				}
			}
			for k, id := range c.a.idents {
				if !idOK[k] {
					if _, ok := s.idents[id]; ok {
						idOK[k] = true
						ids++
					}
				}
			}
		}
		return cnt >= c.need && nums == len(c.a.nums) && ids == len(c.a.idents) && neg == c.a.neg
	}
	// The narrowest window that supports the claim, the earliest among equals: the
	// span a reader is shown is the passage that says it, not what surrounds it.
	for size := 1; size <= window; size++ {
		for i := 0; i+size <= n; i++ {
			if supports(p.sets[i : i+size]) {
				return strings.Join(p.sents[i:i+size], " "), true
			}
		}
	}
	return "", false
}
