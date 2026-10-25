package wire

// Names of the confinement mechanisms a worker reports in Sandbox.Applied and
// Sandbox.Missing. They are stable: results and audit records carry them.
const (
	NoNewPrivs   = "no_new_privs"
	RlimitCPU    = "rlimit_cpu"
	RlimitCore   = "rlimit_core"
	RlimitFsize  = "rlimit_fsize"
	RlimitNofile = "rlimit_nofile"
	RlimitAS     = "rlimit_as"
	LandlockFS   = "landlock_fs"
	LandlockNet  = "landlock_net"
	SeccompNet   = "seccomp_net"
	SeccompExec  = "seccomp_exec"
	JobObject    = "job_object"
)

// ASSlack is added to the memory budget, on top of the address space the worker
// already occupies when it confines itself, to form RLIMIT_AS on Linux. The Go
// runtime reserves a large amount of address space before main runs (about
// 1.2 GiB on linux/amd64 with Go 1.27 and 16 CPUs, measured in
// docs/efficiency/M2-04.md, and it varies with the Go version and the CPU
// count), so a fixed limit cannot be right: the worker reads its own VmSize
// (VMBaseline) and limits what it may map from then on to the budget plus one
// heap arena (64 MiB) of slack. The RSS watchdog and the in-process guard hold
// the budget itself; RLIMIT_AS is the hard cap behind them.
const ASSlack = 64 << 20

// VMBaseline is the key, in Sandbox.Observed, under which a Linux worker
// reports the address space it occupied when it applied RLIMIT_AS.
const VMBaseline = "vm_baseline_bytes"

// RequiredMechanisms is what the host insists on finding in Sandbox.Applied for
// a worker on the given operating system. Anything else a worker applies is
// best effort and is reported, not required. An empty result means the
// platform has no supported confinement and the host refuses to parse.
func RequiredMechanisms(goos string) []string {
	switch goos {
	case "windows":
		return []string{JobObject}
	case "linux":
		return []string{NoNewPrivs, RlimitCPU, RlimitCore, RlimitFsize, RlimitNofile}
	case "darwin":
		return []string{RlimitCPU, RlimitCore, RlimitFsize, RlimitNofile}
	}
	return nil
}
