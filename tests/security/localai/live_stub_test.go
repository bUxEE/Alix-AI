//go:build !live

package localaisec

import "testing"

// Without the live tag the live halves of SEC-069 and SEC-070 are no-ops: nothing here talks to a running stack.

func secLocalAILiveAgentsProbe(t *testing.T) { t.Helper() }

func secLocalAILiveInstallProbe(t *testing.T) { t.Helper() }
