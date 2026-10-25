package vector

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

type jobsFixture struct {
	*pipelineFixture
	clk     *clock.Fake
	queue   *jobs.Queue
	pool    *jobs.Pool
	allowed contracts.ErrCode // when set, the scope checker fails with it
}

func newJobsFixture(t *testing.T) *jobsFixture {
	t.Helper()
	jf := &jobsFixture{pipelineFixture: newPipelineFixture(t, 8), clk: clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))}
	var err error
	jf.queue, err = jobs.NewQueue(jf.db, jobs.Options{Clock: jf.clk, Jitter: func() float64 { return 0 }})
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	jf.pool, err = jobs.NewPool(jf.queue, jobs.PoolOptions{ScopeChecker: jobs.ScopeCheckerFunc(func(context.Context, contracts.Scope) error {
		if jf.allowed != "" {
			return contracts.New(jf.allowed, "scope revoked")
		}
		return nil
	})})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := jf.pool.Shutdown(ctx); err != nil {
			t.Errorf("pool shutdown: %v", err)
		}
	})
	if err := jf.pipe.Register(jf.queue, jf.pool); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return jf
}

func (jf *jobsFixture) wait(scope contracts.Scope, id string, want jobs.State) jobs.Job {
	jf.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		j, err := jf.queue.Get(bg, scope, id)
		if err != nil {
			jf.t.Fatalf("Get: %v", err)
		}
		if j.State == want {
			return j
		}
		if time.Now().After(deadline) {
			jf.t.Fatalf("job %s is %s (code %q, attempts %d), waited for %s", id, j.State, j.ErrorCode, j.Attempts, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func (jf *jobsFixture) poll() {
	jf.t.Helper()
	if _, err := jf.pool.Poll(bg); err != nil {
		jf.t.Fatalf("Poll: %v", err)
	}
}

// Master plan 3 rule 13 and 17.1: embedding is queued, class-5 ingestion work.
func TestEmbedJobRunsOnTheQueueAtClassFive(t *testing.T) {
	jf := newJobsFixture(t)
	scope := storetest.Scope("app", "tenA", "ws1")
	cs := chunkSet("q", 20)
	jf.src.put(scope, cs...)

	if EmbedJobSpec().Class != jobs.ClassIngest || jobs.ClassIngest != 5 {
		t.Fatalf("embedding job class = %d, want 5 (ingestion and embeddings)", EmbedJobSpec().Class)
	}
	req := EmbedRequest{Generation: "gen_1", SourceID: "src_1", ChunkIDs: idsOf(cs)}
	res, err := EnqueueEmbed(bg, jf.queue, scope, req)
	if err != nil {
		t.Fatalf("EnqueueEmbed: %v", err)
	}
	if res.Duplicate || res.Job.Class != jobs.ClassIngest || res.Job.Type != EmbedJobType {
		t.Fatalf("enqueued %+v, want a new class-5 %s job", res.Job, EmbedJobType)
	}
	// The payload is references: ids, never chunk text (payloads sit in clear text).
	if p := string(res.Job.Payload); strings.Contains(p, "alpha") || strings.Contains(p, "para") || !strings.Contains(p, cs[0].ID) {
		t.Fatalf("payload %s carries more than ids", p)
	}
	// Repeating the producer's call does not queue the work twice, whatever order the ids come in.
	rev := append([]string(nil), req.ChunkIDs...)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	again, err := EnqueueEmbed(bg, jf.queue, scope, EmbedRequest{Generation: "gen_1", SourceID: "src_1", ChunkIDs: rev})
	if err != nil || !again.Duplicate || again.Job.ID != res.Job.ID {
		t.Fatalf("second enqueue = %+v (%v), want the first job back", again, err)
	}

	jf.poll()
	jf.wait(scope, res.Job.ID, jobs.StateSucceeded)
	st, err := jf.index.Stats(bg, scope)
	if err != nil || st.Vectors != 20 {
		t.Fatalf("stats after the job = %+v (%v), want 20 vectors", st, err)
	}
	q := fake.EmbedVector(EmbeddingText(cs[3]), embedDim)
	hit, err := jf.index.Search(bg, scope, Query{Vector: q, Model: testModel, K: 1, Generations: []string{"gen_1"}})
	if err != nil || len(hit.Hits) != 1 || hit.Hits[0].ChunkID != cs[3].ID {
		t.Fatalf("search after the job = %+v (%v)", hit, err)
	}
}

func TestEmbedJobRetriesWhenTheModelIsDown(t *testing.T) {
	jf := newJobsFixture(t)
	scope := storetest.Scope("app", "tenA", "ws1")
	cs := chunkSet("r", 5)
	jf.src.put(scope, cs...)
	res, err := EnqueueEmbed(bg, jf.queue, scope, EmbedRequest{Generation: "gen_1", SourceID: "src_1", ChunkIDs: idsOf(cs)})
	if err != nil {
		t.Fatal(err)
	}
	jf.emb.FailNext(fake.FailUnavailable)
	jf.poll()
	var j jobs.Job
	deadline := time.Now().Add(10 * time.Second)
	for {
		j, _ = jf.queue.Get(bg, scope, res.Job.ID)
		if j.State == jobs.StatePending && j.Attempts == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job is %s after an unavailable embedder, want pending for a retry", j.State)
		}
		time.Sleep(time.Millisecond)
	}
	if j.ErrorCode != contracts.ErrUnavailable {
		t.Fatalf("recorded code %q, want unavailable", j.ErrorCode)
	}
	if st, _ := jf.index.Stats(bg, scope); st.Vectors != 0 {
		t.Fatalf("a failed attempt stored %d vectors", st.Vectors)
	}
	jf.clk.Advance(time.Hour)
	jf.poll()
	jf.wait(scope, res.Job.ID, jobs.StateSucceeded)
	if st, _ := jf.index.Stats(bg, scope); st.Vectors != 5 {
		t.Fatalf("after the retry: %d vectors, want 5", st.Vectors)
	}
}

// A job whose initiating scope was revoked while it waited never reaches the
// embedder or the chunk source (the Pool re-validates the scope first).
func TestEmbedJobWithRevokedScopeDoesNothing(t *testing.T) {
	jf := newJobsFixture(t)
	scope := storetest.Scope("app", "tenA", "ws1")
	cs := chunkSet("v", 3)
	jf.src.put(scope, cs...)
	res, err := EnqueueEmbed(bg, jf.queue, scope, EmbedRequest{Generation: "gen_1", SourceID: "src_1", ChunkIDs: idsOf(cs)})
	if err != nil {
		t.Fatal(err)
	}
	jf.allowed = contracts.ErrPermissionDenied
	jf.poll()
	j := jf.wait(scope, res.Job.ID, jobs.StateFailed)
	if j.ErrorCode != contracts.ErrPermissionDenied {
		t.Fatalf("code %q, want permission_denied", j.ErrorCode)
	}
	if jf.emb.Calls() != 0 || jf.src.loads != 0 {
		t.Fatalf("the handler ran for a revoked scope: %d embedder calls, %d source loads", jf.emb.Calls(), jf.src.loads)
	}
}

func TestEmbedJobRefusesMalformedPayloads(t *testing.T) {
	jf := newJobsFixture(t)
	scope := storetest.Scope("app", "tenA", "ws1")
	for name, payload := range map[string]string{
		"unknown field":  `{"generation":"g","source_id":"s","chunk_ids":["c"],"model":"other"}`,
		"not json":       `{"generation":`,
		"no chunk ids":   `{"generation":"g","source_id":"s","chunk_ids":[]}`,
		"wrong type":     `{"generation":"g","source_id":"s","chunk_ids":"c"}`,
		"too many chunk": `{"generation":"g","source_id":"s","chunk_ids":[` + strings.Repeat(`"c",`, MaxJobChunks) + `"c"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			// A broken payload cannot be built by EnqueueEmbed, so go through the queue.
			var raw json.RawMessage = json.RawMessage(payload)
			if !json.Valid(raw) {
				raw = json.RawMessage(`"not an object"`)
			}
			res, err := jf.queue.Enqueue(bg, scope, jobs.EnqueueRequest{Type: EmbedJobType, Payload: raw, IdempotencyKey: name})
			if err != nil {
				t.Fatal(err)
			}
			jf.poll()
			j := jf.wait(scope, res.Job.ID, jobs.StateFailed)
			if j.Attempts != 1 || j.ErrorCode != contracts.ErrValidationFailed {
				t.Fatalf("job = state %s, attempts %d, code %q; want one attempt and validation_failed (no retry can fix a bad payload)", j.State, j.Attempts, j.ErrorCode)
			}
		})
	}
	if jf.emb.Calls() != 0 {
		t.Fatal("the embedder ran for a malformed job")
	}
}

// A job runs under the scope it was queued with: chunk ids that belong to
// another tenant are simply not there.
func TestEmbedJobStaysInItsScope(t *testing.T) {
	jf := newJobsFixture(t)
	a := storetest.Scope("app", "tenA", "ws1")
	b := storetest.Scope("app", "tenB", "ws1")
	secret := chunkSet("b", 3)
	jf.src.put(b, secret...)
	res, err := EnqueueEmbed(bg, jf.queue, a, EmbedRequest{Generation: "gen_1", SourceID: "src_1", ChunkIDs: idsOf(secret)})
	if err != nil {
		t.Fatal(err)
	}
	jf.poll()
	jf.wait(a, res.Job.ID, jobs.StateSucceeded)
	if jf.emb.Calls() != 0 {
		t.Fatal("another tenant's chunks were embedded under this job's scope")
	}
	for _, sc := range []contracts.Scope{a, b} {
		if st, _ := jf.index.Stats(bg, sc); st.Vectors != 0 {
			t.Fatalf("scope %s holds %d vectors", sc.TenantID, st.Vectors)
		}
	}
}
