package parse

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/parse/wire"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Parsing runs as a job on the durable queue: the handler loads the document
// under the job's scope, parses it in the sandbox, and the result chunks.
func TestParseJob(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	db := storetest.Open(t, nil)
	q, err := jobs.NewQueue(db, jobs.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterJob(q); err != nil {
		t.Fatal(err)
	}
	if spec := JobSpec(); spec.Class != jobs.ClassIngest || spec.MaxAttempts != 2 {
		t.Fatalf("spec = %+v", spec)
	}
	pool, err := jobs.NewPool(q, jobs.PoolOptions{ScopeChecker: jobs.ScopeCheckerFunc(func(context.Context, contracts.Scope) error { return nil })})
	if err != nil {
		t.Fatal(err)
	}

	docs := map[string][]byte{
		"ver_good": []byte("# Policy\n\nRefunds are paid within 14 days.\n"),
		"ver_bomb": []byte(strings.Repeat("a ", 1<<19)), // 1 MiB of text against a 64 KiB output cap
	}
	r := newTestRunner(t, func(c *Config) { c.MaxOutputBytes = 64 << 10 })

	var mu sync.Mutex
	chunks := map[string]int{}
	loaded := map[string]contracts.Scope{}
	done := make(chan string, 8)
	load := func(ctx context.Context, scope contracts.Scope, p JobPayload) ([]byte, error) {
		mu.Lock()
		loaded[p.VersionID] = scope
		mu.Unlock()
		return docs[p.VersionID], nil
	}
	ck, _ := chunk.New(chunk.DefaultConfig())
	sink := func(ctx context.Context, scope contracts.Scope, p JobPayload, res *Result) error {
		out, err := ck.Chunk(ctx, res.Document(p.SourceID))
		if err != nil {
			return err
		}
		mu.Lock()
		chunks[p.VersionID] = len(out.Children)
		mu.Unlock()
		done <- p.VersionID
		return nil
	}
	if err := pool.Handle(JobType, NewJobHandler(r, load, sink)); err != nil {
		t.Fatal(err)
	}

	scope := storetest.Scope("app", "tenantA", "ws1")
	enqueue := func(version string) jobs.Job {
		payload, err := JobPayload{SourceID: "src_01", VersionID: version, Format: wire.FormatMD}.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		res, err := q.Enqueue(context.Background(), scope, jobs.EnqueueRequest{Type: JobType, Payload: payload, IdempotencyKey: version})
		if err != nil {
			t.Fatal(err)
		}
		return res.Job
	}
	good, bomb := enqueue("ver_good"), enqueue("ver_bomb")

	// Poll until both have finished (the handlers run on their own goroutines).
	deadline := time.Now().Add(90 * time.Second)
	for {
		if _, err := pool.Poll(context.Background()); err != nil {
			t.Fatal(err)
		}
		gj, _ := q.Get(context.Background(), scope, good.ID)
		bj, _ := q.Get(context.Background(), scope, bomb.ID)
		if gj.State.Terminal() && bj.State.Terminal() {
			if gj.State != jobs.StateSucceeded {
				t.Fatalf("the good document's job is %s (%s %s)", gj.State, gj.ErrorCode, gj.ErrorMessage)
			}
			// The output-cap refusal is deterministic: failed at once, not retried.
			if bj.State != jobs.StateFailed || bj.ErrorCode != contracts.ErrValidationFailed || bj.Attempts != 1 {
				t.Fatalf("the oversized document's job: state %s code %s attempts %d (%s)", bj.State, bj.ErrorCode, bj.Attempts, bj.ErrorMessage)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the jobs did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if chunks["ver_good"] == 0 {
		t.Error("the parsed document produced no chunks")
	}
	if loaded["ver_good"].TenantID != "tenantA" {
		t.Errorf("the document was loaded under scope %+v, not the job's", loaded["ver_good"])
	}
}

func TestJobErrorMapsLimitKillsToDeterministicRefusals(t *testing.T) {
	cases := []struct {
		reason string
		in     contracts.ErrCode
		want   contracts.ErrCode
	}{
		{"wall_timeout", contracts.ErrTimeout, contracts.ErrValidationFailed},
		{"cpu_time", contracts.ErrTimeout, contracts.ErrValidationFailed},
		{"memory_limit", contracts.ErrResourceExhausted, contracts.ErrValidationFailed},
		{"startup_timeout", contracts.ErrTimeout, contracts.ErrTimeout},
		{"sandbox_unavailable", contracts.ErrUnavailable, contracts.ErrUnavailable},
		{"worker_crashed", contracts.ErrInternal, contracts.ErrInternal},
	}
	for _, c := range cases {
		err := jobError(contracts.New(c.in, "x").WithDetails("reason", c.reason))
		if got := contracts.CodeOf(err); got != c.want {
			t.Errorf("%s: %s becomes %s, want %s", c.reason, c.in, got, c.want)
		}
		if retry := contracts.CodeOf(err).Retryable(); retry != (c.want == contracts.ErrTimeout || c.want == contracts.ErrUnavailable) {
			t.Errorf("%s: retryable = %v", c.reason, retry)
		}
	}
}
