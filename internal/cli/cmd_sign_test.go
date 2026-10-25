package cli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
)

const signApp = "demo-library"

func signPackageDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"alixai.yaml":                          "application:\n  id: demo-library\n",
		"capabilities/catalog-find-title.yaml": "id: catalog.find_title\n",
		"documentation/index.yaml":             "entries: []\n",
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func keygen(t *testing.T, outDir, id string) (privPath, pubB64 string) {
	t.Helper()
	res, _ := run(t, Default, nil, "package", "keygen", "-key-id", id, "-out", outDir)
	if res.code != ExitOK {
		t.Fatalf("keygen %s: exit %d\n%s", id, res.code, res.stderr)
	}
	pubText, err := os.ReadFile(filepath.Join(outDir, id+".pub"))
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(outDir, id+".key"), strings.TrimSpace(string(pubText))
}

func verifyWith(t *testing.T, dir string, now time.Time, keys map[string]string, publisherIDs ...string) error {
	t.Helper()
	clk := clock.NewFake(now)
	st := trust.NewStore(storetest.Open(t, nil), clk)
	for id, b64 := range keys {
		pub, err := trust.DecodePublicKey(b64)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.RegisterKey(context.Background(), signApp, id, pub, "cli test"); err != nil {
			t.Fatal(err)
		}
	}
	_, err := trust.NewVerifier(st, clk).VerifyPackageDir(context.Background(), dir, trust.PackageOptions{
		ApplicationID: signApp, PackageVersion: "1.2.3", PublisherKeyIDs: publisherIDs,
	})
	return err
}

func TestPackageCommandsAreRegistered(t *testing.T) {
	names := Default.Names()
	for _, want := range []string{"package sign", "package keygen"} {
		if !slices.Contains(names, want) {
			t.Errorf("command %q is not registered (have %v)", want, names)
		}
	}
}

func TestPackageKeygenThenSign(t *testing.T) {
	keys := t.TempDir()
	priv, pub := keygen(t, keys, "dev-publisher")
	dir := signPackageDir(t)

	res, _ := run(t, Default, nil, "package", "sign",
		"-app", signApp, "-version", "1.2.3", "-key", "dev-publisher="+priv,
		"-signed-at", "2026-10-02T09:00:00Z", "-valid-for", "720h", dir)
	if res.code != ExitOK {
		t.Fatalf("sign: exit %d\n%s", res.code, res.stderr)
	}
	for _, want := range []string{"demo-library 1.2.3", "3 files", "1 signature(s) by dev-publisher", "2026-11-01T09:00:00Z", trust.SignatureManifestPath} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, res.stdout)
		}
	}
	if res.stderr != "" {
		t.Errorf("unexpected stderr: %s", res.stderr)
	}

	inside := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	if err := verifyWith(t, dir, inside, map[string]string{"dev-publisher": pub}, "dev-publisher"); err != nil {
		t.Fatalf("a package signed by the CLI does not verify: %v", err)
	}
	if err := verifyWith(t, dir, time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC), map[string]string{"dev-publisher": pub}, "dev-publisher"); err == nil {
		t.Error("the signature did not expire at expires_at")
	}

	t.Run("a changed file after signing fails verification", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(dir, "alixai.yaml"), []byte("application:\n  id: evil\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := verifyWith(t, dir, inside, map[string]string{"dev-publisher": pub}, "dev-publisher"); err == nil {
			t.Error("tampered package verified")
		}
	})
}

func TestPackageSignAppendKeepsBothKeysValid(t *testing.T) {
	keys := t.TempDir()
	oldPriv, oldPub := keygen(t, keys, "publisher-2026")
	newPriv, newPub := keygen(t, keys, "publisher-2027")
	dir := signPackageDir(t)
	common := []string{"-app", signApp, "-version", "1.2.3", "-signed-at", "2026-10-02T09:00:00Z"}

	res, _ := run(t, Default, nil, append(append([]string{"package", "sign"}, common...), "-key", "publisher-2026="+oldPriv, dir)...)
	if res.code != ExitOK {
		t.Fatalf("first sign: %d\n%s", res.code, res.stderr)
	}
	res, _ = run(t, Default, nil, append(append([]string{"package", "sign"}, common...), "-append", "-key", "publisher-2027="+newPriv, dir)...)
	if res.code != ExitOK || !strings.Contains(res.stdout, "2 signature(s) by publisher-2026, publisher-2027") {
		t.Fatalf("append: %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	both := map[string]string{"publisher-2026": oldPub, "publisher-2027": newPub}
	if err := verifyWith(t, dir, now, both, "publisher-2026"); err != nil {
		t.Errorf("verifies under the old key: %v", err)
	}
	if err := verifyWith(t, dir, now, both, "publisher-2027"); err != nil {
		t.Errorf("verifies under the new key: %v", err)
	}

	t.Run("without -append the manifest is replaced", func(t *testing.T) {
		res, _ := run(t, Default, nil, append(append([]string{"package", "sign"}, common...), "-key", "publisher-2027="+newPriv, dir)...)
		if res.code != ExitOK || !strings.Contains(res.stdout, "1 signature(s) by publisher-2027") {
			t.Fatalf("%d\n%s%s", res.code, res.stdout, res.stderr)
		}
		if err := verifyWith(t, dir, now, both, "publisher-2026"); err == nil {
			t.Error("the old key's signature survived a replacing sign")
		}
	})
}

func TestPackageSignRefusesWhatItCannotSignSafely(t *testing.T) {
	keys := t.TempDir()
	priv, _ := keygen(t, keys, "k1")
	base := []string{"package", "sign", "-app", signApp, "-version", "1.2.3", "-key", "k1=" + priv}

	t.Run("hidden file in the package is an error, not skipped", func(t *testing.T) {
		dir := signPackageDir(t)
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		res, _ := run(t, Default, nil, append(base, dir)...)
		if res.code != ExitError || !strings.Contains(res.stderr, ".git") {
			t.Errorf("exit %d, stderr %q", res.code, res.stderr)
		}
		if _, err := os.Stat(filepath.Join(dir, "signatures")); err == nil {
			t.Error("a refused signing left a signatures directory behind")
		}
	})

	t.Run("no alixai.yaml", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "other.yaml"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if res, _ := run(t, Default, nil, append(base, dir)...); res.code != ExitError {
			t.Errorf("exit %d", res.code)
		}
	})

	t.Run("missing package directory and missing key file", func(t *testing.T) {
		if res, _ := run(t, Default, nil, append(base, filepath.Join(t.TempDir(), "nope"))...); res.code != ExitError {
			t.Errorf("missing directory: exit %d", res.code)
		}
		res, _ := run(t, Default, nil, "package", "sign", "-app", signApp, "-version", "1.2.3", "-key", "k1="+filepath.Join(keys, "absent.key"), signPackageDir(t))
		if res.code != ExitError {
			t.Errorf("missing key file: exit %d", res.code)
		}
	})

	t.Run("expiry not after the signing time", func(t *testing.T) {
		res, _ := run(t, Default, nil, append(base, "-signed-at", "2026-10-02T09:00:00Z", "-expires-at", "2026-10-02T09:00:00Z", signPackageDir(t))...)
		if res.code != ExitError {
			t.Errorf("exit %d\n%s", res.code, res.stderr)
		}
	})

	t.Run("application id and version are validated", func(t *testing.T) {
		dir := signPackageDir(t)
		res, _ := run(t, Default, nil, "package", "sign", "-app", "Not Valid", "-version", "1.2.3", "-key", "k1="+priv, dir)
		if res.code != ExitError {
			t.Errorf("bad app: exit %d", res.code)
		}
		res, _ = run(t, Default, nil, "package", "sign", "-app", signApp, "-version", "one", "-key", "k1="+priv, dir)
		if res.code != ExitError {
			t.Errorf("bad version: exit %d", res.code)
		}
	})

	t.Run("usage errors", func(t *testing.T) {
		dir := signPackageDir(t)
		for name, args := range map[string][]string{
			"no directory":        {"package", "sign", "-app", signApp, "-version", "1.2.3", "-key", "k1=" + priv},
			"two directories":     append(base, dir, dir),
			"no app":              {"package", "sign", "-version", "1.2.3", "-key", "k1=" + priv, dir},
			"no version":          {"package", "sign", "-app", signApp, "-key", "k1=" + priv, dir},
			"no key":              {"package", "sign", "-app", signApp, "-version", "1.2.3", dir},
			"key without id":      {"package", "sign", "-app", signApp, "-version", "1.2.3", "-key", priv, dir},
			"key with empty path": {"package", "sign", "-app", signApp, "-version", "1.2.3", "-key", "k1=", dir},
			"both expiries":       append(base, "-valid-for", "1h", "-expires-at", "2030-01-01T00:00:00Z", dir),
			"zero validity":       append(base, "-valid-for", "0s", dir),
			"offset time":         append(base, "-signed-at", "2026-10-02T09:00:00+02:00", dir),
			"time without zone":   append(base, "-expires-at", "2026-10-02T09:00:00", dir),
			"unknown flag":        append(base, "-nope", dir),
		} {
			res, _ := run(t, Default, nil, args...)
			if res.code != ExitUsage {
				t.Errorf("%s: exit %d, want %d\n%s", name, res.code, ExitUsage, res.stderr)
			}
		}
	})
}

func TestPackageSignNoticesTheTestOnlyKey(t *testing.T) {
	devKey := filepath.Join("..", "..", "tools", "devkeys", "test-only-publisher.key.pem")
	if _, err := os.Stat(devKey); err != nil {
		t.Skipf("tools/devkeys not present: %v", err)
	}
	res, _ := run(t, Default, nil, "package", "sign", "-app", signApp, "-version", "1.2.3", "-key", "alixai-test-only-publisher="+devKey, signPackageDir(t))
	if res.code != ExitOK {
		t.Fatalf("exit %d\n%s", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, "TEST-ONLY") {
		t.Errorf("no notice that the key is TEST-ONLY: %q", res.stderr)
	}
}

func TestPackageKeygenErrors(t *testing.T) {
	out := t.TempDir()
	keygen(t, out, "dev")

	t.Run("an existing key is never overwritten", func(t *testing.T) {
		before, _ := os.ReadFile(filepath.Join(out, "dev.key"))
		res, _ := run(t, Default, nil, "package", "keygen", "-key-id", "dev", "-out", out)
		if res.code != ExitError || !strings.Contains(res.stderr, "already exists") {
			t.Errorf("exit %d, stderr %q", res.code, res.stderr)
		}
		after, _ := os.ReadFile(filepath.Join(out, "dev.key"))
		if string(before) != string(after) {
			t.Error("the key file changed")
		}
	})

	t.Run("key id cannot climb out of the directory", func(t *testing.T) {
		inner := filepath.Join(out, "inner")
		for _, id := range []string{"../escape", "a/b", `a\b`, ".hidden", "UPPER"} {
			res, _ := run(t, Default, nil, "package", "keygen", "-key-id", id, "-out", inner)
			if res.code != ExitError {
				t.Errorf("%q: exit %d", id, res.code)
			}
		}
		if _, err := os.Stat(filepath.Join(out, "escape.key")); err == nil {
			t.Error("a key file escaped the output directory")
		}
	})

	t.Run("usage", func(t *testing.T) {
		for name, args := range map[string][]string{
			"no id":     {"package", "keygen"},
			"extra arg": {"package", "keygen", "-key-id", "x", "y"},
		} {
			if res, _ := run(t, Default, nil, args...); res.code != ExitUsage {
				t.Errorf("%s: exit %d", name, res.code)
			}
		}
	})
}

// TestSecKeygenOwnerOnly: "package keygen" creates the private key readable by
// the owner only, in a directory only the owner can enter.
//
// SEC-081: key files are owner-only (enforced on Unix; Windows modes are
// advisory, see trust.WriteKeyFiles).
func TestSecKeygenOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are advisory on Windows")
	}
	out := filepath.Join(t.TempDir(), "keys")
	priv, _ := keygen(t, out, "dev")
	if st, err := os.Stat(priv); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("private key mode = %v (%v), want 0600", st.Mode().Perm(), err)
	}
	if st, err := os.Stat(out); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("key directory mode = %v (%v), want 0700", st.Mode().Perm(), err)
	}
}
