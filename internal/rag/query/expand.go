package query

import (
	"context"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/index"
)

func parentKey(h *index.Hit) string {
	return h.GenerationID + "\x00" + h.DocumentID + "\x00" + h.ParentID
}

// expand replaces a hit by the section it sits under, so the model reads the
// passage in its context. It is bounded three ways, all from the resource
// budget: at most MaxParentLookups sections are read per query (a lookup that
// finds nothing counts), a section larger than MaxParentToks keeps the child,
// and a section must be strictly larger than the child it replaces. Walking the
// candidates in rank order, a lower-ranked child of a section that already
// replaced a better child is dropped: its text is in the package already.
//
// The candidate keeps its own chunk ID and generation (the final recheck
// compares them); only the text and the locator are the section's. A section
// that holds hidden text is never returned by the index and is not used here.
// A failure to read a section degrades to the child text and is recorded; only
// the caller's cancellation and a scope failure are errors.
func (r *run) expand(total context.Context, ordered []*cand) ([]*cand, error) {
	cfg := r.p.cfg
	t0 := r.p.clk.Now()
	defer func() { r.diag.Expand = r.p.clk.Now().Sub(t0) }()
	sctx, cancel := context.WithTimeout(total, cfg.FTSTimeout)
	defer cancel()

	expanded := map[string]bool{}
	out := make([]*cand, 0, len(ordered))
	stopped := false
	for _, c := range ordered {
		h := c.hit
		if h.ParentID == "" {
			out = append(out, c)
			continue
		}
		pk := parentKey(h)
		if expanded[pk] {
			r.diag.DroppedCovered++
			continue
		}
		if !stopped && r.diag.ParentLookups < cfg.MaxParentLookups {
			r.diag.ParentLookups++
			pr, err := within(sctx, func(c2 context.Context) (parentResult, error) {
				p, ok, err := r.p.d.Lexical.ParentOf(c2, r.scope, *h, false)
				return parentResult{p, ok}, err
			})
			parent, ok := pr.p, pr.ok
			switch {
			case err != nil && r.caller.Err() != nil:
				return nil, providers.ContextError(r.caller.Err())
			case err != nil && fatalVectorError(err):
				return nil, err
			case err != nil:
				r.degrade(DegradedParentFailed)
				stopped = true
			case ok && usableParent(parent, h, cfg.MaxParentToks):
				pp := parent
				c.parent = &pp
				expanded[pk] = true
				r.diag.Expanded++
			}
		}
		out = append(out, c)
	}
	return out, nil
}

type parentResult struct {
	p  index.Parent
	ok bool
}

// usableParent decides whether a section may replace its child. Every field is
// compared with the child's, so a section of another generation, source or
// document is never used.
func usableParent(p index.Parent, h *index.Hit, maxToks int) bool {
	switch {
	case p.Hidden, p.Text == "":
		return false
	case p.GenerationID != h.GenerationID || p.SourceID != h.SourceID || p.DocumentID != h.DocumentID || p.ID != h.ParentID:
		return false
	case p.EstToks > maxToks || p.EstToks <= h.EstToks:
		return false
	case utf8.RuneCountInString(p.Text) > chunk.MaxChunkChars:
		return false
	}
	return true
}
