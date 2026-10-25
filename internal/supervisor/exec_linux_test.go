//go:build linux

package supervisor

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// On Linux the limits are applied to the running child with prlimit, and core
// dumps are always off.
func TestExecAppliesRlimitsOnLinux(t *testing.T) {
	inst, dir := startFake(t, ExecConfig{Limits: Limits{MaxMemoryBytes: 8 << 30, MaxOpenFiles: 512}})
	waitFor(t, 60*time.Second, "the child to be ready", func() bool {
		_, err := os.Stat(readyFile(dir, inst.PID()))
		return err == nil
	})
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/limits", inst.PID()))
	if err != nil {
		t.Skipf("no /proc: %v", err)
	}
	want := map[string]string{
		"Max core file size": "0",
		"Max data size":      fmt.Sprint(uint64(8 << 30)),
		"Max open files":     "512",
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		for name, soft := range want {
			rest, ok := strings.CutPrefix(line, name)
			if !ok {
				continue
			}
			f := strings.Fields(rest) // soft, hard, unit
			if len(f) < 2 || f[0] != soft || f[1] != soft {
				t.Errorf("%s: soft/hard = %v, want %s/%s", name, f, soft, soft)
			}
			delete(want, name)
		}
	}
	for name := range want {
		t.Errorf("%s not found in /proc/<pid>/limits", name)
	}
}
