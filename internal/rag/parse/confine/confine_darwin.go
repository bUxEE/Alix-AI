//go:build darwin

package confine

import (
	"fmt"
	"syscall"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// Apply installs the resource limits macOS honours: CPU time, no core dumps,
// no file writes, few descriptors. macOS does not enforce RLIMIT_AS or
// RLIMIT_RSS, so there is no OS memory limit for the process: memory is held by
// the in-process guard (StartMemoryGuard, which the worker starts because
// Init.SelfGuard is true on this platform) and, from outside, by the host's
// wall-time kill. This platform is not covered by CI: the code is built and
// vetted, and its behaviour is recorded as untested.
func Apply(o Options) (wire.Sandbox, error) {
	sb := wire.Sandbox{Observed: map[string]int64{}}
	set := func(name string, res int, v uint64) error {
		if err := syscall.Setrlimit(res, &syscall.Rlimit{Cur: v, Max: v}); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		var got syscall.Rlimit
		if err := syscall.Getrlimit(res, &got); err != nil || got.Cur != v || got.Max != v {
			return fmt.Errorf("%s: read back %v, want %d", name, got, v)
		}
		sb.Applied = append(sb.Applied, name)
		sb.Observed[name] = int64(v)
		return nil
	}
	cpu := uint64((o.CPUMillis + 999) / 1000)
	if cpu < 1 {
		cpu = 1
	}
	for _, s := range []struct {
		name string
		res  int
		v    uint64
	}{
		{wire.RlimitCore, syscall.RLIMIT_CORE, 0},
		{wire.RlimitFsize, syscall.RLIMIT_FSIZE, 0},
		{wire.RlimitNofile, syscall.RLIMIT_NOFILE, 64},
		{wire.RlimitCPU, syscall.RLIMIT_CPU, cpu},
	} {
		if err := set(s.name, s.res, s.v); err != nil {
			return sb, err
		}
	}
	return sb, nil
}
