//go:build linux

package supervisor

import "golang.org/x/sys/unix"

func limitsSupported(Limits) error { return nil }

// applyLimits sets the child's limits with prlimit(2): core dumps off (they
// would hold prompts and model memory), private memory (RLIMIT_DATA counts
// anonymous memory and not memory-mapped model files, unlike RLIMIT_AS) and
// open files. Both the soft and the hard limit are set, so the child cannot
// raise them again.
func applyLimits(pid int, l Limits) error {
	set := func(resource int, v uint64) error {
		return unix.Prlimit(pid, resource, &unix.Rlimit{Cur: v, Max: v}, nil)
	}
	if err := set(unix.RLIMIT_CORE, 0); err != nil {
		return err
	}
	if l.MaxMemoryBytes > 0 {
		if err := set(unix.RLIMIT_DATA, l.MaxMemoryBytes); err != nil {
			return err
		}
	}
	if l.MaxOpenFiles > 0 {
		if err := set(unix.RLIMIT_NOFILE, l.MaxOpenFiles); err != nil {
			return err
		}
	}
	return nil
}
