package resources

import (
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/jobs"
)

func TestConfigDefaultsValidate(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("defaults do not validate: %v", err)
	}
	if err := (Config{}).Validate(); err != nil {
		t.Fatalf("the zero Config (all defaults) does not validate: %v", err)
	}
	if got := FromConfig(); got.MemoryBudgetBytes != DefaultMemoryBudget || got.InteractiveUpTo != jobs.DefaultInteractiveUpTo {
		t.Fatalf("registered section = %+v", got)
	}
}

func TestConfigRelations(t *testing.T) {
	for name, c := range map[string]Config{
		"idle shorter than warm":     {IdleUnloadAfter: time.Minute, MinWarm: 5 * time.Minute},
		"stale shorter than probe":   {ProbeInterval: 10 * time.Second, StaleAfter: 5 * time.Second},
		"interactive above backgr.":  {InteractiveUpTo: 4, BackgroundFrom: 3},
		"poll longer than the wait":  {YieldPoll: 10 * time.Second, YieldMaxWait: time.Second},
		"reserve above the pause":    {DiskReserveBytes: 3 * gib},
		"pause above the resume":     {DiskPauseFreeBytes: 4 * gib},
		"percent out of range":       {BackgroundPercent: 99},
		"too many pinned models":     {PinnedModels: make([]string, 65)},
		"budget below the minimum":   {MemoryBudgetBytes: 1 << 20},
		"interactive class too high": {InteractiveUpTo: 6},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, c)
		}
	}
	if _, err := New(Options{Config: Config{BackgroundPercent: 99}}); err == nil {
		t.Error("New accepted an invalid Config")
	}
}

func TestOSProbes(t *testing.T) {
	rss, err := OSMemoryProbe{}.ProcessRSS()
	if err != nil || rss < 1<<20 {
		t.Fatalf("ProcessRSS = %d, %v; a Go test process holds more than 1 MiB", rss, err)
	}
	free, err := OSDiskProbe{}.FreeBytes(t.TempDir())
	if err != nil || free == 0 {
		t.Fatalf("FreeBytes = %d, %v", free, err)
	}
	if _, err := (OSDiskProbe{}).FreeBytes(t.TempDir() + "/does/not/exist"); err == nil {
		t.Fatal("a missing path must be an error (unknown), not zero free bytes")
	}
}
