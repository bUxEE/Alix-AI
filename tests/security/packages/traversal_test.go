package packagessec

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/trust"
)

// Check IDs the loader reports (internal/apppkg/checks.go).
const (
	checkUnsafeEntry = "package.unsafe_entry"
	checkLimit       = "package.limit_exceeded"
	checkRatio       = "archive.compression_ratio"
)

// naiveDir is where the control extractor unpacks, four levels below base like the
// directory an installer would write to (data/packages/<app>/<hash>), so that the
// same relative traversal names reach the sentinel directory.
const naiveDir = "data/packages/" + appID + "/naive"

// ---- the hostile archives ----------------------------------------------------

// hostile is one archive entry (or a few) that must never be honoured.
type hostile struct {
	name string
	// entries are the hostile entries, aimed at the rig's sentinel.
	entries func(r *rig) []tarEntry
	// escapes is true when an extractor that joins the name onto its directory and
	// writes (the control) puts something outside that directory.
	escapes bool
}

func hostileCases() []hostile {
	one := func(es ...tarEntry) func(*rig) []tarEntry { return func(*rig) []tarEntry { return es } }
	nativeSlash := func(r *rig) string { return filepath.ToSlash(r.sentinel) }
	// rooted is the sentinel's path without a drive: "/tmp/..." or "/Users/..."
	rooted := func(r *rig) string {
		return filepath.ToSlash(strings.TrimPrefix(r.sentinel, filepath.VolumeName(r.sentinel)))
	}
	return []hostile{
		// -- parent directory traversal --
		{"parent directory onto the sentinel", one(fileEntry("../../../../outside/sentinel.txt", "overwritten")), true},
		{"parent directory onto a new file", one(fileEntry("../../../../outside/escaped.txt", "x")), true},
		{"parent directory above the install directory", one(fileEntry("../evil.txt", "x")), true},
		{"parent directory after a dot segment", one(fileEntry("./../../../../outside/escaped.txt", "x")), true},
		{"parent directory hidden behind a directory", one(fileEntry("docs/../../../../outside/escaped.txt", "x")), true},
		{"bare parent directory entry", one(tarEntry{name: "..", typ: tar.TypeDir}), false},
		{"parent directory as a directory entry", one(tarEntry{name: "../../../../outside/escaped-dir/", typ: tar.TypeDir}), true},
		{"parent directory by backslashes", one(fileEntry(`..\..\..\..\outside\escaped.txt`, "x")), false},
		// -- absolute paths --
		{"absolute path to the sentinel", func(r *rig) []tarEntry { return []tarEntry{fileEntry(nativeSlash(r), "overwritten")} }, true},
		{"rooted path to the sentinel", func(r *rig) []tarEntry { return []tarEntry{fileEntry(rooted(r), "overwritten")} }, false},
		{"absolute path to a system file", one(fileEntry("/etc/passwd", "root::0:0")), false},
		{"absolute path with a parent segment", func(r *rig) []tarEntry {
			return []tarEntry{fileEntry("/"+path.Join(rooted(r), "..", "sentinel.txt"), "x")}
		}, false},
		// -- drive letters, UNC and device paths --
		{"drive letter, forward slashes", one(fileEntry("C:/Windows/System32/evil.dll", "MZ")), false},
		{"drive letter, backslashes", one(fileEntry(`C:\Windows\System32\evil.dll`, "MZ")), false},
		{"drive letter, no separator", one(fileEntry("C:evil.txt", "x")), false},
		{"drive letter, native path with backslashes", func(r *rig) []tarEntry {
			return []tarEntry{fileEntry(strings.ReplaceAll(r.sentinel, "/", `\`), "x")}
		}, false},
		{"UNC path, forward slashes", one(fileEntry("//attacker/share/evil.txt", "x")), false},
		{"UNC path, backslashes", one(fileEntry(`\\attacker\share\evil.txt`, "x")), false},
		{`\\?\ extended path`, func(r *rig) []tarEntry { return []tarEntry{fileEntry(`\\?\`+r.sentinel, "x")} }, false},
		{`\\?\UNC\ extended UNC path`, one(fileEntry(`\\?\UNC\attacker\share\evil.txt`, "x")), false},
		{`//?/ extended path, forward slashes`, func(r *rig) []tarEntry { return []tarEntry{fileEntry("//?/"+nativeSlash(r), "x")} }, false},
		{`\\.\ device path`, one(fileEntry(`\\.\PhysicalDrive0`, "x")), false},
		{"alternate data stream", one(fileEntry("documentation/index.yaml:evil", "x")), false},
		// -- links and special files --
		// A symbolic link's target resolves from the directory that holds the link,
		// so a link in docs/ needs one ".." more than an entry name to reach base/.
		{"symbolic link to a system file", one(linkEntry("docs/link", tar.TypeSymlink, "/etc/passwd")), false},
		{"symbolic link to the sentinel directory", one(linkEntry("docs/link", tar.TypeSymlink, "../../../../../outside")), false},
		{"symbolic link, then a file written through it", one(
			linkEntry("docs/link", tar.TypeSymlink, "../../../../../outside"),
			fileEntry("docs/link/escaped.txt", "x"),
		), true},
		{"symbolic link named like a file of the package", one(linkEntry("alixai.yaml", tar.TypeSymlink, "../../../../outside/sentinel.txt")), false},
		{"hard link to a file of the package", one(linkEntry("docs/hard", tar.TypeLink, "alixai.yaml")), false},
		{"hard link to the sentinel", one(linkEntry("docs/hard", tar.TypeLink, "../../../../outside/sentinel.txt")), false},
		{"hard link, then a file written through it", one(
			linkEntry("docs/hard", tar.TypeLink, "../../../../outside/sentinel.txt"),
			fileEntry("docs/hard", "overwritten"),
		), false},
		{"character device", one(specialEntry("docs/char", tar.TypeChar)), false},
		{"block device", one(specialEntry("docs/block", tar.TypeBlock)), false},
		{"FIFO", one(specialEntry("docs/pipe", tar.TypeFifo)), false},
	}
}

// nameIsHostile reports whether the entry names of the case are themselves outside
// the path grammar (traversal, absolute, drive, UNC), as opposed to a link or a
// special file under a plain name.
func (h hostile) nameIsHostile() bool {
	for _, p := range []string{"symbolic link", "hard link", "character device", "block device", "FIFO"} {
		if strings.HasPrefix(h.name, p) {
			return false
		}
	}
	return true
}

// naiveExtract is the extractor every careless installer starts with, and the
// control of this suite: it joins the entry name onto dest (or uses an absolute
// name as it is) and writes whatever the header says, following links the way a
// file system does. Where the platform does not let an unprivileged test create a
// symbolic link (Windows), it keeps the link in a table and resolves paths through
// it itself, so the control behaves like an extractor on a platform that does,
// dangling links included: nothing is written through a link whose target does not
// exist, as a real file system creates no directory for one. It returns the first
// error of creating a real link, only so that the test can say which kind of link
// it used.
func naiveExtract(dest string, archive []byte) (linkErr error, err error) {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, err
	}
	emulated := map[string]string{} // link path -> where it points
	// target is where the file system puts name. ok is false when the path goes
	// through an emulated link that a real one could not be followed through.
	target := func(name string) (p string, ok bool) {
		n := filepath.FromSlash(name)
		if !filepath.IsAbs(n) {
			n = filepath.Join(dest, n)
		}
		ok = true
		for link, to := range emulated {
			if n == link || strings.HasPrefix(n, link+string(filepath.Separator)) {
				// Writing the link itself needs the directory it points into; writing
				// below it needs the directory it points to.
				need := to
				if n == link {
					need = filepath.Dir(to)
				}
				if fi, err := os.Stat(need); err != nil || !fi.IsDir() {
					ok = false
				}
				n = to + strings.TrimPrefix(n, link)
			}
		}
		return n, ok
	}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return linkErr, nil
		}
		if err != nil {
			return linkErr, err
		}
		p, ok := target(hdr.Name)
		if !ok {
			continue // a real file system refuses it, and this extractor ignores the error
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			_ = os.MkdirAll(p, 0o755)
		case tar.TypeSymlink:
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.Symlink(filepath.FromSlash(hdr.Linkname), p); err != nil {
				if linkErr == nil {
					linkErr = err
				}
				to := filepath.FromSlash(hdr.Linkname)
				if !filepath.IsAbs(to) {
					to = filepath.Join(filepath.Dir(p), to)
				}
				emulated[p] = to
			}
		case tar.TypeLink:
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			old, _ := target(hdr.Linkname) // through a dangling link it does not exist, and Link fails
			if err := os.Link(old, p); err != nil && linkErr == nil {
				linkErr = err
			}
		case tar.TypeReg:
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			b, err := io.ReadAll(tr)
			if err != nil {
				return linkErr, err
			}
			// Through a link this writes where the link points.
			_ = os.WriteFile(p, b, 0o644)
		}
	}
}

// ---- SEC-059 -----------------------------------------------------------------

// TestSecPackageArchiveTraversal proves that loading a .tar.gz application package
// writes nothing outside the install directory and creates no link or special file,
// whatever the archive says, and that an archive that is too big, too deep or too
// compressible is refused before it has been read to the end.
//
// SEC-059
func TestSecPackageArchiveTraversal(t *testing.T) {
	base := baseEntries(t)

	t.Run("the detectors see what they must", func(t *testing.T) { detectorControls(t) })

	t.Run("a benign archive installs exactly its files", func(t *testing.T) {
		r := newRig(t, apppkg.Limits{})
		archive := tarGz(t, base...)
		before := r.snapshot(t)

		res, err := apppkg.Load(context.Background(), apppkg.FromArchive(bytes.NewReader(archive)), r.loadOptions())
		if err != nil || !res.OK() || res.Authenticity != apppkg.AuthVerified {
			t.Fatalf("the benign archive did not load: err=%v ok=%v auth=%s findings=%v", err, res.OK(), res.Authenticity, res.Findings)
		}
		got, err := r.inst.Install(context.Background(), apppkg.FromArchive(bytes.NewReader(archive)))
		if err != nil || !got.Created {
			t.Fatalf("Install: %v (created=%v)", err, got != nil && got.Created)
		}
		after := r.snapshot(t)
		installDir := path.Join("data", "packages", appID, got.Record.ManifestHash)
		if out := outside(diff(before, after), installDir); len(out) > 0 {
			t.Fatalf("the install wrote outside %s: %v", installDir, out)
		}
		var installed []string
		for p, n := range after {
			if n.mode.IsRegular() && strings.HasPrefix(p, installDir+"/") {
				installed = append(installed, strings.TrimPrefix(p, installDir+"/"))
			}
		}
		sort.Strings(installed)
		if want := regularNames(base); strings.Join(installed, "\n") != strings.Join(want, "\n") {
			t.Fatalf("the install directory holds %d files, the archive %d:\ninstalled %v\narchive   %v", len(installed), len(want), installed, want)
		}
		if u := unsafeNodes(after); len(u) > 0 {
			t.Fatalf("the install tree holds links or special files: %v", u)
		}
		r.sentinelIntact(t)
		if r.rows(t) != 1 {
			t.Fatalf("%d package rows, want 1", r.rows(t))
		}
	})

	for _, h := range hostileCases() {
		t.Run(h.name, func(t *testing.T) {
			r := newRig(t, apppkg.Limits{})
			archive := tarGz(t, with(base, h.entries(r)...)...)
			fileArchive := filepath.Join(t.TempDir(), "hostile.tar.gz")
			if err := os.WriteFile(fileArchive, archive, 0o600); err != nil {
				t.Fatal(err)
			}
			before := r.snapshot(t)

			// The loader refuses it, from memory and from a file.
			res, err := apppkg.Load(context.Background(), apppkg.FromArchive(bytes.NewReader(archive)), r.loadOptions())
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			requireRefused(t, res, checkUnsafeEntry)
			res, err = apppkg.Load(context.Background(), apppkg.FromArchiveFile(fileArchive), r.loadOptions())
			if err != nil {
				t.Fatalf("Load(file): %v", err)
			}
			requireRefused(t, res, checkUnsafeEntry)

			// The installer, which is the thing that writes, installs nothing.
			requireInstallRefused(t, r, apppkg.FromArchive(bytes.NewReader(archive)), checkUnsafeEntry)

			after := r.snapshot(t)
			if d := diff(before, after); len(d) > 0 {
				t.Fatalf("the file tree changed: %v", d)
			}
			if u := unsafeNodes(after); len(u) > 0 {
				t.Fatalf("a link or special file exists: %v", u)
			}
			r.sentinelIntact(t)

			// Control: the same kind of archive does escape from an extractor that
			// trusts it, and the walk above sees it.
			if !h.escapes {
				return
			}
			t.Run("a naive extractor escapes with the same archive", func(t *testing.T) {
				ctl := newRig(t, apppkg.Limits{})
				before := ctl.snapshot(t)
				linkErr, err := naiveExtract(filepath.Join(ctl.base, filepath.FromSlash(naiveDir)), tarGz(t, with(base, h.entries(ctl)...)...))
				if err != nil {
					t.Fatalf("the control extraction failed: %v", err)
				}
				if linkErr != nil {
					t.Logf("a real symbolic link cannot be created here (%v): the control resolves the link itself", linkErr)
				}
				esc := outside(diff(before, ctl.snapshot(t)), naiveDir)
				if len(esc) == 0 {
					t.Fatal("the naive extractor wrote nothing outside its directory: the archive cannot cause an escape, so the assertions above prove nothing")
				}
				t.Logf("the naive extractor escaped: %v", esc)
			})
		})
	}

	t.Run("a symbolic link as the archive path is refused", func(t *testing.T) {
		r := newRig(t, apppkg.Limits{})
		dir := t.TempDir()
		real := filepath.Join(dir, "real.tar.gz")
		if err := os.WriteFile(real, tarGz(t, base...), 0o600); err != nil {
			t.Fatal(err)
		}
		// Control first: the regular file loads.
		res, err := apppkg.Load(context.Background(), apppkg.FromArchiveFile(real), r.loadOptions())
		if err != nil || !res.OK() {
			t.Fatalf("the regular archive file did not load: %v %v", err, res.Findings)
		}
		link := filepath.Join(dir, "link.tar.gz")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("this platform does not let the test create a link: %v", err)
		}
		res, err = apppkg.Load(context.Background(), apppkg.FromArchiveFile(link), r.loadOptions())
		if err != nil {
			t.Fatal(err)
		}
		if res.Package != nil || res.ManifestHash != "" {
			t.Fatal("an archive reached through a symbolic link was loaded")
		}
	})

	t.Run("the trust manifest builder lists nothing hostile", func(t *testing.T) { manifestBuilder(t, base) })

	t.Run("limits", func(t *testing.T) { archiveLimits(t, base) })
}

// TestTreeDiffDirectorySizeIsNotContent proves the tree detector ignores a
// directory node's size (the flake on tmpfs/overlayfs/btrfs) and still reports a
// directory replaced by a regular file or a symbolic link.
func TestTreeDiffDirectorySizeIsNotContent(t *testing.T) {
	dir := fs.ModeDir | 0o755
	reg := fs.FileMode(0o644)
	link := fs.ModeSymlink | 0o777

	before := tree{
		".":      {mode: dir, size: 4096},
		"subdir": {mode: dir, size: 4096},
		"a.txt":  {mode: reg, size: 1, sum: "abc"},
	}
	sizeOnly := tree{
		".":      {mode: dir, size: 80},
		"subdir": {mode: dir, size: 160},
		"a.txt":  {mode: reg, size: 1, sum: "abc"},
	}
	if d := diff(before, sizeOnly); len(d) != 0 {
		t.Fatalf("two trees that differ only in directory size must not differ: %v", d)
	}

	asFile := tree{
		".":      {mode: dir, size: 4096},
		"subdir": {mode: reg, size: 3, sum: "def"},
		"a.txt":  {mode: reg, size: 1, sum: "abc"},
	}
	if d := diff(before, asFile); len(d) != 1 || d[0] != "changed subdir" {
		t.Fatalf("a directory replaced by a file is reported as %v", d)
	}

	asLink := tree{
		".":      {mode: dir, size: 4096},
		"subdir": {mode: link},
		"a.txt":  {mode: reg, size: 1, sum: "abc"},
	}
	if d := diff(before, asLink); len(d) != 1 || d[0] != "changed subdir" {
		t.Fatalf("a directory replaced by a symlink is reported as %v", d)
	}
}

// detectorControls proves the tree walk is able to see each way an install can go
// wrong, and does not cry wolf.
func detectorControls(t *testing.T) {
	r := newRig(t, apppkg.Limits{})
	before := r.snapshot(t)
	if d := diff(before, r.snapshot(t)); len(d) != 0 {
		t.Fatalf("two walks of an unchanged tree differ: %v", d)
	}
	if u := unsafeNodes(before); len(u) != 0 {
		t.Fatalf("a clean tree has unsafe nodes: %v", u)
	}

	planted := filepath.Join(r.outsideDir, "planted.txt")
	if err := os.WriteFile(planted, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if d := diff(before, r.snapshot(t)); len(d) != 1 || d[0] != "added outside/planted.txt" {
		t.Fatalf("a planted file outside the install directory is reported as %v", d)
	}
	if err := os.Remove(planted); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(r.sentinel, []byte("overwritten"), 0o600); err != nil {
		t.Fatal(err)
	}
	if d := diff(before, r.snapshot(t)); len(d) != 1 || d[0] != "changed outside/sentinel.txt" {
		t.Fatalf("an overwritten sentinel is reported as %v", d)
	}
	if err := os.WriteFile(r.sentinel, r.sentinelBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if d := diff(before, r.snapshot(t)); len(d) != 0 {
		t.Fatalf("restoring the sentinel left a difference: %v", d)
	}

	if err := os.Remove(r.sentinel); err != nil {
		t.Fatal(err)
	}
	if d := diff(before, r.snapshot(t)); len(d) != 1 || d[0] != "removed outside/sentinel.txt" {
		t.Fatalf("a removed sentinel is reported as %v", d)
	}
	if err := os.WriteFile(r.sentinel, r.sentinelBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	for name, mode := range map[string]fs.FileMode{
		"symbolic link": fs.ModeSymlink, "device": fs.ModeDevice, "character device": fs.ModeDevice | fs.ModeCharDevice,
		"named pipe": fs.ModeNamedPipe, "socket": fs.ModeSocket, "irregular file": fs.ModeIrregular,
	} {
		if !unsafeMode(mode | 0o644) {
			t.Errorf("the link classifier does not flag a %s", name)
		}
	}
	for name, mode := range map[string]fs.FileMode{"regular file": 0o644, "directory": fs.ModeDir | 0o755} {
		if unsafeMode(mode) {
			t.Errorf("the link classifier flags a %s", name)
		}
	}
	link := filepath.Join(r.data, "planted-link")
	if err := os.Symlink(r.sentinel, link); err != nil {
		t.Logf("a real symbolic link cannot be created here (%v); the classifier above is the control", err)
		return
	}
	if u := unsafeNodes(r.snapshot(t)); len(u) != 1 || !strings.HasPrefix(u[0], "data/planted-link") {
		t.Fatalf("a planted symbolic link is reported as %v", u)
	}
}

// manifestBuilder drives the production manifest builder (internal/trust) with the
// entries of the hostile archives: a package that holds a link, a special file or
// a name outside the path grammar can never be listed, and so never be signed.
func manifestBuilder(t *testing.T, base []tarEntry) {
	id := trust.PackageIdentity{ApplicationID: appID, PackageVersion: "1.0.0"}
	benign := fstest.MapFS{}
	for _, e := range base {
		benign[strings.TrimPrefix(e.name, "./")] = &fstest.MapFile{Data: e.data}
	}

	t.Run("control: the benign package is listed", func(t *testing.T) {
		signed, err := trust.BuildSigned(benign, id, tNow)
		if err != nil {
			t.Fatalf("BuildSigned: %v", err)
		}
		if len(signed.Files) == 0 {
			t.Fatal("the manifest lists no file")
		}
		for _, f := range signed.Files {
			if err := trust.ValidateListedPath(f.Path); err != nil {
				t.Errorf("a benign path is refused: %v", err)
			}
		}
	})

	t.Run("an entry that is not a regular file is refused", func(t *testing.T) {
		for name, mode := range map[string]fs.FileMode{
			"symbolic link":    fs.ModeSymlink,
			"hard link":        fs.ModeIrregular,
			"character device": fs.ModeDevice | fs.ModeCharDevice,
			"block device":     fs.ModeDevice,
			"FIFO":             fs.ModeNamedPipe,
			"socket":           fs.ModeSocket,
		} {
			m := fstest.MapFS{}
			for k, v := range benign {
				m[k] = v
			}
			m["docs/odd"] = &fstest.MapFile{Data: []byte("../../../../outside/sentinel.txt"), Mode: mode}
			_, err := trust.BuildSigned(m, id, tNow)
			if !errors.Is(err, trust.ErrUnsafePackage) {
				t.Errorf("%s: BuildSigned error = %v, want ErrUnsafePackage", name, err)
			}
		}
	})

	t.Run("a name outside the path grammar is never listed", func(t *testing.T) {
		r := &rig{base: t.TempDir(), sentinel: filepath.Join(t.TempDir(), "outside", "sentinel.txt")}
		n := 0
		for _, h := range hostileCases() {
			if !h.nameIsHostile() {
				continue // a link or device with a plain name: refused by its mode, above
			}
			for _, e := range h.entries(r) {
				name := strings.TrimSuffix(strings.TrimPrefix(e.name, "./"), "/")
				if err := trust.ValidatePath(name); err == nil {
					t.Errorf("%s: %q passes the path grammar", h.name, name)
				}
				n++
			}
		}
		if n < 15 {
			t.Fatalf("only %d hostile names were checked", n)
		}
		for _, e := range base {
			if err := trust.ValidatePath(strings.TrimPrefix(e.name, "./")); err != nil {
				t.Errorf("control: benign name %q refused: %v", e.name, err)
			}
		}
	})

	t.Run("a link on disk is refused", func(t *testing.T) {
		dir := t.TempDir()
		for _, e := range base {
			p := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(e.name, "./")))
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, e.data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := trust.BuildSignedDir(dir, id, tNow); err != nil {
			t.Fatalf("control: the directory of the benign package is refused: %v", err)
		}
		outsideFile := filepath.Join(t.TempDir(), "secret.txt")
		if err := os.WriteFile(outsideFile, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsideFile, filepath.Join(dir, "documentation", "leak.md")); err != nil {
			t.Skipf("this platform does not let the test create a link: %v", err)
		}
		if _, err := trust.BuildSignedDir(dir, id, tNow); !errors.Is(err, trust.ErrUnsafePackage) {
			t.Fatalf("BuildSignedDir with a symbolic link inside: %v, want ErrUnsafePackage", err)
		}
		linked := filepath.Join(t.TempDir(), "rootlink")
		if err := os.Symlink(dir, linked); err != nil {
			t.Skipf("this platform does not let the test create a link: %v", err)
		}
		if _, err := trust.BuildSignedDir(linked, id, tNow); !errors.Is(err, trust.ErrUnsafePackage) {
			t.Fatalf("BuildSignedDir of a symbolic link as the root: %v, want ErrUnsafePackage", err)
		}
	})
}

// archiveLimits proves the size, depth, count and ratio bounds are enforced
// before the archive is read to the end, and measures the bytes read and the
// memory allocated.
func archiveLimits(t *testing.T, base []tarEntry) {
	deep := strings.Repeat("d/", 20) + "x.md"

	type limitCase struct {
		name   string
		limits apppkg.Limits // what makes the archive too big
		extra  []tarEntry
		// loose are the limits under which the same archive is fine
		loose apppkg.Limits
	}
	for _, c := range []limitCase{
		{"archive larger than the limit", apppkg.Limits{MaxArchiveBytes: 1024}, nil, apppkg.Limits{}},
		{"more files than the limit", apppkg.Limits{MaxFiles: 5}, nil, apppkg.Limits{}},
		{"a file larger than the limit", apppkg.Limits{MaxFileBytes: 512}, nil, apppkg.Limits{}},
		{"a package larger than the limit", apppkg.Limits{MaxTotalBytes: 2048}, nil, apppkg.Limits{}},
		{"a path deeper than the limit", apppkg.Limits{}, []tarEntry{fileEntry("documentation/"+deep, "x")}, apppkg.Limits{MaxDepth: 64}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, c.limits)
			archive := tarGz(t, with(base, c.extra...)...)
			before := r.snapshot(t)
			res, err := apppkg.Load(context.Background(), apppkg.FromArchive(bytes.NewReader(archive)), r.loadOptions())
			if err != nil {
				t.Fatal(err)
			}
			requireRefused(t, res, checkLimit)
			requireInstallRefused(t, r, apppkg.FromArchive(bytes.NewReader(archive)), checkLimit)
			if d := diff(before, r.snapshot(t)); len(d) > 0 {
				t.Fatalf("the file tree changed: %v", d)
			}
			// Control: under generous limits the same bytes are not refused for size.
			loose := newRig(t, c.loose)
			res, err = apppkg.Load(context.Background(), apppkg.FromArchive(bytes.NewReader(archive)), loose.loadOptions())
			if err != nil {
				t.Fatal(err)
			}
			if hasCheck(res, checkLimit) {
				t.Fatalf("under generous limits the archive is still refused as too big: %v", res.Findings)
			}
		})
	}

	const (
		gib = int64(1) << 30
		mib = int64(1) << 20
		kib = int64(1) << 10
	)

	t.Run("a decompression bomb is refused after a few KiB", func(t *testing.T) {
		// The tar header declares 8 GiB of zeros, which compress about 1000 to 1; the
		// file-size and total limits are raised so that only the ratio limit can stop it.
		r := newRig(t, apppkg.Limits{MaxFileBytes: 16 * gib, MaxTotalBytes: 16 * gib})
		before := r.snapshot(t)
		s := newStream(t, base, "documentation/zeros.bin", 8*gib, fillZeros, gzip.BestCompression)
		requireInstallRefused(t, r, apppkg.FromArchive(s), checkRatio)
		if n := s.Count(); n > 128*kib {
			t.Fatalf("the loader read %d compressed bytes of a bomb before refusing it, want at most %d", n, 128*kib)
		}
		s.stop(t)
		if d := diff(before, r.snapshot(t)); len(d) > 0 {
			t.Fatalf("the file tree changed: %v", d)
		}
	})

	t.Run("a decompression bomb under the default limits", func(t *testing.T) {
		// 48 MiB is under the default per-file limit of 64 MiB, so the ratio limit is
		// what stops it; it must stop it after about 1 MiB of output.
		r := newRig(t, apppkg.Limits{})
		before := r.snapshot(t)
		s := newStream(t, base, "documentation/zeros.bin", 48*mib, fillZeros, gzip.BestCompression)
		requireInstallRefused(t, r, apppkg.FromArchive(s), checkRatio)
		if n := s.Count(); n > 128*kib {
			t.Fatalf("the loader read %d compressed bytes before refusing the bomb, want at most %d", n, 128*kib)
		}
		s.stop(t)
		if d := diff(before, r.snapshot(t)); len(d) > 0 {
			t.Fatalf("the file tree changed: %v", d)
		}
	})

	t.Run("a decompression bomb is read in bounded memory", func(t *testing.T) {
		load := func(lim apppkg.Limits) (alloc uint64, res *apppkg.Result) {
			r := newRig(t, lim)
			s := newStream(t, base, "documentation/zeros.bin", 40*mib, fillZeros, gzip.BestCompression)
			runtime.GC()
			var m1, m2 runtime.MemStats
			runtime.ReadMemStats(&m1)
			res, err := apppkg.Load(context.Background(), apppkg.FromArchive(s), r.loadOptions())
			runtime.ReadMemStats(&m2)
			if err != nil {
				t.Fatal(err)
			}
			s.stop(t)
			return m2.TotalAlloc - m1.TotalAlloc, res
		}
		strict, res := load(apppkg.Limits{})
		requireRefused(t, res, checkRatio)
		// Control: with the ratio limit weakened the same archive is read whole, and
		// the allocation shows it, so the small number above is the limit working.
		weak, _ := load(apppkg.Limits{MaxRatio: 1 << 40})
		t.Logf("allocated while loading a 40 MiB bomb: %d KiB with the ratio limit, %d KiB without", strict>>10, weak>>10)
		if strict > uint64(12*mib) {
			t.Errorf("loading a bomb allocated %d MiB, want under 12", strict>>20)
		}
		if weak < uint64(40*mib) {
			t.Errorf("with the ratio limit off the load allocated only %d MiB for a 40 MiB file: the measurement cannot tell a bounded read from an unbounded one", weak>>20)
		}
	})

	t.Run("an oversized archive is cut off at the limit", func(t *testing.T) {
		// 8 MiB that does not compress, against a 256 KiB archive limit.
		const limit = 256 * kib
		r := newRig(t, apppkg.Limits{MaxArchiveBytes: limit})
		before := r.snapshot(t)
		s := newStream(t, base, "documentation/random.bin", 8*mib, fillRandom, gzip.NoCompression)
		requireInstallRefused(t, r, apppkg.FromArchive(s), checkLimit)
		if n := s.Count(); n > limit+64*kib {
			t.Fatalf("the loader read %d bytes of an archive with a limit of %d", n, limit)
		}
		s.stop(t)
		if d := diff(before, r.snapshot(t)); len(d) > 0 {
			t.Fatalf("the file tree changed: %v", d)
		}
		// Control: without the limit the same stream is read to its end, so the
		// count above is what the limit saved.
		open := newRig(t, apppkg.Limits{})
		s2 := newStream(t, base, "documentation/random.bin", 8*mib, fillRandom, gzip.NoCompression)
		if _, err := apppkg.Load(context.Background(), apppkg.FromArchive(s2), open.loadOptions()); err != nil {
			t.Fatal(err)
		}
		if n := s2.Count(); n < 8*mib {
			t.Fatalf("under the default limits the loader read only %d bytes of an 8 MiB file: the stream cannot show what the limit saved", n)
		}
		s2.stop(t)
	})
}
