package resources

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/rag/ingest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// diskFake is a settable DiskProbe: free bytes per path, plus a failure.
type diskFake struct {
	mu   sync.Mutex
	free map[string]uint64
	err  error
}

func (d *diskFake) FreeBytes(path string) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return 0, d.err
	}
	return d.free[path], nil
}

func (d *diskFake) set(path string, free uint64, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.free == nil {
		d.free = map[string]uint64{}
	}
	d.free[path], d.err = free, err
}

// SEC-079
// ingestion pauses under disk pressure before interactive or
// transactional storage is endangered.
func TestDiskPressurePausesIngestion(t *testing.T) {
	clk := clock.NewFake(epoch)
	disk := &diskFake{}
	disk.set("/data", 10*gib, nil)
	guard, err := NewDiskGuard(DiskOptions{Paths: []string{"/data"}, Probe: disk, Clock: clk})
	if err != nil {
		t.Fatalf("NewDiskGuard: %v", err)
	}
	// The real consumer: ingestion's pause over our guard, in front of the queue.
	pause := ingest.NewPause(guard, clk, time.Second, nil)
	pol := pause.Policy(nil)
	ingestJob := jobs.Candidate{ID: "j1", Class: jobs.ClassIngest, Type: ingest.PausedJobTypes()[0]}
	readJob := jobs.Candidate{ID: "j2", Class: jobs.ClassRead, Type: "app.read"}
	step := func(free uint64) {
		t.Helper()
		disk.set("/data", free, nil)
		clk.Advance(3 * time.Second) // past the guard's and the pause's cache
	}

	if got := pol.Pick([]jobs.Candidate{ingestJob}); got != 0 {
		t.Fatalf("plenty free: Pick = %d, want 0", got)
	}
	if guard.Level() != DiskOK {
		t.Fatalf("Level = %s, want ok", guard.Level())
	}

	step(1536 * mib) // below the 2 GiB pause floor
	if got := pol.Pick([]jobs.Candidate{ingestJob}); got != -1 {
		t.Fatalf("below the pause floor: Pick = %d, want -1 (ingestion paused)", got)
	}
	if got := pol.Pick([]jobs.Candidate{ingestJob, readJob}); got != 1 {
		t.Fatalf("interactive work must still be leased: Pick = %d, want 1", got)
	}
	if under, err := guard.UnderPressure(bg); !under || err != nil || guard.Level() != DiskPressure {
		t.Fatalf("UnderPressure = %v, %v, level %s", under, err, guard.Level())
	}

	step(2560 * mib) // above the pause floor, below the 3 GiB resume floor
	if got := pol.Pick([]jobs.Candidate{ingestJob}); got != -1 {
		t.Fatalf("between pause and resume floors: Pick = %d, want -1 (hysteresis)", got)
	}

	step(3584 * mib) // above the resume floor
	if got := pol.Pick([]jobs.Candidate{ingestJob}); got != 0 {
		t.Fatalf("above the resume floor: Pick = %d, want 0", got)
	}

	step(400 * mib) // below the 512 MiB transactional reserve
	if guard.Level() != DiskCritical {
		t.Fatalf("Level = %s, want critical", guard.Level())
	}
	if got := pol.Pick([]jobs.Candidate{ingestJob}); got != -1 {
		t.Fatalf("critical: Pick = %d, want -1", got)
	}

	t.Run("an unmeasurable volume pauses ingestion", func(t *testing.T) {
		disk.set("/data", 10*gib, errors.New("statfs failed"))
		clk.Advance(3 * time.Second)
		under, err := guard.UnderPressure(bg)
		if !under || err == nil || contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Fatalf("UnderPressure = %v, %v; want true and an unavailable error", under, err)
		}
		if v := pause.Verdict(bg); !v.Paused || v.Reason != "disk_pressure_unknown" {
			t.Fatalf("pause verdict = %+v", v)
		}
		if guard.Level() != DiskCritical {
			t.Fatalf("Level on an unreadable volume = %s", guard.Level())
		}
		disk.set("/data", 10*gib, nil)
		clk.Advance(3 * time.Second)
		if under, err := guard.UnderPressure(bg); under || err != nil {
			t.Fatalf("recovered: UnderPressure = %v, %v", under, err)
		}
	})

	t.Run("pressure on any watched volume pauses", func(t *testing.T) {
		g, err := NewDiskGuard(DiskOptions{Paths: []string{"/data", "/index"}, Probe: disk, Clock: clk})
		if err != nil {
			t.Fatal(err)
		}
		disk.set("/data", 10*gib, nil)
		disk.set("/index", 1*gib, nil)
		if under, err := g.UnderPressure(bg); !under || err != nil {
			t.Fatalf("UnderPressure = %v, %v; want true (the index volume is low)", under, err)
		}
	})

	t.Run("the guard reuses a reading for the probe interval", func(t *testing.T) {
		calls := 0
		counting := DiskProbeFunc(func(string) (uint64, error) { calls++; return 10 * gib, nil })
		g, _ := NewDiskGuard(DiskOptions{Paths: []string{"/data"}, Probe: counting, Clock: clk})
		for range 5 {
			_, _ = g.UnderPressure(bg)
		}
		if calls != 1 {
			t.Fatalf("probe called %d times within one interval, want 1", calls)
		}
		clk.Advance(3 * time.Second)
		_, _ = g.UnderPressure(bg)
		if calls != 2 {
			t.Fatalf("probe called %d times after the interval, want 2", calls)
		}
	})

	t.Run("construction is validated", func(t *testing.T) {
		if _, err := NewDiskGuard(DiskOptions{Probe: disk}); err == nil {
			t.Fatal("a guard with nothing to watch must be refused")
		}
		bad := Config{DiskPauseFreeBytes: 100 * mib, DiskReserveBytes: 200 * mib, DiskResumeFreeBytes: 300 * mib}
		if _, err := NewDiskGuard(DiskOptions{Config: bad, Paths: []string{"/data"}, Probe: disk}); err == nil {
			t.Fatal("a reserve above the pause floor must be refused")
		}
	})
}
