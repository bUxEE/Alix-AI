package conformancepkg

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"testing"

	"github.com/buxee/alix-ai/internal/store"
)

// ADR 0016, owed check 1: the resident memory one more registered application costs a process,
// idle and under load, against the cost of a second deployment.
//
// What a registered application is here: its signed package loaded and compiled (capabilities,
// schemas, policy, conversation contracts, field and ambiguity rules), its entry in the
// capability registry, and its adapter behind the scope-checking wrapper. Its host application
// runs elsewhere (here an in-process stand-in whose own data is built before the baseline is
// taken and is not charged to the platform).
//
// Under load, the same traffic is served in the first application, then in each of the others:
// two turns per round (a read and a booking that stops at its confirmation), every round in a
// session of its own. The marginal cost under load is what serving that traffic in the other
// applications adds, per application, once the shared warm-up (the store's page cache, the
// engine's buffers) has been paid by the first one.
//
// The numbers are measured by this process on this host: the resident set (the working set on
// Windows, VmRSS on Linux) after a full collection and a return of freed memory to the system,
// and the Go heap in use. They are dev-host figures, never certification.
const (
	// memoryApplications is how many applications the process ends with.
	memoryApplications = 16
	// memoryRounds is how many rounds of traffic each application is served.
	memoryRounds = 25
	// secondDeploymentFloor is the smallest thing a second deployment adds: the weights of the
	// smallest Lite model candidate (Q4_K_M, 237 MB, ADR 0010), resident in a LocalAI process of
	// their own, not counting alixai, LocalAI, the runtime's buffers or the KV cache.
	secondDeploymentFloor = 237_000_000
	// memoryMaxFraction is the decision rule of ADR 0016: the marginal cost of an application
	// must be a small fraction of a second deployment's. Five per cent.
	memoryMaxFraction = 0.05
)

type memSample struct {
	label string
	rss   uint64
	heap  uint64
}

// settle returns what can be returned and reads the process.
func settle(t *testing.T, label string) memSample {
	t.Helper()
	runtime.GC()
	debug.FreeOSMemory()
	runtime.GC()
	rss, _, _, ok := processMemory()
	if !ok {
		t.Fatal("the resident set of the process cannot be read")
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return memSample{label: label, rss: rss, heap: ms.HeapInuse}
}

func mib(b float64) float64 { return b / (1 << 20) }

func delta(from, to memSample, n int) (rssPer, heapPer float64) {
	return (float64(to.rss) - float64(from.rss)) / float64(n), (float64(to.heap) - float64(from.heap)) / float64(n)
}

func TestMarginalMemoryPerApplication(t *testing.T) {
	if _, _, _, ok := processMemory(); !ok {
		t.Skipf("the resident set cannot be read on %s: this measurement runs on Windows and Linux", runtime.GOOS)
	}
	root := t.TempDir()
	dirs := make([]string, memoryApplications)
	for i := range dirs {
		dirs[i] = signedCopy(t, root, fmt.Sprintf("clinic-%02d", i+1))
	}
	r := newRig(t)

	checkpoints := map[int]bool{1: true, 2: true, 4: true, 8: true, 16: true}
	base := settle(t, "no application")
	samples := []memSample{base}
	var apps []*tenantApp
	for i, dir := range dirs {
		apps = append(apps, r.register(dir))
		if checkpoints[i+1] {
			samples = append(samples, settle(t, fmt.Sprintf("%d idle", i+1)))
		}
	}
	idle1, idleN := samples[1], samples[len(samples)-1]

	serve := func(app *tenantApp) {
		for i := 0; i < memoryRounds; i++ {
			session := fmt.Sprintf("ses-load-%s-%d", app.id, i)
			r.propose("availability.list", map[string]any{"date": "2026-10-14"})
			if res := r.say(app.scope("receptionist", session), "en", sayFree); string(res.Directive.Act) != "present_result" {
				t.Fatalf("%s: a read ended in %s", app.id, res.Directive.Act)
			}
			r.propose("appointments.book", bookLuca)
			if res := r.say(app.scope("receptionist", session), "en", sayBook); string(res.Directive.Act) != "request_confirmation" {
				t.Fatalf("%s: a booking ended in %s", app.id, res.Directive.Act)
			}
		}
	}
	serve(apps[0])
	load1 := settle(t, "1 loaded")
	for _, app := range apps[1:] {
		serve(app)
	}
	loadN := settle(t, fmt.Sprintf("%d loaded", memoryApplications))

	// ---- the report ----
	t.Logf("host: %s/%s, %s, GOMAXPROCS %d", runtime.GOOS, runtime.GOARCH, runtime.Version(), runtime.GOMAXPROCS(0))
	t.Logf("%-18s %12s %12s", "after", "RSS MiB", "heap MiB")
	all := append(append([]memSample{}, samples...), load1, loadN)
	for _, s := range all {
		t.Logf("%-18s %12.1f %12.1f", s.label, mib(float64(s.rss)), mib(float64(s.heap)))
	}
	idleRSS, idleHeap := delta(idle1, idleN, memoryApplications-1)
	loadRSS, loadHeap := delta(load1, loadN, memoryApplications-1)
	firstRSS, firstHeap := delta(base, idle1, 1)
	t.Logf("first application, idle:                 %+.2f MiB RSS, %+.2f MiB heap (includes the one-time cost of the first package load)", mib(firstRSS), mib(firstHeap))
	t.Logf("each further application, idle:          %+.2f MiB RSS, %+.2f MiB heap (from 1 to %d)", mib(idleRSS), mib(idleHeap), memoryApplications)
	t.Logf("each further application, under load:    %+.2f MiB RSS, %+.2f MiB heap (%d rounds of 2 turns each)", mib(loadRSS), mib(loadHeap), memoryRounds)
	// option D of ADR 0016 (a database file per application) would add this much page cache per
	// application, at most, and only as its pages are touched
	t.Logf("one more SQLite database (option D) is budgeted %d KiB of page cache by default (writer cache plus one reader cache per pooled reader)", store.DefaultConfig().PageCacheBudgetKiB())
	limit := memoryMaxFraction * secondDeploymentFloor
	t.Logf("a second deployment adds at least %.0f MiB (model weights alone, ADR 0010); the rule allows %.1f MiB per application (%.0f%%)",
		mib(secondDeploymentFloor), mib(limit), memoryMaxFraction*100)

	// The decision rule. A negative figure is noise from the allocator returning pages, and passes.
	if raceDetector {
		// the race detector's shadow memory scales with the heap and multiplies every figure,
		// so under it the numbers are logged and the rule is not applied
		t.Logf("built with -race: the figures include the race detector's shadow memory, so the decision rule is not asserted")
		return
	}
	for _, c := range []struct {
		what string
		got  float64
	}{
		{"idle RSS", idleRSS}, {"idle heap", idleHeap}, {"loaded RSS", loadRSS}, {"loaded heap", loadHeap},
	} {
		if c.got > limit {
			t.Errorf("each further application costs %.2f MiB of %s, more than %.0f%% of a second deployment's floor (%.1f MiB): ADR 0016's overturn criterion",
				mib(c.got), c.what, memoryMaxFraction*100, mib(limit))
		}
	}
	if os.Getenv("ALIXAI_MEMORY_JSON") != "" {
		keys := make([]string, 0, len(all))
		for _, s := range all {
			keys = append(keys, fmt.Sprintf("%s=%d/%d", s.label, s.rss, s.heap))
		}
		sort.Strings(keys)
		t.Logf("raw (label=rss/heap bytes): %v", keys)
	}
}
