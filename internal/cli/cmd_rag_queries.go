package cli

import (
	"context"
	"fmt"

	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/rag/answer"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The retrieval half of the RAG area of `alixai serve` (M2-18): search, grounded answers with
// citations and the resolution of a citation's reference (gateway.RAGQueries), and the same
// answers for a dialogue turn that no capability of the application handles.
//
// # One Answerer per process
//
// The references an answer hands out live in the Answerer's resolver, so the Answerer that
// issues them is the one that resolves them. ragServeProvide builds exactly one (here), gives it
// to the gateway as RAGQueries (the routes /v1/rag/search, /v1/rag/answer and
// /v1/rag/citations/{file_id} exist only while that service is provided) and gives the dialogue
// engine the same value as its Knowledge, so a citation of a dialogue turn resolves through the
// API too. There is no bridge around the engine: the engine itself asks the sources where a text
// turn would otherwise refuse (dialogue.Deps.Knowledge).
//
// # The model
//
// The grounded answerer asks the controller role for claims and decides in code which of them
// survive; the output is untrusted (internal/rag/answer). The generator is the controller of the
// registry serve built, wrapped in the request budget of the rag_query and rag_answer sections
// (gateway.RAGAnswererBudget). It is looked up on every call, as the dialogue engine's selector
// does: a deployment whose inference path is off (no verified model, no runtime) still serves
// search, and every answer says unavailable instead of the service refusing to start.
//
// # What it never does
//
// It decides nothing about who may read a source: that is the catalogue (ragSources), asked by
// the pipeline and again by the answerer just before a citation is written. It never produces
// egress_restricted: the data-egress filter of the external provider mode acts on the evidence
// package before the answer, in the composition that owns it.

// ragServeProvideQueries builds the one Answerer of the process over the stack's catalogue and
// the registry's controller, provides gateway.RAGQueries to the gateway and gives the dialogue
// engine, when serve has one, the same service as its Knowledge. sections is the result of
// ragSourcesSectionsOf for the registry serve ran on (it holds rag_answer).
func ragServeProvideQueries(d serveDeps, st *ragStack, sections ragSourcesSections) error {
	queries, local, err := ragServeNewQueries(d, st, sections)
	if err != nil {
		return err
	}
	if err := gateway.Provide[gateway.RAGQueries](d.Services, queries); err != nil {
		return err
	}
	// The local answerer and its bounds, for the composition that builds an external answerer beside it over the
	// same citation resolver (External Provider Mode, providers_external.go).
	if err := gateway.Provide(d.Services, local); err != nil {
		return err
	}
	// The engine is built before the areas are provided, so the knowledge is given to the
	// engine that exists. The scope guard is the one the routes use: whatever the implementation
	// does, a turn's question is checked against the same bounds as POST /v1/rag/answer.
	if eng, ok := gateway.Lookup[*dialogue.Engine](d.Services); ok {
		eng.SetKnowledge(gateway.RAGQueriesScoped(queries))
	}
	return nil
}

// ragServeLocalAnswer is the local Answerer of the process and the bounds it was built with. Its Resolver is the
// one every citation of the process is issued and resolved by (answer.Answerer.Resolver), which is why an answerer
// built for another generator takes it as its own (answer.Deps.Resolver) and the references it hands out open
// through /v1/rag/citations like the others.
type ragServeLocalAnswer struct {
	Answerer *answer.Answerer
	Config   answer.Config
	Budget   providers.Budget
}

// ragServeNewQueries is gateway.RAGQueries over the stack's pipeline and a new Answerer, with the Answerer.
func ragServeNewQueries(d serveDeps, st *ragStack, sections ragSourcesSections) (gateway.RAGQueries, *ragServeLocalAnswer, error) {
	answerer, err := answer.New(answer.Deps{
		Generator: ragServeController{reg: d.Providers, budget: gateway.RAGAnswererBudget(sections.query, sections.answer)},
		Catalog:   st.sources,
		Clock:     d.Clock,
	}, sections.answer)
	if err != nil {
		return nil, nil, fmt.Errorf("rag: the answerer: %w", err)
	}
	queries, err := gateway.NewRAGQueries(st.pipeline, answerer)
	if err != nil {
		return nil, nil, fmt.Errorf("rag: the queries: %w", err)
	}
	return queries, &ragServeLocalAnswer{Answerer: answerer, Config: sections.answer, Budget: gateway.RAGAnswererBudget(sections.query, sections.answer)}, nil
}

// ragServeController is the text generator of the answerer: the controller role of the registry,
// looked up for each call and wrapped in the request budget, so a role that is not registered or
// not usable fails the call closed (unavailable) and the rest of the area keeps serving.
type ragServeController struct {
	reg    *providers.Registry
	budget providers.Budget
}

var _ providers.TextGenerator = ragServeController{}

func (c ragServeController) generator() (providers.TextGenerator, error) {
	if c.reg == nil {
		return nil, contracts.New(contracts.ErrUnavailable, "no model is available for the role")
	}
	gen, err := c.reg.TextGenerator(providers.RoleController)
	if err != nil {
		return nil, err
	}
	if !c.reg.Usable(providers.RoleController) {
		return nil, contracts.New(contracts.ErrUnavailable, "the controller model is not available")
	}
	return providers.WithBudget(gen, c.budget)
}

// Chat implements providers.TextGenerator.
func (c ragServeController) Chat(ctx context.Context, scope contracts.Scope, req providers.ChatRequest) (providers.ChatResult, error) {
	gen, err := c.generator()
	if err != nil {
		return providers.ChatResult{}, err
	}
	return gen.Chat(ctx, scope, req)
}

// ChatStream implements providers.TextGenerator.
func (c ragServeController) ChatStream(ctx context.Context, scope contracts.Scope, req providers.ChatRequest) (providers.ChatStream, error) {
	gen, err := c.generator()
	if err != nil {
		return nil, err
	}
	return gen.ChatStream(ctx, scope, req)
}
