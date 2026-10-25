package packs_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/voice/packs"
	"github.com/buxee/alix-ai/internal/voice/profile"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// TestInstallRequiresExplicitApproval: an install without the operator's
// approval of that very pack is refused before anything is read or written; with
// it the pack is installed, and still not active.
func TestInstallRequiresExplicitApproval(t *testing.T) {
	en := newTestPack("en", "en", "stt", "tts", "vad")
	it := newTestPack("it", "it", "stt")
	st, root := newTestStore(t, en, it)
	from := fetched(t, en)

	for _, tt := range []struct {
		name     string
		approval packs.Approval
		from     string
	}{
		{"no approval", packs.Approval{}, from},
		{"approval of another pack", approve("it"), from},
		{"no approval, files not even there", packs.Approval{}, filepath.Join(t.TempDir(), "missing")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res, err := st.Install(t.Context(), packs.InstallRequest{PackID: "en", From: tt.from, Approval: tt.approval})
			wantCode(t, err, contracts.ErrConfirmationRequired)
			if names := entries(t, root); len(names) != 0 {
				t.Fatalf("a refused install left %v on disk", names)
			}
			if res.Plan.ID != "en" || len(res.Plan.Models) != 3 || res.Plan.DiskBytes == 0 {
				t.Errorf("the refusal does not show what would be installed: %+v", res.Plan)
			}
			for _, m := range res.Plan.Models {
				if m.Licence.SPDX == "" || len(m.Files) == 0 {
					t.Errorf("plan model %s shows no licence or files", m.Role)
				}
			}
		})
	}

	if _, err := st.Install(t.Context(), packs.InstallRequest{PackID: "nope", From: from, Approval: approve("nope")}); err == nil {
		t.Error("a pack that is not in the approved set was installed")
	} else {
		wantCode(t, err, contracts.ErrValidationFailed)
	}

	res, err := st.Install(t.Context(), packs.InstallRequest{PackID: "en", From: from, Approval: approve("en")})
	if err != nil {
		t.Fatalf("approved install: %v", err)
	}
	if res.Replaced {
		t.Error("a first install claims to have replaced one")
	}
	installed, err := st.Installed(t.Context())
	if err != nil || len(installed) != 1 || installed[0].Pack.ID != "en" {
		t.Fatalf("Installed = %+v, %v", installed, err)
	}
	if id, active, err := st.ActiveID(); err != nil || active {
		t.Errorf("an install activated %q (%v): activation is a separate step", id, err)
	}
	if err := st.Verify(t.Context(), "en"); err != nil {
		t.Errorf("a fresh install does not verify: %v", err)
	}
	// Only the pack's files and its record are in the directory: no staging left.
	if diff := cmp.Diff([]string{"en"}, entries(t, root)); diff != "" {
		t.Errorf("store directory (-want +got):\n%s", diff)
	}
}

// TestHashMismatchRejected: a file that is not the pinned one is refused at
// install (same size or not) and leaves nothing behind; a pack changed after
// install, or installed from another manifest, cannot be activated or loaded.
func TestHashMismatchRejected(t *testing.T) {
	en := newTestPack("en", "en", "stt", "tts")
	const ttsName = "tts-en.bin"

	t.Run("same size, wrong bytes", func(t *testing.T) {
		st, root := newTestStore(t, en)
		from := fetched(t, en)
		tampered := append([]byte(nil), en.files[ttsName]...)
		tampered[0] ^= 0xff
		writeFile(t, filepath.Join(from, ttsName), tampered)
		_, err := st.Install(t.Context(), packs.InstallRequest{PackID: "en", From: from, Approval: approve("en")})
		wantCode(t, err, contracts.ErrUnavailable)
		if !errors.Is(err, packs.ErrHashMismatch) {
			t.Errorf("error %v is not ErrHashMismatch", err)
		}
		if names := entries(t, root); len(names) != 0 {
			t.Errorf("a rejected install left %v on disk (stt, which was fine, included)", names)
		}
	})
	t.Run("wrong size", func(t *testing.T) {
		st, root := newTestStore(t, en)
		from := fetched(t, en)
		writeFile(t, filepath.Join(from, ttsName), append(append([]byte(nil), en.files[ttsName]...), 'x'))
		_, err := st.Install(t.Context(), packs.InstallRequest{PackID: "en", From: from, Approval: approve("en")})
		wantCode(t, err, contracts.ErrUnavailable)
		if !errors.Is(err, packs.ErrSizeMismatch) {
			t.Errorf("error %v is not ErrSizeMismatch", err)
		}
		if names := entries(t, root); len(names) != 0 {
			t.Errorf("a rejected install left %v on disk", names)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		st, root := newTestStore(t, en)
		from := fetched(t, en)
		if err := os.Remove(filepath.Join(from, ttsName)); err != nil {
			t.Fatal(err)
		}
		_, err := st.Install(t.Context(), packs.InstallRequest{PackID: "en", From: from, Approval: approve("en")})
		wantCode(t, err, contracts.ErrUnavailable)
		if !errors.Is(err, packs.ErrMissingFile) {
			t.Errorf("error %v is not ErrMissingFile", err)
		}
		if names := entries(t, root); len(names) != 0 {
			t.Errorf("a rejected install left %v on disk", names)
		}
	})
	t.Run("a failed reinstall keeps the pack that was there", func(t *testing.T) {
		st, _ := newTestStore(t, en)
		mustInstall(t, st, en)
		from := fetched(t, en)
		writeFile(t, filepath.Join(from, ttsName), make([]byte, len(en.files[ttsName])))
		if _, err := st.Install(t.Context(), packs.InstallRequest{PackID: "en", From: from, Approval: approve("en")}); err == nil {
			t.Fatal("a tampered reinstall succeeded")
		}
		if err := st.Verify(t.Context(), "en"); err != nil {
			t.Errorf("the earlier install was damaged by the failed one: %v", err)
		}
	})

	t.Run("changed after install", func(t *testing.T) {
		st, root := newTestStore(t, en)
		mustInstall(t, st, en)
		tampered := append([]byte(nil), en.files[ttsName]...)
		tampered[1] ^= 0xff
		writeFile(t, filepath.Join(root, "en", ttsName), tampered)

		_, err := st.Activate(t.Context(), "en")
		wantCode(t, err, contracts.ErrUnavailable)
		if !errors.Is(err, packs.ErrHashMismatch) {
			t.Errorf("activation error %v is not ErrHashMismatch", err)
		}
		if id, active, _ := st.ActiveID(); active {
			t.Errorf("a pack that failed verification became active: %q", id)
		}
		wantCode(t, st.Loader().Load(t.Context(), profile.Pack{ID: "en"}), contracts.ErrUnavailable)
	})
	t.Run("a file the manifest does not list", func(t *testing.T) {
		st, root := newTestStore(t, en)
		mustInstall(t, st, en)
		writeFile(t, filepath.Join(root, "en", "planted.onnx"), []byte("x"))
		err := st.Verify(t.Context(), "en")
		wantCode(t, err, contracts.ErrUnavailable)
		if !errors.Is(err, packs.ErrUnexpectedFile) {
			t.Errorf("error %v is not ErrUnexpectedFile", err)
		}
	})
	t.Run("missing after install", func(t *testing.T) {
		st, root := newTestStore(t, en)
		mustInstall(t, st, en)
		if err := os.Remove(filepath.Join(root, "en", ttsName)); err != nil {
			t.Fatal(err)
		}
		if err := st.Verify(t.Context(), "en"); !errors.Is(err, packs.ErrMissingFile) {
			t.Errorf("error %v is not ErrMissingFile", err)
		}
	})
	t.Run("installed from other pins", func(t *testing.T) {
		old, root := newTestStore(t, en)
		mustInstall(t, old, en)
		storeFor := func(tp testPack) *packs.Store {
			s, err := packs.NewStore(root, manifestSet(t, tp), clock.NewFake(testNow))
			if err != nil {
				t.Fatal(err)
			}
			return s
		}
		// Measuring or annotating a pack (quality, resources, notes, name) does not
		// strand the copies already installed: the files are still the pinned ones.
		annotated := en
		annotated.yaml = strings.Replace(en.yaml, "notes: []\n", "notes: [\"revised\"]\n", 1)
		annotated.yaml = strings.Replace(annotated.yaml, "stt_wer: null", "stt_wer: 0.12", 1)
		annotated.yaml = strings.Replace(annotated.yaml, "steady_rss_mb: null", "steady_rss_mb: 300", 1)
		annotated.yaml = strings.Replace(annotated.yaml, `name: "Test pack en"`, `name: "Renamed"`, 1)
		if annotated.yaml == en.yaml {
			t.Fatal("the annotation did not change the manifest")
		}
		if err := storeFor(annotated).Verify(t.Context(), "en"); err != nil {
			t.Errorf("an annotation of the manifest made the installed pack stale: %v", err)
		}
		// What the install cached or pinned does: a new version, another role set
		// or another file, with the files on disk untouched, so it is the staleness
		// check that refuses and not a hash check.
		bumped := en
		bumped.yaml = strings.Replace(en.yaml, `version: "1"`, `version: "2"`, 1)
		moreRoles := newTestPack("en", "en", "stt", "tts", "vad")
		repinned := en
		repinned.yaml = strings.Replace(en.yaml, "size_bytes: ", "size_bytes: 1", 1) // another pin; the size no longer matches disk_bytes either
		repinned.yaml = strings.Replace(repinned.yaml, "disk_bytes: ", "disk_bytes: 1", 1)
		for name, tp := range map[string]testPack{"a new version": bumped, "another role set": moreRoles, "another file pin": repinned} {
			if err := storeFor(tp).Verify(t.Context(), "en"); !errors.Is(err, packs.ErrStaleInstall) {
				t.Errorf("%s: error %v is not ErrStaleInstall", name, err)
			}
		}
	})
}

// TestOnlyOneActivePack: activation is explicit and one pack at a time, across
// processes (a second store over the same directory), through the persisted
// marker and through the profile's own registry.
func TestOnlyOneActivePack(t *testing.T) {
	en := newTestPack("en", "en", "stt", "tts", "vad")
	it := newTestPack("it", "it", "stt")
	st, root := newTestStore(t, en, it)
	mustInstall(t, st, en)
	mustInstall(t, st, it)

	if _, err := st.Activate(t.Context(), "en"); err != nil {
		t.Fatal(err)
	}
	_, err := st.Activate(t.Context(), "it")
	wantCode(t, err, contracts.ErrConflict)
	if cur, _, _ := st.ActiveID(); cur != "en" {
		t.Errorf("a refused activation changed the active pack to %q", cur)
	}
	if p, err := st.Activate(t.Context(), "en"); err != nil || p.ID != "en" {
		t.Errorf("activating the active pack again = %+v, %v", p, err)
	}

	// Another process: a fresh store over the same directory sees the marker.
	other, err := packs.NewStore(root, manifestSet(t, en, it), clock.NewFake(testNow))
	if err != nil {
		t.Fatal(err)
	}
	if cur, ok, err := other.ActiveID(); err != nil || !ok || cur != "en" {
		t.Fatalf("a second process sees the active pack as %q, %v, %v", cur, ok, err)
	}
	_, err = other.Activate(t.Context(), "it")
	wantCode(t, err, contracts.ErrConflict)
	if _, err := st.Install(t.Context(), packs.InstallRequest{PackID: "en", From: fetched(t, en), Approval: approve("en")}); err == nil {
		t.Error("the active pack was reinstalled underneath itself")
	} else {
		wantCode(t, err, contracts.ErrConflict)
	}

	if err := st.Deactivate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := st.Deactivate(t.Context()); err != nil {
		t.Errorf("deactivating with none active: %v", err)
	}
	if p, err := other.Activate(t.Context(), "it"); err != nil || p.ID != "it" || len(p.Roles) != 1 || p.Roles[0] != providers.RoleSTT {
		t.Errorf("activating the other pack after deactivation = %+v, %v", p, err)
	}

	t.Run("a pack that is not installed", func(t *testing.T) {
		if err := st.Deactivate(t.Context()); err != nil {
			t.Fatal(err)
		}
		_, err := st.Activate(t.Context(), "fr")
		wantCode(t, err, contracts.ErrValidationFailed)
		_, err = st.Activate(t.Context(), "EN")
		wantCode(t, err, contracts.ErrValidationFailed)
	})

	t.Run("an unreadable marker is not taken for none", func(t *testing.T) {
		writeFile(t, filepath.Join(root, ".active"), []byte("not json"))
		_, err := st.Activate(t.Context(), "en")
		wantCode(t, err, contracts.ErrSchemaInvalid)
		if err := st.Deactivate(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Activate(t.Context(), "en"); err != nil {
			t.Errorf("deactivate did not clear the bad marker: %v", err)
		}
		_ = st.Deactivate(t.Context())
	})

	t.Run("concurrent activations: one wins", func(t *testing.T) {
		var wg sync.WaitGroup
		results := make([]error, 8)
		for i := range results {
			id := []string{"en", "it"}[i%2]
			wg.Add(1)
			go func() {
				defer wg.Done()
				s, err := packs.NewStore(root, manifestSet(t, en, it), clock.NewFake(testNow))
				if err == nil {
					_, err = s.Activate(t.Context(), id)
				}
				results[i] = err
			}()
		}
		wg.Wait()
		cur, ok, err := st.ActiveID()
		if err != nil || !ok {
			t.Fatalf("no pack active after the race: %v", err)
		}
		for i, err := range results {
			id := []string{"en", "it"}[i%2]
			if id == cur && err != nil {
				t.Errorf("activation of the winner %s failed: %v", id, err)
			}
			if id != cur && !contracts.HasCode(err, contracts.ErrConflict) {
				t.Errorf("activation of %s while %s won = %v, want conflict", id, cur, err)
			}
		}
		_ = st.Deactivate(t.Context())
	})

	t.Run("the profile's registry over this catalog and loader", func(t *testing.T) {
		reg, err := profile.NewPacks(st.Catalog(), st.Loader())
		if err != nil {
			t.Fatal(err)
		}
		installed, err := reg.Installed(t.Context())
		if err != nil || len(installed) != 2 || installed[0].ID != "en" || installed[1].ID != "it" {
			t.Fatalf("Installed = %+v, %v", installed, err)
		}
		if _, err := reg.Activate(t.Context(), "en"); err != nil {
			t.Fatal(err)
		}
		_, err = reg.Activate(t.Context(), "it")
		wantCode(t, err, contracts.ErrConflict)
		if err := reg.Deactivate(t.Context()); err != nil {
			t.Fatal(err)
		}
		// A pack whose files no longer verify cannot be activated through it.
		writeFile(t, filepath.Join(root, "it", "stt-it.bin"), []byte("tampered"))
		_, err = reg.Activate(t.Context(), "it")
		wantCode(t, err, contracts.ErrUnavailable)
		if _, ok := reg.Active(); ok {
			t.Error("a pack that failed verification is active")
		}
	})
}

// The catalog lists from the install records alone, so listing never loads or
// hashes a model (profile.PackCatalog).
func TestCatalogReadsMetadataOnly(t *testing.T) {
	en := newTestPack("en", "en", "stt", "tts")
	st, root := newTestStore(t, en)
	if got, err := st.Catalog().List(t.Context()); err != nil || len(got) != 0 {
		t.Fatalf("empty store: %v, %v", got, err)
	}
	mustInstall(t, st, en)
	writeFile(t, filepath.Join(root, "en", "tts-en.bin"), []byte("tampered"))
	got, err := st.Catalog().List(t.Context())
	if err != nil {
		t.Fatalf("List hashed or opened a model file: %v", err)
	}
	want := []profile.Pack{{ID: "en", Language: "en", Version: "1", Roles: []providers.Role{providers.RoleSTT, providers.RoleTTS}}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("List (-want +got):\n%s", diff)
	}
	// A damaged record is an error, not a pack that is quietly absent.
	writeFile(t, filepath.Join(root, "en", ".installed.json"), []byte("{"))
	_, err = st.Catalog().List(t.Context())
	wantCode(t, err, contracts.ErrSchemaInvalid)
}

func TestReinstallReplacesTheInstalledPack(t *testing.T) {
	en := newTestPack("en", "en", "stt")
	st, root := newTestStore(t, en)
	mustInstall(t, st, en)
	res, err := st.Install(t.Context(), packs.InstallRequest{PackID: "en", From: fetched(t, en), Approval: approve("en")})
	if err != nil || !res.Replaced {
		t.Fatalf("reinstall = %+v, %v", res, err)
	}
	if diff := cmp.Diff([]string{"en"}, entries(t, root)); diff != "" {
		t.Errorf("store directory (-want +got):\n%s", diff)
	}
	if err := st.Verify(t.Context(), "en"); err != nil {
		t.Error(err)
	}
}

// SEC-081: the store's directories are owner-only and its files 0600. Windows
// file modes carry no such bits, so the check runs elsewhere.
func TestSecInstalledPackIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes carry no group or other bits on Windows; the data directory ACL is the operator's")
	}
	en := newTestPack("en", "en", "stt")
	st, root := newTestStore(t, en)
	mustInstall(t, st, en)
	if _, err := st.Activate(t.Context(), "en"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{root, filepath.Join(root, "en"), filepath.Join(root, "en", "stt-en.bin"), filepath.Join(root, "en", ".installed.json"), filepath.Join(root, ".active")} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s has mode %o: group or others can access it", p, fi.Mode().Perm())
		}
	}
}

func writeFile(t testing.TB, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
