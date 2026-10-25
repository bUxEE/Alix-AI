package cli

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/rag/ingest"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

const ragServeClaim = "The Model X kettle has a warranty of 24 months from the date of purchase."

// ragServeIndexedRig is a stack with a source of one document, registered and ingested.
func ragServeIndexedRig(t *testing.T) *ragSourcesRig {
	t.Helper()
	rig := ragSourcesNewRig(t, nil, ragHash)
	rig.write("handbook.md", "# Warranty\n\n"+ragServeClaim)
	rig.register(t, ragSourcesOperator("t1", "w1"), "docs")
	rig.drain()
	return rig
}

// ragServeControllers is a registry whose controller proposes one claim citing the first passage.
func ragServeControllers(t *testing.T, rig *ragSourcesRig) (*providers.Registry, *fake.Provider) {
	t.Helper()
	ctrl := fake.New()
	ctrl.On(fake.Any()).ReplyJSON(map[string]any{"claims": []map[string]any{{"text": ragServeClaim, "evidence_ids": []string{"e1"}}}})
	reg := providers.NewRegistry(rig.clk)
	if err := reg.Register(providers.RoleController, ctrl); err != nil {
		t.Fatal(err)
	}
	return reg, ctrl
}

// ragServeTestEngine is a dialogue engine with nothing behind it: it is what serve gives the
// area to find in the services, and what these tests check the knowledge is given to.
func ragServeTestEngine(t *testing.T, f *ragFixture) *dialogue.Engine {
	t.Helper()
	gw := policy.NewGateway(f.clk)
	sess, err := sessions.New(f.db, f.clk, sessions.Config{})
	if err != nil {
		t.Fatal(err)
	}
	adapterSource := actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) {
		return nil, contracts.New(contracts.ErrUnavailable, "no adapter in this test")
	})
	co, err := actions.New(actions.Deps{
		DB: f.db, Policy: gw, Audit: f.aud, Clock: f.clk, Adapters: adapterSource,
		Packages: actions.PackageSourceFunc(func(context.Context, string) (policy.ActivePackage, error) {
			return nil, contracts.New(contracts.ErrQuarantined, "no package in this test")
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	sel, err := dialogue.LocalSelector(fake.New(), providers.DefaultBudget())
	if err != nil {
		t.Fatal(err)
	}
	eng, err := dialogue.New(dialogue.Deps{
		Sessions: sess, Registry: capabilities.NewRegistry(nil, capabilities.WithClock(f.clk)),
		Packages: dialogue.PackageSourceFunc(func(context.Context, string) (capabilities.Package, error) {
			return nil, contracts.New(contracts.ErrQuarantined, "no package in this test")
		}),
		Policy: gw, Actions: co, Adapters: adapterSource, Providers: sel, Clock: f.clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

// serve provides the retrieval half of the area: search, answers with citations and the
// resolution of a citation's reference stop answering 404, from one Answerer, so a reference an
// answer hands out is one the same service resolves; the dialogue engine serve built is given
// the same sources to ask. A deployment with no controller model still serves search and says
// unavailable for an answer.
func TestRAGQueriesAreProvidedAndAnswerWithCitations(t *testing.T) {
	ctx := context.Background()
	rig := ragServeIndexedRig(t)
	reg, ctrl := ragServeControllers(t, rig)
	services := gateway.NewServices()
	eng := ragServeTestEngine(t, rig.ragFixture)
	if err := gateway.Provide(services, eng); err != nil {
		t.Fatal(err)
	}
	if eng.HasKnowledge() {
		t.Fatal("the engine has a knowledge before the area is provided")
	}
	deps := serveDeps{Log: slog.New(slog.DiscardHandler), Clock: rig.clk, Providers: reg, Services: services}
	if err := ragServeProvideQueries(deps, rig.stack, ragSourcesSectionsOf(nil)); err != nil {
		t.Fatal(err)
	}
	queries, ok := gateway.Lookup[gateway.RAGQueries](services)
	if !ok {
		t.Fatal("serve does not provide gateway.RAGQueries: search, answer and citations would answer 404")
	}
	if !eng.HasKnowledge() {
		t.Fatal("the dialogue engine was not given the sources to ask")
	}
	queries = gateway.RAGQueriesScoped(queries) // what the routes call
	scope := rig.scope

	// Search is retrieval alone: passages of the source the principal may read.
	found, err := queries.Search(ctx, scope, rag.SearchRequest{Query: "warranty of the kettle"})
	if err != nil || len(found.Results) == 0 || !strings.Contains(found.Results[0].Excerpt, "kettle") {
		t.Fatalf("Search = %+v, %v", found, err)
	}
	if ctrl.Calls() != 0 {
		t.Fatalf("a search called the model %d times", ctrl.Calls())
	}

	// An answer is grounded: the claim, a citation of the source with the claim it supports,
	// and a reference that opens it.
	res, err := queries.Answer(ctx, scope, rag.AnswerRequest{Question: "what is the warranty on the kettle"})
	if err != nil {
		t.Fatal(err)
	}
	ga, ok := res.Value.(rag.GroundedAnswer)
	if !ok || len(ga.Claims) != 1 || len(ga.Citations) != 1 {
		t.Fatalf("answer = %+v", res.Value)
	}
	cite := ga.Citations[0]
	if ga.Claims[0].Text != ragServeClaim || len(cite.Claims) != 1 || cite.Claims[0] != ragServeClaim || cite.DisplayName != "Handbook" {
		t.Fatalf("claim %+v, citation %+v", ga.Claims[0], cite)
	}
	file, isFile := cite.OpenRef.Value.(conversation.CitationOpenRefFile)
	if !isFile || file.Kind != "file" {
		t.Fatalf("open_ref = %+v", cite.OpenRef)
	}
	if ctrl.Calls() != 1 {
		t.Fatalf("the model was called %d times for one answer", ctrl.Calls())
	}

	// The reference resolves through the same service (one Answerer holds the references of the
	// answers it wrote), for the caller it was issued to and for nobody else.
	body, err := queries.Open(ctx, scope, string(file.FileID))
	if err != nil || body.DisplayName != "Handbook" || !strings.Contains(body.Preview, "kettle") || body.SourceID != cite.SourceID {
		t.Fatalf("Open = %+v, %v", body, err)
	}
	other := ragScope("t2", "w1", "usr-9")
	if _, err := queries.Open(ctx, other, string(file.FileID)); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Fatalf("another tenant opening the reference: %v", err)
	}

	// Another tenant's question finds nothing of this tenant's source, and says no more than that.
	res, err = queries.Answer(ctx, other, rag.AnswerRequest{Question: "what is the warranty on the kettle"})
	if _, none := res.Value.(rag.NoSupportedEvidence); err != nil || !none {
		t.Fatalf("another tenant's answer = %+v, %v", res.Value, err)
	}
	if ctrl.Calls() != 1 {
		t.Fatalf("a question with no evidence called the model (%d calls)", ctrl.Calls())
	}

	// No controller: search still works, an answer is unavailable (never an uncited one).
	bare := gateway.NewServices()
	if err := ragServeProvideQueries(serveDeps{Clock: rig.clk, Services: bare}, rig.stack, ragSourcesSectionsOf(nil)); err != nil {
		t.Fatal(err)
	}
	lone, _ := gateway.Lookup[gateway.RAGQueries](bare)
	if got, err := lone.Search(ctx, scope, rag.SearchRequest{Query: "warranty of the kettle"}); err != nil || len(got.Results) == 0 {
		t.Fatalf("search without a controller = %+v, %v", got, err)
	}
	if _, err := lone.Answer(ctx, scope, rag.AnswerRequest{Question: "what is the warranty on the kettle"}); contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Fatalf("an answer without a controller = %v, want unavailable", err)
	}
}

// ragServeProvide hands the gateway both halves, so no route of the area is left unmounted.
func TestRAGServeProvidesBothHalvesOfTheArea(t *testing.T) {
	f := newRagFixture(t, func(o *ragFixtureOpts) { o.nopool = true })
	oldParser, oldDisk := ragNewParser, ragNewDiskPressure
	t.Cleanup(func() { ragNewParser, ragNewDiskPressure = oldParser, oldDisk })
	ragNewParser = func(ingest.Config, *slog.Logger) ingest.Parser { return newRagStubParser() }
	ragNewDiskPressure = func(string, int64) ingest.DiskPressure { return &ragFakeDisk{} }
	services := gateway.NewServices()
	if err := gateway.Provide[gateway.ApplicationService](services, ragFakeApps{}); err != nil {
		t.Fatal(err)
	}
	eng := ragServeTestEngine(t, f)
	if err := gateway.Provide(services, eng); err != nil {
		t.Fatal(err)
	}
	deps := serveDeps{Log: slog.New(slog.DiscardHandler), Clock: f.clk, DB: f.db, DataDir: "the-data-dir", Audit: f.aud, Services: services}
	if err := ragServeProvide(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	if _, ok := gateway.Lookup[gateway.RAGQueries](services); !ok {
		t.Error("serve does not provide gateway.RAGQueries")
	}
	if _, ok := gateway.Lookup[gateway.RAGIndexing](services); !ok {
		t.Error("serve does not provide gateway.RAGIndexing")
	}
	if !eng.HasKnowledge() {
		t.Error("serve did not give the dialogue engine its knowledge")
	}
}
