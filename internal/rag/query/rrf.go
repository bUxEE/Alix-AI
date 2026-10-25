package query

import "sort"

// Fused is one document after Reciprocal Rank Fusion.
type Fused struct {
	// Key identifies the document across the lists.
	Key string
	// Score is the sum over the lists holding it of 1 / (k + rank), the rank
	// counted from 1. It is only meaningful inside one fusion; it never leaves
	// the package and is never shown to a client.
	Score float64
	// Ranks holds the 1-based rank in each input list, 0 where the list does
	// not hold the document. It has one entry per input list, in input order.
	Ranks []int
}

// Fuse merges ranked lists by Reciprocal Rank Fusion (Cormack, Clarke and
// Buettcher, 2009): a document's score is the sum, over every list that holds
// it, of 1 / (k + rank), with rank counted from 1. A document found by several
// lists therefore outranks one found by a single list at the same depth, and
// no list's raw scores are needed, which matters because BM25 and cosine
// similarity are not comparable.
//
// Each list is ordered best first; a key repeated inside one list keeps its
// first (best) rank only. The result is ordered by score, best first. Ties,
// which are common (two documents each first in a different list), are broken
// deterministically: by the best rank the document has in any list, then by
// the number of lists holding it, then by the order of first appearance when
// the lists are read in input order, then by key. The same input always gives
// the same output.
//
// The terms of a score are added in ascending order of rank, so two documents
// whose ranks form the same multiset get bit-identical scores whatever list
// each rank came from, and a tie is a tie, not a rounding accident.
func Fuse(k int, lists ...[]string) []Fused {
	if k < 1 {
		k = DefaultRRFK
	}
	type acc struct {
		key   string
		ranks []int
		first int // order of first appearance
	}
	byKey := map[string]*acc{}
	var order []*acc
	for li, list := range lists {
		seen := map[string]bool{}
		rank := 0
		for _, key := range list {
			if seen[key] {
				continue
			}
			seen[key] = true
			rank++
			a := byKey[key]
			if a == nil {
				a = &acc{key: key, ranks: make([]int, len(lists)), first: len(order)}
				byKey[key] = a
				order = append(order, a)
			}
			a.ranks[li] = rank
		}
	}
	out := make([]Fused, len(order))
	best := make([]int, len(order))
	held := make([]int, len(order))
	firsts := make([]int, len(order))
	for i, a := range order {
		var present []int
		for _, r := range a.ranks {
			if r > 0 {
				present = append(present, r)
			}
		}
		sort.Ints(present)
		var score float64
		for _, r := range present {
			score += 1 / float64(k+r)
		}
		out[i] = Fused{Key: a.key, Score: score, Ranks: a.ranks}
		best[i], held[i], firsts[i] = present[0], len(present), a.first
	}
	idx := make([]int, len(out))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(x, y int) bool {
		i, j := idx[x], idx[y]
		switch {
		case out[i].Score != out[j].Score:
			return out[i].Score > out[j].Score
		case best[i] != best[j]:
			return best[i] < best[j]
		case held[i] != held[j]:
			return held[i] > held[j]
		case firsts[i] != firsts[j]:
			return firsts[i] < firsts[j]
		}
		return out[i].Key < out[j].Key
	})
	sorted := make([]Fused, len(out))
	for n, i := range idx {
		sorted[n] = out[i]
	}
	return sorted
}
