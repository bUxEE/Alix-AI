package query

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// Deps are the collaborators of a Pipeline. Lexical and Catalog are required;
// everything else is optional and its absence is a labelled degradation, never
// an error.
type Deps struct {
	// Lexical is the authorised lexical index (*index.Index).
	Lexical Lexical
	// Catalog says which sources the principal may read and what each one's
	// declaration says about egress.
	Catalog SourceCatalog
	// Vectors is the vector index. Nil means FTS-only.
	Vectors Vectors
	// Hits reads the chunks the vector stage found. Nil means: use Lexical if
	// it implements HitReader, else drop vector-only candidates (labelled).
	Hits HitReader
	// Providers holds the Embedder and Reranker roles (and the Router role for a
	// ModelRewriter). Nil means no embedder and no reranker.
	Providers *providers.Registry
	// EmbeddingModel is the component identifier of the embedding model the
	// index generations were built with (vector.Pipeline.Model of the ingestion
	// side). A query vector is only compared with vectors of this model. Empty
	// means: the model the embedder reports.
	EmbeddingModel string
	// Quarantine lists the derived indexes that were isolated (internal/upgrade's
	// Quarantine). An isolated index is not searched and the result says so:
	// the vector stage is skipped and labelled reduced_semantic_recall, and with
	// the lexical index isolated the query runs on the vector index alone or, if
	// that cannot run either, fails with unavailable: an empty package means
	// "nothing authorised matched", never "an index was down". Nil means nothing
	// is isolated.
	Quarantine Quarantine
	// Rewriter proposes alternative queries when Config.Rewrite is on. Nil
	// means the no-model VariantRewriter.
	Rewriter Rewriter
	// Clock is the injected clock; nil means clock.Real.
	Clock clock.Clock
}

// Pipeline is the authorised hybrid retrieval pipeline of master plan 12.4. It
// is safe for concurrent use and holds no per-query state: there is no cache,
// so there is nothing keyed by scope that could be served to the wrong one.
type Pipeline struct {
	d    Deps
	cfg  Config
	clk  clock.Clock
	ids  *ids.Generator
	hits HitReader

	// hook is a test seam called between stages with the stage's name; nil in
	// production. "before_recheck" runs after reranking and before the final
	// authorisation recheck: the window in which a source can be revoked.
	hook func(stage string)
}

// New returns a Pipeline. The configuration is validated: nothing is clamped.
func New(d Deps, cfg Config) (*Pipeline, error) {
	if d.Lexical == nil {
		return nil, contracts.New(contracts.ErrInternal, "query: no lexical index")
	}
	if d.Catalog == nil {
		return nil, contracts.New(contracts.ErrInternal, "query: no source catalog: authorisation cannot be established")
	}
	if err := cfg.Validate(); err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "query: invalid configuration")
	}
	clk := d.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	hits := d.Hits
	if hits == nil {
		if hr, ok := d.Lexical.(HitReader); ok {
			hits = hr
		}
	}
	if cfg.Rewrite && d.Rewriter == nil {
		d.Rewriter = VariantRewriter{}
	}
	return &Pipeline{d: d, cfg: cfg, clk: clk, ids: ids.NewGenerator(clk), hits: hits}, nil
}

// Config returns the configuration the pipeline runs with.
func (p *Pipeline) Config() Config { return p.cfg }

// run is the state of one query.
type run struct {
	p      *Pipeline
	caller context.Context // the caller's context, to tell their cancellation from our deadlines
	scope  contracts.Scope
	req    Request
	text   string

	maxItems, maxToks int
	started           time.Time

	diag    Diagnostics
	methods []rag.RetrievalMethod
	notices []rag.RetrievalNotice

	// lexIso and vecIso: the index is in the quarantine (read once per query).
	lexIso, vecIso bool
}

// Query runs the pipeline for one question in the caller's scope.
//
// Stages, in the order of master plan 12.4:
//
//  1. deterministic filters: the scope, the sources the principal may read
//     (intersected with the request's), hidden text excluded, the question
//     reduced to its identifiers and at most MaxQueryTerms plain terms;
//  2. authorised FTS5/BM25 retrieval, with exact identifiers pinned first,
//     running beside the query embedding and
//  3. authorised vector retrieval, when an embedder and a vector index exist;
//  4. Reciprocal Rank Fusion of the lists (and, for a low-confidence search,
//     of the lists of up to MaxRewrites alternative queries);
//  5. optional parent expansion, bounded in lookups and size;
//  6. optional reranking of a short list (Reranker role; off in Lite);
//  7. the final authorisation and version recheck: index.Recheck for every
//     chunk and a fresh read of the source catalog, just before the package is
//     built;
//  8. the bounded evidence package.
//
// It fails closed. An empty package means that nothing authorised matched; a
// search that could not establish that (a full-text timeout, a failed catalog
// read, a failed recheck, a cancelled context) returns an error with a typed
// code and never an empty package. The optional stages degrade and say so in
// the package's notices and in Result.Diagnostics.
func (p *Pipeline) Query(ctx context.Context, scope contracts.Scope, req Request) (Result, error) {
	if err := scope.Validate(); err != nil {
		return Result{}, err
	}
	text, err := checkQuestion(req.Text)
	if err != nil {
		return Result{}, err
	}
	if len(req.SourceIDs) > MaxRequestSources {
		return Result{}, contracts.Newf(contracts.ErrValidationFailed, "query: %d source ids is over the limit of %d", len(req.SourceIDs), MaxRequestSources)
	}
	maxItems, maxToks, err := p.limits(req)
	if err != nil {
		return Result{}, err
	}
	caller := ctx
	ctx, cancel := context.WithTimeout(ctx, p.cfg.TotalTimeout)
	defer cancel()

	r := &run{p: p, caller: caller, scope: scope, req: req, text: text, maxItems: maxItems, maxToks: maxToks, started: p.clk.Now()}
	res, err := r.do(ctx)
	if err != nil {
		return Result{}, err
	}
	res.Diagnostics.Total = p.clk.Now().Sub(r.started)
	return res, nil
}

// MaxQuestionChars and MaxRequestSources are the schema's bounds of a search
// request (schemas/rag/search-request).
const (
	MaxQuestionChars  = 2000
	MaxRequestSources = 32
)

func checkQuestion(text string) (string, error) {
	switch {
	case !utf8.ValidString(text):
		return "", contracts.New(contracts.ErrValidationFailed, "query: the question is not valid UTF-8")
	case strings.TrimSpace(text) == "":
		return "", contracts.New(contracts.ErrValidationFailed, "query: the question is empty")
	case utf8.RuneCountInString(text) > MaxQuestionChars:
		return "", contracts.Newf(contracts.ErrValidationFailed, "query: the question is over %d characters", MaxQuestionChars)
	case strings.ContainsRune(text, 0):
		return "", contracts.New(contracts.ErrValidationFailed, "query: the question contains a NUL")
	}
	return text, nil
}

// limits resolves the request's caps against the configured ones: a request may
// lower them, never raise them.
func (p *Pipeline) limits(req Request) (items, toks int, err error) {
	items, toks = p.cfg.MaxItems, p.cfg.MaxToks
	if req.MaxItems < 0 || req.MaxToks < 0 {
		return 0, 0, contracts.New(contracts.ErrValidationFailed, "query: a negative cap")
	}
	if req.MaxItems > 0 {
		if req.MaxItems > p.cfg.MaxItems {
			return 0, 0, contracts.Newf(contracts.ErrValidationFailed, "query: max_items %d is over the configured %d", req.MaxItems, p.cfg.MaxItems)
		}
		items = req.MaxItems
	}
	if req.MaxToks > 0 {
		if req.MaxToks > p.cfg.MaxToks {
			return 0, 0, contracts.Newf(contracts.ErrValidationFailed, "query: max_toks %d is over the configured %d", req.MaxToks, p.cfg.MaxToks)
		}
		toks = req.MaxToks
	}
	return items, toks, nil
}

func (r *run) elapsed() time.Duration { return r.p.clk.Now().Sub(r.started) }

func (r *run) stageHook(name string) {
	if r.p.hook != nil {
		r.p.hook(name)
	}
}

func (r *run) degrade(reasons ...string) { r.diag.Degraded = append(r.diag.Degraded, reasons...) }

// readQuarantine reads once which of the indexes are isolated. An isolated index
// would answer with nothing, which this package must never present as "no match".
func (r *run) readQuarantine() {
	q := r.p.d.Quarantine
	if q == nil {
		return
	}
	if r.lexIso = q.Has(LexicalIndexName); r.lexIso {
		r.diag.Isolated = append(r.diag.Isolated, LexicalIndexName)
	}
	if r.vecIso = q.Has(VectorIndexName); r.vecIso {
		r.diag.Isolated = append(r.diag.Isolated, VectorIndexName)
	}
}

func (r *run) addMethod(m rag.RetrievalMethod) {
	for _, have := range r.methods {
		if have == m {
			return
		}
	}
	r.methods = append(r.methods, m)
}

func (r *run) addNotice(n rag.RetrievalNotice) {
	for _, have := range r.notices {
		if have == n {
			return
		}
	}
	r.notices = append(r.notices, n)
}

// stageError turns an error of a stage into the pipeline's typed error. The
// caller's own cancellation wins; then our deadlines (the stage's, then the
// total) become timeout; a typed error passes through; anything else is
// internal. It never returns nil.
func (r *run) stageError(stage string, total, sctx context.Context, err error) error {
	switch {
	case r.caller.Err() != nil:
		return providers.ContextError(r.caller.Err())
	case errors.Is(sctx.Err(), context.DeadlineExceeded):
		return contracts.Wrap(contracts.ErrTimeout, context.DeadlineExceeded, "query: the "+stage+" stage exceeded its time budget").
			WithDetails("stage", stage)
	case errors.Is(total.Err(), context.DeadlineExceeded):
		return contracts.Wrap(contracts.ErrTimeout, context.DeadlineExceeded, "query: the total time budget ended in the "+stage+" stage").
			WithDetails("stage", stage)
	}
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return err
	}
	return contracts.Wrap(contracts.ErrInternal, err, "query: the "+stage+" stage failed")
}

// within runs fn so that the call returns when ctx ends, whether or not fn
// honours it. A provider or index that ignores its context cannot hold the
// query past its budget: the abandoned call finishes on its own goroutine and
// its result is dropped.
func within[T any](ctx context.Context, fn func(context.Context) (T, error)) (T, error) {
	type out struct {
		v   T
		err error
	}
	ch := make(chan out, 1)
	go func() {
		v, err := fn(ctx)
		ch <- out{v, err}
	}()
	select {
	case o := <-ch:
		return o.v, o.err
	case <-ctx.Done():
		select { // prefer a result that is already there
		case o := <-ch:
			return o.v, o.err
		default:
		}
		var zero T
		return zero, ctx.Err()
	}
}

// do is the pipeline body.
func (r *run) do(ctx context.Context) (Result, error) {
	p := r.p
	r.readQuarantine()

	// 1. Deterministic filters: which sources may be searched at all.
	ver, err := p.d.Lexical.Version(ctx, r.scope)
	if err != nil {
		return Result{}, r.stageError("version", ctx, ctx, err)
	}
	r.diag.Version = ver
	activeGen := activeGenerations(ver)
	sources, allAuthorised, err := r.authorisedSources(ctx, activeGen)
	if err != nil {
		return Result{}, err
	}
	if len(sources.ids) == 0 {
		// Nothing authorised: an empty package, and nothing was searched. It is
		// still labelled when the semantic stage could not have run.
		r.addMethod(rag.RetrievalMethodFts)
		r.diag.Mode = ModeFTSOnly
		if why := r.embedderGap(); why != "" {
			r.degrade(why)
			r.addNotice(rag.RetrievalNoticeReducedSemanticRecall)
		}
		// Nothing was searched, but an isolated index is still said to be isolated.
		if r.lexIso {
			r.degrade(DegradedLexicalIsolated)
			r.addNotice(rag.RetrievalNoticeReducedSemanticRecall)
		}
		if r.vecIso {
			r.degrade(DegradedVectorIsolated)
			r.addNotice(rag.RetrievalNoticeReducedSemanticRecall)
		}
		return r.build(nil, nil, false), nil
	}
	var filter []string
	if !allAuthorised {
		// The index applies the filter inside the search, however many sources it
		// names, so unauthorised hits never use up candidate slots. Every hit is
		// still checked against the authority afterwards (sourceSet.has in
		// candidates) and again by the final recheck.
		filter = sources.ids
	}
	var gens []string
	for _, id := range sources.ids {
		gens = append(gens, activeGen[id])
	}
	r.stageHook("authorised")

	// 2 and 3. FTS and the embedding + vector search run side by side: the
	// embedder is the slow part of the vector stage and does not need FTS.
	var (
		vecOut vecOutcome
		wg     sync.WaitGroup
	)
	vctx, vcancel := context.WithCancel(ctx)
	defer vcancel()
	wg.Add(1)
	go func() {
		defer wg.Done()
		vecOut = r.vectorStage(vctx, ctx, gens)
	}()
	var (
		ftsHits []index.Hit
		ftsErr  error
	)
	if r.lexIso {
		// Never search an isolated index: it was emptied, so it would answer
		// "nothing matched". The vector stage still runs, and the result is labelled.
		r.degrade(DegradedLexicalIsolated)
		r.addNotice(rag.RetrievalNoticeReducedSemanticRecall)
	} else {
		ftsHits, ftsErr = r.ftsStage(ctx, r.text, filter, &r.diag.FTS)
	}
	if ftsErr != nil {
		vcancel()
	}
	wg.Wait()
	if ftsErr != nil {
		return Result{}, ftsErr
	}
	if vecOut.err != nil {
		return Result{}, vecOut.err
	}
	r.diag.Embed, r.diag.Vector = vecOut.embedTook, vecOut.searchTook
	r.degrade(vecOut.degraded...)
	if r.lexIso && !vecOut.ran {
		// Both retrieval paths are down. An empty package would claim that nothing
		// authorised matched; the truth is that nothing could be searched.
		return Result{}, contracts.New(contracts.ErrUnavailable,
			"query: the full-text index is isolated and the vector stage could not run: search is unavailable until the index is rebuilt").
			WithDetails("isolated", r.diag.Isolated)
	}
	r.diag.FTSCandidates = len(ftsHits)
	r.diag.VectorCandidates = len(vecOut.hits)
	if !r.lexIso {
		r.addMethod(rag.RetrievalMethodFts)
	}
	if vecOut.ran {
		r.addMethod(rag.RetrievalMethodVector)
	}
	if len(vecOut.degraded) > 0 {
		r.addNotice(rag.RetrievalNoticeReducedSemanticRecall)
	}
	r.diag.Mode = ModeFTSOnly
	switch {
	case r.lexIso:
		r.diag.Mode = ModeVectorOnly
	case vecOut.ran && len(vecOut.degraded) == 0:
		r.diag.Mode = ModeHybrid
	}

	cs := newCandidates()
	ftsKeys := cs.addFTS(r, ftsHits, sources)
	vecKeys := cs.addVector(r, vecOut.hits, sources)
	lists := [][]string{ftsKeys, vecKeys}
	r.stageHook("retrieved")

	// A low-confidence search may try alternative queries.
	extra, err := r.rewritePass(ctx, cs, sources, filter, len(ftsKeys))
	if err != nil {
		return Result{}, err
	}
	lists = append(lists, extra...)
	if r.diag.Rewrites > 0 {
		// The notice says the rewrite ran inside its budget, whether or not it
		// found anything the first pass had not.
		r.addNotice(rag.RetrievalNoticeQueryRewritten)
	}

	// 4. Reciprocal Rank Fusion, exact identifiers pinned first.
	fused := Fuse(p.cfg.RRFK, lists...)
	ordered := cs.order(fused)
	r.diag.Fused = len(ordered)
	cutByShortlist := len(ordered) > p.cfg.Shortlist
	if cutByShortlist {
		ordered = ordered[:p.cfg.Shortlist]
	}
	r.diag.Shortlisted = len(ordered)

	if err := r.hydrate(ctx, ordered); err != nil {
		return Result{}, err
	}
	ordered = dropUnhydrated(ordered, &r.diag)
	r.stageHook("fused")

	// 5. Optional parent expansion.
	if p.cfg.ParentExpansion && p.cfg.MaxParentLookups > 0 {
		if ordered, err = r.expand(ctx, ordered); err != nil {
			return Result{}, err
		}
	}
	r.stageHook("expanded")

	// 6. Optional reranking.
	if ordered, err = r.rerank(ctx, ordered); err != nil {
		return Result{}, err
	}

	// 7. Final authorisation and version recheck, then 8. the package.
	r.stageHook("before_recheck")
	survivors, states, left, err := r.finalRecheck(ctx, ordered)
	if err != nil {
		return Result{}, err
	}
	r.diag.DroppedBudget += left
	return r.build(survivors, states, cutByShortlist || left > 0), nil
}

// activeGenerations parses Version.Active ("<source id>=<generation id>").
func activeGenerations(v index.Version) map[string]string {
	out := make(map[string]string, len(v.Active))
	for _, kv := range v.Active {
		if src, gen, ok := strings.Cut(kv, "="); ok && src != "" && gen != "" {
			out[src] = gen
		}
	}
	return out
}

// sourceSet is the sources a query may search, in a stable order.
type sourceSet struct {
	ids   []string
	state map[string]SourceState
}

func (s sourceSet) has(id string) bool { _, ok := s.state[id]; return ok }

// authorisedSources reads the catalog for the sources of the scope's active
// generations and keeps those the principal may read and that were not removed,
// intersected with the request's own filter. allAuthorised is true when the
// result is every active source and the request named none: the full-text stage
// then needs no source filter.
func (r *run) authorisedSources(ctx context.Context, active map[string]string) (set sourceSet, allAuthorised bool, err error) {
	set = sourceSet{state: map[string]SourceState{}}
	if len(active) == 0 {
		return set, true, nil
	}
	all := make([]string, 0, len(active))
	for id := range active {
		all = append(all, id)
	}
	sort.Strings(all)
	states, err := r.p.d.Catalog.Sources(ctx, r.scope, all)
	if err != nil {
		return set, false, r.catalogError(ctx, err)
	}
	wanted := map[string]bool{}
	for _, id := range r.req.SourceIDs {
		wanted[id] = true
	}
	for _, id := range all {
		st, ok := states[id]
		if !ok || !readable(st) {
			continue
		}
		if len(wanted) > 0 && !wanted[id] {
			continue
		}
		set.ids = append(set.ids, id)
		set.state[id] = st
	}
	return set, len(set.ids) == len(all) && len(wanted) == 0, nil
}

// readable is the authorisation of a source for evidence: the principal may read
// it and it was not removed.
func readable(st SourceState) bool {
	return st.Authorised && st.Availability != conversation.CitationAvailabilityRemoved
}

func (r *run) catalogError(ctx context.Context, err error) error {
	if r.caller.Err() != nil {
		return providers.ContextError(r.caller.Err())
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return contracts.Wrap(contracts.ErrTimeout, context.DeadlineExceeded, "query: the total time budget ended reading the source catalog")
	}
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return err
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, "query: the source catalog could not be read; authorisation is not established")
}
