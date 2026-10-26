package extmodal

import "context"

// Test helpers for packages that must assert the per-call segmentation markers
// without importing unexported context keys.

func UserSegmentedForTest(ctx context.Context) bool { return userSegmented(ctx) }
func VADSegmentedForTest(ctx context.Context) bool  { return vadSegmented(ctx) }
func AudioClassForTest(ctx context.Context) string  { return audioClass(ctx) }
