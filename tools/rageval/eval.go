package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/rag/answer"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// Retrieval modes.
const (
	ModeFTS    = "fts"    // full-text only: no embedder, no vector index, no reranker
	ModeHybrid = "hybrid" // full-text and vectors, fused by RRF
	ModeRerank = "rerank" // hybrid and the reranker over the fused shortlist
)

// EvalConfig is what the labelled evaluation needs.
type EvalConfig struct {
	Mode  string
	K     []int
	Query query.Config
	Ans   answer.Config
	// Embedder and Model serve the vector stage (nil for ModeFTS).
	Embedder providers.Embedder
	Model    string
	// Gen proposes the claims of an answer.
	Gen providers.TextGenerator
	// Warmup questions are run first and left out of every metric.
	Warmup int
	// TempDir is where the throwaway database goes ("" is the system's).
	TempDir string

	// wrapCatalog, when set, wraps the source catalog the pipeline and the
	// answerer use. It is a test seam (a catalog that grants too much must show
	// up as a leak in the judge, which reads the ground truth from the
	// manifest); the command line cannot set it.
	wrapCatalog func(query.SourceCatalog) query.SourceCatalog
}

// EvalResult is the outcome of the labelled evaluation.
type EvalResult struct {
	Records       []QuestionRecord
	Ingest        IngestResult
	Builds        int // generations built (a source with two versions is built twice)
	Store         StoreSize
	Vectors       int
	Duration      time.Duration // the measured phase: every question, warm-up excluded
	WarmupFirstMS *float64
	IngestWall    time.Duration
	ModeCounts    map[string]int
	DegradedCount map[string]int
}

// chunkRef says which document a chunk (child or parent section) belongs to.
type chunkRef struct{ source, doc string }

// RunEval ingests the corpus into a throwaway database, asks every labelled
// question through the query pipeline and the answer package, and judges the
// outcomes against the labels.
func RunEval(ctx context.Context, corpus *Corpus, cfg EvalConfig, progress func(done, total int)) (*EvalResult, error) {
	if err := cfg.check(); err != nil {
		return nil, err
	}
	dbPath, cleanup, err := newTempDB(cfg.TempDir)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	hybrid := cfg.Mode != ModeFTS
	so := StackOptions{Path: dbPath}
	if hybrid {
		so.Embedder, so.Model = cfg.Embedder, cfg.Model
	}
	stack, err := OpenStack(ctx, so)
	if err != nil {
		return nil, err
	}
	defer stack.Close()

	catalog := newCorpusCatalog()
	j := &judge{corpus: corpus, srcKey: map[string]string{}, chunks: map[string]chunkRef{}}
	res := &EvalResult{ModeCounts: map[string]int{}, DegradedCount: map[string]int{}}

	// Ingest every source, version by version; the last version is the one that
	// serves. A deleted source is deleted after it has been indexed, in the
	// index (its chunks are hidden at once) and in the catalog.
	ingestStart := time.Now()
	record := func(sourceKey string, d Doc, r chunk.Result) {
		for _, c := range r.Children {
			j.chunks[c.ID] = chunkRef{source: sourceKey, doc: d.ID}
		}
		for _, c := range r.Parents {
			j.chunks[c.ID] = chunkRef{source: sourceKey, doc: d.ID}
		}
	}
	for i := range corpus.Manifest.Sources {
		def := &corpus.Manifest.Sources[i]
		tenant, _ := corpus.Tenant(def.Tenant)
		scope := scopeFor(corpus, tenant, tenant.Principals[0])
		srcID := newSourceID()
		j.srcKey[srcID] = def.Key
		var last string
		for _, v := range def.Versions {
			docs := make([]Doc, len(v.Documents))
			for k, d := range v.Documents {
				docs[k] = Doc{ID: d.ID, Text: corpus.Doc(d.File)}
			}
			// The hook is keyed by the source's manifest key, not its src_ id.
			r, err := stack.Ingest(ctx, scope, srcID, v.Version, docs, IngestHooks{OnChunks: func(_ string, d Doc, cr chunk.Result) {
				record(def.Key, d, cr)
			}})
			if err != nil {
				return nil, fmt.Errorf("source %s version %s: %w", def.Key, v.Version, err)
			}
			res.Ingest.Add(r)
			res.Builds++
			last = v.Version
		}
		catalog.register(srcID, catalogEntry{
			key: def.Key, name: def.Name, tenantID: tenant.TenantID, workspace: tenant.WorkspaceID,
			readers: def.Readers, state: def.State, version: last,
		})
		if def.State == StateDeleted {
			if _, err := stack.Ix.DeleteSource(ctx, scope, srcID); err != nil {
				return nil, fmt.Errorf("deleting source %s from the index: %w", def.Key, err)
			}
			if stack.Flat != nil {
				if _, err := stack.Flat.DeleteSource(ctx, scope, srcID); err != nil {
					return nil, fmt.Errorf("deleting source %s from the vector index: %w", def.Key, err)
				}
			}
		}
	}
	res.IngestWall = time.Since(ingestStart)
	if res.Store, err = stack.StoreSize(ctx); err != nil {
		return nil, err
	}
	if res.Vectors, err = countVectors(ctx, stack, corpus); err != nil {
		return nil, err
	}

	// The pipeline and the answerer.
	reg := providers.NewRegistry(clock.Real{})
	var cat query.SourceCatalog = catalog
	if cfg.wrapCatalog != nil {
		cat = cfg.wrapCatalog(cat)
	}
	deps := query.Deps{Lexical: stack.Ix, Catalog: cat}
	qcfg := cfg.Query
	if hybrid {
		if err := reg.Register(providers.RoleEmbedder, cfg.Embedder); err != nil {
			return nil, err
		}
		deps.Providers, deps.EmbeddingModel, deps.Vectors = reg, cfg.Model, stack.Flat
	}
	if cfg.Mode == ModeRerank {
		if err := reg.Register(providers.RoleReranker, lexicalReranker{}); err != nil {
			return nil, err
		}
		qcfg.Rerank = true
	}
	pipe, err := query.New(deps, qcfg)
	if err != nil {
		return nil, err
	}
	ans, err := answer.New(answer.Deps{Generator: cfg.Gen, Catalog: cat}, cfg.Ans)
	if err != nil {
		return nil, err
	}

	ask := func(q Question) (QuestionRecord, error) {
		return j.ask(ctx, pipe, ans, q, cfg.K)
	}
	for i := 0; i < cfg.Warmup; i++ {
		w := startWatch()
		if _, err := ask(corpus.Questions[i%len(corpus.Questions)]); err != nil {
			return nil, err
		}
		if i == 0 {
			res.WarmupFirstMS = round3(durMS(w.elapsed()))
		}
	}
	start := time.Now()
	for i, q := range corpus.Questions {
		rec, err := ask(q)
		if err != nil {
			return nil, err
		}
		res.Records = append(res.Records, rec)
		if rec.Status == StatusOK {
			res.ModeCounts[rec.Mode]++
			for _, d := range rec.Degraded {
				res.DegradedCount[d]++
			}
		}
		if progress != nil {
			progress(i+1, len(corpus.Questions))
		}
	}
	res.Duration = time.Since(start)
	return res, nil
}

func (c EvalConfig) check() error {
	switch {
	case c.Mode != ModeFTS && c.Mode != ModeHybrid && c.Mode != ModeRerank:
		return fmt.Errorf("unknown mode %q", c.Mode)
	case c.Mode != ModeFTS && (c.Embedder == nil || c.Model == ""):
		return fmt.Errorf("mode %s needs an embedder and its model id", c.Mode)
	case c.Gen == nil:
		return fmt.Errorf("no answer generator")
	case len(c.K) == 0:
		return fmt.Errorf("no k")
	case c.Warmup < 0:
		return fmt.Errorf("a negative warm-up")
	}
	for _, k := range c.K {
		if k < 1 || k > c.Query.MaxItems {
			return fmt.Errorf("k %d is outside 1..%d, the most items an evidence package holds", k, c.Query.MaxItems)
		}
	}
	return nil
}

// scopeFor is the verified scope of a principal of a tenant of the corpus.
func scopeFor(c *Corpus, t *TenantDef, principal string) contracts.Scope {
	return contracts.Scope{
		DeploymentID:     "dep_rageval",
		ApplicationID:    c.Manifest.Application,
		TenantID:         t.TenantID,
		WorkspaceID:      t.WorkspaceID,
		PrincipalID:      principal,
		Roles:            []string{"member"},
		CapabilityScopes: []string{"rag.search", "rag.answer"},
		SessionID:        "ses_rageval",
	}
}

func countVectors(ctx context.Context, s *Stack, c *Corpus) (int, error) {
	if s.Flat == nil {
		return 0, nil
	}
	total := 0
	for i := range c.Manifest.Tenants {
		t := &c.Manifest.Tenants[i]
		st, err := s.Flat.Stats(ctx, scopeFor(c, t, t.Principals[0]))
		if err != nil {
			return 0, err
		}
		total += int(st.Vectors)
	}
	return total, nil
}

func durMS(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// stageNames are the stages of the retrieval pipeline that a result reports.
var stageNames = []string{"fts", "embed", "vector", "hydrate", "expand", "rerank", "recheck"}

// stageMS is the pipeline's own timing of a query, per stage, in milliseconds.
func stageMS(d query.Diagnostics) map[string]float64 {
	v := map[string]time.Duration{
		"fts": d.FTS, "embed": d.Embed, "vector": d.Vector, "hydrate": d.Hydrate,
		"expand": d.Expand, "rerank": d.Rerank, "recheck": d.Recheck,
	}
	out := make(map[string]float64, len(v))
	for name, dur := range v {
		out[name] = *round3(durMS(dur))
	}
	return out
}

// judge turns the outcome of one question into a QuestionRecord.
type judge struct {
	corpus *Corpus
	srcKey map[string]string   // src_ id -> manifest key
	chunks map[string]chunkRef // chunk id (child or parent) -> its document
}

// ask runs one question and judges it. It returns an error only when the run
// itself cannot continue (the context ended); a failure of the pipeline is a
// recorded outcome.
func (j *judge) ask(ctx context.Context, pipe *query.Pipeline, ans *answer.Answerer, q Question, ks []int) (QuestionRecord, error) {
	tenant, _ := j.corpus.Tenant(q.Tenant)
	scope := scopeFor(j.corpus, tenant, q.Principal)

	w0 := startWatch()
	qres, qerr := pipe.Query(ctx, scope, query.Request{Text: q.Text})
	retrieval := w0.elapsed()
	if qerr != nil {
		if ctx.Err() != nil {
			return QuestionRecord{}, fmt.Errorf("stopped: %w", ctx.Err())
		}
		return j.failed(q, qerr), nil
	}
	w1 := startWatch()
	ares, aerr := ans.Answer(ctx, scope, qres.Package)
	answerTime := w1.elapsed()
	if aerr != nil {
		if ctx.Err() != nil {
			return QuestionRecord{}, fmt.Errorf("stopped: %w", ctx.Err())
		}
		return j.failed(q, aerr), nil
	}
	rec := j.judgeOutcome(q, qres, ares)
	rec.StagesMS = stageMS(qres.Diagnostics)
	rec.RetrievalLatencyMS = round3(durMS(retrieval))
	rec.AnswerLatencyMS = round3(durMS(answerTime))
	rec.LatencyMS = round3(durMS(retrieval + answerTime))
	return rec, nil
}

// failed is the record of a question whose pipeline returned an error: a
// failure of every metric it touches, never skipped.
func (j *judge) failed(q Question, err error) QuestionRecord {
	code := string(contracts.CodeOf(err))
	return QuestionRecord{
		ID: q.ID, Language: q.Language, Kind: q.Kind, Outcome: q.Outcome,
		Status: StatusError, ErrorCode: &code,
		Expected: len(q.Expect), Ranked: [][]int{}, Degraded: []string{}, Citations: []CitationRecord{},
		FactsTotal: len(q.Facts), StagesMS: map[string]float64{},
		LabelOK: labelFailed(q),
	}
}

func labelFailed(q Question) *bool {
	if q.Citation == nil {
		return nil
	}
	f := false
	return &f
}

func (j *judge) judgeOutcome(q Question, qres query.Result, ares answer.Result) QuestionRecord {
	pkg := qres.Package
	rec := QuestionRecord{
		ID: q.ID, Language: q.Language, Kind: q.Kind, Outcome: q.Outcome,
		Status:   StatusOK,
		Expected: len(q.Expect),
		Mode:     string(qres.Diagnostics.Mode),
		Degraded: append([]string{}, qres.Diagnostics.Degraded...),
		Ranked:   make([][]int, len(pkg.Items)), Citations: []CitationRecord{},
		FactsTotal: len(q.Facts),
		ToksIn:     ares.Diagnostics.TokIn, ToksOut: ares.Diagnostics.TokOut,
	}
	asker, _ := j.corpus.Tenant(q.Tenant)

	// The evidence package: which entries each item hits, and whether anything
	// in it is out of bounds for the asker.
	forbiddenSrc := map[string]bool{}
	for _, k := range q.ForbiddenSources {
		forbiddenSrc[k] = true
	}
	var texts []string
	for i, it := range pkg.Items {
		rec.PackageToksEst += int(it.EstToks)
		texts = append(texts, it.Text)
		key := j.srcKey[string(it.SourceID)]
		ref := j.chunks[string(it.ChunkID)]
		hits := []int{}
		for e, ev := range q.Expect {
			if key == ev.Source && ref.doc == ev.Doc && ref.source == ev.Source && holdsAll(it.Text, ev.Anchors) {
				hits = append(hits, e)
			}
		}
		rec.Ranked[i] = hits
		if forbiddenSrc[key] || !j.mayRead(asker, q.Principal, key) {
			rec.ForbiddenSourceHits++
		}
	}

	// The answer.
	var claimText strings.Builder
	switch v := ares.Answer.Value.(type) {
	case rag.NoSupportedEvidence:
		rec.Declined, rec.Reason = true, string(v.Reason)
	case rag.GroundedAnswer:
		rec.Claims = len(v.Claims)
		for _, c := range v.Claims {
			claimText.WriteString(c.Text)
			claimText.WriteByte('\n')
		}
		for _, c := range v.Citations {
			key := j.srcKey[string(c.SourceID)]
			if forbiddenSrc[key] || !j.mayRead(asker, q.Principal, key) {
				rec.ForbiddenSourceHits++
			}
			cr := CitationRecord{
				Source: key, Version: string(c.Version),
				Availability: string(c.Availability), Staleness: string(c.Staleness),
			}
			cr.Correct = citationCorrect(c.SourceID, c.Locator, pkg.Items, rec.Ranked)
			rec.Citations = append(rec.Citations, cr)
		}
	}
	claims := claimText.String()
	for _, f := range q.Facts {
		if containsFold(claims, f) {
			rec.FactsHit++
		}
	}
	for _, f := range q.Forbidden {
		found := containsFold(claims, f)
		for _, t := range texts {
			if found {
				break
			}
			found = containsFold(t, f)
		}
		if found {
			rec.ForbiddenHits++
		}
	}
	rec.Leak = rec.ForbiddenSourceHits > 0 || rec.ForbiddenHits > 0
	rec.LabelOK = labelOK(q, rec)

	switch q.Outcome {
	case OutcomeDecline:
		rec.Correct = rec.Declined && !rec.Leak
	default:
		anyCorrect := false
		for _, c := range rec.Citations {
			anyCorrect = anyCorrect || c.Correct
		}
		rec.Correct = !rec.Declined && rec.FactsHit == rec.FactsTotal && !rec.Leak && anyCorrect &&
			(rec.LabelOK == nil || *rec.LabelOK)
	}
	return rec
}

// holdsAll reports whether text holds every anchor.
func holdsAll(text string, anchors []string) bool {
	for _, a := range anchors {
		if !containsFold(text, a) {
			return false
		}
	}
	return true
}

// mayRead is the ground truth of authorisation, from the manifest alone: the
// source must be the asker's tenant's, not deleted, and readable by the
// principal. The catalog under test applies the same rule from its own state;
// the judge applies it from the manifest, so a catalog that grants too much
// shows up as a leak.
func (j *judge) mayRead(t *TenantDef, principal, sourceKey string) bool {
	def, ok := j.corpus.Source(sourceKey)
	if !ok {
		return false // a source the manifest does not know is nobody's
	}
	if def.Tenant != t.Key || def.State == StateDeleted {
		return false
	}
	return len(def.Readers) == 0 || contains(def.Readers, principal)
}

// spanKey is the code-point extent of a citation locator.
type spanKey struct{ start, end int64 }

func locatorSpan(l *conversation.CitationLocator) (spanKey, bool) {
	if l == nil || l.Span == nil {
		return spanKey{}, false
	}
	return spanKey{l.Span.Start, l.Span.End}, true
}

// citationCorrect reports whether the passage a citation points at hits an
// entry of the label: the item of the package with the citation's source and
// span.
func citationCorrect(src actions.SourceID, loc *conversation.CitationLocator, items []rag.EvidenceItem, ranked [][]int) bool {
	cs, hasSpan := locatorSpan(loc)
	for i, it := range items {
		if it.SourceID != src {
			continue
		}
		if hasSpan {
			if is, ok := locatorSpan(it.Locator); !ok || is != cs {
				continue
			}
		}
		if len(ranked[i]) > 0 {
			return true
		}
	}
	return false
}

// labelOK judges the citation label of a question: some citation of the
// labelled source, and every citation of it, carries the labelled version,
// availability and staleness.
func labelOK(q Question, rec QuestionRecord) *bool {
	if q.Citation == nil {
		return nil
	}
	l := q.Citation
	n, bad := 0, 0
	for _, c := range rec.Citations {
		if l.Source != "" && c.Source != l.Source {
			continue
		}
		n++
		if (l.Version != "" && c.Version != l.Version) ||
			(l.Availability != "" && c.Availability != l.Availability) ||
			(l.Staleness != "" && c.Staleness != l.Staleness) {
			bad++
		}
	}
	ok := n > 0 && bad == 0
	return &ok
}
