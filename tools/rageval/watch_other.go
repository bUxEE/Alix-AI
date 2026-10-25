//go:build !windows

package main

import "time"

// time.Since is fine-grained everywhere else.
func counterNow() int64                        { return 0 }
func counterSince(int64) (time.Duration, bool) { return 0, false }
