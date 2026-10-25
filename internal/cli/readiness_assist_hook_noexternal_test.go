//go:build noexternal

package cli

import "testing"

// The noexternal build holds no X package, so nothing fills the external readiness hook.
func TestReadinessExternalHookIsNilInTheNoexternalBuild(t *testing.T) {
	if rdyCLIExternal != nil {
		t.Fatal("the external hook must be nil in the noexternal build")
	}
}
