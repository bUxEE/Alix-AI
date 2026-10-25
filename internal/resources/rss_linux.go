//go:build linux

package resources

import (
	"bytes"
	"errors"
	"os"
	"strconv"
)

// osProcessRSS is VmRSS of this process, from /proc/self/status.
func osProcessRSS() (uint64, error) {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, err
	}
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		rest, ok := bytes.CutPrefix(line, []byte("VmRSS:"))
		if !ok {
			continue
		}
		f := bytes.Fields(rest)
		if len(f) == 0 {
			break
		}
		kb, err := strconv.ParseUint(string(f[0]), 10, 64)
		if err != nil {
			return 0, err
		}
		return kb << 10, nil
	}
	return 0, errors.New("VmRSS not found in /proc/self/status")
}
