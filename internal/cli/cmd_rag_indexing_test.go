package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/rag/ingest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// ragServeIndexingRig is an ingested source and the indexing service over its stack, behind the
// scope guard the routes use.
func ragServeIndexingRig(t *testing.T) (*ragSourcesRig, gateway.RAGIndexing) {
	t.Helper()
	rig := ragSourcesNewRig(t, nil, ragHash, func(o *ragFixtureOpts) {
		// An operator of another action: the deployment lists it, and it may not manage sources.
		o.operators = append(append([]ragOperator(nil), ragSourcesOperatorEntries...),
			ragOperator{ApplicationID: ragApp, PrincipalID: "mass-only", Actions: []string{string(ingest.ActionConfirmMassDeletion)}})
	})
	rig.write("handbook.md", "# Warranty\n\n"+ragServeClaim)
	rig.register(t, ragSourcesOperator("t1", "w1"), "docs")
	rig.drain()
	return rig, gateway.RAGIndexingScoped(ragServeNewIndexing(rig.stack))
}

func ragServeSourceID(t *testing.T, rig *ragSourcesRig, scope contracts.Scope) string {
	t.Helper()
	recs, err := rig.stack.api.Status(context.Background(), scope, "")
	if err != nil || len(recs) == 0 {
		t.Fatalf("Status = %v, %v", recs, err)
	}
	return string(recs[0].Status.SourceID)
}

func ragServeNotFoundErr(t *testing.T, what string, err error) {
	t.Helper()
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce.Code != contracts.ErrValidationFailed || ce.Details["reason"] != "not_found" {
		t.Fatalf("%s = %v, want validation_failed with details.reason not_found", what, err)
	}
}

// Sync, rebuild and invalidate are operator acts: the manage_sources action of the "rag" section,
// like registering and removing a source, and nothing a token's roles say. Listing generations is
// a read in the caller's own scope.
func TestRAGIndexingRoutesAreGatedByManageSources(t *testing.T) {
	ctx := context.Background()
	rig, idx := ragServeIndexingRig(t)
	op := ragSourcesOperator("t1", "w1")
	id := ragServeSourceID(t, rig, op)

	gens, err := idx.Generations(ctx, op, id)
	if err != nil || len(gens) != 1 || gens[0].State != rag.IndexGenerationStateActive {
		t.Fatalf("Generations = %+v, %v", gens, err)
	}
	genID := string(gens[0].GenerationID)

	// Whoever the deployment lists for another action, whoever claims a role, and everybody else.
	for name, who := range map[string]contracts.Scope{
		"a member":                             ragScope("t1", "w1", "usr-1"),
		"an operator of another action":        ragScope("t1", "w1", "mass-only"),
		"a principal whose roles say operator": ragScope("t1", "w1", "usr-1", "operator", "admin", "ops-lead"),
	} {
		if _, err := idx.Sync(ctx, who, id); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Errorf("%s: Sync = %v, want permission_denied", name, err)
		}
		if _, err := idx.Rebuild(ctx, who, id); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Errorf("%s: Rebuild = %v, want permission_denied", name, err)
		}
		if _, err := idx.Invalidate(ctx, who, id, genID); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Errorf("%s: Invalidate = %v, want permission_denied", name, err)
		}
		// The refusal is the same for a source that does not exist: it says nothing about sources.
		if _, err := idx.Sync(ctx, who, ids.New("src")); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Errorf("%s: Sync of an unknown source = %v, want permission_denied", name, err)
		}
		// Reading the generations of the caller's own scope needs no operator.
		if got, err := idx.Generations(ctx, who, id); err != nil || len(got) != 1 {
			t.Errorf("%s: Generations = %+v, %v", name, got, err)
		}
	}
	if gens, _ := idx.Generations(ctx, op, id); len(gens) != 1 || gens[0].State != rag.IndexGenerationStateActive {
		t.Fatalf("a refused act changed the index: %+v", gens)
	}
	if n := rig.syncJobs(t); n != 1 {
		t.Fatalf("%d sync jobs: a refused request queued work", n)
	}

	// The operator's sync is a durable job under the service identity; a second request while it
	// waits is the same job.
	first, err := idx.Sync(ctx, op, id)
	if err != nil || first.SourceID != id || !strings.HasPrefix(first.JobID, "job_") {
		t.Fatalf("Sync = %+v, %v", first, err)
	}
	again, err := idx.Sync(ctx, op, id)
	if err != nil || again.JobID != first.JobID {
		t.Fatalf("a second sync while one waits = %+v, %v; want the same job %s", again, err, first.JobID)
	}
	if job := rig.jobState(first.JobID); job.ServiceIdentity != ragServiceIdentity || job.Type != ingest.JobSync {
		t.Fatalf("the sync job = %+v", job)
	}
	rig.drain()

	// Invalidating the generation that serves withdraws it: the source answers nothing until a
	// rebuild activates a replacement, and the answer is the generation as it now stands.
	if got := ragTexts(rig.ask("warranty kettle")); len(got) == 0 {
		t.Fatal("the source answers nothing before it is invalidated")
	}
	gone, err := idx.Invalidate(ctx, op, id, genID)
	if err != nil || gone.State != rag.IndexGenerationStateInvalidated || string(gone.GenerationID) != genID || string(gone.SourceID) != id {
		t.Fatalf("Invalidate = %+v, %v", gone, err)
	}
	if got := ragTexts(rig.ask("warranty kettle")); len(got) != 0 {
		t.Fatalf("an invalidated generation still serves: %v", got)
	}
	if _, err := idx.Invalidate(ctx, op, id, genID); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Fatalf("invalidating a generation twice = %v, want conflict", err)
	}

	// The rebuild queues the sync that builds the replacement; the invalidated one is not touched,
	// and the list is newest first.
	job, err := idx.Rebuild(ctx, op, id)
	if err != nil || job.SourceID != id || !strings.HasPrefix(job.JobID, "job_") {
		t.Fatalf("Rebuild = %+v, %v", job, err)
	}
	rig.drain()
	gens, err = idx.Generations(ctx, op, id)
	if err != nil || len(gens) != 2 {
		t.Fatalf("Generations after the rebuild = %+v, %v", gens, err)
	}
	if gens[0].State != rag.IndexGenerationStateActive || string(gens[0].GenerationID) == genID ||
		gens[1].State != rag.IndexGenerationStateInvalidated || string(gens[1].GenerationID) != genID {
		t.Fatalf("generations = %+v, want the replacement first and the invalidated one second", gens)
	}
	if got := ragTexts(rig.ask("warranty kettle")); len(got) == 0 {
		t.Fatal("the rebuilt generation answers nothing")
	}
}

// Every method answers a source outside the scope exactly like one that does not exist.
func TestRAGIndexingForeignSourceIsNotFound(t *testing.T) {
	ctx := context.Background()
	rig, idx := ragServeIndexingRig(t)
	rig.pkg.declare("faq")
	op := ragSourcesOperator("t1", "w1")
	id := ragServeSourceID(t, rig, op)
	gens, err := idx.Generations(ctx, op, id)
	if err != nil || len(gens) != 1 {
		t.Fatalf("Generations = %+v, %v", gens, err)
	}
	genID := string(gens[0].GenerationID)
	second := string(rig.register(t, op, "faq").Status.SourceID)

	// The operator of another tenant, another workspace, and a source that does not exist.
	for name, tc := range map[string]struct {
		scope contracts.Scope
		src   string
	}{
		"another tenant":    {ragSourcesOperator("t2", "w1"), id},
		"another workspace": {ragSourcesOperator("t1", "w2"), id},
		"an unknown source": {op, ids.New("src")},
	} {
		if _, err := idx.Sync(ctx, tc.scope, tc.src); err == nil {
			t.Errorf("%s: Sync succeeded", name)
		} else {
			ragServeNotFoundErr(t, name+": Sync", err)
		}
		_, err := idx.Rebuild(ctx, tc.scope, tc.src)
		ragServeNotFoundErr(t, name+": Rebuild", err)
		_, err = idx.Generations(ctx, tc.scope, tc.src)
		ragServeNotFoundErr(t, name+": Generations", err)
		_, err = idx.Invalidate(ctx, tc.scope, tc.src, genID)
		ragServeNotFoundErr(t, name+": Invalidate", err)
	}

	// A generation the source does not have: one of another source of the same scope, one that
	// never existed and one of the same scope reached through another tenant's source id.
	_, err = idx.Invalidate(ctx, op, second, genID)
	ragServeNotFoundErr(t, "Invalidate of another source's generation", err)
	_, err = idx.Invalidate(ctx, op, id, ids.New("gen"))
	ragServeNotFoundErr(t, "Invalidate of an unknown generation", err)
	if got, err := idx.Generations(ctx, op, second); err != nil || len(got) != 0 {
		t.Fatalf("the second source's generations = %+v, %v; want an empty list, not the first source's", got, err)
	}

	// None of it changed anything.
	if got, _ := idx.Generations(ctx, op, id); len(got) != 1 || got[0].State != rag.IndexGenerationStateActive {
		t.Fatalf("a request for a source or generation outside the scope changed the index: %+v", got)
	}

	// A source whose removal began is not found either.
	if err := rig.stack.sources.Remove(ctx, op, second); err != nil {
		t.Fatal(err)
	}
	_, err = idx.Sync(ctx, op, second)
	ragServeNotFoundErr(t, "Sync of a removed source", err)
}
