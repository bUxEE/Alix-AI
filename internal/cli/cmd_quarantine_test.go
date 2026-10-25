package cli

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/history"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/upgrade"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// quarantineEmbedder is the embedding runtime of a test, which can be taken down. Every text,
// a chunk or a question, embeds to the same vector, so a question with no word in common with
// the documents is found by the vector index alone.
type quarantineEmbedder struct {
	mu   sync.Mutex
	down bool
}

func (e *quarantineEmbedder) setDown(down bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.down = down
}

func (e *quarantineEmbedder) embed(req providers.EmbedRequest) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.down {
		return nil, contracts.New(contracts.ErrUnavailable, "the embedding runtime is down")
	}
	out := make([][]float32, len(req.Inputs))
	for i := range out {
		out[i] = fake.EmbedVector("tide tables", 8)
	}
	return out, nil
}

// quarantineRig is a RAG stack with two documents ingested, over a quarantine of its own.
type quarantineRig struct {
	f   *ragFixture
	q   *upgrade.Quarantine
	emb *quarantineEmbedder
}

func newQuarantineRig(t *testing.T, withEmbedder bool) *quarantineRig {
	t.Helper()
	r := &quarantineRig{q: upgrade.NewQuarantine(t.TempDir(), nil), emb: &quarantineEmbedder{}}
	r.f = newRagFixture(t, func(o *ragFixtureOpts) {
		o.quarantine = r.q
		if withEmbedder {
			o.embedding, o.embedFunc = true, r.emb.embed
		}
	})
	r.f.write("tides.md", "# Tides\n\nThe harbour dock schedule lists the tide tables.")
	r.f.write("moorings.md", "# Moorings\n\nChains and shackles for the moorings.")
	r.f.register()
	r.f.drain()
	return r
}

func (r *quarantineRig) vectors() int { return r.f.countRows(`SELECT count(*) FROM vector_rows`) }

// isolate does to the vector index what the repair of an upgrade does to a damaged one: the record
// is written and the vectors are dropped.
func (r *quarantineRig) isolate(t *testing.T) {
	t.Helper()
	if err := r.q.Add(upgrade.VectorIndexName, "test: damaged"); err != nil {
		t.Fatal(err)
	}
	if err := upgrade.VectorDerived(nil)(r.f.db)[0].Isolate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := r.vectors(); n != 0 {
		t.Fatalf("%d vectors are left in an isolated index", n)
	}
}

func quarantineHas(res query.Result, notice rag.RetrievalNotice) bool {
	return slices.Contains(res.Package.Retrieval.Notices, notice)
}

// An isolated vector index is rebuilt by re-embedding the chunks when an embedder exists, and only
// then released, so retrieval does not stay full-text only forever; until then it says it is.
func TestIsolatedVectorIndexIsRebuiltAndReleasedWhenAnEmbedderExists(t *testing.T) {
	ctx := context.Background()
	r := newQuarantineRig(t, true)
	f := r.f
	before := r.vectors()
	if before < 2 {
		t.Fatalf("the fixture embedded %d chunks", before)
	}
	if res := f.ask("zzqxv"); res.Diagnostics.VectorCandidates == 0 || res.Diagnostics.Mode != query.ModeHybrid || len(res.Diagnostics.Isolated) != 0 {
		t.Fatalf("control: a healthy index answers a vector-only question: %+v", res.Diagnostics)
	}
	// One chunk becomes hidden after it was embedded: hidden text is never embedded (SEC-012).
	quarantineExec(t, f, `UPDATE ragindex_chunk SET hidden = 1 WHERE text LIKE '%shackles%'`)
	hidden := f.countRows(`SELECT count(*) FROM ragindex_chunk WHERE hidden = 1`)
	if hidden != 1 {
		t.Fatalf("the fixture hid %d chunks, want 1", hidden)
	}
	// The records of other indexes sit beside it and must outlive the release.
	for _, name := range []string{history.SearchIndexName, "notes_fts"} {
		if err := r.q.Add(name, "test: isolated"); err != nil {
			t.Fatal(err)
		}
	}
	r.isolate(t)

	res := f.ask("zzqxv")
	if d := res.Diagnostics; d.Mode != query.ModeFTSOnly || !slices.Equal(d.Isolated, []string{query.VectorIndexName}) || d.VectorCandidates != 0 ||
		!slices.Contains(d.Degraded, query.DegradedVectorIsolated) || !quarantineHas(res, rag.RetrievalNoticeReducedSemanticRecall) {
		t.Fatalf("an isolated vector index is not labelled: %+v notices %v", d, res.Package.Retrieval.Notices)
	}
	if got := f.ask("tide tables"); len(got.Package.Items) == 0 {
		t.Error("full-text retrieval found nothing while the vector index was isolated")
	}

	// The embedding runtime is down: nothing is released, the index stays empty and labelled.
	r.emb.setDown(true)
	released, err := f.stack.quarantineRelease(ctx, f.stack.quarantineVectorRebuild())
	if released || contracts.CodeOf(err) != contracts.ErrUnavailable || !r.q.Has(upgrade.VectorIndexName) || r.vectors() != 0 {
		t.Fatalf("with the embedder down: released %v, %v, held %v, %d vectors", released, err, r.q.Has(upgrade.VectorIndexName), r.vectors())
	}
	r.emb.setDown(false)

	// A source the process registered and does not serve now (its package is not active) holds the
	// release back: the index would otherwise be released with a hole in it.
	f.pkgs.deactivate(ragApp)
	released, err = f.stack.quarantineRelease(ctx, f.stack.quarantineVectorRebuild())
	if released || err == nil || !r.q.Has(upgrade.VectorIndexName) {
		t.Fatalf("with a source that is not served: released %v, %v, held %v", released, err, r.q.Has(upgrade.VectorIndexName))
	}
	f.pkgs.set(ragApp, ragHash)

	// A generation built with another embedding model cannot be re-embedded by this embedder.
	quarantineExec(t, f, `UPDATE vector_generations SET model_id = 'another-model'`)
	released, err = f.stack.quarantineRelease(ctx, f.stack.quarantineVectorRebuild())
	if released || contracts.CodeOf(err) != contracts.ErrConflict || !r.q.Has(upgrade.VectorIndexName) {
		t.Fatalf("with a generation of another model: released %v, %v, held %v", released, err, r.q.Has(upgrade.VectorIndexName))
	}
	quarantineExec(t, f, `UPDATE vector_generations SET model_id = 'fake-model'`)

	// The embedder is up: the chunks are embedded again, the record goes and no other does.
	released, err = f.stack.quarantineRelease(ctx, f.stack.quarantineVectorRebuild())
	if !released || err != nil {
		t.Fatalf("the re-embedding: released %v, %v", released, err)
	}
	if r.q.Has(upgrade.VectorIndexName) {
		t.Error("the vector index is still in the quarantine after it was embedded again")
	}
	for _, name := range []string{history.SearchIndexName, "notes_fts"} {
		if !r.q.Has(name) {
			t.Errorf("the record of %s was released with the vector index", name)
		}
	}
	if got := r.vectors(); got != before-hidden {
		t.Errorf("%d vectors after the re-embedding, want %d (every chunk but the hidden one)", got, before-hidden)
	}
	if n := f.countRows(`SELECT count(*) FROM vector_rows AS v JOIN ragindex_chunk AS c ON c.chunk_id = v.chunk_id WHERE c.hidden = 1`); n != 0 {
		t.Errorf("%d hidden chunks were embedded", n)
	}

	res = f.ask("zzqxv")
	if d := res.Diagnostics; d.Mode != query.ModeHybrid || d.VectorCandidates == 0 || len(d.Isolated) != 0 || quarantineHas(res, rag.RetrievalNoticeReducedSemanticRecall) || len(res.Package.Items) == 0 {
		t.Errorf("after the release retrieval is not hybrid: %+v items %d notices %v", d, len(res.Package.Items), res.Package.Retrieval.Notices)
	}
	// Released once: another pass has nothing to do and does not embed.
	if released, err := f.stack.quarantineRelease(ctx, f.stack.quarantineVectorRebuild()); released || err != nil {
		t.Errorf("a pass over an index that is not isolated: released %v, %v", released, err)
	}
}

// Without an embedder nothing can rebuild the vector index: it stays isolated and retrieval stays
// labelled, and serve says so instead of starting a loop that could never succeed.
func TestIsolatedVectorIndexStaysIsolatedAndLabelledWithoutAnEmbedder(t *testing.T) {
	r := newQuarantineRig(t, false)
	f := r.f
	if err := r.q.Add(upgrade.VectorIndexName, "test: damaged"); err != nil {
		t.Fatal(err)
	}
	if f.stack.quarantineVectorRebuild() != nil {
		t.Fatal("a stack with no embedder offers to re-embed")
	}
	released, err := f.stack.quarantineRelease(context.Background(), f.stack.quarantineVectorRebuild())
	if released || !errors.Is(err, upgrade.ErrReembedRequired) || !r.q.Has(upgrade.VectorIndexName) {
		t.Fatalf("without an embedder: released %v, %v, held %v", released, err, r.q.Has(upgrade.VectorIndexName))
	}
	res := f.ask("tide tables")
	if d := res.Diagnostics; !slices.Equal(d.Isolated, []string{query.VectorIndexName}) || !quarantineHas(res, rag.RetrievalNoticeReducedSemanticRecall) || len(res.Package.Items) == 0 {
		t.Errorf("an isolated vector index with no embedder is not labelled: %+v notices %v", d, res.Package.Retrieval.Notices)
	}

	// The provider starts no loop without an embedder.
	services := gateway.NewServices()
	if err := gateway.Provide(services, f.stack); err != nil {
		t.Fatal(err)
	}
	started := 0
	d := serveDeps{Log: slog.New(slog.DiscardHandler), Services: services, AddService: func(func(context.Context) func()) { started++ }}
	if err := quarantineProvide(context.Background(), d); err != nil || started != 0 {
		t.Errorf("quarantineProvide with no embedder: %v, %d services started", err, started)
	}
}

// With an embedder the provider runs a loop that retries a failed pass until it succeeds, and stopping
// it waits for it.
func TestQuarantineReleaseLoopRetriesUntilTheEmbedderIsUp(t *testing.T) {
	r := newQuarantineRig(t, true)
	f := r.f
	r.isolate(t)
	r.emb.setDown(true)

	oldEvery, oldPass := quarantineEvery, quarantineOnPass
	t.Cleanup(func() { quarantineEvery, quarantineOnPass = oldEvery, oldPass })
	quarantineEvery = 5 * time.Millisecond
	passes := make(chan error, 1000)
	var released = make(chan struct{}, 1)
	quarantineOnPass = func(ok bool, err error) {
		select {
		case passes <- err:
		default:
		}
		if ok {
			select {
			case released <- struct{}{}:
			default:
			}
		}
	}

	services := gateway.NewServices()
	if err := gateway.Provide(services, f.stack); err != nil {
		t.Fatal(err)
	}
	var start func(context.Context) func()
	d := serveDeps{Log: slog.New(slog.DiscardHandler), Services: services, AddService: func(s func(context.Context) func()) { start = s }}
	if err := quarantineProvide(context.Background(), d); err != nil || start == nil {
		t.Fatalf("quarantineProvide: %v (service registered %v)", err, start != nil)
	}
	stop := start(context.Background())

	// The first passes fail: the record stays.
	for i := 0; i < 2; i++ {
		select {
		case err := <-passes:
			if err == nil {
				t.Fatal("a pass succeeded with the embedder down")
			}
		case <-time.After(15 * time.Second):
			t.Fatal("the loop did not run")
		}
	}
	if !r.q.Has(upgrade.VectorIndexName) {
		t.Fatal("the record was released while the embedder was down")
	}
	r.emb.setDown(false)
	select {
	case <-released:
	case <-time.After(15 * time.Second):
		t.Fatal("the loop never released the vector index after the embedder came up")
	}
	stop()
	if r.q.Has(upgrade.VectorIndexName) || r.vectors() == 0 {
		t.Errorf("after the loop: held %v, %d vectors", r.q.Has(upgrade.VectorIndexName), r.vectors())
	}
	// Stopped means stopped: nothing runs a pass any more.
	for len(passes) > 0 {
		<-passes
	}
	time.Sleep(30 * time.Millisecond)
	if len(passes) != 0 {
		t.Error("the loop ran a pass after it was stopped")
	}
}

// serve hands the quarantine of its data directory to retrieval and to history, not only a test that
// builds the pieces by hand: an isolated index is labelled reduced recall (retrieval) or unavailable
// (history search) in the process the providers build.
func TestServeQuarantineReachesRetrievalAndHistory(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(ragStart)
	db := storetest.Open(t, nil)
	aud, err := audit.New(db, audit.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sessions.New(db, clk, sessions.Config{})
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	services := gateway.NewServices()
	for _, perr := range []error{gateway.Provide[gateway.ApplicationService](services, ragFakeApps{}), gateway.Provide(services, sess)} {
		if perr != nil {
			t.Fatal(perr)
		}
	}
	deps := serveDeps{Log: slog.New(slog.DiscardHandler), Clock: clk, DB: db, DataDir: dataDir, Audit: aud, Services: services}
	if err := ragServeProvide(ctx, deps); err != nil {
		t.Fatalf("ragServeProvide: %v", err)
	}
	if err := historyProvide(ctx, deps); err != nil {
		t.Fatalf("historyProvide: %v", err)
	}
	pipe, ok := gateway.Lookup[*query.Pipeline](services)
	hist, ok2 := gateway.Lookup[*history.Service](services)
	if !ok || !ok2 {
		t.Fatalf("the providers gave a pipeline (%v) and a history service (%v)", ok, ok2)
	}

	// Another process, the upgrade, writes the records; serve reads them from the same directory.
	q := upgrade.NewQuarantine(dataDir, clk)
	rscope := ragScope("t1", "w1", "usr-1")
	hscope := historyRetentionTestScope("ten_a", history.CapSearch)
	historyRetentionSay(t, db, clk.Now(), hscope, "one")
	ask := func() query.Result {
		t.Helper()
		res, err := pipe.Query(ctx, rscope, query.Request{Text: "tide tables"})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		return res
	}
	search := func() (history.SearchPage, error) {
		return hist.Search(ctx, hscope, history.SearchQuery{Text: "question"})
	}

	// Nothing isolated: nothing is labelled, and history answers.
	if d := ask().Diagnostics; len(d.Isolated) != 0 || slices.Contains(d.Degraded, query.DegradedLexicalIsolated) || slices.Contains(d.Degraded, query.DegradedVectorIsolated) {
		t.Fatalf("control: nothing is isolated and retrieval says %+v", d)
	}
	if page, err := search(); err != nil || len(page.Hits) == 0 {
		t.Fatalf("control: a healthy history index answers: %+v, %v", page, err)
	}

	for _, c := range []struct {
		name, degraded string
	}{
		{query.LexicalIndexName, query.DegradedLexicalIsolated},
		{query.VectorIndexName, query.DegradedVectorIsolated},
	} {
		if err := q.Add(c.name, "test: isolated by an upgrade"); err != nil {
			t.Fatal(err)
		}
		res := ask()
		if !slices.Contains(res.Diagnostics.Isolated, c.name) || !slices.Contains(res.Diagnostics.Degraded, c.degraded) || !quarantineHas(res, rag.RetrievalNoticeReducedSemanticRecall) {
			t.Errorf("retrieval does not say that %s is isolated: %+v notices %v", c.name, res.Diagnostics, res.Package.Retrieval.Notices)
		}
		if err := q.Clear(c.name); err != nil {
			t.Fatal(err)
		}
		if d := ask().Diagnostics; len(d.Isolated) != 0 {
			t.Errorf("retrieval still says %v after %s was released", d.Isolated, c.name)
		}
	}

	if err := q.Add(history.SearchIndexName, "test: isolated by an upgrade"); err != nil {
		t.Fatal(err)
	}
	page, err := search()
	if contracts.CodeOf(err) != contracts.ErrUnavailable || len(page.Hits) != 0 {
		t.Errorf("history search over an isolated index: %+v, %v; want unavailable and no page", page, err)
	}
	if err := q.Clear(history.SearchIndexName); err != nil {
		t.Fatal(err)
	}
	if page, err := search(); err != nil || len(page.Hits) == 0 {
		t.Errorf("history search after the index was released: %+v, %v", page, err)
	}

	// A serveDeps with no data directory has no quarantine to read, and says nothing is isolated.
	if quarantineOf("", clk) != nil || quarantineQueryOf(nil) != nil || quarantineHistoryOf(nil) != nil {
		t.Error("no data directory must mean no quarantine configured")
	}
}

// quarantineExec changes the database the way damage or another process would.
func quarantineExec(t *testing.T, f *ragFixture, stmt string) {
	t.Helper()
	if _, err := f.db.ExecContext(context.Background(), stmt); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}
