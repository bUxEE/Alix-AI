package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The footprint probe measures what the queue costs a Lite deployment. It runs
// in a child process (this test binary, re-executed) so the figures are the
// queue's and not the accumulated state of the rest of the test run, and so the
// OS-level numbers mean something. Reproduce docs/efficiency/M0-06.md with:
//
//	go test -run TestJobsFootprint -v ./internal/jobs/
//
// Knobs (optional): ALIXAI_PROBE_JOBS (queued jobs; default 3000, the
// efficiency review quotes 10000, which takes about 30 s).

type probeResult struct {
	Jobs        int  `json:"jobs"`
	MeasuredRSS bool `json:"measured_rss"`

	BaselineRSS    uint64 `json:"baseline_rss"`
	AfterOpenRSS   uint64 `json:"after_open_rss"`
	AfterEnqueue   uint64 `json:"after_enqueue_rss"`
	AfterDrainRSS  uint64 `json:"after_drain_rss"`
	PeakRSS        uint64 `json:"peak_rss"`
	GoHeapIdle     uint64 `json:"go_heap_idle_pool"`
	GoHeapQueued   uint64 `json:"go_heap_queued"`
	GoroutinesIdle int    `json:"goroutines_idle"`
	GoroutinesBase int    `json:"goroutines_base"`

	IdlePollNs     float64 `json:"idle_poll_ns"`
	IdlePollAllocs float64 `json:"idle_poll_allocs"`
	IdlePollBytes  float64 `json:"idle_poll_bytes"`

	EnqueueFirstUs float64 `json:"enqueue_first_chunk_us"`
	EnqueueLastUs  float64 `json:"enqueue_last_chunk_us"`
	EnqueuePerSec  float64 `json:"enqueue_per_sec"`

	LeaseUs         float64 `json:"lease_us"`
	CompleteUs      float64 `json:"complete_us"`
	LeaseBatch16Per float64 `json:"lease_batch16_per_job_us"`
	LifecyclePerSec float64 `json:"lifecycle_per_sec"`

	PoolDrainPerSec1 float64 `json:"pool_drain_per_sec_1_worker"`
	PoolDrainPerSec8 float64 `json:"pool_drain_per_sec_8_workers"`

	StatusMs float64 `json:"status_ms"`
	ReapMs   float64 `json:"reap_idle_ms"`
	PruneMs  float64 `json:"prune_1000_ms"`

	PolicyOneTenantUs    float64 `json:"policy_one_tenant_us"`
	PolicyManyTenantsUs  float64 `json:"policy_many_tenants_us"`
	PolicyManyTenants    int     `json:"policy_many_tenants"`
	DefaultManyTenantsUs float64 `json:"default_many_tenants_us"`

	DBBytes      int64 `json:"db_bytes"`
	WALBytes     int64 `json:"wal_bytes"`
	DBAfterCkpt  int64 `json:"db_bytes_after_checkpoint"`
	PayloadBytes int   `json:"payload_bytes"`
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func fileSize(path string) int64 {
	if st, err := os.Stat(path); err == nil {
		return st.Size()
	}
	return 0
}

func pct(sorted []float64, p int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[min(len(sorted)-1, len(sorted)*p/100)]
}

func micros(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1000 }
func ms(d time.Duration) float64     { return float64(d.Microseconds()) / 1000 }
func mib(b uint64) float64           { return float64(b) / (1 << 20) }

func summarise(lat []float64) (p50, p95 float64) {
	s := append([]float64(nil), lat...)
	sort.Float64s(s)
	return pct(s, 50), pct(s, 95)
}

var allowAll = ScopeCheckerFunc(func(context.Context, contracts.Scope) error { return nil })

// probeQueue builds a queue and a pool handling every class with a no-op handler.
func probeQueue(t *testing.T, cfg Config, opts ...func(*Options)) (*store.DB, *Queue, *Pool) {
	t.Helper()
	db := storetest.Open(t, nil)
	o := Options{Config: cfg}
	for _, m := range opts {
		m(&o)
	}
	q, err := NewQueue(db, o)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewPool(q, PoolOptions{ScopeChecker: allowAll})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range Classes() {
		name := typeOf(c)
		if err := q.Register(TypeSpec{Name: name, Class: c}); err != nil {
			t.Fatal(err)
		}
		if err := p.Handle(name, func(context.Context, Job) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	return db, q, p
}

func probePayload(i int) []byte {
	return []byte(fmt.Sprintf(`{"source":"src_%08d","doc":"doc_%08d","op":"parse","hint":"%s"}`, i, i, strings.Repeat("x", 120)))
}

// TestJobsProbeChild is the measuring half. It does nothing unless the parent
// sets ALIXAI_PROBE_OUT, so a plain `go test` run passes it instantly.
func TestJobsProbeChild(t *testing.T) {
	out := os.Getenv("ALIXAI_PROBE_OUT")
	if out == "" {
		return
	}
	n := envInt("ALIXAI_PROBE_JOBS", 3000)
	res := probeResult{Jobs: n, PayloadBytes: len(probePayload(1)), GoroutinesBase: runtime.NumGoroutine()}
	var ok bool
	res.BaselineRSS, _, _, ok = processMemory()
	res.MeasuredRSS = ok

	big := ClassConfig{Workers: 1, MaxDepth: 1_000_000}
	cfg := Config{Class4: ClassConfig{Workers: 2, MaxDepth: 1_000_000}, Class5: big}
	db, q, pool := probeQueue(t, cfg)
	res.AfterOpenRSS, _, _, _ = processMemory()
	res.GoroutinesIdle = runtime.NumGoroutine()
	var ms0, ms1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms0)
	res.GoHeapIdle = ms0.HeapInuse

	// Idle poll: nothing queued, every class has a handler. This is what a quiet
	// deployment pays once per PollInterval.
	const polls = 3000
	runtime.ReadMemStats(&ms0)
	start := time.Now()
	for i := 0; i < polls; i++ {
		if _, err := pool.Poll(bg); err != nil {
			t.Fatal(err)
		}
	}
	el := time.Since(start)
	runtime.ReadMemStats(&ms1)
	res.IdlePollNs = float64(el.Nanoseconds()) / polls
	res.IdlePollAllocs = float64(ms1.Mallocs-ms0.Mallocs) / polls
	res.IdlePollBytes = float64(ms1.TotalAlloc-ms0.TotalAlloc) / polls

	// Enqueue n jobs of class 5 over five tenants, one transaction each.
	scopes := make([]contracts.Scope, 5)
	for i := range scopes {
		scopes[i] = storetest.Scope("app", fmt.Sprintf("tenant%d", i), "ws1")
	}
	const chunk = 1000
	chunkUs := make([]float64, 0, n/chunk+1)
	start = time.Now()
	chunkStart := start
	for i := 0; i < n; i++ {
		if _, err := q.Enqueue(bg, scopes[i%5], EnqueueRequest{Type: typeOf(ClassIngest), Payload: probePayload(i)}); err != nil {
			t.Fatal(err)
		}
		if (i+1)%chunk == 0 {
			chunkUs = append(chunkUs, micros(time.Since(chunkStart))/chunk)
			chunkStart = time.Now()
		}
	}
	res.EnqueuePerSec = float64(n) / time.Since(start).Seconds()
	if len(chunkUs) > 0 {
		res.EnqueueFirstUs, res.EnqueueLastUs = chunkUs[0], chunkUs[len(chunkUs)-1]
	}
	res.AfterEnqueue, _, _, _ = processMemory()
	runtime.GC()
	runtime.ReadMemStats(&ms1)
	res.GoHeapQueued = ms1.HeapInuse
	res.DBBytes, res.WALBytes = fileSize(db.Path()), fileSize(db.Path()+"-wal")
	if _, err := db.ExecContext(bg, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Logf("checkpoint: %v", err)
	}
	res.DBAfterCkpt = fileSize(db.Path())

	st := time.Now()
	if _, err := q.Status(bg); err != nil {
		t.Fatal(err)
	}
	res.StatusMs = ms(time.Since(st))
	st = time.Now()
	if _, err := q.Reap(bg); err != nil {
		t.Fatal(err)
	}
	res.ReapMs = ms(time.Since(st))

	// Lease one job at a time (one transaction each), then complete each: the
	// per-operation cost at depth n.
	const single = 1000
	held1 := make([]Leased, 0, single)
	start = time.Now()
	for i := 0; i < single; i++ {
		got, err := q.Lease(bg, LeaseRequest{Classes: []Class{ClassIngest}, Max: 1})
		if err != nil || len(got) != 1 {
			t.Fatalf("Lease: %v (%d)", err, len(got))
		}
		held1 = append(held1, got[0])
	}
	leaseEl := time.Since(start)
	start = time.Now()
	for _, l := range held1 {
		if err := q.Complete(bg, l); err != nil {
			t.Fatal(err)
		}
	}
	doneEl := time.Since(start)
	res.LeaseUs, res.CompleteUs = micros(leaseEl)/single, micros(doneEl)/single
	res.LifecyclePerSec = float64(single) / (leaseEl + doneEl).Seconds()

	// Lease sixteen at a time (one transaction), then complete each.
	const batches = 50
	var leaseOnly time.Duration
	var held []Leased
	for i := 0; i < batches; i++ {
		t0 := time.Now()
		got, err := q.Lease(bg, LeaseRequest{Classes: []Class{ClassIngest}, Max: 16})
		leaseOnly += time.Since(t0)
		if err != nil || len(got) != 16 {
			t.Fatalf("batch Lease: %v (%d)", err, len(got))
		}
		held = append(held, got...)
	}
	res.LeaseBatch16Per = micros(leaseOnly) / float64(len(held))
	for _, l := range held {
		if err := q.Complete(bg, l); err != nil {
			t.Fatal(err)
		}
	}

	// Retention: delete 1000 finished jobs.
	if _, err := db.ExecContext(bg, `UPDATE jobs SET finished_at = 1 WHERE state = 'succeeded'`); err != nil {
		t.Fatal(err)
	}
	st = time.Now()
	if got, err := q.Prune(bg); err != nil || got != pruneBatch {
		t.Fatalf("Prune = %d, %v", got, err)
	}
	res.PruneMs = ms(time.Since(st))

	// Drain what is left through the pool with a no-op handler: end-to-end
	// jobs per second with one class-5 worker, then with eight class-4 workers.
	remaining := n - single - batches*16
	st = time.Now()
	runCtx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { _ = pool.Run(runCtx); close(done) }()
	for {
		var pending int
		if err := db.QueryRowContext(bg, `SELECT count(*) FROM jobs WHERE state IN ('pending', 'running')`).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending == 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	res.PoolDrainPerSec1 = float64(remaining) / time.Since(st).Seconds()
	cancel()
	<-done
	// Sampled before the extra databases below are opened, so the figures are
	// one queue's.
	res.AfterDrainRSS, res.PeakRSS, _, _ = processMemory()

	db8, q8, p8 := probeQueue(t, Config{Class4: ClassConfig{Workers: 8, MaxDepth: 1_000_000}})
	for i := 0; i < 3000; i++ {
		if _, err := q8.Enqueue(bg, scopes[i%5], EnqueueRequest{Type: typeOf(ClassRead), Payload: probePayload(i)}); err != nil {
			t.Fatal(err)
		}
	}
	st = time.Now()
	runCtx8, cancel8 := context.WithCancel(bg)
	done8 := make(chan struct{})
	go func() { _ = p8.Run(runCtx8); close(done8) }()
	for {
		var pending int
		if err := db8.QueryRowContext(bg, `SELECT count(*) FROM jobs WHERE state IN ('pending', 'running')`).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending == 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	res.PoolDrainPerSec8 = 3000 / time.Since(st).Seconds()
	cancel8()
	<-done8

	// A custom dequeue policy: the cost of building the one-candidate-per-tenant
	// window, with one tenant and with many tenants queued.
	pick0 := DequeuePolicyFunc(func([]Candidate) int { return 0 })
	probePolicy := func(policy DequeuePolicy, tenants, perTenant int) float64 {
		_, pq, _ := probeQueue(t, Config{Class5: ClassConfig{Workers: 1, MaxDepth: 1_000_000}}, func(o *Options) { o.Policy = policy })
		for tn := 0; tn < tenants; tn++ {
			s := storetest.Scope("app", fmt.Sprintf("tenant%05d", tn), "ws1")
			for i := 0; i < perTenant; i++ {
				if _, err := pq.Enqueue(bg, s, EnqueueRequest{Type: typeOf(ClassIngest), Payload: probePayload(i)}); err != nil {
					t.Fatal(err)
				}
			}
		}
		const leases = 200
		t0 := time.Now()
		for i := 0; i < leases; i++ {
			if got, err := pq.Lease(bg, LeaseRequest{Classes: []Class{ClassIngest}, Max: 1}); err != nil || len(got) != 1 {
				t.Fatalf("policy Lease: %v (%d)", err, len(got))
			}
		}
		return micros(time.Since(t0)) / leases
	}
	res.PolicyOneTenantUs = probePolicy(pick0, 1, 1500)
	res.PolicyManyTenants = 500
	res.PolicyManyTenantsUs = probePolicy(pick0, 500, 3)
	res.DefaultManyTenantsUs = probePolicy(nil, 500, 3)

	b, _ := json.Marshal(res)
	if err := os.WriteFile(out, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestJobsFootprint runs the probe in a child process, logs the figures that
// docs/efficiency/M0-06.md quotes, and holds the queue to a memory ceiling so
// a change that quietly inflates it fails here.
func TestJobsFootprint(t *testing.T) {
	if raceEnabled {
		t.Log("built with -race: its overhead would make the footprint figures meaningless; measured in the normal build")
		return
	}
	if testing.Short() {
		t.Skip("footprint probe skipped in -short mode")
	}
	out := filepath.Join(t.TempDir(), "probe.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestJobsProbeChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), "ALIXAI_PROBE_OUT="+out)
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("probe child failed: %v\n%s", err, o)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("probe wrote no result: %v", err)
	}
	var r probeResult
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	t.Logf("jobs=%d payload=%d bytes", r.Jobs, r.PayloadBytes)
	t.Logf("idle pool (6 classes, all with handlers): %.0f ns, %.1f allocs, %.0f bytes per Poll; goroutines %d -> %d; go heap in use %.2f MiB",
		r.IdlePollNs, r.IdlePollAllocs, r.IdlePollBytes, r.GoroutinesBase, r.GoroutinesIdle, mib(r.GoHeapIdle))
	t.Logf("enqueue (one transaction each): %.0f jobs/s overall; first 1000 %.0f us each, last 1000 %.0f us each (depth grows to %d)",
		r.EnqueuePerSec, r.EnqueueFirstUs, r.EnqueueLastUs, r.Jobs)
	t.Logf("at depth ~%d: lease (1 job per tx) %.0f us, complete %.0f us, lease+complete %.0f jobs/s; lease in batches of 16: %.0f us per job",
		r.Jobs, r.LeaseUs, r.CompleteUs, r.LifecyclePerSec, r.LeaseBatch16Per)
	t.Logf("pool end-to-end (no-op handler): 1 worker %.0f jobs/s, 8 workers %.0f jobs/s", r.PoolDrainPerSec1, r.PoolDrainPerSec8)
	t.Logf("Status %.2f ms, idle Reap %.2f ms, Prune(1000) %.1f ms", r.StatusMs, r.ReapMs, r.PruneMs)
	t.Logf("lease with a custom dequeue policy: one tenant x1500 queued %.0f us; %d tenants x3 queued %.0f us (default FIFO on the same data %.0f us)",
		r.PolicyOneTenantUs, r.PolicyManyTenants, r.PolicyManyTenantsUs, r.DefaultManyTenantsUs)
	t.Logf("storage after %d jobs: db %.2f MiB, wal %.2f MiB; db after checkpoint %.2f MiB = %.0f bytes per job",
		r.Jobs, mib(uint64(r.DBBytes)), mib(uint64(r.WALBytes)), mib(uint64(r.DBAfterCkpt)), float64(r.DBAfterCkpt)/float64(r.Jobs))
	if !r.MeasuredRSS {
		t.Log("no OS memory counters on this platform; skipping the RSS ceiling")
		return
	}
	t.Logf("RSS MiB: baseline %.1f -> after open+queue+pool %.1f -> after %d enqueued %.1f -> after drains and policy runs %.1f; peak %.1f",
		mib(r.BaselineRSS), mib(r.AfterOpenRSS), r.Jobs, mib(r.AfterEnqueue), mib(r.AfterDrainRSS), mib(r.PeakRSS))
	const ceilingMiB = 128 // generous: the store's page caches dominate, not the queue
	if growth := mib(r.PeakRSS) - mib(r.BaselineRSS); growth > ceilingMiB {
		t.Errorf("queue probe grew the process by %.1f MiB at peak, ceiling %d MiB", growth, ceilingMiB)
	}
}
