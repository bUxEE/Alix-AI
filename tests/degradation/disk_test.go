package degradation

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/rag/ingest"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/resources"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// freeSpace is the disk as the guard measures it, set by the test.
type freeSpace struct {
	mu   sync.Mutex
	free uint64
	err  error
}

func (f *freeSpace) set(free uint64, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.free, f.err = free, err
}

func (f *freeSpace) FreeBytes(string) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.free, f.err
}

// Section 18, "Disk pressure": pause ingestion before interactive or transactional storage is endangered.
func TestDegradeDiskPressure(t *testing.T) {
	const (
		mib = uint64(1) << 20
		gib = uint64(1) << 30
	)
	cfg := resources.DefaultConfig() // reserve 512 MiB < pause 2 GiB < resume 3 GiB
	clk := clock.NewFake(t0)
	disk := &freeSpace{free: 10 * gib}
	guard, err := resources.NewDiskGuard(resources.DiskOptions{Config: cfg, Paths: []string{t.TempDir()}, Probe: disk, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	pause := ingest.NewPause(guard, clk, 0, nil)

	db := storetest.Open(t, nil)
	q, err := jobs.NewQueue(db, jobs.Options{Clock: clk, Policy: pause.Policy(nil)})
	if err != nil {
		t.Fatal(err)
	}
	specs := append(ingest.JobSpecs(), vector.EmbedJobSpec(), jobs.TypeSpec{Name: "action.commit", Class: jobs.ClassAuthCommit})
	for _, sp := range specs {
		if err := q.Register(sp); err != nil {
			t.Fatalf("Register %s: %v", sp.Name, err)
		}
	}
	scope := storetest.Scope("app", "tenant-a", "w1")
	alog, err := audit.New(db, audit.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}

	// settle moves the clock past both caches, so the next question reaches the disk.
	settle := func(free uint64, err error) {
		t.Helper()
		disk.set(free, err)
		clk.Advance(time.Minute)
	}
	enqueue := func(typ string) jobs.Job {
		t.Helper()
		res, err := q.Enqueue(ctxb(), scope, jobs.EnqueueRequest{Type: typ, Payload: []byte(`{"n":1}`)})
		if err != nil {
			t.Fatalf("Enqueue(%s): %v", typ, err)
		}
		return res.Job
	}
	leaseAll := func() []jobs.Leased {
		t.Helper()
		got, err := q.Lease(ctxb(), jobs.LeaseRequest{Classes: jobs.Classes(), Max: 10})
		if err != nil {
			t.Fatalf("Lease: %v", err)
		}
		return got
	}
	// interactiveStaysUp proves what the pause must protect: a class-1 job (a commit) is leased and completed, and the
	// store takes writes, whatever the disk says.
	interactiveStaysUp := func(label string) {
		t.Helper()
		commit := enqueue("action.commit")
		got := leaseAll()
		if len(got) != 1 || got[0].ID != commit.ID {
			t.Fatalf("%s: leased %d jobs, want only the class-1 commit", label, len(got))
		}
		if err := q.Complete(ctxb(), got[0]); err != nil {
			t.Fatalf("%s: completing the commit: %v", label, err)
		}
		if _, err := alog.Append(ctxb(), scope, audit.NewEvent{Type: audit.EventActionPrepared, Data: map[string]any{"preparation_id": "prep_" + label}}); err != nil {
			t.Fatalf("%s: an audit write failed under disk pressure: %v", label, err)
		}
	}

	// Control: with room to spare, ingestion is leased at once. No class 1 or 2 job is waiting, so this is the pause
	// being off, not the yield rule.
	ingestJob := enqueue(ingest.JobSync)
	if guard.Level() != resources.DiskOK {
		t.Fatalf("control: level %s", guard.Level())
	}
	got := leaseAll()
	if len(got) != 1 || got[0].ID != ingestJob.ID {
		t.Fatalf("control: leased %d jobs with a healthy disk, want the ingestion job", len(got))
	}
	if err := q.Release(ctxb(), got[0]); err != nil { // back to pending for the phases below
		t.Fatal(err)
	}
	if err := q.Complete(ctxb(), mustReLease(t, q, ingestJob.ID)); err != nil {
		t.Fatal(err)
	}

	t.Run("below the pause floor ingestion is not leased, interactive and transactional work is", func(t *testing.T) {
		job := enqueue(ingest.JobSync)
		embed := enqueue(vector.EmbedJobType)
		reindex := enqueue(ingest.JobReindexParse)
		settle(gib+gib/2, nil) // 1.5 GiB: under the 2 GiB floor, over the 512 MiB reserve
		if lvl := guard.Level(); lvl != resources.DiskPressure {
			t.Fatalf("level %s, want pressure (not critical: the store still has room)", lvl)
		}
		if v := pause.Verdict(ctxb()); !v.Paused || v.Reason != "disk_pressure" {
			t.Fatalf("verdict %+v", v)
		}
		if got := leaseAll(); len(got) != 0 {
			t.Fatalf("leased %d ingestion jobs (%s) under disk pressure", len(got), got[0].Type)
		}
		interactiveStaysUp("pressure")
		// Nothing was spent: the jobs wait, with no attempt used and no state change.
		for _, j := range []jobs.Job{job, embed, reindex} {
			cur, err := q.Get(ctxb(), scope, j.ID)
			if err != nil || cur.State != jobs.StatePending || cur.Attempts != 0 {
				t.Fatalf("job %s: %+v err %v, want pending with no attempt used", j.Type, cur, err)
			}
		}

		// Hysteresis: above the pause floor but below the resume floor is still pressure.
		settle(2*gib+gib/2, nil)
		if got := leaseAll(); len(got) != 0 {
			t.Fatalf("ingestion resumed at 2.5 GiB, below the 3 GiB resume floor")
		}
		// Critical: below the reserve everything that is left belongs to the store.
		settle(100*mib, nil)
		if lvl := guard.Level(); lvl != resources.DiskCritical {
			t.Fatalf("level %s, want critical", lvl)
		}
		if got := leaseAll(); len(got) != 0 {
			t.Fatal("ingestion leased on a critical disk")
		}
		interactiveStaysUp("critical")

		// Resume at the resume floor: every held job is leased, the first time.
		settle(3*gib, nil)
		if lvl := guard.Level(); lvl != resources.DiskOK {
			t.Fatalf("level %s at the resume floor, want ok", lvl)
		}
		resumed := map[string]bool{}
		for _, l := range leaseAll() {
			resumed[l.ID] = true
			if l.Attempts != 1 {
				t.Errorf("job %s resumed with %d attempts, want 1: the pause must not burn attempts", l.Type, l.Attempts)
			}
		}
		// A class 5 job and the embedding job run; the class 6 reindex runs too, once nothing interactive waits.
		if !resumed[job.ID] || !resumed[embed.ID] || !resumed[reindex.ID] {
			t.Fatalf("resumed %v, want all three held jobs", resumed)
		}
	})

	t.Run("a disk that cannot be measured counts as pressure", func(t *testing.T) {
		enqueue(ingest.JobSync)
		settle(0, errors.New("statfs: device not ready"))
		if lvl := guard.Level(); lvl != resources.DiskCritical {
			t.Fatalf("level %s, want critical for an unreadable volume", lvl)
		}
		under, err := guard.UnderPressure(ctxb())
		if !under || contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Fatalf("UnderPressure = %v, %v: an unknown disk must pause ingestion and say why", under, err)
		}
		if v := pause.Verdict(ctxb()); !v.Paused || v.Reason != "disk_pressure_unknown" {
			t.Fatalf("verdict %+v, want paused: disk_pressure_unknown", v)
		}
		if got := leaseAll(); len(got) != 0 {
			t.Fatalf("leased %d ingestion jobs on a disk nobody could measure", len(got))
		}
		interactiveStaysUp("unknown")
		// And it recovers when the disk can be read again.
		settle(10*gib, nil)
		if got := leaseAll(); len(got) != 1 {
			t.Fatalf("leased %d jobs after the disk came back, want the held ingestion job", len(got))
		}
	})
}

// mustReLease leases the one pending ingestion job with the given id.
func mustReLease(t testing.TB, q *jobs.Queue, id string) jobs.Leased {
	t.Helper()
	got, err := q.Lease(ctxb(), jobs.LeaseRequest{Classes: jobs.Classes(), Max: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range got {
		if l.ID == id {
			return l
		}
	}
	t.Fatalf("job %s was not leasable", id)
	return jobs.Leased{}
}
