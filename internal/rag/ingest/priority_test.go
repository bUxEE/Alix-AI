package ingest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// eventLog records the order in which things happened across goroutines.
type eventLog struct {
	mu sync.Mutex
	ev []string
}

func (l *eventLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ev = append(l.ev, s)
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.ev...)
}

func (f *fixture) enqueueTest(typ string) jobs.Job {
	f.t.Helper()
	res, err := f.q.Enqueue(bg, f.scope, jobs.EnqueueRequest{Type: typ, Payload: []byte(`{}`)})
	if err != nil {
		f.t.Fatalf("Enqueue(%s): %v", typ, err)
	}
	return res.Job
}

func (f *fixture) job(id string) jobs.Job {
	f.t.Helper()
	j, err := f.q.Get(bg, f.scope, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return j
}

func (f *fixture) waitState(id string, want jobs.State) jobs.Job {
	f.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if j := f.job(id); j.State == want {
			return j
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("job %s did not reach %s", id, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// SEC-079: ingestion yields to interactive work and cannot starve it.
func TestIngestYieldsToInteractivePriority(t *testing.T) {
	// The ingestion job types are classes 5 and 6, and so is the embedding job:
	// the class belongs to the type, so ingestion cannot promote itself.
	for _, sp := range JobSpecs() {
		if sp.Class != jobs.ClassIngest && sp.Class != jobs.ClassMaintenance {
			t.Errorf("%s is class %d", sp.Name, sp.Class)
		}
	}
	mustEqual(t, "embedding job class", vector.EmbedJobSpec().Class, jobs.ClassIngest)

	for _, class := range []jobs.Class{jobs.ClassAuthCommit, jobs.ClassTurn, jobs.ClassRAGQuery, jobs.ClassRead} {
		t.Run(class.String(), func(t *testing.T) {
			f := newFixture(t, func(o *fixtureOpts) {
				o.jobs = func(c *jobs.Config) {
					c.Class1.Workers, c.Class2.Workers, c.Class3.Workers, c.Class4.Workers = 1, 1, 1, 1
					c.InteractiveUpTo = int(jobs.ClassRead) // ingestion yields to classes 1 to 4
				}
			})
			corpus(f, 2)
			log := &eventLog{}
			f.parser.fail = func([]byte) error { log.add("ingest:parse"); return nil }

			if err := f.q.Register(jobs.TypeSpec{Name: "test.interactive", Class: class}); err != nil {
				t.Fatal(err)
			}
			release := make(chan struct{})
			started := make(chan string, 4)
			if err := f.pool.Handle("test.interactive", func(ctx context.Context, job jobs.Job) error {
				started <- job.ID
				select {
				case <-release:
				case <-ctx.Done():
				}
				log.add("interactive:done")
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			// Ingestion is queued first. Two interactive jobs follow: the first
			// takes the class's only worker, the second has to wait.
			sync := f.requestSync()
			first, second := f.enqueueTest("test.interactive"), f.enqueueTest("test.interactive")
			if _, err := f.pool.Poll(bg); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(10 * time.Second):
				t.Fatal("the interactive job did not start")
			}
			// Interactive work is waiting: ingestion leases nothing, however many
			// times the pool looks.
			for i := 0; i < 3; i++ {
				if n, err := f.pool.Poll(bg); err != nil || n != 0 {
					t.Fatalf("poll %d started %d jobs while interactive work waited (%v)", i, n, err)
				}
			}
			if j := f.job(sync.ID); j.State != jobs.StatePending || j.Attempts != 0 {
				t.Fatalf("ingestion ran while interactive work was waiting: %s after %d attempts", j.State, j.Attempts)
			}
			mustEqual(t, "ingestion workers busy", f.pool.Running(jobs.ClassIngest), 0)
			mustEqual(t, "documents parsed", f.parser.calls.Load(), 0)

			// The interactive work finishes; ingestion follows.
			close(release)
			f.waitState(first.ID, jobs.StateSucceeded)
			f.drain()
			f.waitState(second.ID, jobs.StateSucceeded)
			mustEqual(t, "sync", f.job(sync.ID).State, jobs.StateSucceeded)
			mustEqual(t, "documents searchable", len(docHits(f.search("alpha"))), 2)
			ev := log.snapshot()
			firstParse := -1
			lastInteractive := -1
			for i, e := range ev {
				if e == "ingest:parse" && firstParse < 0 {
					firstParse = i
				}
				if e == "interactive:done" {
					lastInteractive = i
				}
			}
			if firstParse < 0 || lastInteractive < 0 || firstParse < lastInteractive-1 {
				t.Fatalf("ingestion started before the interactive work was through: %v", ev)
			}
			// Everything ingestion queued ran as class 5 (the reindex lane is 6).
			rows, err := f.db.QueryContext(bg, `SELECT DISTINCT class FROM jobs WHERE type LIKE 'rag.%'`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for rows.Next() {
				var c int
				if err := rows.Scan(&c); err != nil {
					t.Fatal(err)
				}
				if c != int(jobs.ClassIngest) && c != int(jobs.ClassMaintenance) {
					t.Errorf("an ingestion job ran as class %d", c)
				}
			}
		})
	}
}

// SEC-027: a background job whose initiating scope was revoked fails with
// permission_denied before it does any work.
func TestRevokedScopeJobFails(t *testing.T) {
	f := newFixture(t)
	corpus(f, 2)
	f.sync()
	base := f.counters()
	versions := f.countRows(`SELECT count(*) FROM ragingest_version`)
	last := f.status().LastSuccess

	// The principal that initiated the sync is revoked before the job runs: the
	// pool refuses to run it, so nothing is scanned, parsed or indexed.
	f.writeAt("doc0.md", doc("Title 0", para("p0a"), para("brandnewtext")), oldTime.Add(time.Hour))
	f.gate.set(contracts.New(contracts.ErrPermissionDenied, "principal revoked"))
	j := f.sync()
	mustEqual(t, "sync state", j.State, jobs.StateFailed)
	mustEqual(t, "sync code", j.ErrorCode, contracts.ErrPermissionDenied)
	mustEqual(t, "sync attempts", j.Attempts, 1)
	if f.counters() != base {
		t.Fatalf("a revoked scope cost work: %+v -> %+v", base, f.counters())
	}
	mustEqual(t, "version rows", f.countRows(`SELECT count(*) FROM ragingest_version`), versions)
	mustEqual(t, "hits for the new text", len(f.search("brandnewtext")), 0)
	if !f.status().LastSuccess.Equal(last) {
		t.Fatal("a job that never ran advanced the source's last success")
	}

	// Revoked in the middle of the pipeline: the scan ran, the parse stage's job
	// is refused, the version stays waiting and nothing is indexed.
	f.gate.set(nil)
	f.requestSync()
	f.poll() // the sync itself
	mustEqual(t, "versions waiting", f.versionStates()[vPending], 1)
	f.gate.set(contracts.New(contracts.ErrPermissionDenied, "session expired"))
	f.drain()
	parse := f.jobsOf(JobParse)
	last2 := parse[len(parse)-1]
	mustEqual(t, "parse state", last2.State, jobs.StateFailed)
	mustEqual(t, "parse code", last2.ErrorCode, contracts.ErrPermissionDenied)
	mustEqual(t, "documents parsed", f.parser.calls.Load(), base.parses)
	mustEqual(t, "hits for the new text", len(f.search("brandnewtext")), 0)
	mustEqual(t, "version state", f.versionStates()[vPending], 1)

	// Access restored: the next sync queues the stage again and the pipeline
	// finishes. A job failed by a revoked scope is a failed job, not a lost one.
	f.gate.set(nil)
	f.sync()
	mustEqual(t, "hits for the new text after recovery", len(f.search("brandnewtext")), 1)
	mustEqual(t, "documents parsed after recovery", f.parser.calls.Load(), base.parses+1)
}

// SEC-079: ingestion pauses under disk pressure, before interactive storage is
// endangered.
func TestDiskPressurePausesIngestion(t *testing.T) {
	f := newFixture(t)
	corpus(f, 2)
	turns := 0
	if err := f.q.Register(jobs.TypeSpec{Name: "test.turn", Class: jobs.ClassTurn}); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.Handle("test.turn", func(context.Context, jobs.Job) error { turns++; return nil }); err != nil {
		t.Fatal(err)
	}

	f.press.set(true, nil)
	sync := f.requestSync()
	turn := f.enqueueTest("test.turn")
	f.poll()
	// Interactive work is not held back, ingestion is.
	mustEqual(t, "interactive job", f.job(turn.ID).State, jobs.StateSucceeded)
	mustEqual(t, "interactive jobs run", turns, 1)
	if j := f.job(sync.ID); j.State != jobs.StatePending || j.Attempts != 0 {
		t.Fatalf("the sync ran under disk pressure: %s after %d attempts", j.State, j.Attempts)
	}
	if v := f.svc.Paused(bg); !v.Paused || v.Reason != "disk_pressure" {
		t.Fatalf("verdict = %+v", v)
	}
	for i := 0; i < 3; i++ {
		f.clk.Advance(time.Minute)
		if n := f.poll(); n != 0 {
			t.Fatalf("poll %d started %d jobs under pressure", i, n)
		}
	}
	mustEqual(t, "documents parsed", f.parser.calls.Load(), 0)
	mustEqual(t, "attempts spent on the pause", f.job(sync.ID).Attempts, 0)

	// An answer that cannot be had is pressure: ingestion fails closed.
	f.press.set(false, errors.New("statfs: the volume is not available"))
	f.clk.Advance(time.Minute)
	if n := f.poll(); n != 0 {
		t.Fatalf("started %d jobs while the pressure hook failed", n)
	}
	if v := f.svc.Paused(bg); !v.Paused || v.Reason != "disk_pressure_unknown" {
		t.Fatalf("verdict = %+v", v)
	}

	// A stage that was leased just before the pressure came checks again when it
	// starts, writes nothing and asks to be retried.
	err := f.svc.syncHandler(bg, f.job(sync.ID))
	codeOf(t, err, contracts.ErrDegraded)
	mustEqual(t, "snapshot rows", f.countRows(`SELECT count(*) FROM ragingest_item`), 0)

	// The pressure goes away and the job, which never lost an attempt, runs.
	f.press.set(false, nil)
	f.clk.Advance(time.Minute)
	f.drain()
	j := f.job(sync.ID)
	mustEqual(t, "sync state", j.State, jobs.StateSucceeded)
	mustEqual(t, "sync attempts", j.Attempts, 1)
	mustEqual(t, "documents searchable", len(docHits(f.search("alpha"))), 2)

	// Embeddings write to disk too: pressure holds the embedding job back and
	// full-text search keeps working.
	f.write("doc9.md", doc("Title 9", para("p9a")))
	f.requestSync()
	for i := 0; i < 20 && len(pendingOf(f, vector.EmbedJobType)) == 0; i++ {
		f.poll()
	}
	if len(pendingOf(f, vector.EmbedJobType)) == 0 {
		t.Fatal("no embedding job was queued")
	}
	f.press.set(true, nil)
	f.clk.Advance(time.Minute)
	for i := 0; i < 3; i++ {
		if n := f.poll(); n != 0 {
			t.Fatalf("poll %d started %d jobs under pressure", i, n)
		}
	}
	for _, e := range pendingOf(f, vector.EmbedJobType) {
		mustEqual(t, "embedding attempts", e.Attempts, 0)
	}
	mustEqual(t, "new document searchable under pressure", len(f.search("p9a")) > 0, true)
	mustEqual(t, "state", f.activeGen().State, rag.IndexGenerationStateActive)
	f.press.set(false, nil)
	f.clk.Advance(time.Minute)
	f.drain()
	g := f.activeGen()
	mustEqual(t, "embedded chunks after the pressure", g.Counts.EmbeddedChunks, g.Counts.Chunks)
}

func pendingOf(f *fixture, typ string) []jobs.Job {
	var out []jobs.Job
	for _, j := range f.jobsOf(typ) {
		if j.State == jobs.StatePending {
			out = append(out, j)
		}
	}
	return out
}
