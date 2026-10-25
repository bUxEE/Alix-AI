package apppkg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Snapshot is an immutable, in-memory copy of a package: every file, read once.
// It is the only thing the loader ever looks at. Signature verification
// (trust.Verifier.VerifyPackage), parsing, installation and the manifest hash
// all read the same Snapshot, so there is no time-of-check gap between "the
// bytes that were verified" and "the bytes that were used": a package whose
// files change on disk after the snapshot was taken is not affected.
//
// A Snapshot holds only regular files. Symbolic links, devices, sockets, hard
// links, hidden names, names outside the package path grammar, duplicate and
// case-colliding names never get in: the source that builds it reports them as
// findings and no Snapshot is produced.
//
// It implements fs.FS, fs.ReadFileFS, fs.ReadDirFS and fs.StatFS.
type Snapshot struct {
	files map[string][]byte
	dirs  map[string][]string // directory -> sorted child names
	paths []string            // sorted file paths
	size  int64
}

var (
	_ fs.FS         = (*Snapshot)(nil)
	_ fs.ReadFileFS = (*Snapshot)(nil)
	_ fs.ReadDirFS  = (*Snapshot)(nil)
	_ fs.StatFS     = (*Snapshot)(nil)
)

// Paths returns the sorted package-relative paths of all files.
func (s *Snapshot) Paths() []string { return append([]string(nil), s.paths...) }

// Len is the number of files.
func (s *Snapshot) Len() int { return len(s.paths) }

// Size is the sum of the file sizes in bytes.
func (s *Snapshot) Size() int64 { return s.size }

// Has reports whether the snapshot holds the file.
func (s *Snapshot) Has(name string) bool { _, ok := s.files[name]; return ok }

// bytes returns the stored content without copying. Callers inside this
// package must not modify or retain-and-modify it.
func (s *Snapshot) bytes(name string) ([]byte, bool) {
	b, ok := s.files[name]
	return b, ok
}

// ReadFile returns a copy of the file, so a caller cannot alter the snapshot.
func (s *Snapshot) ReadFile(name string) ([]byte, error) {
	b, ok := s.files[name]
	if !ok {
		return nil, &fs.PathError{Op: "readfile", Path: name, Err: fs.ErrNotExist}
	}
	return bytes.Clone(b), nil
}

// Open implements fs.FS.
func (s *Snapshot) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if b, ok := s.files[name]; ok {
		return &memFile{Reader: bytes.NewReader(b), info: fileInfo{name: path.Base(name), size: int64(len(b))}}, nil
	}
	if kids, ok := s.dirs[name]; ok {
		return &memDir{info: fileInfo{name: path.Base(name), dir: true}, snap: s, dir: name, kids: kids}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// Stat implements fs.StatFS.
func (s *Snapshot) Stat(name string) (fs.FileInfo, error) {
	f, err := s.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Stat()
}

// ReadDir implements fs.ReadDirFS.
func (s *Snapshot) ReadDir(name string) ([]fs.DirEntry, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
	}
	kids, ok := s.dirs[name]
	if !ok {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	return s.entries(name, kids), nil
}

func (s *Snapshot) entries(dir string, kids []string) []fs.DirEntry {
	out := make([]fs.DirEntry, 0, len(kids))
	for _, k := range kids {
		p := k
		if dir != "." {
			p = dir + "/" + k
		}
		if b, ok := s.files[p]; ok {
			out = append(out, dirEntry{fileInfo{name: k, size: int64(len(b))}})
		} else {
			out = append(out, dirEntry{fileInfo{name: k, dir: true}})
		}
	}
	return out
}

type fileInfo struct {
	name string
	size int64
	dir  bool
}

func (i fileInfo) Name() string { return i.name }
func (i fileInfo) Size() int64  { return i.size }
func (i fileInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (i fileInfo) ModTime() time.Time { return time.Time{} }
func (i fileInfo) IsDir() bool        { return i.dir }
func (i fileInfo) Sys() any           { return nil }

type dirEntry struct{ fileInfo }

func (e dirEntry) Type() fs.FileMode          { return e.Mode().Type() }
func (e dirEntry) Info() (fs.FileInfo, error) { return e.fileInfo, nil }

type memFile struct {
	*bytes.Reader
	info fileInfo
}

func (f *memFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *memFile) Close() error               { return nil }

type memDir struct {
	info fileInfo
	snap *Snapshot
	dir  string
	kids []string
	pos  int
}

func (d *memDir) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *memDir) Close() error               { return nil }
func (d *memDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.dir, Err: errors.New("is a directory")}
}
func (d *memDir) ReadDir(n int) ([]fs.DirEntry, error) {
	kids := d.kids[d.pos:]
	if n <= 0 {
		d.pos = len(d.kids)
		return d.snap.entries(d.dir, kids), nil
	}
	if len(kids) == 0 {
		return nil, io.EOF
	}
	if n < len(kids) {
		kids = kids[:n]
	}
	d.pos += len(kids)
	return d.snap.entries(d.dir, kids), nil
}

// problem is an entry or limit that keeps a package from being snapshotted.
type problem struct {
	check *check
	path  string
	msg   string
}

// builder assembles a Snapshot, enforcing path grammar, uniqueness and limits.
type builder struct {
	lim      Limits
	files    map[string][]byte
	dirsSeen map[string]bool   // explicit directory entries (tar)
	folded   map[string]string // letter-case-folded name -> the name as spelt
	// foldDirs also folds every directory above a name, so Docs/a and docs/b
	// collide. Off only for a package read back from its install directory.
	foldDirs bool
	size     int64
	problems []problem
}

func newBuilder(lim Limits) *builder {
	return &builder{
		lim:      lim,
		files:    map[string][]byte{},
		dirsSeen: map[string]bool{},
		folded:   map[string]string{},
		foldDirs: true,
	}
}

const maxReportedProblems = 50

func (b *builder) problem(c *check, p, format string, args ...any) {
	if len(b.problems) >= maxReportedProblems {
		return
	}
	b.problems = append(b.problems, problem{check: c, path: clipPath(p), msg: fmt.Sprintf(format, args...)})
}

// checkPath validates a file or directory path and the structural rules every
// entry must satisfy. It returns false (and records a problem) when the entry
// must not enter the snapshot.
func (b *builder) checkPath(p string) bool {
	if err := trust.ValidatePath(p); err != nil {
		b.problem(chkUnsafeEntry, p, "path is outside the safe path grammar (relative, forward slashes, no dot segments, no hidden names, letters digits . _ - only)")
		return false
	}
	if n := strings.Count(p, "/") + 1; n > b.lim.MaxDepth {
		b.problem(chkLimit, p, "path has %d segments, the limit is %d", n, b.lim.MaxDepth)
		return false
	}
	return true
}

// reserve checks the per-file, count and total limits before a file is read.
// It returns false when a limit is exceeded; a limit problem stops the load.
func (b *builder) reserve(p string, size int64) bool {
	switch {
	case size > b.lim.MaxFileBytes:
		b.problem(chkLimit, p, "file is larger than the limit of %d bytes", b.lim.MaxFileBytes)
	case len(b.files) >= b.lim.MaxFiles:
		b.problem(chkLimit, p, "package has more than %d files", b.lim.MaxFiles)
	case b.size+size > b.lim.MaxTotalBytes:
		b.problem(chkLimit, p, "package is larger than the limit of %d bytes in total", b.lim.MaxTotalBytes)
	default:
		return true
	}
	return false
}

// addFile inserts a file, refusing duplicates, case-colliding names and a path
// that is both a file and the parent of another path.
func (b *builder) addFile(p string, data []byte) bool {
	if !b.admit(p) {
		return false
	}
	b.put(p, data)
	return true
}

// admit refuses p when it is already in the snapshot or collides with a name in
// it by letter case, before its content is read.
func (b *builder) admit(p string) bool {
	if _, dup := b.files[p]; dup {
		b.problem(chkUnsafeEntry, p, "entry appears more than once")
		return false
	}
	return b.fold(p)
}

// put stores the content of an admitted file.
func (b *builder) put(p string, data []byte) {
	b.files[p] = data
	b.size += int64(len(data))
}

// fold records the letter-case-folded spelling of p and, with foldDirs, of
// every directory above it, files and directories in one namespace. It refuses
// p when one of them differs from a name already recorded only by letter case:
// on a case-insensitive file system the two are the same file or directory.
func (b *builder) fold(p string) bool {
	names := []string{p}
	if b.foldDirs {
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			names = append(names, d)
		}
	}
	for _, n := range names {
		if prev, ok := b.folded[strings.ToLower(n)]; ok && prev != n {
			b.problem(chkUnsafeEntry, p, "name differs only by letter case from %s, which collides on a case-insensitive file system", clipPath(prev))
			return false
		}
	}
	for _, n := range names {
		b.folded[strings.ToLower(n)] = n
	}
	return true
}

// finish checks file/directory conflicts and builds the Snapshot. A builder
// with problems returns none.
func (b *builder) finish() (*Snapshot, []problem) {
	if len(b.problems) > 0 {
		return nil, b.problems
	}
	for p := range b.files {
		for d := path.Dir(p); d != "." && d != "/"; d = path.Dir(d) {
			if _, isFile := b.files[d]; isFile {
				b.problem(chkUnsafeEntry, d, "path is both a file and a directory")
			}
		}
	}
	for d := range b.dirsSeen {
		if _, isFile := b.files[d]; isFile {
			b.problem(chkUnsafeEntry, d, "path is both a file and a directory")
		}
	}
	if len(b.problems) > 0 {
		sort.Slice(b.problems, func(i, j int) bool { return b.problems[i].path < b.problems[j].path })
		return nil, b.problems
	}
	s := &Snapshot{files: b.files, dirs: map[string][]string{".": nil}, size: b.size}
	kids := map[string]map[string]struct{}{".": {}}
	for p := range b.files {
		s.paths = append(s.paths, p)
		child := p
		for {
			dir := path.Dir(child)
			if kids[dir] == nil {
				kids[dir] = map[string]struct{}{}
			}
			kids[dir][path.Base(child)] = struct{}{}
			if dir == "." {
				break
			}
			child = dir
		}
	}
	sort.Strings(s.paths)
	for d, set := range kids {
		names := make([]string, 0, len(set))
		for n := range set {
			names = append(names, n)
		}
		sort.Strings(names)
		s.dirs[d] = names
	}
	return s, nil
}

// Source is something a package can be read from. Constructors: FromDir,
// FromArchive, FromArchiveFile, FromFS, FromPath. Reading a source produces a
// Snapshot or a list of problems; it never executes or interprets anything.
type Source interface {
	snapshot(ctx context.Context, lim Limits) (*Snapshot, []problem, error)
	describe() string
}

// FromDir reads a package directory. The root must be a real directory (a
// symbolic link as the root is refused); entries below it are never followed
// when they are links. The directory is read once into a Snapshot, which is
// all the loader ever uses.
func FromDir(dir string) Source { return dirSource{dir: dir} }

// FromInstalledDir is FromDir for a package read back from its install
// directory (<data_dir>/packages/<app>/<hash>), as Installer.Open and the
// integration audit read it: directory names are not letter-case-folded, so a
// package installed on a case-sensitive file system before they were keeps
// loading as it was written. Every other check applies.
func FromInstalledDir(dir string) Source { return dirSource{dir: dir, installed: true} }

// dirSource reads a package directory; installed is FromInstalledDir's.
type dirSource struct {
	dir       string
	installed bool
}

func (d dirSource) describe() string { return "directory " + clipPath(d.dir) }

func (d dirSource) snapshot(ctx context.Context, lim Limits) (*Snapshot, []problem, error) {
	st, err := os.Lstat(d.dir)
	if err != nil {
		return nil, nil, contracts.Wrap(contracts.ErrValidationFailed, err, "package directory cannot be read")
	}
	if !st.IsDir() {
		return nil, []problem{{chkUnsafeEntry, ".", "package root is not a directory (a symbolic link or a file)"}}, nil
	}
	root, err := os.OpenRoot(d.dir)
	if err != nil {
		return nil, nil, contracts.Wrap(contracts.ErrValidationFailed, err, "package directory cannot be opened")
	}
	defer root.Close()
	return snapshotFS(ctx, root.FS(), lim, !d.installed)
}

// FromFS reads a package from an arbitrary fs.FS, with the same rules as
// FromDir. It is how tests and embedding callers hand over an in-memory
// package.
func FromFS(fsys fs.FS) Source { return fsSource{fsys} }

type fsSource struct{ fsys fs.FS }

func (fsSource) describe() string { return "file system" }

func (f fsSource) snapshot(ctx context.Context, lim Limits) (*Snapshot, []problem, error) {
	return snapshotFS(ctx, f.fsys, lim, true)
}

// snapshotFS walks fsys without following links: an entry that is neither a
// regular file nor a directory is a problem, and is never opened. foldDirs is
// the builder's.
func snapshotFS(ctx context.Context, fsys fs.FS, lim Limits, foldDirs bool) (*Snapshot, []problem, error) {
	b := newBuilder(lim)
	b.foldDirs = foldDirs
	var walk func(dir string) error
	var stop bool
	walk = func(dir string) error {
		entries, err := fs.ReadDir(fsys, dir)
		if err != nil {
			return contracts.Wrap(contracts.ErrUnavailable, err, "package directory cannot be read")
		}
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if stop {
				return nil
			}
			p := e.Name()
			if dir != "." {
				p = dir + "/" + e.Name()
			}
			typ := e.Type()
			switch {
			case typ.IsDir():
				if !b.checkPath(p) {
					continue
				}
				if err := walk(p); err != nil {
					return err
				}
			case typ.IsRegular():
				if !b.checkPath(p) {
					continue
				}
				data, ok, err := readRegular(fsys, p, b)
				if err != nil {
					return err
				}
				if !ok {
					if len(b.problems) > 0 && b.problems[len(b.problems)-1].check == chkLimit {
						stop = true
					}
					continue
				}
				b.addFile(p, data)
			default:
				b.problem(chkUnsafeEntry, p, "is not a regular file or directory (%s); links and special files are never accepted", describeType(typ))
			}
		}
		return nil
	}
	if err := walk("."); err != nil {
		return nil, nil, err
	}
	snap, probs := b.finish()
	return snap, probs, nil
}

// readRegular opens p, requires the opened object to still be a regular file
// and reads it within the limits.
func readRegular(fsys fs.FS, p string, b *builder) ([]byte, bool, error) {
	f, err := fsys.Open(p)
	if err != nil {
		return nil, false, contracts.Wrap(contracts.ErrUnavailable, err, "package file cannot be opened")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, false, contracts.Wrap(contracts.ErrUnavailable, err, "package file cannot be inspected")
	}
	if !st.Mode().IsRegular() {
		b.problem(chkUnsafeEntry, p, "changed type while it was being read (%s)", describeType(st.Mode().Type()))
		return nil, false, nil
	}
	if !b.reserve(p, st.Size()) {
		return nil, false, nil
	}
	data, err := io.ReadAll(io.LimitReader(f, st.Size()+1))
	if err != nil {
		return nil, false, contracts.Wrap(contracts.ErrUnavailable, err, "package file cannot be read")
	}
	if int64(len(data)) != st.Size() {
		b.problem(chkUnsafeEntry, p, "changed size while it was being read")
		return nil, false, nil
	}
	return data, true, nil
}

func describeType(m fs.FileMode) string {
	switch {
	case m&fs.ModeSymlink != 0:
		return "symbolic link"
	case m&fs.ModeNamedPipe != 0:
		return "named pipe"
	case m&fs.ModeSocket != 0:
		return "socket"
	case m&fs.ModeDevice != 0:
		return "device"
	case m&fs.ModeIrregular != 0:
		return "irregular file, such as a reparse point"
	}
	return "mode " + m.String()
}

// FromPath chooses the source by what path is: a directory is read with
// FromDir, a file with FromArchiveFile.
func FromPath(p string) (Source, error) {
	st, err := os.Lstat(p)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "package path cannot be read")
	}
	if st.IsDir() {
		return FromDir(p), nil
	}
	return FromArchiveFile(p), nil
}

// clipPath bounds an untrusted path for use in a message and replaces control
// characters, so a hostile entry name cannot forge log lines or findings.
func clipPath(p string) string {
	p = filepath.ToSlash(p)
	var sb strings.Builder
	n := 0
	for _, r := range p {
		if n >= 120 {
			sb.WriteString("...")
			break
		}
		if r < 0x20 || r == 0x7f || r == 0xfffd {
			r = '?'
		}
		sb.WriteRune(r)
		n++
	}
	return sb.String()
}
