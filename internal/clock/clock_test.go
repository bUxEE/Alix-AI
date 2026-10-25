package clock

import (
	"sync"
	"testing"
	"time"
)

func TestRealIsUTCAndCurrent(t *testing.T) {
	var c Clock = Real{}
	before := time.Now().UTC().Add(-time.Second)
	got := c.Now()
	if got.Location() != time.UTC {
		t.Fatalf("Real.Now location = %v, want UTC", got.Location())
	}
	if got.Before(before) || got.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("Real.Now = %v, not close to wall clock", got)
	}
}

func TestFakeAdvanceAndSet(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600))
	f := NewFake(start)
	if !f.Now().Equal(start) || f.Now().Location() != time.UTC {
		t.Fatalf("NewFake.Now = %v, want %v in UTC", f.Now(), start)
	}
	if got, want := f.Advance(90*time.Second), start.Add(90*time.Second); !got.Equal(want) || !f.Now().Equal(want) {
		t.Fatalf("after Advance: returned %v, Now %v, want %v", got, f.Now(), want)
	}
	f.Set(start)
	if !f.Now().Equal(start) {
		t.Fatalf("after Set: Now = %v, want %v", f.Now(), start)
	}
}

func TestFakeNegativeAdvancePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Advance(-1) did not panic")
		}
	}()
	NewFake(time.Unix(0, 0)).Advance(-1)
}

func TestFakeConcurrentUse(t *testing.T) {
	f := NewFake(time.Unix(0, 0))
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.Advance(time.Millisecond)
			_ = f.Now()
		}()
	}
	wg.Wait()
	if got, want := f.Now(), time.Unix(0, 0).UTC().Add(50*time.Millisecond); !got.Equal(want) {
		t.Fatalf("Now = %v, want %v", got, want)
	}
}
