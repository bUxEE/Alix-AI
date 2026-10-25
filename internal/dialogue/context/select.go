package dctx

import (
	"math/bits"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/sessions"
)

// This file is relevant-turn selection: which of the rows of the recent window are
// replayed. It is deterministic and cheap (set intersections over short rows); there is
// no embedding and no model.
//
// A row is eligible for one of three reasons, in this priority:
//
//	0  recent      one of the newest RecentRows rows: the answer to the last question
//	               needs the question, whether or not they share a word with it
//	1  referenced  the row belongs to a turn the state points at: the turn that set a
//	               value of the goal, or the turn of the last directive
//	2  lexical     the row shares a word with this turn: the message, the goal's
//	               capability, the values it holds, the field it waits for
//
// Every other row is not replayed. Within a class the order is
// newest first (classes 0 and 1) or best score first, newest first (class 2). The best
// MaxTurnRows are kept; trimming under budget removes from the end of this ranking, so
// the oldest lexical match goes before the newest recent row.

// rowClass is the reason a row is replayed; lower is more important.
type rowClass int

const (
	classRecent rowClass = iota
	classReferenced
	classLexical
)

// candRow is a row of the window with the reason it is eligible.
type candRow struct {
	turn sessions.Turn
	idx  int // position in the window, oldest first

	// item and toks are the rendered row and its estimated tokens, set once the row is selected
	item  Item
	toks  int
	class rowClass
	score int
}

// maxTerms bounds the words a turn is matched on.
const maxTerms = 64

// stopwords are words of four or more letters that carry no topic in English or Italian.
// Shorter words never count (see words).
var stopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`
		this that with have from what when where which will would could should about there their them they
		please thanks thank also just like want need make take give tell show here some more than then
		your yours mine been being does done into over only very
		vorrei voglio questo questa quello quella questi queste della delle degli dello dalla dalle nella nelle
		anche come dove quando quale quali sono stato stata fare fatto essere grazie prego perche pero
		allora ancora subito favore potrei posso vuole`) {
		m[w] = true
	}
	return m
}()

// words returns the distinct lower-case words of s of at least four letters or digits,
// without stopwords, in first-seen order and at most limit of them (0 means no limit).
func words(s string, limit int) []string {
	var out []string
	seen := map[string]bool{}
	var cur []rune
	// add closes the word being read and reports whether more words may follow.
	add := func() bool {
		defer func() { cur = cur[:0] }()
		if len(cur) < 4 {
			return true
		}
		w := string(cur)
		if stopwords[w] || seen[w] {
			return true
		}
		seen[w] = true
		out = append(out, w)
		return limit <= 0 || len(out) < limit
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur = append(cur, unicode.ToLower(r))
			continue
		}
		if !add() {
			return out
		}
	}
	add()
	return out
}

// terms returns the words this turn is about: the message first, then the goal's
// capability and the field it waits for, then the values it holds. Slot values are shown
// only where the caller allows (vis), like everywhere else.
func terms(utterance string, st *sessions.State, vis Visible) termSet {
	var src []string
	src = append(src, utterance)
	if g := st.Goal; g != nil && vis.capability(g.CapabilityID) {
		src = append(src, strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(g.CapabilityID))
		if g.Pending != nil {
			src = append(src, strings.ReplaceAll(g.Pending.Field, "_", " "))
		}
		for _, name := range sortedSlotNames(g) {
			if vis.field(g.CapabilityID, name) {
				src = append(src, slotText(g.Slots[name].Value))
			}
		}
	}
	out := termSet{}
	for _, s := range src {
		for _, w := range words(s, maxTerms) {
			if len(out) >= maxTerms {
				return out
			}
			if _, dup := out[w]; !dup {
				out[w] = len(out)
			}
		}
	}
	return out
}

// termSet is the words a turn is matched on, each with its bit index (below maxTerms).
type termSet map[string]int

// scoreRow counts the distinct terms of want that body contains. It reads body once
// without allocating: a word is looked up as it is read, and the distinct matches are
// the bits of one word.
func scoreRow(body string, want termSet) int {
	if len(want) == 0 {
		return 0
	}
	var (
		buf      [64]byte // a word longer than this cannot be a term
		n, runes int
		long     bool
		mask     uint64
	)
	for _, r := range body {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			r = unicode.ToLower(r)
			if n+utf8.RuneLen(r) > len(buf) {
				long = true
			} else {
				n += utf8.EncodeRune(buf[n:], r)
			}
			runes++
			continue
		}
		if runes >= 4 && !long {
			if i, ok := want[string(buf[:n])]; ok {
				mask |= 1 << uint(i)
			}
		}
		n, runes, long = 0, 0, false
	}
	if runes >= 4 && !long {
		if i, ok := want[string(buf[:n])]; ok {
			mask |= 1 << uint(i)
		}
	}
	return bits.OnesCount64(mask)
}

// referencedTurns are the turn IDs the state points at: the turns that set a visible
// slot of the active goal and the turn of the last directive.
func referencedTurns(st *sessions.State, vis Visible) map[string]bool {
	out := map[string]bool{}
	if g := st.Goal; g != nil && vis.capability(g.CapabilityID) {
		for name, sl := range g.Slots {
			if sl.TurnID != "" && vis.field(g.CapabilityID, name) {
				out[sl.TurnID] = true
			}
		}
	}
	if l := st.Last; l != nil && l.TurnID != "" {
		out[l.TurnID] = true
	}
	return out
}

// selectRows ranks the eligible rows of the window, best first, and keeps at most
// p.MaxTurnRows. Only the newest p.CandidateRows rows of the window are looked at: the
// work is bounded by the profile, not by what the caller passed.
func selectRows(p Profile, window []sessions.Turn, refs map[string]bool, want termSet) []candRow {
	if len(window) > p.CandidateRows {
		window = window[len(window)-p.CandidateRows:]
	}
	var cands []candRow
	for i, t := range window {
		c := candRow{turn: t, idx: i}
		switch {
		case i >= len(window)-p.RecentRows:
			c.class = classRecent
		case refs[t.TurnID]:
			c.class = classReferenced
		default:
			c.score = scoreRow(t.Body, want)
			if c.score == 0 {
				continue
			}
			c.class = classLexical
		}
		cands = append(cands, c)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		switch {
		case a.class != b.class:
			return a.class < b.class
		case a.score != b.score:
			return a.score > b.score
		}
		return a.idx > b.idx
	})
	if len(cands) > p.MaxTurnRows {
		cands = cands[:p.MaxTurnRows]
	}
	return cands
}

// inOrder returns the first k ranked rows in conversation order, oldest first.
func inOrder(ranked []candRow, k int) []candRow {
	if k > len(ranked) {
		k = len(ranked)
	}
	out := append([]candRow(nil), ranked[:k]...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].idx < out[j].idx })
	return out
}
