package query

import (
	"context"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// rerank reorders the head of the fused shortlist with the Reranker role. It is
// optional in every direction: it is off unless Config.Rerank is set (it is off
// in the Lite profile even when a reranker is registered, master plan 7.1), and
// a missing, unhealthy, slow or misbehaving reranker leaves the fused order
// unchanged (master plan 18: "Reranker unavailable: use fused retrieval order")
// and is recorded.
//
// The reranker sees the question and the text of at most RerankCandidates child
// chunks, all of which are already authorised for the scope; it can only
// reorder them. The documents are the child passages, not the expanded
// sections, so one call stays inside a compact reranker's budget whatever
// sections were read. Candidates that hold an exact identifier of the question
// stay first: a score never outranks a deterministic match.
func (r *run) rerank(total context.Context, ordered []*cand) ([]*cand, error) {
	cfg := r.p.cfg
	if !cfg.Rerank || len(ordered) < 2 {
		return ordered, nil
	}
	reg := r.p.d.Providers
	if reg == nil || !reg.Has(providers.RoleReranker) || !reg.Usable(providers.RoleReranker) {
		r.degrade(DegradedNoReranker)
		return ordered, nil
	}
	rr, err := providers.Lookup[providers.Reranker](reg, providers.RoleReranker)
	if err != nil {
		r.degrade(DegradedNoReranker)
		return ordered, nil
	}
	n := min(len(ordered), cfg.RerankCandidates)
	docs := make([]string, n)
	for i := 0; i < n; i++ {
		docs[i] = ordered[i].hit.Text
	}
	req := providers.RerankRequest{Query: r.text, Documents: docs}

	t0 := r.p.clk.Now()
	defer func() { r.diag.Rerank = r.p.clk.Now().Sub(t0) }()
	sctx, cancel := context.WithTimeout(total, cfg.RerankTimeout)
	defer cancel()
	res, err := within(sctx, func(c context.Context) (providers.RerankResult, error) { return rr.Rerank(c, r.scope, req) })
	if err == nil {
		res, err = res.Normalize(req) // indexes in range and unique, scores finite
	}
	if err != nil {
		switch {
		case r.caller.Err() != nil:
			return nil, providers.ContextError(r.caller.Err())
		case fatalVectorError(err):
			return nil, err
		}
		r.degrade(DegradedRerankFailed)
		return ordered, nil
	}

	out := make([]*cand, 0, len(ordered))
	used := make([]bool, n)
	for _, h := range res.Hits {
		used[h.Index] = true
		out = append(out, ordered[h.Index])
	}
	for i := 0; i < n; i++ { // documents the reranker did not score keep their place after the scored ones
		if !used[i] {
			out = append(out, ordered[i])
		}
	}
	out = append(out, ordered[n:]...)
	r.addMethod(rag.RetrievalMethodRerank)
	r.diag.Reranked = true
	return pinExact(out), nil
}
