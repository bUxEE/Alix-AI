package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// hostSource is a MemorySource over host processes: for a stack run natively
// (no container), the resident memory and the swap of the processes that make
// it up. On Linux both come from /proc/<pid>/status (VmRSS, VmSwap), so swap is
// measured, not assumed. Elsewhere the tool cannot read a process's swap and
// refuses to start with --pid.
type hostSource struct {
	pids []int
	// read returns the text of /proc/<pid>/status. Tests replace it.
	read func(pid int) (string, error)
}

// newHostSource validates the process ids.
func newHostSource(pids []int) (*hostSource, error) {
	if !hostProcSupported {
		return nil, errors.New("--pid reads /proc and works on Linux only: sample a container (--container) on this system")
	}
	if len(pids) == 0 {
		return nil, errors.New("at least one --pid is needed to sample a host process")
	}
	seen := map[int]bool{}
	for _, p := range pids {
		if p <= 0 {
			return nil, fmt.Errorf("pid %d is not a process id", p)
		}
		if seen[p] {
			return nil, fmt.Errorf("pid %d is listed twice", p)
		}
		seen[p] = true
	}
	return &hostSource{pids: append([]int(nil), pids...), read: readProcStatus}, nil
}

// Describe implements MemorySource.
func (h *hostSource) Describe() MemoryDescriptor {
	ps := make([]string, len(h.pids))
	for i, p := range h.pids {
		ps[i] = strconv.Itoa(p)
	}
	return MemoryDescriptor{Kind: MemSourceHost, Target: "pid " + strings.Join(ps, ",")}
}

// Sample implements MemorySource: resident memory and swap summed over the
// processes. A process that is gone is an error, never a smaller sum.
func (h *hostSource) Sample(_ context.Context) (MemSample, error) {
	var rss, swap uint64
	for _, pid := range h.pids {
		text, err := h.read(pid)
		if err != nil {
			return MemSample{}, fmt.Errorf("pid %d: %w", pid, err)
		}
		r, s, err := parseProcStatus(text)
		if err != nil {
			return MemSample{}, fmt.Errorf("pid %d: %w", pid, err)
		}
		rss += r
		swap += s
	}
	return MemSample{UsedMiB: float64(rss) / 1024, SwapMiB: float64(swap) / 1024, SwapKnown: true}, nil
}

// parseProcStatus returns VmRSS and VmSwap of /proc/<pid>/status text, in kiB.
// Both must be present: a kernel thread has neither and is not a stack process.
func parseProcStatus(text string) (rssKiB, swapKiB uint64, err error) {
	var haveRSS, haveSwap bool
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "VmRSS":
			rssKiB, haveRSS, err = procKiB(v)
		case "VmSwap":
			swapKiB, haveSwap, err = procKiB(v)
		}
		if err != nil {
			return 0, 0, err
		}
	}
	if !haveRSS || !haveSwap {
		return 0, 0, errors.New("the status has no VmRSS and VmSwap (not a user process?)")
	}
	return rssKiB, swapKiB, nil
}

func procKiB(v string) (uint64, bool, error) {
	f := strings.Fields(v)
	if len(f) == 0 {
		return 0, false, errors.New("empty memory field")
	}
	n, err := strconv.ParseUint(f[0], 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("bad memory field %q", clipRunes(f[0], 30))
	}
	if len(f) > 1 && !strings.EqualFold(f[1], "kB") {
		return 0, false, fmt.Errorf("unexpected unit %q", clipRunes(f[1], 10))
	}
	return n, true, nil
}
