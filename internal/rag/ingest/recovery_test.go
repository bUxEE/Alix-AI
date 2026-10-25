package ingest

import (
	"testing"

	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

func TestSyncRequestsCoalesceAndAreNotSwallowedAfterAFailure(t *testing.T) {
	f := newFixture(t)
	corpus(f, 2)

	// Requests that arrive while a sync waits are one sync.
	first, err := f.svc.RequestSync(bg, f.scope, f.srcID, SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.svc.RequestSync(bg, f.scope, f.srcID, SyncOptions{})
	if err != nil || !again.Duplicate || again.Job.ID != first.Job.ID {
		t.Fatalf("a second request did not coalesce: %+v %v", again, err)
	}

	// A sync that was refused (the principal is revoked) did not scan anything,
	// so the next request is a new sync and not the failed one handed back.
	f.gate.set(contracts.New(contracts.ErrPermissionDenied, "revoked"))
	f.drain()
	mustEqual(t, "first sync", f.job(first.Job.ID).State, jobs.StateFailed)
	f.gate.set(nil)
	next, err := f.svc.RequestSync(bg, f.scope, f.srcID, SyncOptions{})
	if err != nil || next.Duplicate || next.Job.ID == first.Job.ID {
		t.Fatalf("the request was swallowed by the failed job: %+v %v", next, err)
	}
	f.drain()
	mustEqual(t, "second sync", f.job(next.Job.ID).State, jobs.StateSucceeded)
	mustEqual(t, "documents", len(docHits(f.search("alpha"))), 2)

	// An explicit key is honoured exactly: the same key is the same job, finished
	// or not.
	a, err := f.svc.RequestSync(bg, f.scope, f.srcID, SyncOptions{Key: "scheduler:tick-1"})
	if err != nil {
		t.Fatal(err)
	}
	f.drain()
	b, err := f.svc.RequestSync(bg, f.scope, f.srcID, SyncOptions{Key: "scheduler:tick-1"})
	if err != nil || !b.Duplicate || b.Job.ID != a.Job.ID {
		t.Fatalf("an explicit key was not idempotent: %+v %v", b, err)
	}
}

func TestAGenerationWhoseActivationFailedIsActivatedByTheNextSync(t *testing.T) {
	f := newFixture(t)
	corpus(f, 2)
	f.requestSync()
	for i := 0; i < 50 && len(pendingOf(f, JobFinalize)) == 0; i++ {
		f.poll()
	}
	if len(pendingOf(f, JobFinalize)) == 0 {
		t.Fatal("the build never reached its activation")
	}
	// The activation job is refused (the scope was revoked), so the generation
	// stays under construction and nothing serves yet.
	f.gate.set(contracts.New(contracts.ErrPermissionDenied, "revoked"))
	for i := 0; i < 20 && len(pendingOf(f, JobFinalize)) > 0; i++ {
		f.poll()
	}
	fin := f.jobsOf(JobFinalize)
	mustEqual(t, "finalize state", fin[0].State, jobs.StateFailed)
	gens := f.generations()
	mustEqual(t, "generations", len(gens), 1)
	mustEqual(t, "generation state", gens[0].State, rag.IndexGenerationStateBuilding)
	mustEqual(t, "documents searchable", len(f.search("alpha")), 0)
	mustEqual(t, "state", f.status().State, rag.SourceStatusStateIndexing)

	// Access restored: the next sync sees a complete build and queues the
	// activation again; it is not swallowed by the failed job of the same key.
	f.gate.set(nil)
	f.sync()
	mustEqual(t, "generation state after recovery", f.activeGen().State, rag.IndexGenerationStateActive)
	mustEqual(t, "documents searchable", len(docHits(f.search("alpha"))), 2)
	mustEqual(t, "state", f.status().State, rag.SourceStatusStateReady)
}

func TestAnEmptySourceActivatesAnEmptyGeneration(t *testing.T) {
	f := newFixture(t)
	j := f.sync()
	mustEqual(t, "sync", j.State, jobs.StateSucceeded)
	g := f.activeGen()
	mustEqual(t, "documents", g.Counts.Documents, 0)
	mustEqual(t, "state", f.status().State, rag.SourceStatusStateReady)
}

func TestAFailingRebuildDoesNotReplaceAServingGeneration(t *testing.T) {
	f := newFixture(t)
	corpus(f, 2)
	f.sync()
	gen1 := f.activeGen()

	// A new parser version that cannot parse anything: the rebuild would leave the
	// source with no documents, so it is refused and the serving generation stays.
	f.parser.setVersion("2.0.0")
	f.parser.fail = func([]byte) error {
		return contracts.New(contracts.ErrValidationFailed, "unsupported by this parser").WithDetails("reason", "unsupported")
	}
	f.sync()
	mustEqual(t, "serving generation", f.activeGen().GenerationID, gen1.GenerationID)
	mustEqual(t, "documents searchable", len(docHits(f.search("alpha"))), 2)
	failed := 0
	for _, g := range f.generations() {
		if g.State == rag.IndexGenerationStateFailed {
			failed++
		}
	}
	mustEqual(t, "failed generations", failed, 1)
	mustEqual(t, "finalize", f.jobsOf(JobReindexFinalize)[0].State, jobs.StateFailed)

	// The parser is fixed: the next sync builds again and activates.
	f.parser.fail = nil
	f.sync()
	if f.activeGen().GenerationID == gen1.GenerationID {
		t.Fatal("the rebuild never replaced the old generation")
	}
	mustEqual(t, "documents searchable", len(docHits(f.search("alpha"))), 2)
}
