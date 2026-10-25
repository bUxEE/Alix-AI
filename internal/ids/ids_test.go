package ids

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/oklog/ulid/v2"

	"github.com/buxee/alix-ai/internal/clock"
)

var idFormat = regexp.MustCompile(`^[a-z][a-z0-9_]*_[0-9A-HJKMNP-TV-Z]{26}$`)

func TestPrefixedULID(t *testing.T) {
	start := time.Date(2026, 3, 4, 5, 6, 7, 8_000_000, time.UTC)
	g := NewGenerator(clock.NewFake(start))

	for _, prefix := range []string{"ses", "prep", "rcpt", "src", "job", "aud", "audit_evt"} {
		id := g.New(prefix)
		if !idFormat.MatchString(id) {
			t.Fatalf("New(%q) = %q, does not match %s", prefix, id, idFormat)
		}
		if !strings.HasPrefix(id, prefix+"_") {
			t.Fatalf("New(%q) = %q, missing prefix", prefix, id)
		}
		gotPrefix, u, err := Parse(id)
		if err != nil {
			t.Fatalf("Parse(%q): %v", id, err)
		}
		if gotPrefix != prefix {
			t.Errorf("Parse(%q) prefix = %q, want %q", id, gotPrefix, prefix)
		}
		if got, want := u.Time(), ulid.Timestamp(start); got != want {
			t.Errorf("ULID time = %d ms, want %d ms", got, want)
		}
		// Round trip through the library's strict parser.
		if _, err := ulid.ParseStrict(strings.TrimPrefix(id, prefix+"_")); err != nil {
			t.Errorf("ULID part of %q is not a strict ULID: %v", id, err)
		}
		if !Valid(id, prefix) || Valid(id, prefix+"x") {
			t.Errorf("Valid(%q, ...) misreports prefix", id)
		}
	}

	// The package-level New uses the real clock and is well formed too.
	if id := New("ses"); !idFormat.MatchString(id) {
		t.Errorf("New(ses) = %q, malformed", id)
	}
}

func TestPrefixedULIDRejectsBadPrefix(t *testing.T) {
	g := NewGenerator(nil)
	for _, bad := range []string{"", "Ses", "ses_", "_ses", "s es", "9ses", "ses__x", "a-b", strings.Repeat("a", maxPrefixLen+1)} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("New(%q) did not panic", bad)
				}
			}()
			_ = g.New(bad)
		}()
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	good := New("ses")
	body := strings.TrimPrefix(good, "ses_")
	for name, id := range map[string]string{
		"empty":          "",
		"no separator":   body,
		"no prefix":      "_" + body,
		"upper prefix":   "SES_" + body,
		"lowercase ulid": "ses_" + strings.ToLower(body),
		"short ulid":     "ses_" + body[:25],
		"long ulid":      "ses_" + body + "0",
		"bad alphabet":   "ses_" + strings.Repeat("U", 26),
		"overflow ulid":  "ses_" + "8" + strings.Repeat("Z", 25),
	} {
		if _, _, err := Parse(id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("%s: Parse(%q) err = %v, want ErrInvalidID", name, id, err)
		}
	}
}

func TestIDsMonotonicWithinMillisecond(t *testing.T) {
	// Freeze the clock so every ID is minted in the same millisecond: the
	// ordering below can then only come from the monotonic entropy source.
	frozen := time.Date(2026, 5, 6, 7, 8, 9, 123_000_000, time.UTC)
	g := NewGenerator(clock.NewFake(frozen))

	const n = 2000
	got := make([]string, n)
	for i := range got {
		got[i] = g.New("job")
	}

	for i := 1; i < n; i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("IDs not strictly increasing at %d: %q then %q", i, got[i-1], got[i])
		}
	}
	for i, id := range got {
		_, u, err := Parse(id)
		if err != nil {
			t.Fatalf("Parse(%q): %v", id, err)
		}
		if u.Time() != ulid.Timestamp(frozen) {
			t.Fatalf("ID %d timestamp = %d, want %d (all IDs must share one millisecond)", i, u.Time(), ulid.Timestamp(frozen))
		}
	}
	// The sorted order equals the creation order.
	sorted := append([]string(nil), got...)
	sort.Strings(sorted)
	if diff := cmp.Diff(got, sorted); diff != "" {
		t.Fatalf("lexical order differs from creation order (-created +sorted):\n%s", diff)
	}

	// Advancing the clock keeps the ordering across milliseconds.
	fake := clock.NewFake(frozen)
	g2 := NewGenerator(fake)
	prev := g2.New("job")
	for i := 0; i < 100; i++ {
		fake.Advance(time.Millisecond)
		next := g2.New("job")
		if prev >= next {
			t.Fatalf("not increasing across milliseconds: %q then %q", prev, next)
		}
		prev = next
	}
}

func TestGeneratorConcurrentUniqueAndRaceFree(t *testing.T) {
	g := NewGenerator(clock.NewFake(time.Unix(1_700_000_000, 0)))
	const workers, per = 8, 250
	var mu sync.Mutex
	seen := make(map[string]struct{}, workers*per)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]string, 0, per)
			for i := 0; i < per; i++ {
				local = append(local, g.New("src"))
			}
			mu.Lock()
			defer mu.Unlock()
			for _, id := range local {
				if _, dup := seen[id]; dup {
					t.Errorf("duplicate ID %q", id)
				}
				seen[id] = struct{}{}
			}
		}()
	}
	wg.Wait()
	if len(seen) != workers*per {
		t.Fatalf("got %d unique IDs, want %d", len(seen), workers*per)
	}
}
