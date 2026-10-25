//go:build !noexternal

package cli

import "testing"

// The default build fills the external readiness hook from readiness_assist_external.go; it still
// refuses to use an external model without external mode, the guard and a class above local_only.
func TestReadinessExternalHookIsFilledInTheDefaultBuild(t *testing.T) {
	if rdyCLIExternal == nil {
		t.Fatal("the default build must fill the external readiness hook")
	}
}
