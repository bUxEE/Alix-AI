package ingest

import (
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

func TestIngestIncrementalSkipsUnchanged(t *testing.T) {
	f := newFixture(t)
	corpus(f, 4)
	j := f.sync()
	mustEqual(t, "first sync state", j.State, jobs.StateSucceeded)
	c1 := f.counters()
	mustEqual(t, "documents parsed by the first sync", c1.parses, 4)
	if c1.embedInputs == 0 {
		t.Fatal("the first sync embedded nothing")
	}
	gen1 := f.activeGen()
	chunks1 := gen1.Counts.Chunks
	vecs1 := f.vectors()

	// Nothing changed: nothing is parsed, nothing is embedded, nothing is queued
	// beyond the sync itself.
	before := f.countRows(`SELECT count(*) FROM jobs`)
	mustEqual(t, "second sync", f.sync().State, jobs.StateSucceeded)
	c2 := f.counters()
	if c2 != c1 {
		t.Fatalf("an unchanged tree cost work: %+v -> %+v", c1, c2)
	}
	if got := f.countRows(`SELECT count(*) FROM jobs`); got != before+1 {
		t.Fatalf("an unchanged tree queued %d jobs besides the sync", got-before-1)
	}
	mustEqual(t, "version rows", f.countRows(`SELECT count(*) FROM ragingest_version`), 4)

	// One paragraph of one document changes: only that document is parsed and
	// only the chunk that changed is embedded; the other chunks reuse theirs.
	f.writeAt("doc2.md", doc("Title 2", para("p2a"), para("p2bNEW"), para("p2c")), oldTime.Add(time.Hour))
	mustEqual(t, "third sync", f.sync().State, jobs.StateSucceeded)
	c3 := f.counters()
	mustEqual(t, "documents parsed after one change", c3.parses-c2.parses, 1)
	mustEqual(t, "chunks embedded after one change", c3.embedInputs-c2.embedInputs, 1)

	gen3 := f.activeGen()
	mustEqual(t, "generation", gen3.GenerationID, gen1.GenerationID)
	mustEqual(t, "chunks", gen3.Counts.Chunks, chunks1)
	mustEqual(t, "vectors", f.vectors(), vecs1)
	mustEqual(t, "embedded chunks", gen3.Counts.EmbeddedChunks, gen3.Counts.Chunks)
	if h := f.search("p2bNEW"); len(h) != 1 {
		t.Fatalf("the new text is not searchable: %d hits", len(h))
	}
	if h := f.search("p2b"); len(h) != 0 {
		t.Fatalf("the replaced text is still searchable: %d hits", len(h))
	}
	// The replaced version is superseded and its vanished chunk left the vector
	// index with it (one vector for one chunk: the count did not grow).
	st := f.versionStates()
	mustEqual(t, "indexed versions", st[vIndexed], 4)
	mustEqual(t, "superseded versions", st[vSuperseded], 1)
}

func TestIngestIdempotentRerun(t *testing.T) {
	f := newFixture(t)
	corpus(f, 3)
	f.sync()
	snapshot := func() []int {
		g := f.activeGen()
		return []int{
			int(g.Counts.Documents), int(g.Counts.Chunks), int(g.Counts.EmbeddedChunks), f.vectors(),
			f.countRows(`SELECT count(*) FROM ragingest_version`), f.countRows(`SELECT count(*) FROM ragingest_embed`),
			f.countRows(`SELECT count(*) FROM ragindex_chunk WHERE tombstoned_at IS NULL`), len(f.generations()),
		}
	}
	want := snapshot()
	c1 := f.counters()

	// A second sync of an unchanged tree.
	f.sync()
	// At-least-once delivery: every finished job is delivered again, in order.
	for _, typ := range []string{JobSync, JobParse, JobIndex, vector.EmbedJobType, JobFinalize} {
		for _, j := range f.jobsOf(typ) {
			var h jobs.Handler
			switch typ {
			case JobSync:
				h = f.svc.syncHandler
			case JobParse:
				h = f.svc.stageHandler(laneIngest, f.svc.parseStage)
			case JobIndex:
				h = f.svc.stageHandler(laneIngest, f.svc.indexStage)
			case vector.EmbedJobType:
				h = f.svc.embedHandler
			case JobFinalize:
				h = f.svc.finalizeHandler(laneIngest)
			}
			if err := h(bg, j); err != nil {
				t.Fatalf("redelivered %s job: %v", typ, err)
			}
		}
	}
	f.drain()
	got := snapshot()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("a repeat changed the index: %v -> %v", want, got)
		}
	}
	if c2 := f.counters(); c2 != c1 {
		t.Fatalf("a repeat cost work: %+v -> %+v", c1, c2)
	}
	mustEqual(t, "documents", len(docHits(f.search("alpha"))), 3)
}

func TestComponentVersionChangeCreatesGeneration(t *testing.T) {
	f := newFixture(t)
	corpus(f, 3)
	f.sync()
	gen1 := f.activeGen()
	want := rag.ComponentVersion{Name: "stubparse", Version: "1.0.0"}
	if gen1.Components.Parser != want || gen1.Components.Chunker != f.svc.chunker.Component() ||
		gen1.Components.Embedder == nil || *gen1.Components.Embedder != f.svc.emb.Component || gen1.Components.OCR != nil {
		t.Fatalf("the generation does not record the component versions: %+v", gen1.Components)
	}
	c1 := f.counters()

	t.Run("parser", func(t *testing.T) {
		f.parser.setVersion("2.0.0")
		f.requestSync()
		// The sync builds a generation beside the serving one; until it is valid
		// the old one keeps answering.
		f.poll()
		gens := f.generations()
		mustEqual(t, "generations during the build", len(gens), 2)
		if h := f.search("alpha"); len(h) == 0 || h[0].GenerationID != string(gen1.GenerationID) {
			t.Fatalf("the serving generation does not answer during the build: %+v", h)
		}
		f.drain()

		gens = f.generations()
		mustEqual(t, "generations", len(gens), 2)
		gen2 := f.activeGen()
		if gen2.GenerationID == gen1.GenerationID {
			t.Fatal("no new generation was activated")
		}
		mustEqual(t, "new parser version", gen2.Components.Parser.Version, "2.0.0")
		old, err := f.ix.GetGeneration(bg, f.scope, string(gen1.GenerationID))
		if err != nil {
			t.Fatal(err)
		}
		mustEqual(t, "old generation state", old.State, rag.IndexGenerationStateSuperseded)
		mustEqual(t, "documents re-parsed", f.counters().parses-c1.parses, 3)
		// Same chunk ids and exact inputs: the embeddings are reused, none is made.
		mustEqual(t, "chunks embedded again", f.counters().embedInputs, c1.embedInputs)
		mustEqual(t, "embedded chunks", gen2.Counts.EmbeddedChunks, gen2.Counts.Chunks)
		mustEqual(t, "vectors held (the replaced generation's are released)", f.vectors(), int(gen2.Counts.Chunks))
		// The rebuild is reindexing: class 6, behind ingestion.
		for _, typ := range []string{JobReindexParse, JobReindexIndex, JobReindexFinalize} {
			js := f.jobsOf(typ)
			if len(js) == 0 {
				t.Fatalf("no %s job ran", typ)
			}
			mustEqual(t, typ+" class", js[0].Class, jobs.ClassMaintenance)
		}
		mustEqual(t, "documents found", len(docHits(f.search("alpha"))), 3)
		// Running again with the same components builds nothing.
		g := f.counters()
		f.sync()
		mustEqual(t, "generations after an unchanged sync", len(f.generations()), 2)
		if f.counters() != g {
			t.Fatal("an unchanged sync after the rebuild cost work")
		}
	})

	t.Run("chunker", func(t *testing.T) {
		cfg := chunk.DefaultConfig()
		cfg.ChildMaxToks = 128
		chk, err := chunk.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		before := f.activeGen()
		f.svc.chunker = chk
		f.sync()
		after := f.activeGen()
		if after.GenerationID == before.GenerationID {
			t.Fatal("a changed chunker did not create a generation")
		}
		mustEqual(t, "chunker component", after.Components.Chunker, chk.Component())
	})

	t.Run("embedder", func(t *testing.T) {
		before := f.activeGen()
		f.svc.emb.Component.Version = "2"
		f.sync()
		after := f.activeGen()
		if after.GenerationID == before.GenerationID {
			t.Fatal("a changed embedder did not create a generation")
		}
		mustEqual(t, "embedder version", after.Components.Embedder.Version, "2")
		mustEqual(t, "embedded chunks", after.Counts.EmbeddedChunks, after.Counts.Chunks)
	})
}

// During a rebuild, an item whose first version was indexed into the new
// generation and whose replacement then fails keeps that first copy, as it would
// in a serving generation: the build is validated against what it holds and is
// activated, not thrown away over the count.
func TestRebuildKeepsADocumentWhoseReplacementFailed(t *testing.T) {
	f := newFixture(t)
	f.write("x.md", doc("X", para("xold")))
	f.write("y.md", doc("Y", para("ytext")))
	f.sync()
	gen1 := f.activeGen()

	// The rebuild indexes x and is held open by y, whose parse keeps failing for
	// a reason of the environment.
	holdY := true
	f.parser.fail = func(d []byte) error {
		switch s := string(d); {
		case holdY && strings.Contains(s, "ytext"):
			return contracts.New(contracts.ErrUnavailable, "the sandbox could not start")
		case strings.Contains(s, "xbroken"):
			return contracts.New(contracts.ErrValidationFailed, "the document is malformed").WithDetails("reason", "malformed")
		}
		return nil
	}
	f.parser.setVersion("2.0.0")
	f.sync()
	gens := f.generations()
	mustEqual(t, "generations during the rebuild", len(gens), 2)
	build := gens[0]
	mustEqual(t, "the rebuild", build.State, rag.IndexGenerationStateBuilding)

	// x changes and its new version cannot be parsed.
	f.write("x.md", doc("X", para("xbroken")))
	f.sync()
	mustEqual(t, "the rebuild after x's replacement failed", f.generations()[0].State, rag.IndexGenerationStateBuilding)

	holdY = false
	f.sync()
	active := f.activeGen()
	mustEqual(t, "the active generation", active.GenerationID, build.GenerationID)
	if active.GenerationID == gen1.GenerationID {
		t.Fatal("the rebuild was not activated")
	}
	mustEqual(t, "x's indexed copy", len(docHits(f.search("xold"))), 1)
	mustEqual(t, "y", len(docHits(f.search("ytext"))), 1)
	mustEqual(t, "vectors held (the replaced generation's are released)", f.vectors(), int(active.Counts.Chunks))
}

func TestFTSAvailableBeforeEmbeddings(t *testing.T) {
	f := newFixture(t)
	corpus(f, 3)
	f.emb.down.Store(true)
	mustEqual(t, "sync", f.sync().State, jobs.StateSucceeded)

	// Every document is searchable and its generation serves, while no embedding
	// exists and the embedding jobs have all failed against the dead runtime.
	g := f.activeGen()
	mustEqual(t, "state", g.State, rag.IndexGenerationStateActive)
	mustEqual(t, "documents searchable", len(docHits(f.search("alpha"))), 3)
	mustEqual(t, "vectors", f.vectors(), 0)
	mustEqual(t, "embedded chunks", g.Counts.EmbeddedChunks, 0)
	st := f.status()
	mustEqual(t, "state", st.State, rag.SourceStatusStateReady)
	if st.Counts.EmbeddingPending == 0 || st.Counts.Indexed != 3 {
		t.Fatalf("the status does not show embeddings trailing the index: %+v", st.Counts)
	}
	for _, j := range f.jobsOf(vector.EmbedJobType) {
		mustEqual(t, "embed job state", j.State, jobs.StateFailed)
		mustEqual(t, "embed job code", j.ErrorCode, contracts.ErrUnavailable)
	}

	// The runtime comes back: the next sync queues the batches whose jobs
	// ended failed, and the embeddings catch up.
	f.emb.down.Store(false)
	f.sync()
	g = f.activeGen()
	mustEqual(t, "embedded chunks after recovery", g.Counts.EmbeddedChunks, g.Counts.Chunks)
	mustEqual(t, "vectors after recovery", f.vectors(), int(g.Counts.Chunks))
	mustEqual(t, "documents still searchable", len(docHits(f.search("alpha"))), 3)
}
