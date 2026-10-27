package contracts

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

var update = flag.Bool("update", false, "rewrite testdata/errcodes.golden from the current taxonomy")

const goldenPath = "testdata/errcodes.golden"

// renderErrCodes is the golden format: one "<code> retryable=<bool>" line per
// code, in canonical order.
func renderErrCodes() string {
	var sb strings.Builder
	for _, c := range AllErrCodes() {
		fmt.Fprintf(&sb, "%s retryable=%t\n", c, c.Retryable())
	}
	return sb.String()
}

func TestErrCodesMatchGolden(t *testing.T) {
	got := renderErrCodes()
	if *update {
		if err := os.WriteFile(filepath.FromSlash(goldenPath), []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(filepath.FromSlash(goldenPath))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	want := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("error-code taxonomy changed (-golden +current); this is a public API break. "+
			"If intended, review it, then run `go test ./pkg/contracts -run TestErrCodesMatchGolden -update`:\n%s", diff)
	}
}

func TestErrCodesAreWellFormed(t *testing.T) {
	codes := AllErrCodes()
	if len(codes) != 34 {
		t.Fatalf("taxonomy has %d codes, want 34", len(codes))
	}
	seen := map[ErrCode]bool{}
	for _, c := range codes {
		if seen[c] {
			t.Errorf("duplicate code %q", c)
		}
		seen[c] = true
		if !c.Valid() {
			t.Errorf("%q not Valid()", c)
		}
		if s := string(c); s == "" || s != strings.ToLower(s) || strings.ContainsAny(s, " -.") {
			t.Errorf("code %q must be non-empty lower snake_case", c)
		}
	}
	if ErrCode("").Valid() || ErrCode("nope").Valid() {
		t.Error("undefined codes must not be Valid()")
	}
	// An out-of-taxonomy code is never retryable (fail closed).
	if ErrCode("nope").Retryable() {
		t.Error("undefined code must not be retryable")
	}
	// AllErrCodes returns a copy.
	codes[0] = "mutated"
	if AllErrCodes()[0] == "mutated" {
		t.Error("AllErrCodes must return a copy")
	}
}

func TestRetryableOnlyForTransientCodes(t *testing.T) {
	retryable := map[ErrCode]bool{
		ErrUnavailable: true, ErrDegraded: true, ErrTimeout: true,
		ErrRateLimited: true, ErrResourceExhausted: true,
	}
	for _, c := range AllErrCodes() {
		if got := c.Retryable(); got != retryable[c] {
			t.Errorf("%s.Retryable() = %t, want %t", c, got, retryable[c])
		}
	}
	// Blindly re-sending these can double-commit.
	for _, c := range []ErrCode{ErrUncertainOutcome, ErrPartialFailure, ErrConflict, ErrStale} {
		if c.Retryable() {
			t.Errorf("%s must not be retryable", c)
		}
	}
}

func TestErrorTypeBasics(t *testing.T) {
	cause := errors.New("db locked")
	e := Wrap(ErrUnavailable, cause, "store busy").WithDetails("module", "store")
	if !e.Retryable || e.Code != ErrUnavailable {
		t.Fatalf("Wrap: %+v", e)
	}
	if !errors.Is(e, cause) {
		t.Error("errors.Is must reach the wrapped cause")
	}
	if !errors.Is(e, New(ErrUnavailable, "")) {
		t.Error("errors.Is must match on code")
	}
	if errors.Is(e, New(ErrTimeout, "")) {
		t.Error("errors.Is must not match a different code")
	}
	if errors.Is(e, New(ErrUnavailable, "other message")) {
		t.Error("a target with a message must match the message too")
	}
	if got, want := e.Error(), "unavailable: store busy: db locked"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}

	wrapped := fmt.Errorf("handler: %w", e)
	if got := CodeOf(wrapped); got != ErrUnavailable {
		t.Errorf("CodeOf(wrapped) = %q", got)
	}
	if !HasCode(wrapped, ErrUnavailable) || HasCode(wrapped, ErrInternal) {
		t.Error("HasCode mismatch")
	}
	if got := CodeOf(errors.New("plain")); got != ErrInternal {
		t.Errorf("untyped error must map to internal, got %q", got)
	}
	if got := CodeOf(nil); got != "" {
		t.Errorf("CodeOf(nil) = %q", got)
	}
	joined := errors.Join(errors.New("a"), New(ErrConflict, "x"))
	if !HasCode(joined, ErrConflict) {
		t.Error("HasCode must see through errors.Join")
	}
	if got := Newf(ErrTimeout, "after %dms", 30).Message; got != "after 30ms" {
		t.Errorf("Newf message = %q", got)
	}
	for _, c := range AllErrCodes() {
		if New(c, "m").Retryable != c.Retryable() {
			t.Errorf("New(%s).Retryable mismatches Code.Retryable()", c)
		}
	}
}
