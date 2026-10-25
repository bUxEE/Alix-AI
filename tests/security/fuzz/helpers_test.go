package fuzzsec

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"testing"
	"time"
)

// randWord returns a lower-case, letters-only word of n characters from
// crypto/rand. Canaries and markers are generated per run, never literals, and
// letters only so that no identifier-shaped token (a digit or an inner
// separator) can arise from them.
func randWord(tb testing.TB, n int) string {
	tb.Helper()
	const letters = "abcdefghijklmnopqrstuvwxyz"
	max := big.NewInt(int64(len(letters)))
	b := make([]byte, n)
	for i := range b {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			tb.Fatal(err)
		}
		b[i] = letters[v.Int64()]
	}
	return string(b)
}

// within runs fn and fails the test when it does not return inside d. It is the
// "never hangs" half of every property: a goroutine that outlives the deadline
// is abandoned (the process ends with the test binary), the failure is reported.
func within(tb testing.TB, d time.Duration, what string, fn func()) {
	tb.Helper()
	done := make(chan struct{})
	var panicked any
	go func() {
		defer close(done)
		defer func() { panicked = recover() }()
		fn()
	}()
	select {
	case <-done:
		if panicked != nil {
			tb.Fatalf("%s panicked: %v", what, panicked)
		}
	case <-time.After(d):
		tb.Fatalf("%s did not finish within %s", what, d)
	}
}

// quoteShort renders arbitrary input for a failure message without flooding the log.
func quoteShort(b []byte) string {
	if len(b) > 200 {
		return fmt.Sprintf("%q... (%d bytes)", b[:200], len(b))
	}
	return fmt.Sprintf("%q", b)
}
