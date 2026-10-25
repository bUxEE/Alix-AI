package models

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// installed writes content to a temp file and returns a qualified-emulated
// entry that pins exactly that content, plus the file's path.
func installed(t *testing.T, content []byte) (Entry, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "model-Q4_K_M.gguf")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	e := qualifiedEntry("verify-me")
	e.Artifact.SHA256 = strp(hex.EncodeToString(sum[:]))
	e.Artifact.SizeBytes = i64p(int64(len(content)))
	return e, path
}

// requireUnavailable asserts the SEC-063 contract: Verify refuses with the
// unavailable error code, and the specific reason can still be told apart.
func requireUnavailable(t *testing.T, err error, reason error) {
	t.Helper()
	if err == nil {
		t.Fatalf("Verify accepted the artefact, want a refusal for: %v", reason)
	}
	if got := contracts.CodeOf(err); got != contracts.ErrUnavailable {
		t.Fatalf("code = %q, want %q; err = %v", got, contracts.ErrUnavailable, err)
	}
	if !errors.Is(err, reason) {
		t.Fatalf("error is not %v: %v", reason, err)
	}
	// A refusal for one reason must not also look like another.
	for _, other := range []error{ErrHashMismatch, ErrNotPinned, ErrArtefactUnreadable, ErrSizeMismatch, ErrNotActivatable, ErrRuntimeNotAllowed, ErrInvalidEntry} {
		if other != reason && errors.Is(err, other) {
			t.Errorf("error for %v also matches %v: %v", reason, other, err)
		}
	}
}

// TestSecVerifyRefusesHashMismatch is SEC-063: a model file that is replaced,
// corrupted or downgraded is refused and reported unavailable.
//
// SEC-063
func TestSecVerifyRefusesHashMismatch(t *testing.T) {
	content := bytes.Repeat([]byte("alixai model weights "), 1000)
	e, path := installed(t, content)

	// Positive control: the pinned file verifies, so each refusal below is
	// caused by the change made to it and nothing else.
	if err := Verify(e, path); err != nil {
		t.Fatalf("the pinned file was refused: %v", err)
	}

	t.Run("one flipped byte", func(t *testing.T) {
		tampered := bytes.Clone(content)
		tampered[len(tampered)/2] ^= 0x01
		if err := os.WriteFile(path, tampered, 0o600); err != nil {
			t.Fatal(err)
		}
		defer os.WriteFile(path, content, 0o600)
		err := Verify(e, path)
		requireUnavailable(t, err, ErrHashMismatch)
		// The message names both digests so an operator can see what changed.
		if !strings.Contains(err.Error(), *e.Artifact.SHA256) {
			t.Errorf("the pinned digest is missing from the error: %v", err)
		}
	})

	t.Run("replaced by another model of the same size", func(t *testing.T) {
		other := bytes.Repeat([]byte("x"), len(content))
		p := filepath.Join(t.TempDir(), "other.gguf")
		if err := os.WriteFile(p, other, 0o600); err != nil {
			t.Fatal(err)
		}
		requireUnavailable(t, Verify(e, p), ErrHashMismatch)
	})

	t.Run("truncated or extended", func(t *testing.T) {
		for name, data := range map[string][]byte{
			"truncated": content[:len(content)-1],
			"extended":  append(bytes.Clone(content), 0),
			"emptied":   nil,
		} {
			p := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(p, data, 0o600); err != nil {
				t.Fatal(err)
			}
			requireUnavailable(t, Verify(e, p), ErrSizeMismatch)
		}
	})

	t.Run("size not recorded still catches a different file", func(t *testing.T) {
		noSize := e
		noSize.Artifact.SizeBytes = nil
		if err := Verify(noSize, path); err != nil {
			t.Fatalf("control: %v", err)
		}
		p := filepath.Join(t.TempDir(), "other.gguf")
		if err := os.WriteFile(p, []byte("different"), 0o600); err != nil {
			t.Fatal(err)
		}
		requireUnavailable(t, Verify(noSize, p), ErrHashMismatch)
	})
}

// TestSecVerifyFailsClosed is SEC-063: with nothing to compare against, or
// nothing to read, or an entry that may not run, the answer is no.
//
// SEC-063
func TestSecVerifyFailsClosed(t *testing.T) {
	e, path := installed(t, []byte("weights"))
	if err := Verify(e, path); err != nil {
		t.Fatalf("control: %v", err)
	}

	t.Run("no pinned hash", func(t *testing.T) {
		unpinned := candidateEntry("verify-me")
		if err := unpinned.Validate(); len(err) != 0 {
			t.Fatalf("a candidate without a hash is valid: %v", err)
		}
		err := Verify(unpinned, path)
		requireUnavailable(t, err, ErrNotPinned)
		// The message names the entry, not its status.
		if msg := err.Error(); !strings.Contains(msg, "pins no sha256 for verify-me") || strings.Contains(msg, "for candidate") {
			t.Errorf("the message does not name the entry: %v", err)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		requireUnavailable(t, Verify(e, filepath.Join(t.TempDir(), "absent.gguf")), ErrArtefactUnreadable)
	})

	t.Run("empty path", func(t *testing.T) {
		requireUnavailable(t, Verify(e, ""), ErrArtefactUnreadable)
	})

	t.Run("directory", func(t *testing.T) {
		requireUnavailable(t, Verify(e, t.TempDir()), ErrArtefactUnreadable)
	})

	t.Run("symlink to the pinned file is the pinned file", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "link.gguf")
		if err := os.Symlink(path, link); err != nil {
			t.Skipf("cannot create a symlink here: %v", err)
		}
		if err := Verify(e, link); err != nil {
			t.Fatalf("a link to the pinned file was refused: %v", err)
		}
		// And a link to anything else is judged by what it points at.
		other := filepath.Join(t.TempDir(), "other.gguf")
		if err := os.WriteFile(other, []byte("WEIGHTS"), 0o600); err != nil { // same size, different content
			t.Fatal(err)
		}
		link2 := filepath.Join(t.TempDir(), "link2.gguf")
		if err := os.Symlink(other, link2); err != nil {
			t.Skipf("cannot create a symlink here: %v", err)
		}
		requireUnavailable(t, Verify(e, link2), ErrHashMismatch)
	})

	t.Run("rejected and retired entries never verify", func(t *testing.T) {
		for _, st := range []Status{StatusRejected, StatusRetired} {
			x := e
			x.Status = st
			requireUnavailable(t, Verify(x, path), ErrNotActivatable)
		}
	})

	t.Run("an invalid entry is no reference", func(t *testing.T) {
		for name, mutate := range map[string]func(x *Entry){
			"upper-case digest": func(x *Entry) { x.Artifact.SHA256 = strp(strings.ToUpper(*x.Artifact.SHA256)) },
			"no licence":        func(x *Entry) { x.Licence.SPDX = "" },
			"certified without evidence": func(x *Entry) {
				x.Status = StatusCertified
				x.Review.EvidencePath = nil
			},
		} {
			x := e
			mutate(&x)
			t.Run(name, func(t *testing.T) { requireUnavailable(t, Verify(x, path), ErrInvalidEntry) })
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := Verify(e, path, WithContext(ctx))
		if err == nil || contracts.CodeOf(err) != contracts.ErrTimeout || !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled verification must refuse with a timeout, got %v", err)
		}
	})
}

// TestSecVerifyRuntimeAllowlist is SEC-063's backend and runtime version hook:
// the entry's own list and any deployment allowlist must both allow the
// runtime, and an empty list allows nothing.
//
// SEC-063
func TestSecVerifyRuntimeAllowlist(t *testing.T) {
	e, path := installed(t, []byte("weights"))
	e.Backend.AllowedRuntimeVersions = []string{"v4.10.0", "v4.11.0"}
	rt := RuntimeInfo{Runtime: "localai", Version: "v4.10.0", Backend: "llama-cpp", BackendVersion: "sha256:aaaa"}

	// Without WithRuntime the hash check alone applies.
	if err := Verify(e, path); err != nil {
		t.Fatalf("hash-only verification: %v", err)
	}
	if err := Verify(e, path, WithRuntime(rt)); err != nil {
		t.Fatalf("an allowed runtime was refused: %v", err)
	}

	t.Run("version not on the entry's list", func(t *testing.T) {
		bad := rt
		bad.Version = "v3.0.0"
		requireUnavailable(t, Verify(e, path, WithRuntime(bad)), ErrRuntimeNotAllowed)
	})
	t.Run("empty version", func(t *testing.T) {
		bad := rt
		bad.Version = ""
		requireUnavailable(t, Verify(e, path, WithRuntime(bad)), ErrRuntimeNotAllowed)
	})
	t.Run("empty list allows nothing", func(t *testing.T) {
		none := e
		none.Backend.AllowedRuntimeVersions = []string{}
		requireUnavailable(t, Verify(none, path, WithRuntime(rt)), ErrRuntimeNotAllowed)
		none.Backend.AllowedRuntimeVersions = nil
		requireUnavailable(t, Verify(none, path, WithRuntime(rt)), ErrRuntimeNotAllowed)
	})
	t.Run("wrong backend or runtime", func(t *testing.T) {
		for name, mutate := range map[string]func(r *RuntimeInfo){
			"backend": func(r *RuntimeInfo) { r.Backend = "whisper" },
			"runtime": func(r *RuntimeInfo) { r.Runtime = "other" },
		} {
			bad := rt
			mutate(&bad)
			t.Run(name, func(t *testing.T) { requireUnavailable(t, Verify(e, path, WithRuntime(bad)), ErrRuntimeNotAllowed) })
		}
	})

	deployment := Allowlist{
		Runtime: map[string][]string{"localai": {"v4.10.0"}},
		Backend: map[string][]string{"llama-cpp": {"sha256:aaaa"}},
	}
	t.Run("deployment allowlist allows", func(t *testing.T) {
		if err := Verify(e, path, WithRuntime(rt, deployment.Check)); err != nil {
			t.Fatalf("an allowlisted runtime was refused: %v", err)
		}
	})
	t.Run("deployment allowlist denies a version the entry allows", func(t *testing.T) {
		// v4.11.0 is on the entry's list but not on the deployment's.
		newer := rt
		newer.Version = "v4.11.0"
		err := Verify(e, path, WithRuntime(newer, deployment.Check))
		requireUnavailable(t, err, ErrRuntimeNotAllowed)
		if !strings.Contains(err.Error(), "allowlist") {
			t.Errorf("the refusal does not say which list denied it: %v", err)
		}
	})
	t.Run("deployment allowlist denies an unknown backend build", func(t *testing.T) {
		other := rt
		other.BackendVersion = "sha256:bbbb"
		requireUnavailable(t, Verify(e, path, WithRuntime(other, deployment.Check)), ErrRuntimeNotAllowed)
	})
	t.Run("an empty deployment allowlist allows nothing", func(t *testing.T) {
		requireUnavailable(t, Verify(e, path, WithRuntime(rt, Allowlist{}.Check)), ErrRuntimeNotAllowed)
		requireUnavailable(t, Verify(e, path, WithRuntime(rt, Allowlist{Runtime: map[string][]string{"localai": {}}}.Check)), ErrRuntimeNotAllowed)
	})
	t.Run("custom hook can deny", func(t *testing.T) {
		deny := func(Entry, RuntimeInfo) error { return errors.New("pulled by the release manifest") }
		err := Verify(e, path, WithRuntime(rt, deny))
		requireUnavailable(t, err, ErrRuntimeNotAllowed)
		if !strings.Contains(err.Error(), "release manifest") {
			t.Errorf("the hook's reason is lost: %v", err)
		}
	})
	t.Run("a nil hook is not an allow", func(t *testing.T) {
		requireUnavailable(t, Verify(e, path, WithRuntime(rt, nil)), ErrRuntimeNotAllowed)
	})
	t.Run("runtime is checked before the file is read", func(t *testing.T) {
		bad := rt
		bad.Version = "v0"
		err := Verify(e, filepath.Join(t.TempDir(), "absent.gguf"), WithRuntime(bad))
		requireUnavailable(t, err, ErrRuntimeNotAllowed)
	})
}

// TestHashFile: the streaming hasher agrees with crypto/sha256, across
// several read buffers, and refuses what is not a regular file.
func TestHashFile(t *testing.T) {
	// "abc" is the FIPS 180 test vector.
	p := filepath.Join(t.TempDir(), "abc")
	if err := os.WriteFile(p, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, size, err := HashFile(context.Background(), p)
	if err != nil || sum != testSHA || size != 3 {
		t.Fatalf("HashFile(abc) = %q, %d, %v", sum, size, err)
	}

	// Larger than the 1 MiB read buffer, so the loop runs more than once.
	big := bytes.Repeat([]byte{0xA5, 0x5A, 0x00, 0xFF}, (3<<20)/4+17)
	bp := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(bp, big, 0o600); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(big)
	sum, size, err = HashFile(context.Background(), bp)
	if err != nil || sum != hex.EncodeToString(want[:]) || size != int64(len(big)) {
		t.Fatalf("HashFile(big) = %q, %d, %v", sum, size, err)
	}

	if _, _, err := HashFile(context.Background(), t.TempDir()); err == nil {
		t.Error("a directory was hashed")
	}
	if _, _, err := HashFile(context.Background(), ""); err == nil {
		t.Error("an empty path was hashed")
	}
}
