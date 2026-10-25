package query

import (
	"context"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/upgrade"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// quarantineEnv is a source with a warranty handbook in a real index and a real flat
// vector index, and the upgrade's real quarantine beside them. isolateLexical and
// isolateVector do what the upgrade does to a damaged index: record it in the
// quarantine and empty it (upgrade.IsolateDamaged), so the index really answers
// with nothing.
type quarantineEnv struct {
	*env
	q   *upgrade.Quarantine
	sc  contracts.Scope
	lex *spyLex
	vec *recordingVectors
}

// A question that matches the refund passage by its vector alone: the fake
// embedder returns the passage's own vector for every input, and "zzqxv" shares
// no word with any chunk, so the full-text stage cannot find it.
func newQuarantineEnv(t *testing.T, embOpts ...fake.Option) *quarantineEnv {
	t.Helper()
	target := fake.EmbedVector(refundText, fake.DefaultEmbedDim)
	embOpts = append([]fake.Option{fake.WithEmbedFunc(func(req providers.EmbedRequest) ([][]float32, error) {
		out := make([][]float32, len(req.Inputs))
		for i := range out {
			out[i] = target
		}
		return out, nil
	})}, embOpts...)
	e := newEnv(t, embOpts...)
	qe := &quarantineEnv{env: e, q: upgrade.NewQuarantine(t.TempDir(), e.clk), sc: scopeOf("ta")}
	e.addSource(qe.sc, "Warranty handbook", docSpec{id: "warranty", md: warrantyMD()})
	return qe
}

func (e *quarantineEnv) deps() Deps {
	d := e.env.deps()
	e.lex = &spyLex{Lexical: e.ix}
	e.vec = &recordingVectors{inner: e.flat}
	d.Lexical, d.Vectors, d.Quarantine = e.lex, e.vec, e.q
	// The spy hides index.Hits behind the bare interface; hydration reads through the fixture's reader.
	d.Hits = e.hits
	return d
}

func (e *quarantineEnv) isolate(idx upgrade.DerivedIndex) {
	e.t.Helper()
	ctx := context.Background()
	if err := e.q.Add(idx.Name(), "test: damaged"); err != nil {
		e.t.Fatal(err)
	}
	if err := idx.Isolate(ctx); err != nil {
		e.t.Fatal(err)
	}
}

func (e *quarantineEnv) isolateLexical() {
	e.t.Helper()
	idx, err := upgrade.FTSIndexes(context.Background(), e.db)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, ix := range idx {
		if ix.Name() == LexicalIndexName {
			e.isolate(ix)
			return
		}
	}
	e.t.Fatalf("the upgrade does not find the lexical index %s", LexicalIndexName)
}

func (e *quarantineEnv) isolateVector() {
	e.t.Helper()
	e.isolate(upgrade.VectorDerived(nil)(e.db)[0])
}

func (e *quarantineEnv) query(req Request) (Result, error) {
	e.t.Helper()
	p := e.pipeline(e.deps(), DefaultConfig())
	return p.Query(context.Background(), e.sc, req)
}

func (e *quarantineEnv) vectorSearches() int {
	e.vec.mu.Lock()
	defer e.vec.mu.Unlock()
	return len(e.vec.scope)
}

func mustHaveNotice(t *testing.T, res Result) {
	t.Helper()
	if !hasNotice(res.Package, rag.RetrievalNoticeReducedSemanticRecall) {
		t.Errorf("notices %v: an isolated index reduced recall and the package must say so", res.Package.Retrieval.Notices)
	}
}

// Master plan 18: an isolated derived index is labelled, not silently empty. The
// upgrade empties a damaged index and records it in its quarantine; a query that
// searched it anyway would find "nothing" and the package would claim that nothing
// authorised matched. The pipeline consults the quarantine instead, never searches
// what is isolated, answers from what is left and says that recall was reduced.
func TestIsolatedIndexIsLabelledReducedRecallNotEmpty(t *testing.T) {
	const byWords = "does the warranty cover water damage"
	const byMeaning = "zzqxv"

	t.Run("control: nothing isolated, nothing labelled", func(t *testing.T) {
		e := newQuarantineEnv(t)
		res, err := e.query(Request{Text: byWords})
		if err != nil {
			t.Fatal(err)
		}
		d := res.Diagnostics
		if d.Mode != ModeHybrid || len(d.Degraded) != 0 || len(d.Isolated) != 0 || len(res.Package.Retrieval.Notices) != 0 || len(res.Package.Items) == 0 {
			t.Errorf("mode %s degraded %v isolated %v notices %v items %d: a healthy deployment answers in full", d.Mode, d.Degraded, d.Isolated, res.Package.Retrieval.Notices, len(res.Package.Items))
		}
	})

	t.Run("the hazard: searched anyway, an isolated lexical index is an empty answer that says nothing", func(t *testing.T) {
		e := newQuarantineEnv(t)
		e.isolateLexical()
		d := e.env.deps() // no Quarantine: what the pipeline did before it consulted one
		d.Providers = providers.NewRegistry(e.clk)
		res := mustQuery(t, e.pipeline(d, DefaultConfig()), e.sc, Request{Text: byWords})
		if len(res.Package.Items) != 0 || res.Diagnostics.FTSCandidates != 0 {
			t.Fatalf("items %q, %d candidates: the fixture did not isolate the index", texts(res.Package), res.Diagnostics.FTSCandidates)
		}
		if hasString(res.Diagnostics.Degraded, DegradedLexicalIsolated) || len(res.Diagnostics.Isolated) != 0 {
			t.Fatalf("degraded %v isolated %v: nothing here can know about the isolation", res.Diagnostics.Degraded, res.Diagnostics.Isolated)
		}
		// With the quarantine consulted, the same deployment says it cannot search (the next-but-one case).
	})

	t.Run("lexical index isolated: answers from the vector index, labelled, full-text never searched", func(t *testing.T) {
		e := newQuarantineEnv(t)
		e.isolateLexical()
		res, err := e.query(Request{Text: byMeaning})
		if err != nil {
			t.Fatal(err)
		}
		d := res.Diagnostics
		if len(res.Package.Items) == 0 || !strings.Contains(res.Package.Items[0].Text, refundText) {
			t.Fatalf("package %q: the vector index still has the answer and an isolated full-text index must not hide it", texts(res.Package))
		}
		mustHaveNotice(t, res)
		if d.Mode != ModeVectorOnly || !hasString(d.Degraded, DegradedLexicalIsolated) || len(d.Isolated) != 1 || d.Isolated[0] != LexicalIndexName {
			t.Errorf("mode %s degraded %v isolated %v", d.Mode, d.Degraded, d.Isolated)
		}
		if n := e.lex.searchCount(); n != 0 {
			t.Errorf("the isolated lexical index was searched %d times: %q", n, e.lex.searchTexts())
		}
		if e.vectorSearches() == 0 {
			t.Error("the vector stage did not run")
		}
		for _, m := range res.Package.Retrieval.Methods {
			if m == rag.RetrievalMethodFts {
				t.Errorf("methods %v name full-text retrieval that did not run", res.Package.Retrieval.Methods)
			}
		}
	})

	t.Run("lexical index isolated: no rewrite pass over it either", func(t *testing.T) {
		e := newQuarantineEnv(t)
		e.isolateLexical()
		cfg := DefaultConfig()
		cfg.Rewrite = true
		res := mustQuery(t, e.pipeline(e.deps(), cfg), e.sc, Request{Text: byMeaning})
		if n := e.lex.searchCount(); n != 0 || res.Diagnostics.Rewrites != 0 {
			t.Errorf("%d full-text searches, %d rewrites over an isolated index: %q", n, res.Diagnostics.Rewrites, e.lex.searchTexts())
		}
		mustHaveNotice(t, res)
	})

	t.Run("vector index isolated: answers from full text, labelled, vectors never searched", func(t *testing.T) {
		e := newQuarantineEnv(t)
		e.isolateVector()
		res, err := e.query(Request{Text: byWords})
		if err != nil {
			t.Fatal(err)
		}
		d := res.Diagnostics
		if len(res.Package.Items) == 0 || !strings.Contains(allText(res.Package), "water ingress") {
			t.Fatalf("package %q: full text still has the answer", texts(res.Package))
		}
		mustHaveNotice(t, res)
		if d.Mode != ModeFTSOnly || !hasString(d.Degraded, DegradedVectorIsolated) || len(d.Isolated) != 1 || d.Isolated[0] != VectorIndexName {
			t.Errorf("mode %s degraded %v isolated %v", d.Mode, d.Degraded, d.Isolated)
		}
		if n := e.vectorSearches(); n != 0 {
			t.Errorf("the isolated vector index was searched %d times", n)
		}
		if n := len(e.emb.Prompts()); n != 0 {
			t.Errorf("the question was embedded %d times for a vector stage that cannot run", n)
		}
		if e.lex.searchCount() == 0 {
			t.Error("the full-text stage did not run")
		}
	})

	t.Run("both isolated: unavailable, not an empty package", func(t *testing.T) {
		e := newQuarantineEnv(t)
		e.isolateLexical()
		e.isolateVector()
		res, err := e.query(Request{Text: byWords})
		if !contracts.HasCode(err, contracts.ErrUnavailable) {
			t.Fatalf("err = %v, package %q: with nothing left to search the answer is unavailable, never an empty package", err, texts(res.Package))
		}
		if e.lex.searchCount() != 0 || e.vectorSearches() != 0 {
			t.Errorf("%d full-text and %d vector searches over isolated indexes", e.lex.searchCount(), e.vectorSearches())
		}
	})

	t.Run("lexical index isolated and no embedder: unavailable", func(t *testing.T) {
		e := newQuarantineEnv(t)
		e.isolateLexical()
		d := e.deps()
		d.Providers = providers.NewRegistry(e.clk)
		_, err := e.pipeline(d, DefaultConfig()).Query(context.Background(), e.sc, Request{Text: byWords})
		if !contracts.HasCode(err, contracts.ErrUnavailable) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("nothing authorised: an empty package that still names the isolation", func(t *testing.T) {
		e := newQuarantineEnv(t)
		e.isolateLexical()
		other := scopeOf("tb") // a tenant with no source: nothing to search, nothing it may read
		res, err := e.pipeline(e.deps(), DefaultConfig()).Query(context.Background(), other, Request{Text: byWords})
		if err != nil || len(res.Package.Items) != 0 {
			t.Fatalf("err %v items %q: nothing is authorised, so the package is empty", err, texts(res.Package))
		}
		mustHaveNotice(t, res)
		if !hasString(res.Diagnostics.Degraded, DegradedLexicalIsolated) {
			t.Errorf("degraded %v", res.Diagnostics.Degraded)
		}
	})

	t.Run("released: the label goes when the record does", func(t *testing.T) {
		e := newQuarantineEnv(t)
		e.isolateVector()
		if err := e.q.Clear(VectorIndexName); err != nil {
			t.Fatal(err)
		}
		res, err := e.query(Request{Text: byWords})
		if err != nil || len(res.Diagnostics.Isolated) != 0 || hasString(res.Diagnostics.Degraded, DegradedVectorIsolated) {
			t.Errorf("err %v, isolated %v, degraded %v after the record was cleared", err, res.Diagnostics.Isolated, res.Diagnostics.Degraded)
		}
	})
}

// The names this package consults are the names the upgrade records. If either
// side renamed an index, a query would search an isolated index and say nothing
// about it, which is what the quarantine exists to prevent.
func TestQuarantineNamesAreTheUpgradesNames(t *testing.T) {
	e := newEnv(t)
	idx, err := upgrade.FTSIndexes(context.Background(), e.db)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ix := range idx {
		if ix.Name() == LexicalIndexName {
			found = true
		}
	}
	if !found {
		t.Errorf("the upgrade does not list %s among the full-text indexes of the database", LexicalIndexName)
	}
	if VectorIndexName != upgrade.VectorIndexName {
		t.Errorf("the pipeline consults %q, the upgrade quarantines %q", VectorIndexName, upgrade.VectorIndexName)
	}
	if err := upgrade.VectorDerived(nil)(e.db)[0].Check(context.Background()); err != nil {
		t.Errorf("the vector tables of the database fail the upgrade's vector check: %v", err)
	}
	// What the upgrade holds, a *upgrade.Quarantine, is what Deps takes.
	var _ Quarantine = (*upgrade.Quarantine)(nil)
}
