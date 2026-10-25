package main

import (
	"time"

	"github.com/buxee/alix-ai/internal/clock"
)

func fakeClockForTest() clock.Clock { return clock.NewFake(fakeStart.Add(time.Hour)) }
