package query

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// maxVectorBatches bounds the vector searches one query may make when the scope
// has more active generations than one search may name (vector.MaxGenerations).
const maxVectorBatches = 8

// ftsStage runs authorised BM25 retrieval for text and reports how long it took
// in *took. The text is first reduced to the pipeline's term cap. The stage
// runs under FTSTimeout and the total budget; a stage that outlives it is a
// typed timeout (an empty result would claim that nothing matched).
func (r *run) ftsStage(total context.Context, text string, filter []string, took *time.Duration) ([]index.Hit, error) {
	reduced := reduceQuery(text, r.p.cfg.MaxQueryTerms)
	if reduced == "" {
		return nil, nil
	}
	sctx, cancel := context.WithTimeout(total, r.p.cfg.FTSTimeout)
	defer cancel()
	t0 := r.p.clk.Now()
	res, err := within(sctx, func(c context.Context) (index.Result, error) {
		return r.p.d.Lexical.Search(c, r.scope, index.Query{
			Text:          reduced,
			Limit:         r.p.cfg.FTSCandidates,
			SourceIDs:     filter,
			IncludeHidden: false,
		})
	})
	*took += r.p.clk.Now().Sub(t0)
	if err != nil {
		return nil, r.stageError("fts", total, sctx, err)
	}
	return res.Hits, nil
}

// embedderGap says why a query embedding cannot be made at all, or "" when an
// embedder is registered and usable. It is the cheap, call-free part of the
// vector stage's preconditions.
func (r *run) embedderGap() string {
	reg := r.p.d.Providers
	switch {
	case reg == nil || !reg.Has(providers.RoleEmbedder):
		return DegradedNoEmbedder
	case !reg.Usable(providers.RoleEmbedder):
		return DegradedEmbedderDown
	case r.p.d.Vectors == nil:
		return DegradedNoVectorIndex
	}
	return ""
}

// vecOutcome is what the vector branch hands back.
type vecOutcome struct {
	hits       []vector.Hit
	ran        bool     // at least one generation was searched
	degraded   []string // why the stage did not fully contribute
	err        error    // a failure that must fail the query (caller cancelled, scope violation)
	embedTook  time.Duration
	searchTook time.Duration
}

// fatalVectorError reports whether err must fail the whole query instead of
// degrading the vector stage: the caller's own cancellation and anything that
// says the scope was violated or missing. Authorisation failures are never
// "degraded around".
func fatalVectorError(err error) bool {
	return contracts.HasCode(err, contracts.ErrScopeViolation) || contracts.HasCode(err, contracts.ErrScopeMissing)
}

// vectorStage embeds the question and searches the active generations of the
// authorised sources. Everything that can go wrong here degrades to FTS-only
// and is reported; only the caller's cancellation and a scope failure are
// errors. vctx is cancelled when the full-text stage has already failed.
func (r *run) vectorStage(vctx, total context.Context, gens []string) (out vecOutcome) {
	p := r.p
	reg := p.d.Providers
	switch {
	case r.vecIso:
		// Isolated: its vectors were dropped, so a search would find nothing. Skip it
		// and say so.
		out.degraded = append(out.degraded, DegradedVectorIsolated)
		return out
	case reg == nil || !reg.Has(providers.RoleEmbedder):
		out.degraded = append(out.degraded, DegradedNoEmbedder)
		return out
	case !reg.Usable(providers.RoleEmbedder):
		out.degraded = append(out.degraded, DegradedEmbedderDown)
		return out
	case p.d.Vectors == nil:
		out.degraded = append(out.degraded, DegradedNoVectorIndex)
		return out
	case len(gens) == 0:
		return out
	}
	emb, err := providers.Lookup[providers.Embedder](reg, providers.RoleEmbedder)
	if err != nil {
		out.degraded = append(out.degraded, DegradedNoEmbedder)
		return out
	}

	// Embed the question as asked. The embedder receives the caller's scope and
	// this text, nothing else.
	req := providers.EmbedRequest{Inputs: []string{r.text}, Purpose: providers.EmbedQuery}
	ectx, ecancel := context.WithTimeout(vctx, p.cfg.EmbedTimeout)
	defer ecancel()
	t0 := p.clk.Now()
	er, err := within(ectx, func(c context.Context) (providers.EmbedResult, error) { return emb.Embed(c, r.scope, req) })
	out.embedTook = p.clk.Now().Sub(t0)
	if err == nil {
		err = er.CheckResult(req)
	}
	if err != nil {
		return r.vectorFailure(out, total, vctx, err, DegradedEmbedFailed)
	}
	model := p.d.EmbeddingModel
	switch {
	case model == "":
		model = er.Model
	case er.Model != "" && er.Model != model:
		out.degraded = append(out.degraded, DegradedModelMismatch)
		return out
	}
	if model == "" {
		out.degraded = append(out.degraded, DegradedModelMismatch)
		return out
	}

	sctx, scancel := context.WithTimeout(vctx, p.cfg.VectorTimeout)
	defer scancel()
	t1 := p.clk.Now()
	defer func() { out.searchTook = p.clk.Now().Sub(t1) }()
	var all []vector.Hit
	searched := 0
	skipped := false
	for b := 0; b*vector.MaxGenerations < len(gens); b++ {
		if b >= maxVectorBatches {
			skipped = true
			break
		}
		lo, hi := b*vector.MaxGenerations, min((b+1)*vector.MaxGenerations, len(gens))
		q := vector.Query{Vector: er.Vectors[0], Model: model, K: p.cfg.VectorCandidates, Generations: gens[lo:hi]}
		sr, err := within(sctx, func(c context.Context) (vector.SearchResult, error) { return p.d.Vectors.Search(c, r.scope, q) })
		if err != nil {
			out = r.vectorFailure(out, total, vctx, err, DegradedVectorFailed)
			break
		}
		all = append(all, sr.Hits...)
		searched += (hi - lo) - len(sr.Skipped)
		if len(sr.Skipped) > 0 {
			skipped = true
		}
	}
	if out.err != nil {
		return out
	}
	if skipped {
		out.degraded = append(out.degraded, DegradedGenerationsSkipped)
	}
	out.ran = searched > 0
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Score != all[j].Score {
			return all[i].Score > all[j].Score
		}
		return all[i].ChunkID < all[j].ChunkID
	})
	if len(all) > p.cfg.VectorCandidates {
		all = all[:p.cfg.VectorCandidates]
	}
	// The list is best first, so the floor cuts a suffix.
	for i, h := range all {
		if float64(h.Score) < p.cfg.VectorMinScore {
			all = all[:i]
			break
		}
	}
	out.hits = all
	return out
}

// vectorFailure classifies a failure of the vector branch: fatal for the
// caller's cancellation and scope errors, silent when the branch was cancelled
// because the full-text stage failed (that error is reported instead), and a
// labelled degradation for everything else.
func (r *run) vectorFailure(out vecOutcome, total, vctx context.Context, err error, reason string) vecOutcome {
	switch {
	case r.caller.Err() != nil:
		out.err = providers.ContextError(r.caller.Err())
	case fatalVectorError(err):
		out.err = err
	case errors.Is(vctx.Err(), context.Canceled) && total.Err() == nil:
		// cancelled by the pipeline itself; the FTS error is the one reported
	default:
		out.degraded = append(out.degraded, reason)
	}
	return out
}

// hydrate reads the chunks that only the vector stage found, from the lexical
// index, so they have text, a span, a document version and a parent. Without a
// HitReader (or when it fails) those candidates are dropped and the result is
// labelled reduced_semantic_recall.
func (r *run) hydrate(total context.Context, ordered []*cand) error {
	var need []*cand
	for _, c := range ordered {
		if c.hit == nil {
			need = append(need, c)
		}
	}
	if len(need) == 0 {
		return nil
	}
	if r.p.hits == nil {
		r.degrade(DegradedNoHitReader)
		r.addNotice(rag.RetrievalNoticeReducedSemanticRecall)
		return nil
	}
	t0 := r.p.clk.Now()
	defer func() { r.diag.Hydrate = r.p.clk.Now().Sub(t0) }()
	refs := make([]index.Ref, len(need))
	want := make(map[string]*cand, len(need))
	for i, c := range need {
		refs[i] = c.ref
		want[c.key] = c
	}
	sctx, cancel := context.WithTimeout(total, r.p.cfg.VectorTimeout)
	defer cancel()
	hits, err := within(sctx, func(c context.Context) ([]index.Hit, error) { return r.p.hits.Hits(c, r.scope, refs, false) })
	if err != nil {
		if r.caller.Err() != nil {
			return providers.ContextError(r.caller.Err())
		}
		if fatalVectorError(err) {
			return err
		}
		r.degrade(DegradedHydrateFailed)
		r.addNotice(rag.RetrievalNoticeReducedSemanticRecall)
		return nil
	}
	for i := range hits {
		h := hits[i]
		c := want[candKey(h.GenerationID, h.ChunkID)]
		if c == nil || c.hit != nil || h.Hidden || h.SourceID != c.source {
			// Not what was asked for (or not the source the vector index named):
			// refuse it rather than trust it.
			continue
		}
		c.hit = &h
		c.ref = index.RefOf(h)
	}
	return nil
}

// rewritePass runs the alternative queries of a low-confidence search and
// returns their ranked lists for fusion. A search is low-confidence when its
// first full-text pass left fewer than RewriteBelowHits authorised candidates
// (found). Only the full-text list counts: a rewrite is more full-text passes,
// and the vector stage always returns its top K however poor the match, so its
// count says nothing about confidence. The pass is
// bounded three ways: at most MaxRewrites alternatives (one full-text pass
// each), the rewriter under RewriteTimeout, and no rewrite at all once half of
// the total time budget is spent. A failed rewrite is a labelled degradation;
// the first pass's result stands.
func (r *run) rewritePass(total context.Context, cs *candidates, sources sourceSet, filter []string, found int) ([][]string, error) {
	cfg := r.p.cfg
	if r.lexIso || !cfg.Rewrite || found >= cfg.RewriteBelowHits || r.p.d.Rewriter == nil {
		return nil, nil // a rewrite is more full-text passes: none over an isolated index
	}
	if r.elapsed() >= cfg.TotalTimeout/2 || total.Err() != nil {
		return nil, nil
	}
	t0 := r.p.clk.Now()
	defer func() { r.diag.Rewrite = r.p.clk.Now().Sub(t0) }()
	rctx, cancel := context.WithTimeout(total, cfg.RewriteTimeout)
	defer cancel()
	alts, err := within(rctx, func(c context.Context) ([]string, error) {
		return r.p.d.Rewriter.Rewrite(c, r.scope, r.text, cfg.MaxRewrites)
	})
	if err != nil {
		if r.caller.Err() != nil {
			return nil, providers.ContextError(r.caller.Err())
		}
		if fatalVectorError(err) {
			return nil, err
		}
		r.degrade(DegradedRewriteFailed)
		return nil, nil
	}
	alts = cleanAlternatives(r.text, alts, cfg.MaxRewrites)
	var lists [][]string
	for _, alt := range alts {
		if r.elapsed() >= cfg.TotalTimeout || total.Err() != nil {
			break
		}
		var took time.Duration
		hits, err := r.ftsStage(total, alt, filter, &took)
		r.diag.FTS += took
		if err != nil {
			if r.caller.Err() != nil || total.Err() != nil || fatalVectorError(err) {
				return nil, err
			}
			r.degrade(DegradedRewriteFailed)
			break
		}
		r.diag.Rewrites++
		lists = append(lists, cs.addRewrite(r, hits, sources))
	}
	return lists, nil
}
