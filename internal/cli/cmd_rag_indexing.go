package cli

import (
	"context"
	"errors"

	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/ingest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// The indexing half of the RAG area of `alixai serve` (M2-18): requesting a sync, building a
// replacement generation, listing and withdrawing the index generations of a source
// (gateway.RAGIndexing). Until serve provides it the routes
//
//	POST /v1/rag/sources/{source_id}/sync
//	POST /v1/rag/sources/{source_id}/rebuild
//	GET  /v1/rag/sources/{source_id}/generations
//	POST /v1/rag/sources/{source_id}/generations/{generation_id}/invalidate
//
// are not mounted and answer 404; both halves of the area are optional services on purpose, so a
// deployment that has not wired one does not stop serve.
//
// # Who may call
//
// Sync, Rebuild and Invalidate are operator acts, exactly like registering, re-auditing and
// removing a source (ragSourcesService): the verified principal must be listed in the "rag"
// section's operators with the manage_sources action. Nothing a token's roles or capability scopes
// say is read, and nothing a package or a model says either. The operator decision is made first
// and says the same to every caller who may not (permission_denied), so it tells a caller nothing
// about which sources exist; only then is the source looked up. Generations is a read inside the
// caller's own scope and needs only the verified scope.
//
// # The scope
//
// A source is one the registry holds for exactly the verified application, tenant and workspace.
// A source outside that scope, a generation of another source and one that does not exist are one
// answer, validation_failed with details.reason not_found, which the route maps to 404.
//
// # Sync and rebuild
//
// A sync is ingest.Service.RequestSync under the service identity, like the one registering a
// source queues, so a long scan does not depend on the session that asked for it; requests that
// arrive while a sync waits or runs are the same job. A rebuild is the same request: what
// ingestion builds from it is decided by planGeneration, which never touches the generation that
// serves. It builds a replacement beside it when the components of the pipeline changed (a new
// parser, chunker or embedding model), and when the source has no serving generation (the one it
// had was invalidated) it builds the first one from the snapshot; the replacement is validated and
// activated in one step, so there is never a moment without a serving generation once one existed
// and was not withdrawn. With the components unchanged and a generation serving there is nothing
// to replace, and the sync is incremental: forcing a second build of an identical generation needs
// an exported ingestion API for it and is not done by emptying or invalidating anything here.
//
// # Invalidate
//
// ragServeIndexing.Invalidate withdraws the generation (index.InvalidateGeneration): it is never
// retrievable again, and the answer is the generation as it stands. Withdrawing the generation
// that serves leaves the source without one, and its queries find nothing in it, until a rebuild
// activates a replacement.

// ragServeIndexingCause is what an invalidated generation records as its reason.
var ragServeIndexingCause = contracts.New(contracts.ErrConflict, "withdrawn by an operator through the API")

// ragServeIndexing is gateway.RAGIndexing over the registry (which sources the scope holds), the
// ingestion service and the index.
type ragServeIndexing struct {
	sources   *ragSources
	svc       *ingest.Service
	ix        *index.Index
	operators ingest.OperatorAuthorizer
}

var _ gateway.RAGIndexing = (*ragServeIndexing)(nil)

// ragServeProvideIndexing provides gateway.RAGIndexing over the stack.
func ragServeProvideIndexing(d serveDeps, st *ragStack) error {
	return gateway.Provide[gateway.RAGIndexing](d.Services, ragServeNewIndexing(st))
}

func ragServeNewIndexing(st *ragStack) *ragServeIndexing {
	return &ragServeIndexing{sources: st.sources, svc: st.svc, ix: st.ix, operators: ragOperators{entries: st.input.Config.Operators}}
}

func ragServeNotFound(what string) error {
	return contracts.New(contracts.ErrValidationFailed, "rag: no such "+what).WithDetails("reason", "not_found")
}

// operate is the scope check and the operator decision of the acts that change the index.
func (g *ragServeIndexing) operate(ctx context.Context, scope contracts.Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	return g.operators.AuthorizeOperator(ctx, scope, ragSourcesActionManage)
}

// held is the registration of the source in the scope: the scope's own, not one that is being
// removed, and nothing else.
func (g *ragServeIndexing) held(scope contracts.Scope, sourceID string) (*ragEntry, error) {
	e := g.sources.lookup(scope, sourceID)
	if e == nil {
		return nil, ragServeNotFound("source")
	}
	return e, nil
}

// queue is the sync of a held source, queued under the service identity.
func (g *ragServeIndexing) queue(ctx context.Context, scope contracts.Scope, sourceID string) (gateway.RAGSyncJob, error) {
	e, err := g.held(scope, sourceID)
	if err != nil {
		return gateway.RAGSyncJob{}, err
	}
	if !e.bound {
		// Registered and read back at start, not rebuilt from the active package yet: the
		// process re-binds it on its own, and a job queued now would only be retried.
		return gateway.RAGSyncJob{}, contracts.New(contracts.ErrUnavailable, "rag: the registration is being re-bound to the active package")
	}
	res, err := g.svc.RequestSync(ctx, scope, sourceID, ingest.SyncOptions{ServiceIdentity: ragServiceIdentity})
	if err != nil {
		return gateway.RAGSyncJob{}, err
	}
	return gateway.RAGSyncJob{SourceID: sourceID, JobID: res.Job.ID}, nil
}

// Sync implements gateway.RAGIndexing.
func (g *ragServeIndexing) Sync(ctx context.Context, scope contracts.Scope, sourceID string) (gateway.RAGSyncJob, error) {
	if err := g.operate(ctx, scope); err != nil {
		return gateway.RAGSyncJob{}, err
	}
	return g.queue(ctx, scope, sourceID)
}

// Rebuild implements gateway.RAGIndexing.
func (g *ragServeIndexing) Rebuild(ctx context.Context, scope contracts.Scope, sourceID string) (gateway.RAGSyncJob, error) {
	if err := g.operate(ctx, scope); err != nil {
		return gateway.RAGSyncJob{}, err
	}
	return g.queue(ctx, scope, sourceID)
}

// Generations implements gateway.RAGIndexing. It is a read inside the caller's own scope.
func (g *ragServeIndexing) Generations(ctx context.Context, scope contracts.Scope, sourceID string) ([]rag.IndexGeneration, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if _, err := g.held(scope, sourceID); err != nil {
		return nil, err
	}
	gens, err := g.ix.ListGenerations(ctx, scope, sourceID) // newest first
	if err != nil {
		return nil, err
	}
	if gens == nil {
		gens = []rag.IndexGeneration{}
	}
	return gens, nil
}

// Invalidate implements gateway.RAGIndexing.
func (g *ragServeIndexing) Invalidate(ctx context.Context, scope contracts.Scope, sourceID, generationID string) (rag.IndexGeneration, error) {
	if err := g.operate(ctx, scope); err != nil {
		return rag.IndexGeneration{}, err
	}
	if _, err := g.held(scope, sourceID); err != nil {
		return rag.IndexGeneration{}, err
	}
	// A generation of another source, of another scope and one that never existed are the same
	// answer: the one the index gives for an id it does not hold in this scope.
	gen, err := g.ix.GetGeneration(ctx, scope, generationID)
	if err != nil {
		return rag.IndexGeneration{}, ragServeGenerationErr(err)
	}
	if string(gen.SourceID) != sourceID {
		return rag.IndexGeneration{}, ragServeNotFound("generation")
	}
	if err := g.ix.InvalidateGeneration(ctx, scope, generationID, ragServeIndexingCause); err != nil {
		return rag.IndexGeneration{}, ragServeGenerationErr(err)
	}
	return g.ix.GetGeneration(ctx, scope, generationID)
}

// ragServeGenerationErr says not_found as the API does for an error of the index that already
// says it, and leaves every other error as it is.
func ragServeGenerationErr(err error) error {
	var ce *contracts.Error
	if errors.As(err, &ce) && ce.Code == contracts.ErrValidationFailed && ce.Details["reason"] == "not_found" {
		return ragServeNotFound("generation")
	}
	return err
}
