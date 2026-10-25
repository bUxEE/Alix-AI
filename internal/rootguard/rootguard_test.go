package rootguard

import (
	"path/filepath"
	"testing"
)

func TestRefusal(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for _, c := range []struct {
		path, goos string
		env        map[string]string
		refused    bool
	}{
		{"/", "linux", nil, true},
		{"/etc/ssl", "linux", nil, true},
		{"/home", "linux", nil, true},
		{"/home/ann", "linux", map[string]string{"HOME": "/home/ann"}, true},
		{"/home/ann/docs", "linux", map[string]string{"HOME": "/home/ann"}, false},
		{"/srv/library", "linux", nil, false},
	} {
		if filepath.Separator != '/' {
			continue // POSIX paths do not clean the same way on Windows; the scanner's tests cover both systems
		}
		if got := RefusalWith(c.path, env(c.env), c.goos) != ""; got != c.refused {
			t.Errorf("RefusalWith(%q) refused = %v, want %v", c.path, got, c.refused)
		}
	}
	if why := Refusal(t.TempDir()); why != "" {
		t.Errorf("a temporary directory was refused: %s", why)
	}
	if Refusal(filepath.VolumeName(t.TempDir())+string(filepath.Separator)) == "" {
		t.Error("a filesystem root was allowed")
	}
}
