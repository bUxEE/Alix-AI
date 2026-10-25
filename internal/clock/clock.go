// Package clock is the single source of time for AlixAI. Production code takes
// a Clock instead of calling time.Now so that tests can control time and so
// that every persisted timestamp is UTC.
package clock

import (
	"sync"
	"time"
)

// Clock reports the current time.
type Clock interface {
	Now() time.Time
}

// Real is the wall clock. Its readings are always in UTC.
type Real struct{}

// Now returns the current wall-clock time in UTC.
func (Real) Now() time.Time { return time.Now().UTC() }

// Fake is a manually driven Clock for tests. It is safe for concurrent use.
// The zero value reads as the zero time.Time until Set or Advance is called.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// NewFake returns a Fake whose current time is start (normalised to UTC).
func NewFake(start time.Time) *Fake {
	return &Fake{now: start.UTC()}
}

// Now returns the fake's current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance moves the fake forward by d and returns the new time. A negative d
// panics: time that runs backwards hides ordering bugs; use Set to jump
// explicitly.
func (f *Fake) Advance(d time.Duration) time.Time {
	if d < 0 {
		panic("clock: Fake.Advance called with a negative duration")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	return f.now
}

// Set jumps the fake to t (normalised to UTC), forwards or backwards.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t.UTC()
}
