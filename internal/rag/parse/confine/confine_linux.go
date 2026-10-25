//go:build linux

package confine

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// Apply installs, in this order: no_new_privs (so nothing the worker runs can
// gain privilege and so seccomp needs no capability), resource limits,
// Landlock with an empty ruleset (no filesystem access at all, and no TCP bind
// or connect on kernels with Landlock ABI 4 or later), and a seccomp filter
// that denies sockets, program execution and a few dangerous system calls. It
// returns an error only when a mandatory mechanism fails.
func Apply(o Options) (wire.Sandbox, error) {
	sb := wire.Sandbox{Observed: map[string]int64{}}
	if err := setNoNewPrivs(); err != nil {
		return sb, fmt.Errorf("%s: %w", wire.NoNewPrivs, err)
	}
	sb.Applied = append(sb.Applied, wire.NoNewPrivs)

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
	limits := []struct {
		name string
		res  int
		v    uint64
	}{
		{wire.RlimitCore, syscall.RLIMIT_CORE, 0},
		{wire.RlimitFsize, syscall.RLIMIT_FSIZE, 0},
		{wire.RlimitNofile, syscall.RLIMIT_NOFILE, 64},
		// Soft equal to hard: the kernel then kills with SIGKILL at the limit
		// instead of sending a SIGXCPU the Go runtime would ignore.
		{wire.RlimitCPU, syscall.RLIMIT_CPU, cpu},
	}
	for _, l := range limits {
		if err := set(l.name, l.res, l.v); err != nil {
			return sb, err
		}
	}
	if o.MemoryBytes > 0 {
		// Relative to the address space already mapped: the Go runtime reserves
		// more than a budget-sized fixed limit would leave (see wire.ASSlack).
		base, err := addressSpace()
		if err != nil {
			return sb, fmt.Errorf("%s: %w", wire.RlimitAS, err)
		}
		sb.Observed[wire.VMBaseline] = base
		if err := set(wire.RlimitAS, syscall.RLIMIT_AS, uint64(base+o.MemoryBytes+wire.ASSlack)); err != nil {
			return sb, err
		}
	}

	// Best effort from here on: each is reported, none is fatal.
	missing := func(name string, err error) {
		sb.Missing = append(sb.Missing, fmt.Sprintf("%s: %v", name, err))
	}
	if r, err := applyLandlock(); err != nil {
		missing(wire.LandlockFS, err)
	} else {
		sb.Applied = append(sb.Applied, wire.LandlockFS)
		sb.Observed["landlock_abi"] = int64(r.abi)
		if r.net {
			sb.Applied = append(sb.Applied, wire.LandlockNet)
		} else {
			missing(wire.LandlockNet, fmt.Errorf("kernel Landlock ABI %d has no network rules (needs 4)", r.abi))
		}
	}
	if applied, err := applySeccomp(); err != nil {
		missing(wire.SeccompNet, err)
	} else {
		sb.Applied = append(sb.Applied, applied...)
	}
	return sb, nil
}

// addressSpace is the size of this process's address space in bytes, from
// /proc/self/statm (its first field, in pages). It is read before Landlock
// closes the filesystem.
func addressSpace() (int64, error) {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) < 1 {
		return 0, fmt.Errorf("unexpected /proc/self/statm %q", b)
	}
	pages, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil || pages <= 0 {
		return 0, fmt.Errorf("unexpected /proc/self/statm %q", b)
	}
	return pages * int64(os.Getpagesize()), nil
}
