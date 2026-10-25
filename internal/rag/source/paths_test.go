package source

import (
	"errors"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
)

var (
	win   = Paths{Windows: true}
	posix = Paths{}
)

// The Windows rules are string functions, so they run on every platform.
func TestNormalizeWindowsForms(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{`C:\data\docs`, `C:\data\docs`},
		{`c:/data/docs/`, `C:\data\docs`},
		{`C:\data\.\docs\\x`, `C:\data\docs\x`},
		{`\\?\C:\data\docs`, `C:\data\docs`},
		{`\\?\c:\data`, `C:\data`},
		{`\??\C:\data`, `C:\data`},
		{`\\?\UNC\nas\share\docs`, `\\nas\share\docs`},
		{`\\?\unc\nas\share`, `\\nas\share`},
		{`\\nas\share\docs\a`, `\\nas\share\docs\a`},
		{`//nas/share/docs`, `\\nas\share\docs`},
		{`C:\`, `C:\`},
		{`D:`, ``},
	}
	for _, tc := range tests {
		got, err := win.Normalize(tc.in)
		if tc.want == "" {
			if err == nil {
				t.Errorf("Normalize(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestNormalizeRefusesUnsafeRoots(t *testing.T) {
	windows := []string{
		``, `relative\dir`, `\rooted`, `C:drive-relative`, `\\.\C:\x`, `\\.\PhysicalDrive0`,
		`\\?\Volume{01234567-89ab-cdef-0123-456789abcdef}\x`, `\\?\GLOBALROOT\Device\x`, `\\?\C:`,
		`\\server`, `\\server\`, `\\?\UNC\server`, `C:\a\..\b`, `C:\a\x` + "\x00" + `y`, "C:\\a\nb",
	}
	for _, in := range windows {
		if got, err := win.Normalize(in); err == nil {
			t.Errorf("windows Normalize(%q) = %q, want an error", in, got)
		}
	}
	posixBad := []string{``, `relative/x`, `./x`, `/a/../b`, `/a/x` + "\x00", `C:\x`, `\\?\C:\x`}
	for _, in := range posixBad {
		if got, err := posix.Normalize(in); err == nil {
			t.Errorf("posix Normalize(%q) = %q, want an error", in, got)
		}
	}
	if got, err := posix.Normalize("/srv//docs/./a/"); err != nil || got != "/srv/docs/a" {
		t.Errorf("posix Normalize clean = %q, %v", got, err)
	}
}

func TestWithinIsSeparatorAndCaseAware(t *testing.T) {
	type tc struct {
		p          Paths
		root, path string
		want       bool
		rel        string
	}
	tests := []tc{
		{win, `C:\root`, `C:\root`, true, ""},
		{win, `C:\root`, `C:\root\a\b.txt`, true, "a/b.txt"},
		{win, `C:\root`, `c:\ROOT\A\b.txt`, true, "A/b.txt"},
		{win, `C:\root`, `\\?\C:\root\a`, true, "a"},
		{win, `\\?\C:\root`, `C:\root\a`, true, "a"},
		{win, `C:\root`, `C:\root2\a`, false, ""},
		{win, `C:\root`, `C:\roo`, false, ""},
		{win, `C:\root`, `D:\root\a`, false, ""},
		{win, `C:\root`, `C:\`, false, ""},
		{win, `C:\root`, `C:\root\..\other`, false, ""},
		{win, `C:\`, `C:\x\y`, true, "x/y"},
		{win, `\\nas\share\docs`, `\\NAS\Share\docs\a.md`, true, "a.md"},
		{win, `\\nas\share\docs`, `\\nas\share\docs2\a.md`, false, ""},
		{win, `\\nas\share\docs`, `\\nas2\share\docs\a.md`, false, ""},
		{win, `\\nas\share\docs`, `\\?\UNC\nas\share\docs\a.md`, true, "a.md"},
		{win, `C:\root`, `\\.\C:\root\a`, false, ""},
		{posix, `/srv/docs`, `/srv/docs/a/b`, true, "a/b"},
		{posix, `/srv/docs`, `/srv/docs`, true, ""},
		{posix, `/srv/docs`, `/srv/docs2/a`, false, ""},
		{posix, `/srv/docs`, `/srv/DOCS/a`, false, ""},
		{posix, `/srv/docs`, `/srv`, false, ""},
		{posix, `/srv/docs`, `/srv/docs/../x`, false, ""},
		{posix, `/`, `/etc/passwd`, true, "etc/passwd"},
	}
	for _, c := range tests {
		got, ok := c.p.Rel(c.root, c.path)
		if ok != c.want || (ok && got != c.rel) {
			t.Errorf("windows=%v Rel(%q, %q) = %q, %v; want %q, %v", c.p.Windows, c.root, c.path, got, ok, c.rel, c.want)
		}
		if c.p.Within(c.root, c.path) != c.want {
			t.Errorf("windows=%v Within(%q, %q) != %v", c.p.Windows, c.root, c.path, c.want)
		}
	}
}

func TestIsVolumeRoot(t *testing.T) {
	for _, p := range []string{`C:\`, `c:/`, `\\nas\share`, `\\nas\share\`, `\\?\C:\`, `\\?\UNC\nas\share`} {
		if !win.IsVolumeRoot(p) {
			t.Errorf("windows IsVolumeRoot(%q) = false", p)
		}
	}
	for _, p := range []string{`C:\data`, `\\nas\share\docs`, `relative`, ``} {
		if win.IsVolumeRoot(p) {
			t.Errorf("windows IsVolumeRoot(%q) = true", p)
		}
	}
	if !posix.IsVolumeRoot("/") || !posix.IsVolumeRoot("//") || posix.IsVolumeRoot("/srv") || posix.IsVolumeRoot("srv") {
		t.Error("posix IsVolumeRoot is wrong")
	}
}

func TestValidKey(t *testing.T) {
	ok := []string{"a.md", "dir/a.md", "dir/sub dir/a b.md", "a:b.txt (colon is a legal POSIX name; Windows case below)", "ünï/çödé.md", "aux-notes.md", "console/x", "nulled.txt"}
	// A colon name is legal on POSIX only.
	for _, k := range ok {
		if strings.Contains(k, ":") {
			if err := posix.ValidKey("a:b.txt"); err != nil {
				t.Errorf("posix ValidKey(a:b.txt) = %v", err)
			}
			if err := win.ValidKey("a:b.txt"); err == nil {
				t.Error("windows ValidKey(a:b.txt) accepted a colon")
			}
			continue
		}
		for _, p := range []Paths{posix, win} {
			if err := p.ValidKey(k); err != nil {
				t.Errorf("windows=%v ValidKey(%q) = %v", p.Windows, k, err)
			}
		}
	}

	everywhere := []string{
		"", "/", "/etc/passwd", "a/", "a//b", "a/./b", "./a", ".", "..", "../a", "a/..", "a/../b", "a/../../b",
		`..\a`, `a\b`, `\a`, `C:\x`, `\\?\C:\x`, `\\srv\share\x`, "a\x00b", "a\nb", "a\x7fb", "\xff\xfe", strings.Repeat("a/", maxKeyBytes),
	}
	for _, k := range everywhere {
		for _, p := range []Paths{posix, win} {
			err := p.ValidKey(k)
			if err == nil {
				t.Errorf("windows=%v ValidKey(%q) accepted it", p.Windows, k)
				continue
			}
			if !errors.Is(err, ErrEscapesRoot) || contracts.CodeOf(err) != contracts.ErrScopeViolation {
				t.Errorf("windows=%v ValidKey(%q) = %v, want ErrEscapesRoot / scope_violation", p.Windows, k, err)
			}
		}
	}

	windowsOnly := []string{
		"a.txt::$DATA", "a.txt:stream", "C:x", "dir/a.txt:zone", "private./x", "private /x", "a/b.", "a/b ", "CON", "con", "NUL.txt", "dir/AUX", "dir/aux.md",
		"COM1", "com9.txt", "LPT1", "lpt3.log", "CONIN$", "CONOUT$", "dir/PRN.md", "a<b", `a"b`, "a|b", "a?b", "a*b", "COM\u00b9", "con .txt",
		strings.Repeat("x", 256),
	}
	for _, k := range windowsOnly {
		if err := win.ValidKey(k); err == nil {
			t.Errorf("windows ValidKey(%q) accepted it", k)
		}
	}
	for _, k := range []string{"private./x", "CON", "aux.md", "a:b", "a<b", "a?b"} {
		if err := posix.ValidKey(k); err != nil {
			t.Errorf("posix ValidKey(%q) = %v; those names are legal outside Windows", k, err)
		}
	}
}

func TestJoinAndFold(t *testing.T) {
	if got := win.Join(`C:\root`, "a/b.md"); got != `C:\root\a\b.md` {
		t.Errorf("windows Join = %q", got)
	}
	if got := win.Join(`C:\`, "a.md"); got != `C:\a.md` {
		t.Errorf("windows Join at drive root = %q", got)
	}
	if got := posix.Join("/srv/docs", "a/b.md"); got != "/srv/docs/a/b.md" {
		t.Errorf("posix Join = %q", got)
	}
	if got := posix.Join("/", "a.md"); got != "/a.md" {
		t.Errorf("posix Join at root = %q", got)
	}
	if win.Fold("AbC") != "abc" || posix.Fold("AbC") != "AbC" {
		t.Error("Fold is wrong")
	}
}
