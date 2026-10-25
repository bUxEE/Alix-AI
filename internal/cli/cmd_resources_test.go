package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/rag/ingest"
	"github.com/buxee/alix-ai/internal/resources"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The tests of M3-30: serve builds the resource governor and the disk guard, runs the loop,
// installs the governor's policy on the one job queue, and arbitrates live turns.

// resourcesTestWiring is the production wiring over the given clock and fixed probes: 100 MiB
// of resident memory and 100 GiB of free disk, whatever the host has.
func resourcesTestWiring(t testing.TB, clk clock.Clock, mutate ...func(*resourcesInput)) *resourcesWiring {
	t.Helper()
	in := resourcesInput{
		Log: discardLog(), Clock: clk, DataDir: t.TempDir(),
		MemoryProbe: resources.MemoryProbeFunc(func() (uint64, error) { return 100 << 20, nil }),
		DiskProbe:   resources.DiskProbeFunc(func(string) (uint64, error) { return 100 << 30, nil }),
	}
	for _, m := range mutate {
		m(&in)
	}
	w, err := resourcesNew(in)
	if err != nil {
		t.Fatalf("resourcesNew: %v", err)
	}
	return w
}

// resourcesFakeGen is a TextGenerator whose calls the test controls.
type resourcesFakeGen struct {
	// chat runs inside Chat; nil answers at once.
	chat   func(ctx context.Context) error
	stream providers.ChatStream
	// streamErr fails ChatStream.
	streamErr error
}

func (g *resourcesFakeGen) Chat(ctx context.Context, _ contracts.Scope, _ providers.ChatRequest) (providers.ChatResult, error) {
	if g.chat != nil {
		if err := g.chat(ctx); err != nil {
			return providers.ChatResult{}, err
		}
	}
	return providers.ChatResult{Text: "ok"}, nil
}

func (g *resourcesFakeGen) ChatStream(context.Context, contracts.Scope, providers.ChatRequest) (providers.ChatStream, error) {
	return g.stream, g.streamErr
}

// resourcesFakeStream yields its chunks, then io.EOF, and counts its closes.
type resourcesFakeStream struct {
	mu     sync.Mutex
	chunks []providers.ChatChunk
	closes int
}

func (s *resourcesFakeStream) Recv() (providers.ChatChunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.chunks) == 0 {
		return providers.ChatChunk{}, io.EOF
	}
	c := s.chunks[0]
	s.chunks = s.chunks[1:]
	return c, nil
}

func (s *resourcesFakeStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closes++
	return nil
}

// ---- the class boundaries ----------------------------------------------------------

// resources.interactive_up_to_class and background_from_class are not a second place to
// configure the yield rule: they are the queue's effective values, jobs defaults raised to the
// RAG value of 4, whatever the resources section says.
func TestGovernorClassBoundariesComeFromTheJobsConfig(t *testing.T) {
	cases := []struct {
		name               string
		upTo, from         int
		wantUpTo, wantFrom int
	}{
		{"defaults", 0, 0, 4, 5},
		{"raised to the RAG value", 2, 0, 4, 5},
		{"a later background class", 0, 6, 4, 6},
		{"the rule is off", 0, 7, 4, 7},
		{"already above the RAG value", 5, 7, 5, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jc := jobs.DefaultConfig()
			jc.InteractiveUpTo, jc.BackgroundFrom = tc.upTo, tc.from
			upTo, from := resourcesClassBoundaries(jc)
			if upTo != tc.wantUpTo || from != tc.wantFrom {
				t.Fatalf("boundaries = %d, %d, want %d, %d", upTo, from, tc.wantUpTo, tc.wantFrom)
			}
			// And they are what the queue serve builds actually runs with: one rule, two readers.
			f := newRagFixture(t, func(o *ragFixtureOpts) {
				o.nopool = true
				o.jobs = func(c *jobs.Config) { *c = jc }
			})
			got := f.stack.queue.Config()
			if got.InteractiveUpTo != tc.wantUpTo || got.BackgroundFrom != tc.wantFrom {
				t.Errorf("the queue runs with %d, %d, the governor would copy %d, %d", got.InteractiveUpTo, got.BackgroundFrom, tc.wantUpTo, tc.wantFrom)
			}
		})
	}

	t.Run("what the resources section says is overridden", func(t *testing.T) {
		reg := config.NewRegistry()
		registerServeSections(reg)
		path := filepath.Join(t.TempDir(), "alixai.yaml")
		doc := "jobs:\n  interactive_up_to_class: 2\n  background_from_class: 6\nresources:\n  interactive_up_to_class: 1\n  background_from_class: 7\n"
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := reg.LoadFrom(path, nil); err != nil {
			t.Fatalf("load: %v", err)
		}
		w := resourcesTestWiring(t, clock.Real{}, func(in *resourcesInput) { in.Config = reg })
		if c := w.Gov.Config(); c.InteractiveUpTo != 4 || c.BackgroundFrom != 6 {
			t.Errorf("the governor runs with %d, %d; the jobs section says 2 (raised to 4) and 6", c.InteractiveUpTo, c.BackgroundFrom)
		}
	})

	t.Run("a boundary the queue would refuse is refused", func(t *testing.T) {
		jc := jobs.DefaultConfig()
		jc.BackgroundFrom = 4 // not above the raised interactive_up_to_class of 4
		upTo, from := resourcesClassBoundaries(jc)
		cfg := resources.DefaultConfig()
		cfg.InteractiveUpTo, cfg.BackgroundFrom = upTo, from
		if _, err := resources.New(resources.Options{Config: cfg}); err == nil {
			t.Error("the governor accepted a rule in which no class is background")
		}
	})
}

// ---- serve builds and runs the governor ------------------------------------------

// serve builds one governor and one disk guard in wireV1, gives them to the RAG stack (the guard
// is the pause's hook, the governor's policy is the queue's), and runs the loop for as long as
// it runs: the readings become known, so background work is no longer held from the start.
func TestServeBuildsAndRunsTheGovernor(t *testing.T) {
	oldHook := ragOnCompose
	stacks := make(chan *ragStack, 1)
	ragOnCompose = func(s *ragStack) { stacks <- s }
	t.Cleanup(func() { ragOnCompose = oldHook })

	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	// A pause floor no disk has: with the guard as the pause's hook ingestion is paused.
	cfg, _ := serveConfig(t, f, "resources:\n  disk_pause_free_bytes: 562949953421312\n  disk_resume_free_bytes: 562949953421313\n")
	srv := startServe(t, cfg)
	var st *ragStack
	select {
	case st = <-stacks:
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not build the RAG stack")
	}

	res := st.input.Resources
	if res == nil || res.Gov == nil || res.Disk == nil {
		t.Fatalf("the stack was composed without the governor and the disk guard: %+v", res)
	}
	if res.jobQueue() != st.queue {
		t.Error("the governor's wiring does not hold the queue the stack built")
	}
	if c := res.Gov.Config(); c.InteractiveUpTo != 4 || c.BackgroundFrom != jobs.DefaultBackgroundFrom {
		t.Errorf("class boundaries = %d, %d; want the queue's 4, %d", c.InteractiveUpTo, c.BackgroundFrom, jobs.DefaultBackgroundFrom)
	}

	// The loop is running: readings are known and nothing holds background work.
	pollUntil(t, 10*time.Second, "the governor's first readings", func() bool {
		s := res.Gov.Stats()
		return s.RSSBytes > 0 && !s.BackgroundHeld
	})
	if s := res.Gov.Stats(); s.BudgetBytes == 0 {
		t.Errorf("stats = %+v", s)
	}

	// The disk guard is the pause's hook: the floor above is what ingestion pauses on.
	if v := st.svc.Paused(context.Background()); !v.Paused || v.Reason != "disk_pressure" {
		t.Errorf("the pause is %+v, want paused on disk pressure from the disk guard", v)
	}
	if lvl := res.Disk.Level(); lvl == resources.DiskOK {
		t.Errorf("the disk guard reports %s", lvl)
	}

	if err := srv.stop(); err != nil {
		t.Fatalf("serve stopped with %v", err)
	}
}

// Without a loop the readings are unknown and background work holds: the governor is safe before
// it is run.
func TestGovernorHoldsBackgroundWorkUntilItIsRun(t *testing.T) {
	w := resourcesTestWiring(t, clock.Real{})
	if held, why := w.Gov.BackgroundHold(); !held || why != resources.ReasonMemoryUnknown {
		t.Fatalf("a governor that never ran holds = %v (%s), want memory_unknown", held, why)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stop := w.start(ctx)
	pollUntil(t, 5*time.Second, "the loop's first reading", func() bool { held, _ := w.Gov.BackgroundHold(); return !held })
	cancel()
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not stop with its context")
	}
}

// ---- live turns ----------------------------------------------------------------------

// A live turn is registered with the governor for as long as it lasts, and the queue's policy,
// the governor's around the pause's, leases no background job meanwhile. The same stack leases it
// once the turn ends, by Close, by the stream's last chunk, by an error or by its context.
func TestLiveTurnYieldsBackgroundWorkThroughTheGovernor(t *testing.T) {
	f := newRagFixture(t, func(o *ragFixtureOpts) { o.resources = true })
	gov := f.res.Gov
	ctx := context.Background()
	f.register()
	var jobID string
	if err := f.db.QueryRowContext(ctx, `SELECT id FROM jobs WHERE type = ?`, ingest.JobSync).Scan(&jobID); err != nil {
		t.Fatalf("registering the source queued no sync: %v", err)
	}
	leased := func() bool { return f.jobState(jobID).State != jobs.StatePending || f.jobState(jobID).Attempts != 0 }

	// Before the loop has read anything, memory is unknown: background work holds.
	if n := f.poll(); n != 0 || leased() {
		t.Fatalf("a governor that has read nothing leased %d jobs", n)
	}
	gov.Refresh(ctx)

	type opener func(ctx context.Context) (end func())
	openers := map[string]opener{
		"a stream closed by the caller": func(ctx context.Context) func() {
			s := &resourcesFakeStream{chunks: []providers.ChatChunk{{Delta: "a"}}}
			st, err := f.res.generator(&resourcesFakeGen{stream: s}, nil).ChatStream(ctx, contracts.Scope{}, providers.ChatRequest{})
			if err != nil {
				t.Fatal(err)
			}
			return func() { _ = st.Close() }
		},
		"a stream read to its last chunk": func(ctx context.Context) func() {
			s := &resourcesFakeStream{chunks: []providers.ChatChunk{{Delta: "a"}, {Done: true}}}
			st, err := f.res.generator(&resourcesFakeGen{stream: s}, nil).ChatStream(ctx, contracts.Scope{}, providers.ChatRequest{})
			if err != nil {
				t.Fatal(err)
			}
			return func() {
				for {
					if _, err := st.Recv(); err != nil {
						return
					}
				}
			}
		},
		"a stream that fails": func(ctx context.Context) func() {
			s := &resourcesFakeStream{}
			st, err := f.res.generator(&resourcesFakeGen{stream: s}, nil).ChatStream(ctx, contracts.Scope{}, providers.ChatRequest{})
			if err != nil {
				t.Fatal(err)
			}
			return func() { _, _ = st.Recv() } // io.EOF before a Done chunk: terminal all the same
		},
		"a stream nobody closes, whose request ends": func(ctx context.Context) func() {
			rctx, cancel := context.WithCancel(ctx)
			s := &resourcesFakeStream{chunks: []providers.ChatChunk{{Delta: "a"}}}
			if _, err := f.res.generator(&resourcesFakeGen{stream: s}, nil).ChatStream(rctx, contracts.Scope{}, providers.ChatRequest{}); err != nil {
				t.Fatal(err)
			}
			return cancel
		},
		"a stream from the selector serve gives the engine": func(ctx context.Context) func() {
			ctrl := fake.New()
			ctrl.On(fake.Any()).Reply("hello")
			reg := providers.NewRegistry(f.clk)
			if err := reg.Register(providers.RoleController, ctrl); err != nil {
				t.Fatal(err)
			}
			sel := localSelector{
				reg: reg, res: f.res,
				budget: providers.Budget{MaxPromptTokens: 2000, MaxOutputTokens: 256, MaxBatchItems: 8, Timeout: 10 * time.Second},
			}
			prov, err := sel.Select(ctx, f.scope, providers.RoleController)
			if err != nil {
				t.Fatal(err)
			}
			st, err := prov.Generator.ChatStream(ctx, f.scope, providers.ChatRequest{Messages: []providers.Message{{Role: providers.MsgUser, Content: "hi"}}})
			if err != nil {
				t.Fatal(err)
			}
			return func() { _ = st.Close() }
		},
		"a blocking call": func(ctx context.Context) func() {
			release, entered, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			gen := &resourcesFakeGen{chat: func(context.Context) error { close(entered); <-release; return nil }}
			go func() {
				defer close(done)
				_, _ = f.res.generator(gen, nil).Chat(ctx, contracts.Scope{}, providers.ChatRequest{})
			}()
			<-entered
			return func() { close(release); <-done }
		},
	}
	for name, open := range openers {
		func() {
			end := open(ctx)
			pollUntil(t, 5*time.Second, name+": the live turn to register", func() bool { return gov.InteractiveActive() == 1 })
			if held, why := gov.BackgroundHold(); !held || why != resources.ReasonInteractiveActive {
				t.Fatalf("%s: with a live turn the governor holds = %v (%s)", name, held, why)
			}
			if n := f.poll(); n != 0 || leased() {
				t.Fatalf("%s: a background job was leased (%d) while a live turn was active: %+v", name, n, f.jobState(jobID))
			}
			end()
			pollUntil(t, 5*time.Second, name+": the turn to end", func() bool { return gov.InteractiveActive() == 0 })
			if held, why := gov.BackgroundHold(); held {
				t.Fatalf("%s: after the turn the governor still holds (%s)", name, why)
			}
		}()
	}

	// The turn is over: the same queue now leases the job.
	f.drain()
	if got := f.jobState(jobID); got.State == jobs.StatePending {
		t.Errorf("the sync job is still pending after the turns ended")
	}
}

// A call that fails to start leaves nothing registered, and a nil wiring changes nothing.
func TestResourcesGeneratorFailuresLeaveNothingRegistered(t *testing.T) {
	w := resourcesTestWiring(t, clock.Real{})
	boom := errors.New("boom")
	gen := w.generator(&resourcesFakeGen{streamErr: boom, chat: func(context.Context) error { return boom }}, nil)
	if _, err := gen.ChatStream(context.Background(), contracts.Scope{}, providers.ChatRequest{}); !errors.Is(err, boom) {
		t.Fatalf("ChatStream error = %v", err)
	}
	if _, err := gen.Chat(context.Background(), contracts.Scope{}, providers.ChatRequest{}); !errors.Is(err, boom) {
		t.Fatalf("Chat error = %v", err)
	}
	if n := w.Gov.InteractiveActive(); n != 0 {
		t.Errorf("%d live turns are still registered after failed calls", n)
	}

	var nilW *resourcesWiring
	inner := &resourcesFakeGen{}
	if got := nilW.generator(inner, nil); got != providers.TextGenerator(inner) {
		t.Error("a nil wiring wrapped the generator")
	}
}

// ---- model loads -----------------------------------------------------------------------

// A call that may have to load the controller (the warm tracker says cold) is admitted against
// the memory budget with the registry's figure first, and is rejected, typed, when it does not
// fit: nothing is unloaded to make room. A warm call is not asked.
func TestResourcesGeneratorAdmitsAColdModelLoad(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	// 64 MiB is the smallest budget; the process already holds 100 MiB in the probe, and a model
	// of 512 MiB cannot fit in what is left whatever the process does.
	w := resourcesTestWiring(t, clk, func(in *resourcesInput) {
		in.Controller, in.ControllerBytes = "ctl-model", 512<<20
	})
	cfg := resources.DefaultConfig()
	cfg.MemoryBudgetBytes = 64 << 20
	small, err := resources.New(resources.Options{
		Config: cfg, Clock: clk, Memory: resources.MemoryProbeFunc(func() (uint64, error) { return 10 << 20, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	small.Refresh(context.Background())
	w.Gov = small

	warm := serveWarmNewTracker(clk, nil)
	gen := w.generator(&resourcesFakeGen{}, warm)
	_, err = gen.Chat(context.Background(), contracts.Scope{}, providers.ChatRequest{})
	if got := contracts.CodeOf(err); got != contracts.ErrResourceExhausted {
		t.Fatalf("a cold load that does not fit = %v (%v), want resource_exhausted", got, err)
	}
	if n := small.InteractiveActive(); n != 0 {
		t.Errorf("a rejected load left %d turns registered", n)
	}
	if st := small.Stats(); st.Rejected != 1 || st.ReservedBytes != 0 {
		t.Errorf("stats after the rejection = %+v", st)
	}

	// Once the controller has answered it is warm: no admission, and the call goes through.
	warm.touch()
	if _, err := gen.Chat(context.Background(), contracts.Scope{}, providers.ChatRequest{}); err != nil {
		t.Fatalf("a warm call = %v", err)
	}
	if st := small.Stats(); st.Rejected != 1 || st.Admitted != 0 {
		t.Errorf("a warm call was judged: %+v", st)
	}
}

// The estimate is the registry's measured peak, then its steady figure, then the artifact's size.
func TestResourcesModelBytes(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	size := func(v int64) *int64 { return &v }
	cases := []struct {
		name string
		e    models.Entry
		want uint64
	}{
		{"peak first", models.Entry{Memory: models.Memory{PeakRSSMB: f(853), SteadyRSSMB: f(836)}, Artifact: models.Artifact{SizeBytes: size(300 << 20)}}, 853 << 20},
		{"steady when there is no peak", models.Entry{Memory: models.Memory{SteadyRSSMB: f(836)}}, 836 << 20},
		{"the artifact's size when nothing was measured", models.Entry{Artifact: models.Artifact{SizeBytes: size(300 << 20)}}, 300 << 20},
		{"nothing known", models.Entry{}, 0},
	}
	for _, tc := range cases {
		if got := resourcesModelBytes(tc.e); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
	// The shipped registry gives the controller an estimate: the fixtures' model is measured.
	mr, err := loadModelRegistry()
	if err != nil {
		t.Fatal(err)
	}
	e, ok := mr.Get(serveModelID)
	if !ok || resourcesModelBytes(e) == 0 {
		t.Errorf("the shipped %s entry gives no memory estimate", serveModelID)
	}
}
