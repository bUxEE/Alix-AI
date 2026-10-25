//go:build windows

package parse

import (
	"testing"

	"golang.org/x/sys/windows"
)

// killProcess ends a process from outside, the way an administrator or an
// out-of-memory killer might.
func killProcess(t testing.TB, pid int) {
	t.Helper()
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		t.Fatalf("OpenProcess: %v", err)
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 9); err != nil {
		t.Fatalf("TerminateProcess: %v", err)
	}
}
