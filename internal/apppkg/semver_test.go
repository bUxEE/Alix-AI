package apppkg

import "testing"

func TestCompareSemverPrereleaseOrdering(t *testing.T) {
	// SemVer 2.0.0: a release has higher precedence than any of its prereleases;
	// 1.1.0-dev (unstamped core) must sit inside [1.0.0, 2.0.0) with stamped 1.1.0.
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.1.0", -1},
		{"1.1.0-dev", "1.0.0", 1},
		{"1.1.0-dev", "1.1.0", -1},
		{"1.1.0", "1.1.0-dev", 1},
		{"1.1.0-dev", "2.0.0", -1},
		{"1.0.0-rc.1", "1.0.0", -1},
		{"2.0.0-rc.1", "2.0.0", -1},
		{"1.1.0-alpha", "1.1.0-beta", -1},
		{"1.1.0-1", "1.1.0-alpha", -1}, // numeric identifiers are lower than alphanumeric
	}
	for _, tc := range cases {
		a, okA := parseSemver(tc.a)
		b, okB := parseSemver(tc.b)
		if !okA || !okB {
			t.Fatalf("parseSemver(%q)=%v parseSemver(%q)=%v", tc.a, okA, tc.b, okB)
		}
		if got := compareSemver(a, b); got != tc.want {
			t.Errorf("compareSemver(%s, %s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
	if DevCoreVersion != "1.1.0-dev" {
		t.Fatalf("DevCoreVersion = %q, want 1.1.0-dev (derived from version.ReleaseLine)", DevCoreVersion)
	}
	if CoreVersionOrDev("dev") != DevCoreVersion {
		t.Fatalf("CoreVersionOrDev(dev) = %q, want %q", CoreVersionOrDev("dev"), DevCoreVersion)
	}
	if CoreVersionOrDev("1.1.0") != "1.1.0" {
		t.Fatalf("CoreVersionOrDev(1.1.0) = %q", CoreVersionOrDev("1.1.0"))
	}
}
