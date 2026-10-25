package apppkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// tarEntry is one entry of a hand-built archive.
type tarEntry struct {
	name string
	typ  byte
	data string
	link string
	size int64 // header size when it should differ from len(data)
}

func tf(name, data string) tarEntry { return tarEntry{name: name, typ: tar.TypeReg, data: data} }

// rawTar builds an uncompressed tar from entries, with exactly the headers asked for.
func rawTar(t testing.TB, entries ...tarEntry) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range entries {
		size := int64(len(e.data))
		if e.size != 0 {
			size = e.size
		}
		if e.typ != tar.TypeReg {
			size = 0
		}
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Size: size, Linkname: e.link, Format: tar.FormatPAX}
		if e.typ == tar.TypeDir {
			hdr.Mode = 0o755
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg && e.size == 0 {
			if _, err := tw.Write([]byte(e.data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func gz(t testing.TB, raw []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// tarGz archives a package the way "tar czf x.tar.gz -C dir ." would.
func tarGz(t testing.TB, fsys fstest.MapFS) []byte {
	t.Helper()
	names := make([]string, 0, len(fsys))
	for n := range fsys {
		names = append(names, n)
	}
	sort.Strings(names)
	entries := make([]tarEntry, 0, len(names))
	for _, n := range names {
		entries = append(entries, tf("./"+n, string(fsys[n].Data)))
	}
	return gz(t, rawTar(t, entries...))
}

func loadArchive(t testing.TB, data []byte, opts Options) *Result {
	t.Helper()
	res, err := Load(context.Background(), FromArchive(bytes.NewReader(data)), opts)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return res
}

// TestArchiveRoundTrip: an archive of the package loads exactly like the
// directory, to the same content address.
func TestArchiveRoundTrip(t *testing.T) {
	signed := valid(t).signed(t)
	fromDir := mustLoad(t, signed, loadOptions())
	fromTar := loadArchive(t, tarGz(t, signed), loadOptions())
	requireCritical(t, fromTar)
	if fromTar.ManifestHash == "" || fromTar.ManifestHash != fromDir.ManifestHash {
		t.Fatalf("hash from the archive %q, from the directory %q", fromTar.ManifestHash, fromDir.ManifestHash)
	}

	// also from a file on disk, through FromPath
	path := filepath.Join(t.TempDir(), "demo.tar.gz")
	if err := os.WriteFile(path, tarGz(t, signed), 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := FromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Load(context.Background(), src, loadOptions())
	if err != nil || res.ManifestHash != fromDir.ManifestHash {
		t.Fatalf("FromPath(file): %v hash=%q", err, res.ManifestHash)
	}
}

// TestSec059ArchiveEntries (SEC-059): an archive that carries a traversal, an
// absolute path, a link or a special file is refused, never repaired, and
// nothing is extracted because nothing ever is.
func TestSec059ArchiveEntries(t *testing.T) {
	base := func(t *testing.T) []tarEntry {
		var out []tarEntry
		for n, b := range valid(t) {
			out = append(out, tf(n, string(b)))
		}
		sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
		return out
	}
	cases := []struct {
		name  string
		extra tarEntry
		want  string
	}{
		{"parent directory", tf("../evil.txt", "x"), "package.unsafe_entry"},
		{"deep parent directory", tf("a/b/../../../evil.txt", "x"), "package.unsafe_entry"},
		{"absolute path", tf("/etc/evil.txt", "x"), "package.unsafe_entry"},
		{"windows drive path", tf("C:/evil.txt", "x"), "package.unsafe_entry"},
		{"backslash separators", tf(`docs\evil.txt`, "x"), "package.unsafe_entry"},
		{"dot segment in the middle", tf("docs/./evil.txt", "x"), "package.unsafe_entry"},
		{"hidden file", tf(".git/config", "x"), "package.unsafe_entry"},
		{"hidden segment", tf("docs/.hidden/x.md", "x"), "package.unsafe_entry"},
		{"empty segment", tf("docs//evil.md", "x"), "package.unsafe_entry"},
		{"control character", tf("docs/evil\x01.md", "x"), "package.unsafe_entry"},
		{"symbolic link", tarEntry{name: "docs/link", typ: tar.TypeSymlink, link: "/etc/passwd"}, "package.unsafe_entry"},
		{"hard link", tarEntry{name: "docs/hard", typ: tar.TypeLink, link: "alixai.yaml"}, "package.unsafe_entry"},
		{"character device", tarEntry{name: "docs/dev", typ: tar.TypeChar}, "package.unsafe_entry"},
		{"block device", tarEntry{name: "docs/blk", typ: tar.TypeBlock}, "package.unsafe_entry"},
		{"fifo", tarEntry{name: "docs/pipe", typ: tar.TypeFifo}, "package.unsafe_entry"},
		{"duplicate name", tf("alixai.yaml", "again"), "package.unsafe_entry"},
		{"case collision", tf("ALIXAI.yaml", "x"), "package.unsafe_entry"},
		{"case collision of a directory", tf("Documentation/extra.md", "x"), "package.unsafe_entry"},
		{"case collision of a directory entry", tarEntry{name: "Documentation/", typ: tar.TypeDir}, "package.unsafe_entry"},
		{"case collision of a file with a directory", tf("Documentation", "x"), "package.unsafe_entry"},
		{"file below a file", tf("alixai.yaml/inner", "x"), "package.unsafe_entry"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := gz(t, rawTar(t, append(base(t), c.extra)...))
			res := loadArchive(t, data, loadOptions())
			if res.Package != nil || res.Authenticity == AuthVerified || res.ManifestHash != "" {
				t.Fatalf("a hostile archive yielded a package: %+v", res)
			}
			requireCritical(t, res, c.want)
			if f, _ := find(res.Findings, c.want); strings.ContainsAny(f.Message+f.Path, "\x00\x01\r") {
				t.Errorf("a finding repeats a control character of the entry name: %q", f.Path)
			}
		})
	}

	t.Run("a leading ./ is how tar writes a package, not an escape", func(t *testing.T) {
		requireCritical(t, loadArchive(t, tarGz(t, valid(t).signed(t)), loadOptions()))
	})

	t.Run("nothing outside the install directory is written", func(t *testing.T) {
		h := newHarness(t, true)
		sentinelDir := filepath.Dir(h.dataDir)
		before := listTree(t, sentinelDir)
		data := gz(t, rawTar(t, append(base(t), tf("../evil.txt", "x"), tf("/abs-evil.txt", "x"))...))
		if _, err := h.in.Install(context.Background(), FromArchive(bytes.NewReader(data))); err == nil {
			t.Fatal("a hostile archive must not install")
		}
		after := listTree(t, sentinelDir)
		if strings.Join(before, "\n") != strings.Join(after, "\n") {
			t.Fatalf("the file tree changed:\nbefore %v\nafter  %v", before, after)
		}
		if h.rowCount(t) != 0 {
			t.Fatal("a row was recorded")
		}
	})
}

// Once an entry is refused the archive is refused, so nothing after it is
// decompressed: neither the refused entry's data nor the entries that follow.
func TestArchiveStopsAtTheFirstRefusedEntry(t *testing.T) {
	big := make([]byte, 4<<20)
	if _, err := rand.Read(big); err != nil { // incompressible: every byte must be read to skip it
		t.Fatal(err)
	}
	var base []tarEntry
	for n, b := range valid(t) {
		base = append(base, tf(n, string(b)))
	}
	sort.Slice(base, func(i, j int) bool { return base[i].name < base[j].name })
	for name, extra := range map[string][]tarEntry{
		"unsafe name":    {tf("../evil.bin", string(big)), tf("documentation/after.md", string(big))},
		"duplicate name": {tf("alixai.yaml", string(big))},
		"case collision": {tf("Documentation/extra.bin", string(big))},
	} {
		t.Run(name, func(t *testing.T) {
			data := gz(t, rawTar(t, append(append([]tarEntry(nil), base...), extra...)...))
			r := &countReader{r: bytes.NewReader(data)}
			res, err := Load(context.Background(), FromArchive(r), loadOptions())
			if err != nil {
				t.Fatal(err)
			}
			requireCritical(t, res, "package.unsafe_entry")
			if r.n > 1<<20 {
				t.Errorf("read %d of %d compressed bytes: the refused entry was decompressed", r.n, len(data))
			}
		})
	}
}

func listTree(t testing.TB, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// TestSec059ArchiveShapeAndLimits (SEC-059): malformed streams, trailing data,
// decompression bombs and oversized, over-deep or over-wide packages are
// refused before anything is parsed.
func TestSec059ArchiveShapeAndLimits(t *testing.T) {
	good := tarGz(t, valid(t).signed(t))
	small := Options{CoreVersion: "0.1.0"}

	cases := []struct {
		name string
		data []byte
		opts Options
		want string
	}{
		{"not gzip", []byte("this is not an archive"), small, "archive.invalid"},
		{"empty input", nil, small, "archive.invalid"},
		{"truncated", good[:len(good)/2], small, "archive.invalid"},
		{"trailing garbage after the gzip member", append(append([]byte(nil), good...), []byte("garbage")...), small, "archive.invalid"},
		{"a second gzip member", append(append([]byte(nil), good...), good...), small, "archive.invalid"},
		{"entry shorter than its header says (truncated tar)", gz(t, rawTar(t, tf("alixai.yaml", strings.Repeat("a", 2000)))[:700]), small, "archive.invalid"},
		{"decompression bomb", gz(t, rawTar(t, tf("alixai.yaml", "a: 1\n"), tf("documentation/zeros.md", strings.Repeat("\x00", 24<<20)))), small, "archive.compression_ratio"},
		{"file larger than the limit", gz(t, rawTar(t, tf("alixai.yaml", strings.Repeat("a", 4096)))), Options{CoreVersion: "0.1.0", Limits: Limits{MaxFileBytes: 1024}}, "package.limit_exceeded"},
		{"package larger than the limit", gz(t, rawTar(t, tf("a.md", strings.Repeat("a", 800)), tf("b.md", strings.Repeat("b", 800)))), Options{CoreVersion: "0.1.0", Limits: Limits{MaxTotalBytes: 1000}}, "package.limit_exceeded"},
		{"too many files", gz(t, rawTar(t, tf("a.md", "a"), tf("b.md", "b"), tf("c.md", "c"))), Options{CoreVersion: "0.1.0", Limits: Limits{MaxFiles: 2}}, "package.limit_exceeded"},
		{"path too deep", gz(t, rawTar(t, tf("a/b/c/d/e.md", "a"))), Options{CoreVersion: "0.1.0", Limits: Limits{MaxDepth: 3}}, "package.limit_exceeded"},
		{"archive larger than the limit", good, Options{CoreVersion: "0.1.0", Limits: Limits{MaxArchiveBytes: 1000}}, "package.limit_exceeded"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := loadArchive(t, c.data, c.opts)
			if res.Package != nil {
				t.Fatal("a refused archive yielded a package")
			}
			if got := ids(res.Findings, contracts.SeverityCritical); !contains(got, c.want) {
				t.Fatalf("critical checks = %v, want %s\n%s", got, c.want, dump(res.Findings))
			}
		})
	}
}

// TestFromDirRefusesLinks (SEC-059): a directory is read without following
// links; a link as the root or inside the tree is refused.
func TestFromDirRefusesLinks(t *testing.T) {
	dir := writeTree(t, valid(t).signed(t))
	target := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "documentation", "leak.md")); err != nil {
		t.Skipf("symbolic links are not available here: %v", err)
	}
	res, err := Load(context.Background(), FromDir(dir), loadOptions())
	if err != nil {
		t.Fatal(err)
	}
	requireCritical(t, res, "package.unsafe_entry")

	link := filepath.Join(t.TempDir(), "rootlink")
	if err := os.Symlink(dir, link); err != nil {
		t.Skip("cannot create a directory link")
	}
	res, err = Load(context.Background(), FromDir(link), loadOptions())
	if err != nil {
		t.Fatal(err)
	}
	requireCritical(t, res, "package.unsafe_entry")
}

// TestFromDirReadsSnapshot: the directory is read once; a file changed on disk
// after Load returned does not change the Package.
func TestFromDirReadsSnapshot(t *testing.T) {
	dir := writeTree(t, valid(t).signed(t))
	res, err := Load(context.Background(), FromDir(dir), loadOptions())
	if err != nil || !res.OK() {
		t.Fatalf("Load: %v %v", err, res)
	}
	hash := res.ManifestHash
	if err := os.WriteFile(filepath.Join(dir, "alixai.yaml"), []byte("changed after the load\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res.Package.Manifest.Application.Name == "" || res.ManifestHash != hash {
		t.Fatal("the loaded package changed with the disk")
	}
}
