package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/tools/devkeys"
)

// tarball packs a package directory as a .tar.gz.
func tarball(t *testing.T, src string) string {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if err := tw.WriteHeader(&tar.Header{Name: "./" + filepath.ToSlash(rel), Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = tw.Write(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(raw.Bytes())
	_ = zw.Close()
	out := filepath.Join(t.TempDir(), "package.tar.gz")
	if err := os.WriteFile(out, gz.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPackageInstallIntoQuarantine(t *testing.T) {
	e := newOpsEnv(t, "")
	src := filepath.Join(fixtures, "valid-minimal")

	// 1. the publisher is not trusted yet: refused, nothing written
	res := e.run("package", "install", src)
	if res.code != ExitError {
		t.Fatalf("an untrusted publisher: exit %d\n%s", res.code, res.stdout)
	}
	var refused struct {
		Installed bool `json:"installed"`
		Findings  []struct {
			CheckID  string `json:"check_id"`
			Severity string `json:"severity"`
		} `json:"findings"`
	}
	decode(t, res.stdout, &refused)
	if refused.Installed || len(refused.Findings) == 0 || refused.Findings[0].CheckID != "signature.invalid" {
		t.Fatalf("refusal = %+v", refused)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "data", "packages")); err == nil {
		t.Errorf("a refused package must leave nothing on disk")
	}

	// 2. an unsigned package is refused even from a trusted publisher
	e.mustRun("trust", "add-key", "-app", "demo-library", "-key-id", devkeys.KeyID, "-public-key", devkeys.PublicKeyBase64(), "-allow-test-keys")
	if res := e.run("package", "install", filepath.Join(fixtures, "broken-unsigned")); res.code != ExitError {
		t.Errorf("an unsigned package: exit %d", res.code)
	}
	if res := e.run("package", "install", filepath.Join(fixtures, "broken-tampered")); res.code != ExitError {
		t.Errorf("a tampered package: exit %d", res.code)
	}
	if res := e.run("package", "install", filepath.Join(fixtures, "broken-missing-doc-facet")); res.code != ExitError {
		t.Errorf("a package with a critical finding: exit %d", res.code)
	}

	// 3. a trusted, signed, clean package is installed, quarantined
	var out installOut
	decode(t, e.mustRun("package", "install", "-operator", "root", src).stdout, &out)
	if !out.Created || out.State != "quarantined" || out.ApplicationID != "demo-library" || out.SignedBy != devkeys.KeyID ||
		len(out.ManifestHash) != 64 || out.Path != "packages/demo-library/"+out.ManifestHash {
		t.Fatalf("install = %+v", out)
	}
	if st, err := os.Stat(filepath.Join(e.dir, "data", filepath.FromSlash(out.Path), "alixai.yaml")); err != nil || st.Size() == 0 {
		t.Errorf("the verified bytes are stored: %v", err)
	}
	if !contains(e.eventTypes("demo-library"), "package.installed") {
		t.Errorf("an install is recorded in the audit log")
	}

	// 4. the same content again, as a directory and as an archive, is the same
	// record and never resets its state
	var again installOut
	decode(t, e.mustRun("package", "install", src).stdout, &again)
	if again.Created || again.ManifestHash != out.ManifestHash {
		t.Errorf("again = %+v", again)
	}
	decode(t, e.mustRun("package", "install", tarball(t, src)).stdout, &again)
	if again.Created || again.ManifestHash != out.ManifestHash {
		t.Errorf("an archive of the same bytes is the same package: %+v", again)
	}
	if n := count(e.eventTypes("demo-library"), "package.installed"); n != 1 {
		t.Errorf("only the first install is an event, got %d", n)
	}
	// nothing is activated by installing: the package is quarantined
	if again.State != "quarantined" {
		t.Errorf("state = %s", again.State)
	}
}

func TestPackageInstallUsage(t *testing.T) {
	e := newOpsEnv(t, "")
	for name, args := range map[string][]string{
		"no package":   {"package", "install"},
		"two packages": {"package", "install", "a", "b"},
		"bad operator": {"package", "install", "-operator", "no good", filepath.Join(fixtures, "valid-minimal")},
	} {
		if res := e.run(args...); res.code != ExitUsage {
			t.Errorf("%s: exit %d, want usage\n%s", name, res.code, res.stderr)
		}
	}
	if res := e.run("package", "install", filepath.Join(e.dir, "does-not-exist")); res.code != ExitError || strings.Contains(res.stdout, "installed\": true") {
		t.Errorf("a missing package: exit %d", res.code)
	}
}
