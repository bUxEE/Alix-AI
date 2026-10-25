//go:build !windows && !linux && !darwin

package parse

import (
	"errors"
	"os"
	"os/exec"
)

// There is no supported confinement on this operating system, so no worker is
// ever started: parsing fails closed with "unavailable".
func newConfinement(cmd *exec.Cmd, lim hostLimits) (confinement, error) {
	return nil, errors.New("no parser sandbox is implemented for this operating system")
}

func signalName(ps *os.ProcessState) string { return "" }
