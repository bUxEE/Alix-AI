package runtimesec

import "testing"

// SEC-072: a LocalAI or backend upgrade leaves the runtime in a state AlixAI was not qualified
// against. At startup AlixAI compares the runtime version with the qualified set and an
// unqualified combination reports unavailable instead of serving; the staged upgrade refuses an
// unqualified runtime or backend set before it changes anything and, when the runtime that comes
// up after the switch is not the qualified one, restores the previous qualified set.
//
// Backlog gap G4 (docs/security/test-backlog.md section 7).
func TestSecIncompatibleBackendRefusedAndRolledBack(t *testing.T) {
	t.Run("startup: the runtime release is compared with the qualified set", startupHalf)
	t.Run("upgrade: an unqualified set is refused, a bad one is rolled back", upgradeHalf)
}
