package trust

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

var testID = PackageIdentity{ApplicationID: testApp, PackageVersion: "1.0.0"}

// mapPackage is samplePackage as an in-memory file system.
func mapPackage() fstest.MapFS {
	m := fstest.MapFS{}
	for n, c := range samplePackage() {
		m[n] = &fstest.MapFile{Data: []byte(c)}
	}
	return m
}

// TestManifestDeterministic: the integrity manifest depends only on the
// content of the package (paths and bytes), never on creation order, file
// system, timestamps or map iteration order, so the same package signs and
// verifies identically everywhere.
//
// SEC-057: the manifest is deterministic.
func TestManifestDeterministic(t *testing.T) {
	now := t0.Add(123456789 * time.Nanosecond)

	forward := t.TempDir()
	writeFiles(t, forward, samplePackage()) // creates in sorted order
	reverse := t.TempDir()
	names := make([]string, 0)
	for n := range samplePackage() {
		names = append(names, n)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, n := range names {
		writeFiles(t, reverse, map[string]string{n: samplePackage()[n]})
	}

	a, err := BuildSignedDir(forward, testID, now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildSignedDir(reverse, testID, now)
	if err != nil {
		t.Fatal(err)
	}
	m, err := BuildSigned(mapPackage(), testID, now)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := a.Canonical()
	cb, _ := b.Canonical()
	cm, _ := m.Canonical()
	if string(ca) != string(cb) {
		t.Errorf("creation order changed the manifest:\n%s\n%s", ca, cb)
	}
	if string(ca) != string(cm) {
		t.Errorf("directory and in-memory package differ:\n%s\n%s", ca, cm)
	}

	t.Run("sorted by byte order of the whole path", func(t *testing.T) {
		paths := make([]string, len(a.Files))
		for i, f := range a.Files {
			paths[i] = f.Path
		}
		if !sort.StringsAreSorted(paths) {
			t.Errorf("files are not sorted: %v", paths)
		}
		// "a.b.en.yaml" < "a/b.en.yaml" by bytes ('.' < '/'), although a
		// directory walk visits directory "a" before file "a.b.en.yaml".
		iDot, iSlash := indexOf(paths, "conversation-contracts/a.b.en.yaml"), indexOf(paths, "conversation-contracts/a/b.en.yaml")
		if iDot < 0 || iSlash < 0 || iDot > iSlash {
			t.Errorf("a.b.en.yaml (%d) must come before a/b.en.yaml (%d)", iDot, iSlash)
		}
		if len(paths) != len(samplePackage()) {
			t.Errorf("%d files listed, want %d", len(paths), len(samplePackage()))
		}
	})

	t.Run("created_at is the injected clock truncated to seconds", func(t *testing.T) {
		if a.CreatedAt != "2026-10-02T09:00:00Z" {
			t.Errorf("created_at = %q", a.CreatedAt)
		}
	})

	t.Run("modification times do not matter, content does", func(t *testing.T) {
		old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
		if err := os.Chtimes(filepath.Join(forward, "alixai.yaml"), old, old); err != nil {
			t.Fatal(err)
		}
		again, err := BuildSignedDir(forward, testID, now)
		if err != nil {
			t.Fatal(err)
		}
		if c, _ := again.Canonical(); string(c) != string(ca) {
			t.Error("a changed mtime changed the manifest")
		}
		writeFiles(t, forward, map[string]string{"alixai.yaml": samplePackage()["alixai.yaml"] + "# edit\n"})
		changed, err := BuildSignedDir(forward, testID, now)
		if err != nil {
			t.Fatal(err)
		}
		if c, _ := changed.Canonical(); string(c) == string(ca) {
			t.Error("a changed file left the manifest unchanged")
		}
	})

	t.Run("signing twice is byte-identical, and signatures/ is not listed", func(t *testing.T) {
		key := newKey(t, "k1")
		sign := func() []byte {
			if _, err := SignPackageDir(reverse, SignOptions{
				PackageIdentity: testID, Keys: []SigningKey{key.signing()}, Now: now, ExpiresAt: now.Add(time.Hour),
			}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(reverse, filepath.FromSlash(SignatureManifestPath)))
			if err != nil {
				t.Fatal(err)
			}
			return data
		}
		first, second := sign(), sign() // the second run sees signatures/ already there
		if string(first) != string(second) {
			t.Errorf("re-signing changed the file:\n%s\n%s", first, second)
		}
		sm, err := ParseSignatureManifest(first)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range sm.Signed.Files {
			if strings.HasPrefix(f.Path, SignaturesDir) {
				t.Errorf("signatures/ is listed: %s", f.Path)
			}
		}
		if c, _ := sm.Signed.Canonical(); string(c) != string(ca) {
			t.Errorf("signing a directory that already has signatures/ changed the file list")
		}
	})
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

// TestManifestDetectsTamper: after signing, any difference between the package
// and its signed manifest fails verification, whether or not the signature
// itself is valid. A file that is in the package but not in the manifest is
// tampering just like a modified one.
//
// SEC-057: one changed or one extra file cannot pass.
func TestManifestDetectsTamper(t *testing.T) {
	const loanPath = "documentation/policies/loan-period.md"
	cases := []struct {
		name     string
		mutate   func(t *testing.T, dir string)
		wantPath string
		wantKind DiffKind
	}{
		{"one byte changed, same size", func(t *testing.T, dir string) {
			writeFiles(t, dir, map[string]string{loanPath: "Loans last 99 days.\n"})
		}, loanPath, DiffModified},
		{"byte appended", func(t *testing.T, dir string) {
			f, err := os.OpenFile(filepath.Join(dir, filepath.FromSlash(loanPath)), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteString("\n"); err != nil {
				t.Fatal(err)
			}
		}, loanPath, DiffModified},
		{"truncated to empty", func(t *testing.T, dir string) {
			writeFiles(t, dir, map[string]string{loanPath: ""})
		}, loanPath, DiffModified},
		{"the package manifest edited", func(t *testing.T, dir string) {
			writeFiles(t, dir, map[string]string{"alixai.yaml": samplePackage()["alixai.yaml"] + "network:\n  egress: [evil.example]\n"})
		}, "alixai.yaml", DiffModified},
		{"extra file in a subdirectory", func(t *testing.T, dir string) {
			writeFiles(t, dir, map[string]string{"capabilities/evil.yaml": "id: evil\n"})
		}, "capabilities/evil.yaml", DiffAdded},
		{"extra file at the root", func(t *testing.T, dir string) {
			writeFiles(t, dir, map[string]string{"extra.txt": "x"})
		}, "extra.txt", DiffAdded},
		{"extra file in a new directory", func(t *testing.T, dir string) {
			writeFiles(t, dir, map[string]string{"new/deep/dir/payload.bin": "x"})
		}, "new/deep/dir/payload.bin", DiffAdded},
		{"empty extra file", func(t *testing.T, dir string) {
			writeFiles(t, dir, map[string]string{"empty.txt": ""})
		}, "empty.txt", DiffAdded},
		{"hidden file added", func(t *testing.T, dir string) {
			writeFiles(t, dir, map[string]string{".env": "SECRET=1"})
		}, ".env", DiffUnsafe},
		{"hidden directory added", func(t *testing.T, dir string) {
			writeFiles(t, dir, map[string]string{".git/config": "x"})
		}, ".git", DiffUnsafe},
		{"unlisted file smuggled into signatures/", func(t *testing.T, dir string) {
			writeFiles(t, dir, map[string]string{"signatures/payload.yaml": "id: evil\n"})
		}, "signatures/payload.yaml", DiffUnsafe},
		{"file deleted", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, filepath.FromSlash(loanPath))); err != nil {
				t.Fatal(err)
			}
		}, loanPath, DiffMissing},
		{"file replaced by a directory", func(t *testing.T, dir string) {
			p := filepath.Join(dir, filepath.FromSlash(loanPath))
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}, loanPath, DiffMissing},
		{"file renamed", func(t *testing.T, dir string) {
			from := filepath.Join(dir, filepath.FromSlash(loanPath))
			if err := os.Rename(from, filepath.Join(dir, "documentation", "policies", "loan-period-2.md")); err != nil {
				t.Fatal(err)
			}
		}, loanPath, DiffMissing},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.mustVerify() // the untouched package passes, so the failure below is the mutation's
			tt.mutate(t, f.dir)

			_, err := f.verify()
			wantFail(t, err, contracts.ErrUnauthenticated, ErrTampered)
			var te *TamperError
			if !errors.As(err, &te) {
				t.Fatalf("error is not a *TamperError: %v", err)
			}
			if !hasDiff(te.Differences, tt.wantPath, tt.wantKind) {
				t.Errorf("differences %+v lack %s %q", te.Differences, tt.wantKind, tt.wantPath)
			}
		})
	}

	t.Run("manifest edited to match the tampered file still fails: the signature covers the digests", func(t *testing.T) {
		f := newFixture(t)
		f.mustVerify()
		writeFiles(t, f.dir, map[string]string{loanPath: "Loans last 99 days.\n"})
		changed, err := hashFile(os.DirFS(f.dir), loanPath)
		if err != nil {
			t.Fatal(err)
		}
		rewriteManifest(t, f.dir, func(sm *SignatureManifest) {
			for i := range sm.Signed.Files {
				if sm.Signed.Files[i].Path == loanPath {
					sm.Signed.Files[i] = changed
				}
			}
		})
		_, err = f.verify()
		wantFail(t, err, contracts.ErrUnauthenticated, ErrBadSignature)
	})

	t.Run("manifest extended with the extra file still fails", func(t *testing.T) {
		f := newFixture(t)
		writeFiles(t, f.dir, map[string]string{"zz-extra.txt": "x"})
		extra, err := hashFile(os.DirFS(f.dir), "zz-extra.txt")
		if err != nil {
			t.Fatal(err)
		}
		rewriteManifest(t, f.dir, func(sm *SignatureManifest) { sm.Signed.Files = append(sm.Signed.Files, extra) })
		_, err = f.verify()
		wantFail(t, err, contracts.ErrUnauthenticated, ErrBadSignature)
	})

	t.Run("manifest entry removed still fails", func(t *testing.T) {
		f := newFixture(t)
		rewriteManifest(t, f.dir, func(sm *SignatureManifest) { sm.Signed.Files = sm.Signed.Files[:len(sm.Signed.Files)-1] })
		_, err := f.verify()
		wantFail(t, err, contracts.ErrUnauthenticated, ErrBadSignature)
	})

	t.Run("a package with no signature manifest is unsigned", func(t *testing.T) {
		f := newFixture(t)
		if err := os.RemoveAll(filepath.Join(f.dir, SignaturesDir)); err != nil {
			t.Fatal(err)
		}
		_, err := f.verify()
		wantFail(t, err, contracts.ErrUnauthenticated, ErrUnsigned)
	})

	t.Run("a symbolic link replacing a listed file", func(t *testing.T) {
		f := newFixture(t)
		p := filepath.Join(f.dir, filepath.FromSlash(loanPath))
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(f.dir, "alixai.yaml"), p); err != nil {
			t.Skipf("cannot create symbolic links here: %v", err)
		}
		_, err := f.verify()
		wantFail(t, err, contracts.ErrUnauthenticated, ErrTampered)
	})
}

func hasDiff(diffs []Difference, path string, kind DiffKind) bool {
	for _, d := range diffs {
		if d.Path == path && d.Kind == kind {
			return true
		}
	}
	return false
}

// rewriteManifest edits the signature manifest on disk without re-signing.
func rewriteManifest(t *testing.T, dir string, edit func(*SignatureManifest)) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(SignatureManifestPath))
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	sm, err := ParseSignatureManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	edit(sm)
	out, err := marshalUnchecked(sm)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestManifestRejectsPathTraversalAndSymlinks: the builder lists only regular
// files inside the root and refuses everything else by name, and a signature
// manifest cannot name a path outside the safe grammar: such a manifest is
// rejected before any file is read.
//
// SEC-059: manifest building refuses symlinks and escaping paths.
func TestManifestRejectsPathTraversalAndSymlinks(t *testing.T) {
	t.Run("builder refuses entries that are not directories or regular files", func(t *testing.T) {
		for name, mode := range map[string]fs.FileMode{
			"symbolic link": fs.ModeSymlink,
			"named pipe":    fs.ModeNamedPipe,
			"device":        fs.ModeDevice,
			"char device":   fs.ModeDevice | fs.ModeCharDevice,
			"socket":        fs.ModeSocket,
			"irregular":     fs.ModeIrregular,
		} {
			t.Run(name, func(t *testing.T) {
				m := mapPackage()
				m["capabilities/odd.yaml"] = &fstest.MapFile{Data: []byte("alixai.yaml"), Mode: mode}
				_, err := BuildSigned(m, testID, t0)
				wantFail(t, err, contracts.ErrValidationFailed, ErrUnsafePackage)
				if err != nil && !strings.Contains(err.Error(), "capabilities/odd.yaml") {
					t.Errorf("error does not name the entry: %v", err)
				}
			})
		}
	})

	t.Run("builder refuses paths outside the safe grammar and skips nothing", func(t *testing.T) {
		for _, bad := range []string{
			`a\b.txt`, "sp ace.txt", ".hidden", ".git/config", "dir/.DS_Store", "-rf", "caf\u00e9.txt",
			"a:b.txt", "tab\there.txt", "star*.txt", "q?.txt", "semi;colon", "dollar$.txt", "~tmp", "x/.y/z.txt",
			strings.Repeat("a", 241),
		} {
			m := mapPackage()
			m[bad] = &fstest.MapFile{Data: []byte("x")}
			_, err := BuildSigned(m, testID, t0)
			if err == nil {
				t.Errorf("BuildSigned accepted %q", bad)
				continue
			}
			wantFail(t, err, contracts.ErrValidationFailed, ErrUnsafePackage)
		}
	})

	t.Run("builder refuses two paths that differ only by case", func(t *testing.T) {
		m := mapPackage()
		m["Capabilities/catalog-find-title.yaml"] = &fstest.MapFile{Data: []byte("x")}
		_, err := BuildSigned(m, testID, t0)
		wantFail(t, err, contracts.ErrValidationFailed, ErrUnsafePackage)
	})

	t.Run("builder allows only the signature manifest in signatures/", func(t *testing.T) {
		ok := mapPackage()
		ok[SignatureManifestPath] = &fstest.MapFile{Data: []byte("{}")}
		got, err := BuildSigned(ok, testID, t0)
		if err != nil {
			t.Fatalf("signature manifest in signatures/ must be allowed (and excluded): %v", err)
		}
		for _, f := range got.Files {
			if strings.HasPrefix(f.Path, SignaturesDir) {
				t.Errorf("listed %s", f.Path)
			}
		}
		for name, extra := range map[string]*fstest.MapFile{
			"signatures/extra.yaml":              {Data: []byte("x")},
			"signatures/sub/signature-manifest":  {Data: []byte("x")},
			"signatures/signature-manifest.json": {Data: []byte("x"), Mode: fs.ModeSymlink},
			"Signatures/signature-manifest.json": {Data: []byte("x")},
			"SIGNATURES/x":                       {Data: []byte("x")},
		} {
			m := mapPackage()
			m[name] = extra
			if _, err := BuildSigned(m, testID, t0); err == nil {
				t.Errorf("BuildSigned accepted %s", name)
			} else {
				wantFail(t, err, contracts.ErrValidationFailed, ErrUnsafePackage)
			}
		}
		file := mapPackage()
		file["signatures"] = &fstest.MapFile{Data: []byte("a file named signatures")}
		if _, err := BuildSigned(file, testID, t0); err == nil {
			t.Error("a file named signatures was accepted")
		}
	})

	t.Run("builder needs the package manifest", func(t *testing.T) {
		m := mapPackage()
		delete(m, ManifestFile)
		_, err := BuildSigned(m, testID, t0)
		wantFail(t, err, contracts.ErrValidationFailed)
	})

	t.Run("a symbolic link on disk is refused, to a file, a directory or a file outside the package", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "pkg")
		writeFiles(t, root, samplePackage())
		writeFiles(t, parent, map[string]string{"outside.txt": "secret", "outside-dir/f.txt": "x"})
		targets := map[string]string{
			"link-to-file.txt":    filepath.Join(root, "alixai.yaml"),
			"link-to-dir":         filepath.Join(root, "capabilities"),
			"link-to-outside.txt": filepath.Join(parent, "outside.txt"),
			"link-to-outside-dir": filepath.Join(parent, "outside-dir"),
			"link-relative.txt":   filepath.Join("..", "outside.txt"),
		}
		made := 0
		for name, target := range targets {
			link := filepath.Join(root, name)
			if err := os.Symlink(target, link); err != nil {
				t.Skipf("cannot create symbolic links here: %v", err)
			}
			made++
			_, err := BuildSignedDir(root, testID, t0)
			wantFail(t, err, contracts.ErrValidationFailed, ErrUnsafePackage)
			if err != nil && !strings.Contains(err.Error(), name) {
				t.Errorf("%s: error does not name the link: %v", name, err)
			}
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
		}
		if made == 0 {
			t.Skip("no link made")
		}
		if _, err := BuildSignedDir(root, testID, t0); err != nil {
			t.Errorf("package is clean again but the build fails: %v", err)
		}
	})

	t.Run("a symbolic link as the package root is refused", func(t *testing.T) {
		parent := t.TempDir()
		real := filepath.Join(parent, "real")
		writeFiles(t, real, samplePackage())
		link := filepath.Join(parent, "link")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("cannot create symbolic links here: %v", err)
		}
		_, err := BuildSignedDir(link, testID, t0)
		wantFail(t, err, contracts.ErrValidationFailed, ErrUnsafePackage)
	})

	t.Run("a directory junction on Windows is refused", func(t *testing.T) {
		if runtime.GOOS != "windows" {
			t.Skip("junctions exist on Windows only")
		}
		parent := t.TempDir()
		root := filepath.Join(parent, "pkg")
		writeFiles(t, root, samplePackage())
		writeFiles(t, parent, map[string]string{"outside-dir/f.txt": "x"})
		out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(root, "junction"), filepath.Join(parent, "outside-dir")).CombinedOutput()
		if err != nil {
			t.Skipf("cannot create a junction here: %v: %s", err, out)
		}
		_, err = BuildSignedDir(root, testID, t0)
		wantFail(t, err, contracts.ErrValidationFailed, ErrUnsafePackage)
	})

	t.Run("a package root that is not a directory is refused", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "file.txt")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := BuildSignedDir(p, testID, t0); err == nil {
			t.Error("a regular file was accepted as a package root")
		}
		if _, err := BuildSignedDir(filepath.Join(t.TempDir(), "missing"), testID, t0); err == nil {
			t.Error("a missing directory was accepted as a package root")
		}
	})

	t.Run("a manifest naming an escaping path is rejected before any file is read", func(t *testing.T) {
		f := newFixture(t)
		parent := filepath.Dir(f.dir)
		writeFiles(t, parent, map[string]string{"outside.txt": "secret"})
		outside, err := hashFile(os.DirFS(parent), "outside.txt")
		if err != nil {
			t.Fatal(err)
		}

		for _, bad := range []string{
			"../outside.txt", "../../etc/passwd", "/etc/passwd", "/abs.txt", `a\b`, `..\outside.txt`, "C:/x", "c:\\x", "C:x",
			".hidden", "a/.hidden", "a//b", "a/./b", "a/../b", "./a", "a/", "a/b/", "", ".", "..", "-x",
			"signatures", "signatures/x.json", "signatures/signature-manifest.json", "sp ace", "caf\u00e9", strings.Repeat("a", 241),
		} {
			evil := Signed{
				ApplicationID: testApp, PackageVersion: "1.0.0", CreatedAt: "2026-10-02T09:00:00Z", HashAlgorithm: HashSHA256,
				Files: []FileEntry{{Path: bad, SHA256: outside.SHA256, Size: outside.Size}, f.sm.Signed.Files[0]},
			}
			sort.Slice(evil.Files, func(i, j int) bool { return evil.Files[i].Path < evil.Files[j].Path })
			sig, err := SignPayload(evil, f.key.id, f.key.priv, t0, t0.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			// Signed by a registered, trusted key: the grammar is the only defence.
			sm := &SignatureManifest{Format: FormatV1, Signed: evil, Signatures: []Signature{sig}}
			raw, err := marshalUnchecked(sm)
			if err != nil {
				t.Fatal(err)
			}
			rec := &recordingFS{FS: fstest.MapFS{
				SignatureManifestPath: &fstest.MapFile{Data: raw},
				"alixai.yaml":         &fstest.MapFile{Data: []byte("x")},
			}}
			_, err = f.v.VerifyPackage(context.Background(), rec, f.opts)
			if err == nil {
				t.Errorf("manifest listing %q verified", bad)
				continue
			}
			if contracts.CodeOf(err) != contracts.ErrSchemaInvalid || !errors.Is(err, ErrMalformedManifest) {
				t.Errorf("manifest listing %q: %v (want a malformed-manifest failure)", bad, err)
			}
			for _, name := range rec.opened {
				if name != SignatureManifestPath {
					t.Errorf("manifest listing %q: verification opened %q before rejecting it", bad, name)
				}
			}
		}
	})
}

// swapFS lists a file as regular but opens something that is not: a stand-in
// for a path swapped for a link between the directory scan and the hashing.
type swapFS struct {
	fstest.MapFS
	swapped string
}

func (s swapFS) Open(name string) (fs.File, error) {
	f, err := s.MapFS.Open(name)
	if err != nil || name != s.swapped {
		return f, err
	}
	return modeFile{File: f}, nil
}

type modeFile struct{ fs.File }

func (m modeFile) Stat() (fs.FileInfo, error) {
	fi, err := m.File.Stat()
	return linkInfo{fi}, err
}

type linkInfo struct{ fs.FileInfo }

func (linkInfo) Mode() fs.FileMode { return fs.ModeSymlink }

// TestVerifyRefusesAFileSwappedAfterTheScan: the object opened for hashing must
// again be a regular file; a path that became a link in between is tampering.
func TestVerifyRefusesAFileSwappedAfterTheScan(t *testing.T) {
	f := newFixture(t)
	snapshot := mapPackageWithSignature(t, f.dir)
	if _, err := f.v.VerifyPackage(context.Background(), snapshot, f.opts); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	_, err := f.v.VerifyPackage(context.Background(), swapFS{MapFS: snapshot, swapped: "alixai.yaml"}, f.opts)
	wantFail(t, err, contracts.ErrUnauthenticated, ErrTampered)
	var te *TamperError
	if !errors.As(err, &te) || !hasDiff(te.Differences, "alixai.yaml", DiffUnsafe) {
		t.Errorf("differences = %+v", te)
	}
}
