package version

import (
	"strings"
	"testing"
)

func TestGetDefaultsAndStamping(t *testing.T) {
	oldV, oldC, oldD := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = oldV, oldC, oldD })

	Version, Commit, Date = "", "", ""
	got := Get()
	if got.Version != "dev" {
		t.Errorf("unstamped Version = %q, want dev", got.Version)
	}
	if got.Commit == "" || got.Date == "" || got.GoVersion == "" || got.OS == "" || got.Arch == "" {
		t.Errorf("Get() left a field empty: %+v", got)
	}

	Version, Commit, Date = "1.2.3", "0123456789abcdef0123", "2026-01-02T03:04:05Z"
	got = Get()
	if got.Version != "1.2.3" || got.Commit != Commit || got.Date != Date {
		t.Errorf("stamped values not preserved: %+v", got)
	}
	s := got.String()
	for _, want := range []string{"alixai 1.2.3", "commit 0123456789ab", "2026-01-02T03:04:05Z"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() = %q, missing %q", s, want)
		}
	}
	if strings.Contains(s, "0123456789abc") {
		t.Errorf("String() = %q, commit not abbreviated to 12 chars", s)
	}
}
