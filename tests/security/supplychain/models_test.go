package supplychainsec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/tests/security/secutil"
)

// activatable lists the registry entries a deployment may run: not rejected,
// not retired.
func activatable(reg *models.Registry) []models.Entry {
	var out []models.Entry
	for _, e := range reg.Entries() {
		if e.Status != models.StatusRejected && e.Status != models.StatusRetired {
			out = append(out, e)
		}
	}
	return out
}

// requireRefused asserts the SEC-063 contract: the artefact is refused with the
// unavailable code and the specific sentinel.
func requireRefused(t *testing.T, err error, reason error) {
	t.Helper()
	if err == nil {
		t.Fatalf("Verify accepted the artefact, want a refusal for: %v", reason)
	}
	if got := contracts.CodeOf(err); got != contracts.ErrUnavailable {
		t.Fatalf("code = %q, want %q (err: %v)", got, contracts.ErrUnavailable, err)
	}
	if !errors.Is(err, reason) {
		t.Fatalf("error is not %v: %v", reason, err)
	}
}

// SEC-063
//
// A model file in the bind-mounted models directory that is replaced, corrupted
// or downgraded is refused and reported unavailable, for every entry the SHIPPED
// registry (models/registry) can activate: the registry as shipped pins a
// sha256 and size for every entry that may run, a `certified` entry cannot be
// written without one, and each kind of change to the file is caught.
//
// The check is made against models.Verify, the function `alixai serve` calls
// before the inference path starts (its own refusal cases are in
// internal/cli TestSecServePreflightRefusesUnverifiedModel and
// internal/models TestSecVerify*); this test adds the cross-registry view.
func TestSecModelHashMismatchRefused(t *testing.T) {
	root := secutil.RepoRoot(t)
	reg, err := models.LoadDir(filepath.Join(root, "models", "registry"))
	if err != nil {
		t.Fatalf("the shipped model registry does not load: %v", err)
	}
	live := activatable(reg)
	if len(live) == 0 {
		t.Fatal("the shipped registry has no activatable entry: the check would pass by looking at nothing")
	}

	content := bytes.Repeat([]byte("alixai model weights "), 2000)
	sum := sha256.Sum256(content)
	size := int64(len(content))

	for _, shipped := range live {
		t.Run(shipped.ID, func(t *testing.T) {
			// Policy of the shipped registry: an entry that can run pins its artefact.
			if shipped.Artifact.SHA256 == nil || shipped.Artifact.SizeBytes == nil {
				t.Errorf("%s (%s) can be activated but pins no sha256 and size", shipped.ID, shipped.Status)
			}
			// A certified entry without a hash cannot even be written.
			if shipped.Status == models.StatusCertified && shipped.Artifact.SHA256 == nil {
				t.Errorf("%s is certified without a sha256", shipped.ID)
			}

			// Bind the entry to a known artefact (fresh pointers: the registry is shared).
			e := shipped
			hexSum := hex.EncodeToString(sum[:])
			e.Artifact.SHA256 = &hexSum
			e.Artifact.SizeBytes = &size
			dir := t.TempDir()
			path := filepath.Join(dir, "model.gguf")
			write := func(p string, data []byte) {
				t.Helper()
				if err := os.WriteFile(p, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			write(path, content)

			// Positive control: the pinned artefact verifies, so every refusal below is the change.
			if err := models.Verify(e, path); err != nil {
				t.Fatalf("the pinned artefact was refused: %v", err)
			}

			flipped := bytes.Clone(content)
			flipped[len(flipped)/3] ^= 0x01
			downgraded := bytes.Repeat([]byte("o"), len(content))
			for name, tc := range map[string]struct {
				data   []byte
				reason error
			}{
				"one byte flipped":                 {flipped, models.ErrHashMismatch},
				"replaced by same-size other file": {downgraded, models.ErrHashMismatch},
				"truncated":                        {content[:len(content)-1], models.ErrSizeMismatch},
				"extended":                         {append(bytes.Clone(content), 0), models.ErrSizeMismatch},
				"emptied":                          {nil, models.ErrSizeMismatch},
			} {
				p := filepath.Join(dir, "changed-"+name)
				write(p, tc.data)
				requireRefused(t, models.Verify(e, p), tc.reason)
			}

			requireRefused(t, models.Verify(e, filepath.Join(dir, "absent.gguf")), models.ErrArtefactUnreadable)
			requireRefused(t, models.Verify(e, dir), models.ErrArtefactUnreadable)

			// No hash, nothing to compare against: refused, never "unverified but allowed".
			unpinned := e
			unpinned.Artifact.SHA256 = nil
			if err := models.Verify(unpinned, path); err == nil {
				t.Error("an entry without a sha256 was accepted")
			} else if contracts.CodeOf(err) != contracts.ErrUnavailable {
				t.Errorf("an unpinned entry is refused with code %q, want unavailable (%v)", contracts.CodeOf(err), err)
			}

			// A runtime version the entry was not qualified with is refused even for the right file.
			rt := models.RuntimeInfo{Runtime: e.Backend.Runtime, Version: "v0.0.0-unqualified", Backend: e.Backend.Name, BackendVersion: "v0"}
			requireRefused(t, models.Verify(e, path, models.WithRuntime(rt)), models.ErrRuntimeNotAllowed)
		})
	}

	// Rejected and retired entries never verify, whatever the file is.
	for _, e := range reg.Entries() {
		if e.Status != models.StatusRejected && e.Status != models.StatusRetired {
			continue
		}
		p := filepath.Join(t.TempDir(), "model.gguf")
		if err := os.WriteFile(p, content, 0o600); err != nil {
			t.Fatal(err)
		}
		requireRefused(t, models.Verify(e, p), models.ErrNotActivatable)
	}
}
