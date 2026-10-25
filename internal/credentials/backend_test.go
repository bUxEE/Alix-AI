package credentials

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/fsperm/fspermtest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func newEnc(t *testing.T, keys KeySource) (*EncryptedFile, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets", FileName)
	e, err := NewEncryptedFile(path, keys)
	if err != nil {
		t.Fatal(err)
	}
	return e, path
}

func TestEncryptedFileRoundTrip(t *testing.T) {
	sources := map[string]func(t *testing.T) KeySource{
		"key file": func(t *testing.T) KeySource {
			return KeyFileSource{Path: filepath.Join(t.TempDir(), "keys", "master.key")}
		},
		"os store": func(t *testing.T) KeySource { return OSStoreKeySource{Store: NewMemoryOSStore()} },
	}
	for name, mk := range sources {
		t.Run(name, func(t *testing.T) {
			keys := mk(t)
			e, path := newEnc(t, keys)
			slot := Slot{Ref: "cred_A", Binding: "fake\x1fdep\x1fapp\x1ften"}
			if _, err := e.Get(bg, slot); !errors.Is(err, ErrNotFound) {
				t.Fatalf("empty file: %v", err)
			}
			if err := e.Put(bg, slot, Secret(tKey)); err != nil {
				t.Fatal(err)
			}
			if err := e.Put(bg, slot.refresh(), Secret(tKey2)); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			notContains(t, "the encrypted file", raw, tKey, tKey2)
			var doc map[string]any
			if json.Unmarshal(raw, &doc) != nil {
				t.Fatalf("the file is not JSON: %s", raw)
			}

			// a second instance over the same file and key (a restart) reads both values
			e2, _ := NewEncryptedFile(path, keys)
			for s, want := range map[Slot]string{slot: tKey, slot.refresh(): tKey2} {
				got, err := e2.Get(bg, s)
				if err != nil || got.Reveal() != want {
					t.Fatalf("restart read: %v", err)
				}
			}

			// authenticated: a record opened under another binding, or copied to another slot, fails
			other := slot
			other.Binding = "fake\x1fdep\x1fapp\x1ften_other"
			if _, err := e.Get(bg, other); err == nil {
				t.Fatal("a record opened under another binding")
			}
			var d encDoc
			_ = json.Unmarshal(raw, &d)
			d.Entries["cred_B"] = d.Entries["cred_A"]
			swapped, _ := json.Marshal(d)
			if err := os.WriteFile(path, swapped, 0o600); err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS == "windows" {
				_ = restrictOwnerOnly(path, false)
			}
			if _, err := e.Get(bg, Slot{Ref: "cred_B", Binding: slot.Binding}); err == nil {
				t.Fatal("a record copied to another slot opened")
			}

			// a flipped byte of a ciphertext fails
			d.Entries["cred_A"] = encRecord{Nonce: d.Entries["cred_A"].Nonce, CT: flip(d.Entries["cred_A"].CT)}
			tampered, _ := json.Marshal(d)
			if err := os.WriteFile(path, tampered, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Get(bg, slot); err == nil {
				t.Fatal("a tampered record opened")
			}

			// delete
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := e.Delete(bg, slot); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Get(bg, slot); !errors.Is(err, ErrNotFound) {
				t.Fatalf("after delete: %v", err)
			}
			if err := e.Delete(bg, slot); err != nil {
				t.Fatalf("deleting an absent record: %v", err)
			}
		})
	}

	t.Run("the wrong master key", func(t *testing.T) {
		e, path := newEnc(t, KeyFileSource{Path: filepath.Join(t.TempDir(), "k1", "master.key")})
		slot := Slot{Ref: "cred_A"}
		if err := e.Put(bg, slot, Secret(tKey)); err != nil {
			t.Fatal(err)
		}
		wrong, _ := NewEncryptedFile(path, KeyFileSource{Path: filepath.Join(t.TempDir(), "k2", "master.key")})
		if err := wrong.Put(bg, Slot{Ref: "cred_Z"}, Secret(tKey2)); err == nil {
			// a Put with a new key would have to re-key a file full of records: refused
			t.Fatal("a file with records was written under another master key")
		}
		if _, err := wrong.Get(bg, slot); err == nil {
			t.Fatal("opened with a key that is not the master key")
		}
	})

	t.Run("a lost master key never re-keys a file full of records", func(t *testing.T) {
		store := NewMemoryOSStore()
		e, _ := newEnc(t, OSStoreKeySource{Store: store})
		if err := e.Put(bg, Slot{Ref: "cred_A"}, Secret(tKey)); err != nil {
			t.Fatal(err)
		}
		_ = store.Delete(OSStoreService, "master-key")
		if err := e.Put(bg, Slot{Ref: "cred_B"}, Secret(tKey2)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Put without the master key: %v", err)
		}
		if store.Len() != 0 {
			t.Fatal("a new master key was made over existing records")
		}
	})
}

func flip(b64 string) string {
	if b64 == "" {
		return b64
	}
	c := b64[0]
	if c == 'A' {
		c = 'B'
	} else {
		c = 'A'
	}
	return string(c) + b64[1:]
}

// SEC-081: key material and secret files are created owner only, and anything looser is refused.
func TestEncryptedFileOwnerOnlyPermissions(t *testing.T) {
	root := t.TempDir()
	keyPath := filepath.Join(root, "keys", "master.key")
	path := filepath.Join(root, "secrets", FileName)
	e, err := NewEncryptedFile(path, KeyFileSource{Path: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Put(bg, Slot{Ref: "cred_A"}, Secret(tKey)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path string
		dir  bool
	}{{filepath.Dir(path), true}, {path, false}, {filepath.Dir(keyPath), true}, {keyPath, false}} {
		if err := checkOwnerOnly(c.path, c.dir); err != nil {
			t.Errorf("%s is not owner only: %v", c.path, err)
		}
		if runtime.GOOS != "windows" {
			want := os.FileMode(0o600)
			if c.dir {
				want = 0o700
			}
			if st, _ := os.Stat(c.path); st.Mode().Perm() != want {
				t.Errorf("%s has mode %v, want %v", c.path, st.Mode().Perm(), want)
			}
		}
	}
	// a rewrite keeps it
	if err := e.Put(bg, Slot{Ref: "cred_B"}, Secret(tKey2)); err != nil {
		t.Fatal(err)
	}
	if err := checkOwnerOnly(path, false); err != nil {
		t.Errorf("after a rewrite: %v", err)
	}

	// the checker sees looseness (negative controls; the same check runs on POSIX modes and Windows ACLs)
	looseFile := filepath.Join(root, "loose.txt")
	if err := os.WriteFile(looseFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fspermtest.Loosen(t, looseFile)
	if err := checkOwnerOnly(looseFile, false); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("a file with the default permissions passed: %v", err)
	}
	looseDir := filepath.Join(root, "loose")
	if err := os.Mkdir(looseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fspermtest.Loosen(t, looseDir)
	if err := ensureSecureDir(looseDir); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("an existing directory with the default permissions was accepted: %v", err)
	}
	loose, _ := NewEncryptedFile(filepath.Join(looseDir, FileName), KeyFileSource{Path: keyPath})
	if err := loose.Put(bg, Slot{Ref: "cred_A"}, Secret(tKey)); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("Put into a loose directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(looseDir, FileName)); err == nil {
		t.Fatal("a file was written into a loose directory")
	}

	// a key file that others can read is refused and nothing is opened with it
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(strings.Repeat("ab", masterKeyLen)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fspermtest.Loosen(t, keyPath)
	if _, err := e.Get(bg, Slot{Ref: "cred_A"}); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("a world-readable key file was used: %v", err)
	}
}

func TestEnvAndFileBackends(t *testing.T) {
	ctx := bg
	t.Run("env", func(t *testing.T) {
		b := NewEnvBackend("")
		t.Setenv("ALIXAI_SECRET_FAKE", "  "+tKey+"\n")
		t.Setenv("PATH_LIKE_SECRET", tKey2)
		v, err := b.Get(ctx, Slot{Locator: "ALIXAI_SECRET_FAKE"})
		if err != nil || v.Reveal() != tKey {
			t.Fatalf("Get: %v", err)
		}
		for _, bad := range []string{"PATH_LIKE_SECRET", "PATH", "alixai_secret_fake", "ALIXAI_SECRET_", "ALIXAI_SECRET_MISSING", ""} {
			if _, err := b.Get(ctx, Slot{Locator: bad}); !errors.Is(err, ErrNotFound) {
				t.Errorf("%q: %v", bad, err)
			}
		}
		if err := b.Put(ctx, Slot{Locator: "ALIXAI_SECRET_FAKE"}, "x"); !errors.Is(err, ErrReadOnly) {
			t.Fatalf("Put: %v", err)
		}
	})
	t.Run("file", func(t *testing.T) {
		dir := t.TempDir()
		if err := ensureSecureDir(filepath.Join(dir, "s")); err != nil {
			t.Fatal(err)
		}
		sdir := filepath.Join(dir, "s")
		b, err := NewFileBackend(sdir)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeFileOwnerOnly(filepath.Join(sdir, "fake.key"), []byte(tKey+"\r\n"), false); err != nil {
			t.Fatal(err)
		}
		v, err := b.Get(ctx, Slot{Locator: "fake.key"})
		if err != nil || v.Reveal() != tKey {
			t.Fatalf("Get: %v", err)
		}
		for _, bad := range []string{"../fake.key", "a/b", "..", "fake.key/..", `a\b`, "", "missing.key"} {
			if _, err := b.Get(ctx, Slot{Locator: bad}); !errors.Is(err, ErrNotFound) {
				t.Errorf("%q: %v", bad, err)
			}
		}
		// readable by others: refused
		loose := filepath.Join(sdir, "loose.key")
		if err := os.WriteFile(loose, []byte(tKey), 0o644); err != nil {
			t.Fatal(err)
		}
		fspermtest.Loosen(t, loose)
		if _, err := b.Get(ctx, Slot{Locator: "loose.key"}); !errors.Is(err, ErrInsecurePermissions) {
			t.Fatalf("a loose secret file was read: %v", err)
		}
		// a link is refused
		if err := os.Symlink(filepath.Join(sdir, "fake.key"), filepath.Join(sdir, "link.key")); err == nil {
			if _, err := b.Get(ctx, Slot{Locator: "link.key"}); err == nil {
				t.Fatal("a symbolic link was followed")
			}
		}
		if err := b.Put(ctx, Slot{Locator: "fake.key"}, "x"); !errors.Is(err, ErrReadOnly) {
			t.Fatalf("Put: %v", err)
		}
	})
}

func TestOSStoreBackendBehindInterface(t *testing.T) {
	mem := NewMemoryOSStore()
	// the OS store is only an interface: the backend and the master-key source take it, and the fake
	// stands in for the real stores (Windows Credential Manager, Keychain, Secret Service)
	var _ OSStore = mem
	var _ Backend = NewOSBackend(mem)
	var _ KeySource = OSStoreKeySource{Store: mem}

	g := newRig(t, func(c *Config) {
		c.Backends = []Backend{NewOSBackend(mem)}
		c.Default = BackendOSStore
	})
	ref := g.addKey(tKey)
	if got := ref.Fingerprint; got == nil || *got != Fingerprint(tKey) {
		t.Fatalf("fingerprint = %v", got)
	}
	// the value is in the (fake) OS store and nowhere else
	found := false
	for _, e := range mem.Dump() {
		if strings.Contains(e, tKey) {
			found = true
		}
	}
	if !found || mem.Len() != 1 {
		t.Fatalf("the value is not in the OS store (%d entries)", mem.Len())
	}
	notContains(t, "the database", allBytes(t, filepath.Dir(g.db.Path())), tKey)

	if v, err := g.resolve(ref.RefID); err != nil || v.Reveal() != tKey {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := g.svc.Rotate(bg, g.scope, ref.RefID, Rotation{Key: Secret(tKey2)}); err != nil {
		t.Fatal(err)
	}
	if v, err := g.resolve(ref.RefID); err != nil || v.Reveal() != tKey2 {
		t.Fatalf("Resolve after rotation: %v", err)
	}
	if _, err := g.svc.Revoke(bg, g.scope, ref.RefID); err != nil {
		t.Fatal(err)
	}
	if mem.Len() != 0 {
		t.Fatalf("revoking left %d entries in the OS store", mem.Len())
	}

	// a locked or unreachable store fails closed, with no text of the OS and no fallback to a file
	ref2 := g.addKey(tKey)
	mem.Fail = errors.New("keychain locked: " + tCanary)
	_, err := g.resolve(ref2.RefID)
	mustCode(t, err, contracts.ErrUnavailable, ReasonBackendFailed)
	notContains(t, "the error", []byte(err.Error()), tCanary)
	if _, err := g.svc.AddAPIKey(bg, g.scope, AddAPIKeyRequest{ProviderID: "fake", Key: Secret(tKey2)}); err == nil {
		t.Fatal("a key was added while the OS store was down")
	} else {
		notContains(t, "the error", []byte(err.Error()), tCanary, tKey2)
	}
	if _, serr := os.Stat(filepath.Join(g.dir, FileName)); serr == nil {
		t.Fatal("a value was written to a file when the OS store failed")
	}
}
