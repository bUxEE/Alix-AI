// Package fspermtest opens a file or directory to other users, for tests of
// the owner-only checks (internal/fsperm). Tests only.
package fspermtest

import "testing"

// Loosen makes path readable by other users: mode 0644 (file) or 0755
// (directory) on POSIX, on Windows a DACL that lets Authenticated Users read it
// (what a directory under E:\ grants by default).
func Loosen(t testing.TB, path string) {
	t.Helper()
	if err := loosen(path); err != nil {
		t.Fatalf("loosen %s: %v", path, err)
	}
}
