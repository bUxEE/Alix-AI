package query

import (
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/vector"
)

// cand is one chunk that some retrieval stage found. Its identity is the
// generation and the chunk ID (a chunk ID is deterministic, so the same text in
// two generations has one chunk ID).
type cand struct {
	key    string
	ref    index.Ref
	source string
	// hit is the chunk as the lexical index reads it. Nil for a candidate that
	// only the vector stage found, until hydrate fills it.
	hit *index.Hit
	// exact: the chunk holds an identifier of the question exactly.
	exact bool

	// parent is the section the chunk was expanded to, when it was.
	parent *index.Parent
}

func candKey(generation, chunk string) string { return generation + "\x00" + chunk }

type candidates struct {
	byKey map[string]*cand
}

func newCandidates() *candidates { return &candidates{byKey: map[string]*cand{}} }

// addFTS registers lexical hits (best first) and returns their keys in order.
// A hit of a source the principal may not read is dropped here, before it can
// reach fusion; the index applied the scope already, so this is the finer,
// source-level authority applied a second time, deterministically.
func (cs *candidates) addFTS(r *run, hits []index.Hit, sources sourceSet) []string {
	return cs.addLexical(r, hits, sources)
}

func (cs *candidates) addRewrite(r *run, hits []index.Hit, sources sourceSet) []string {
	return cs.addLexical(r, hits, sources)
}

func (cs *candidates) addLexical(r *run, hits []index.Hit, sources sourceSet) []string {
	keys := make([]string, 0, len(hits))
	for i := range hits {
		h := hits[i]
		if !sources.has(h.SourceID) {
			r.diag.DroppedUnauthorised++
			continue
		}
		if h.Hidden {
			// Hidden text is never evidence (SEC-012); the index excludes it,
			// and this is the same rule applied a second time.
			r.diag.DroppedInvalid++
			continue
		}
		key := candKey(h.GenerationID, h.ChunkID)
		c := cs.byKey[key]
		if c == nil {
			c = &cand{key: key, ref: index.RefOf(h), source: h.SourceID}
			cs.byKey[key] = c
		}
		if c.hit == nil {
			hh := h
			c.hit = &hh
		}
		if len(h.ExactIdentifiers) > 0 {
			c.exact = true
		}
		keys = append(keys, key)
	}
	return keys
}

// addVector registers vector hits (best first) and returns their keys in order.
func (cs *candidates) addVector(r *run, hits []vector.Hit, sources sourceSet) []string {
	keys := make([]string, 0, len(hits))
	for _, h := range hits {
		if !sources.has(h.SourceID) {
			r.diag.DroppedUnauthorised++
			continue
		}
		key := candKey(h.GenerationID, h.ChunkID)
		c := cs.byKey[key]
		if c == nil {
			c = &cand{key: key, ref: index.Ref{ChunkID: h.ChunkID, GenerationID: h.GenerationID}, source: h.SourceID}
			cs.byKey[key] = c
		}
		keys = append(keys, key)
	}
	return keys
}

// order returns the candidates in fused order with the exact-identifier matches
// pinned first.
func (cs *candidates) order(fused []Fused) []*cand {
	out := make([]*cand, 0, len(fused))
	for _, f := range fused {
		if c := cs.byKey[f.Key]; c != nil {
			out = append(out, c)
		}
	}
	return pinExact(out)
}

// pinExact moves the candidates that hold an identifier of the question exactly
// ahead of the rest, each group keeping its order. An exact identifier is a
// deterministic match (master plan 12.4 lists it before any retrieval), and no
// fusion or reranking score outranks it.
func pinExact(in []*cand) []*cand {
	out := make([]*cand, 0, len(in))
	for _, c := range in {
		if c.exact {
			out = append(out, c)
		}
	}
	for _, c := range in {
		if !c.exact {
			out = append(out, c)
		}
	}
	return out
}

// dropUnhydrated removes the candidates that still have no chunk, counting them.
func dropUnhydrated(in []*cand, d *Diagnostics) []*cand {
	out := in[:0:0]
	for _, c := range in {
		if c.hit == nil {
			d.DroppedUnhydrated++
			continue
		}
		out = append(out, c)
	}
	return out
}
