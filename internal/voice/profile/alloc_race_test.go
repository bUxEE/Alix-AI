//go:build race

package profile_test

import (
	"context"

	"github.com/buxee/alix-ai/internal/voice/profile"
)

// disabledLoadAllocs does not measure under the race detector: its
// instrumentation changes allocation counts, so the check runs in the plain
// build only.
func disabledLoadAllocs(context.Context, profile.Deps) (float64, bool) { return 0, false }
