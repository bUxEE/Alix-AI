//go:build unix && !linux

package supervisor

import "errors"

// limitsSupported: no limit is enforceable by the supervisor on this platform
// (use the service manager's, for example launchd HardResourceLimits).
func limitsSupported(l Limits) error {
	if l != (Limits{}) {
		return errors.New("the supervisor enforces no resource limit on this platform; use the service manager's limits")
	}
	return nil
}

func applyLimits(int, Limits) error { return nil }
