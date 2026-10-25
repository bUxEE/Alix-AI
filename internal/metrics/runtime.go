package metrics

import (
	"context"
	"runtime"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/version"
)

// RuntimeCollector reports the process itself: build information, uptime and
// the Go runtime's memory and scheduler figures. It reads only the process's
// own counters.
func RuntimeCollector(clk clock.Clock) Collector {
	if clk == nil {
		clk = clock.Real{}
	}
	start := clk.Now()
	info := version.Get()
	return CollectorFunc("runtime", func(context.Context) ([]Family, error) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		one := func(name, help string, t Type, v float64) Family {
			return Family{Name: name, Help: help, Type: t, Samples: []Sample{{Value: v}}}
		}
		return []Family{
			{Name: "alixai_build_info", Help: "Build information; the value is always 1.", Type: Gauge, Samples: []Sample{{
				Labels: []Label{{"version", info.Version}, {"commit", info.Commit}, {"go_version", info.GoVersion}},
				Value:  1,
			}}},
			one("alixai_process_uptime_seconds", "Seconds since the process started.", Gauge, clk.Now().Sub(start).Seconds()),
			one("alixai_go_goroutines", "Goroutines that currently exist.", Gauge, float64(runtime.NumGoroutine())),
			one("alixai_go_heap_alloc_bytes", "Bytes of allocated heap objects.", Gauge, float64(m.HeapAlloc)),
			one("alixai_go_sys_bytes", "Bytes of memory obtained from the operating system.", Gauge, float64(m.Sys)),
			one("alixai_go_gc_cycles_total", "Completed garbage collection cycles.", Counter, float64(m.NumGC)),
			one("alixai_go_gc_pause_seconds_total", "Total time spent in garbage collection pauses.", Counter, time.Duration(m.PauseTotalNs).Seconds()),
		}, nil
	})
}
