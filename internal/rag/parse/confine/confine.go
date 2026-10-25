// Package confine is the worker's side of the sandbox: what the parser process
// does to itself before it reads a single byte of an untrusted document, and
// the attestation it reports back so the host can check it.
//
// Confinement is layered (ADR 0014). The host starts the process under the
// strongest limits the OS lets a parent impose from outside (a Windows Job
// Object, a Unix process group and an RSS watchdog) and the worker then
// confines itself from inside (resource limits everywhere, and on Linux
// no_new_privs, Landlock and a seccomp filter). Mechanisms are named:
//
//   - Mandatory ones (Required) must be applied or the worker exits without
//     parsing: the job fails closed, never runs unconfined.
//   - Best-effort ones (Landlock and seccomp on a kernel that lacks them) are
//     reported in Sandbox.Missing and lower the sandbox level the host records.
//
// What each platform actually guarantees is in docs/decisions/0014.
package confine

import (
	"runtime/metrics"
	"time"
)

// Options are what the worker confines itself to, taken from the host's Init.
type Options struct {
	// MemoryBytes is the process memory budget; 0 means none was set.
	MemoryBytes int64
	// CPUMillis is the total CPU time the process may use.
	CPUMillis int64
}

// StartMemoryGuard watches the Go runtime's own account of its memory and calls
// exceeded once it passes limit. It is cooperative (a compromised worker could
// stop it) and is the only memory enforcement on a platform whose OS offers
// none for a process; elsewhere it is defence in depth that stops a runaway
// allocation a few milliseconds before the OS would. It returns a stop
// function.
func StartMemoryGuard(limit int64, exceeded func()) (stop func()) {
	if limit <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		samples := []metrics.Sample{
			{Name: "/memory/classes/total:bytes"},
			{Name: "/memory/classes/heap/released:bytes"},
		}
		t := time.NewTicker(5 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				metrics.Read(samples)
				if samples[0].Value.Kind() != metrics.KindUint64 || samples[1].Value.Kind() != metrics.KindUint64 {
					continue
				}
				if int64(samples[0].Value.Uint64()-samples[1].Value.Uint64()) > limit {
					exceeded()
					return
				}
			}
		}
	}()
	return func() { close(done) }
}
