//go:build !race

package profile_test

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/internal/voice/profile"
)

var loadSink *profile.Profile

// disabledLoadAllocs measures the allocations of one Load with voice disabled.
func disabledLoadAllocs(ctx context.Context, d profile.Deps) (allocs float64, measured bool) {
	return testing.AllocsPerRun(500, func() { loadSink, _, _ = profile.Load(ctx, d) }), true
}
