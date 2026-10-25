package fs

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// A folder source may not point at AlixAI's own data or install directory, at a directory inside one or at one that
// contains one, nor at a directory the readiness scanner refuses (a system or home directory). ResolveRoot answers
// the same as a scan, before anything is opened, and returns the canonical path a permitted root resolves to.
func TestFolderRootRefusesProtectedAndSystemDirectories(t *testing.T) {
	h := newHarness(t)
	data := filepath.Join(h.tr.base, "alixai-data")
	install := filepath.Join(h.tr.base, "alixai-bin")
	for _, d := range []string{filepath.Join(data, "external", "secrets"), install} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	h.p = New(Config{Clock: h.clk, Protected: []string{data, install, ""}})

	refused := map[string]string{
		"the data directory":         data,
		"inside the data directory":  filepath.Join(data, "external", "secrets"),
		"above the data directory":   h.tr.base,
		"the install directory":      install,
		"the home directory":         os.Getenv(map[bool]string{true: "USERPROFILE", false: "HOME"}[runtime.GOOS == "windows"]),
		"an operating system folder": map[bool]string{true: os.Getenv("SystemRoot"), false: "/etc"}[runtime.GOOS == "windows"],
	}
	for name, path := range refused {
		if path == "" {
			continue
		}
		d := h.tr.def()
		d.Root.Path = path
		if _, err := h.p.ResolveRoot(d); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("ResolveRoot(%s) error = %v, want validation_failed", name, err)
		}
		if _, err := h.p.Probe(h.ctx, scope(), d); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("Probe(%s) error = %v, want validation_failed", name, err)
		}
	}

	// A sibling of the protected directories is an ordinary folder: it resolves to its canonical path.
	got, err := h.p.ResolveRoot(h.tr.def())
	if err != nil {
		t.Fatalf("ResolveRoot(an ordinary folder) = %v", err)
	}
	if want, _ := h.p.paths.Normalize(h.tr.root); got != want {
		t.Errorf("ResolveRoot = %q, want %q", got, want)
	}
	if hl, err := h.p.Probe(h.ctx, scope(), h.tr.def()); err != nil || !hl.Reachable() {
		t.Errorf("Probe(an ordinary folder) = %+v, %v", hl, err)
	}

	// An executable at the root of a volume (/alixai in a minimal image) or in the home directory does not make every
	// folder of the disk or of the user "inside the install directory": such a directory is refused as a root on its
	// own and is not kept as a protected one.
	broad := []string{filepath.VolumeName(h.tr.base) + string(filepath.Separator)}
	if home := os.Getenv(map[bool]string{true: "USERPROFILE", false: "HOME"}[runtime.GOOS == "windows"]); home != "" {
		broad = append(broad, home)
	}
	p := New(Config{Clock: h.clk, Protected: broad})
	if _, err := p.ResolveRoot(h.tr.def()); err != nil {
		t.Errorf("with a volume root or the home directory protected, an ordinary folder = %v", err)
	}
}
