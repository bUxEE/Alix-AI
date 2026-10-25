//go:build linux

package main

import (
	"fmt"
	"os"
)

// hostProcSupported says whether this system can read a process's memory and swap.
const hostProcSupported = true

// readProcStatus reads /proc/<pid>/status of a process id the caller validated.
func readProcStatus(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return "", err
	}
	return string(b), nil
}
