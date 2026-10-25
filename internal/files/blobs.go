package files

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// The blob store: content-addressed files below <data_dir>/files.
//
//	files/.tmp/<random>                       an upload being received
//	files/<tenant-dir>/<key[:2]>/<key>        one stored blob
//
// SEC-023: the address of a blob is a function of the scope AND the content.
//
//	tenant-dir = SHA-256("alixai/files/tenant/v1", application, tenant)[:16]
//	key        = SHA-256("alixai/files/blob/v1", application, tenant, workspace, SHA-256(content))
//
// Every part is length-prefixed, so ("ab", "c") and ("a", "bc") cannot collide.
// Two tenants (or two workspaces) that store the same bytes therefore get
// different directories and different file names: dedupe happens only inside
// one scope, so an identical upload in tenant B changes nothing tenant A can
// observe, not an id, not a size, not a name on disk, not a write that is
// skipped. The names are hex digests, never a scope identifier (which may hold
// "..", "/" or ":"), so no value from a request can reach a path.
//
// Every operation goes through an os.Root opened on <data_dir>/files, which
// refuses any path that would leave it, a symlink included. Directories are
// created 0700 and files 0600 (SEC-081; on Windows the ACL of the data
// directory applies).

const (
	tmpDirName   = ".tmp"
	dirMode      = 0o700
	fileMode     = 0o600
	tmpMaxAge    = time.Hour
	blobKeyBytes = sha256.Size
)

var (
	dirRE = regexp.MustCompile(`^[0-9a-f]{32}$`)
	keyRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// addrPart writes one length-prefixed part of a digest input.
func addrPart(h interface{ Write([]byte) (int, error) }, s string) {
	var n [binary.MaxVarintLen64]byte
	h.Write(n[:binary.PutUvarint(n[:], uint64(len(s)))])
	h.Write([]byte(s))
}

// tenantDirName is the directory of every blob of one application's tenant.
func tenantDirName(application, tenant string) string {
	h := sha256.New()
	addrPart(h, "alixai/files/tenant/v1")
	addrPart(h, application)
	addrPart(h, tenant)
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// blobName is the file name of a blob inside its tenant directory: the scope and
// the content digest, hashed together.
func blobName(application, tenant, workspace string, content [sha256.Size]byte) string {
	h := sha256.New()
	addrPart(h, "alixai/files/blob/v1")
	addrPart(h, application)
	addrPart(h, tenant)
	addrPart(h, workspace)
	h.Write(content[:])
	return hex.EncodeToString(h.Sum(nil))
}

// blobStore is the files directory of one data directory. It holds no open
// handle: each operation opens a root on the directory and closes it again, so a
// service that is never closed leaves nothing locked (a data directory can be
// removed, moved or backed up under it), and a root is never shared between
// goroutines.
type blobStore struct {
	dir string
}

// openBlobStore creates <dataDir>/files (and its .tmp) owner-only.
func openBlobStore(dataDir string) (*blobStore, error) {
	b := &blobStore{dir: filepath.Join(dataDir, "files")}
	if err := os.MkdirAll(b.dir, dirMode); err != nil {
		return nil, storageFault("create the files directory", err)
	}
	r, err := b.openRoot()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if err := r.MkdirAll(tmpDirName, dirMode); err != nil {
		return nil, storageFault("create the upload directory", err)
	}
	return b, nil
}

// openRoot opens the confinement of one operation: nothing done through it can
// leave the files directory, a symlink included.
func (b *blobStore) openRoot() (*os.Root, error) {
	r, err := os.OpenRoot(b.dir)
	if err != nil {
		return nil, storageFault("open the files directory", err)
	}
	return r, nil
}

func blobPath(dir, key string) (string, error) {
	if !dirRE.MatchString(dir) || !keyRE.MatchString(key) {
		return "", contracts.New(contracts.ErrInternal, "a malformed blob address was refused")
	}
	return path.Join(dir, key[:2], key), nil
}

// tmpFile creates a new, empty, owner-only file for an upload being received.
func (b *blobStore) tmpFile() (*os.File, string, error) {
	var rnd [16]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return nil, "", storageFault("name an upload", err)
	}
	r, err := b.openRoot()
	if err != nil {
		return nil, "", err
	}
	defer r.Close()
	name := path.Join(tmpDirName, hex.EncodeToString(rnd[:]))
	f, err := r.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return nil, "", storageFault("create an upload file", err)
	}
	return f, name, nil
}

// discard removes a temporary file; absence is fine.
func (b *blobStore) discard(name string) {
	if name == "" {
		return
	}
	if r, err := b.openRoot(); err == nil {
		_ = r.Remove(name)
		r.Close()
	}
}

// place makes the received file the blob dir/key. If the blob is already there
// (the same content stored again in the same scope) the received copy is
// dropped and nothing is written. It is safe to call again after a partial
// success, which a retried transaction does.
func (b *blobStore) place(tmpName, dir, key string) error {
	final, err := blobPath(dir, key)
	if err != nil {
		return err
	}
	r, err := b.openRoot()
	if err != nil {
		return err
	}
	defer r.Close()
	if _, err := r.Lstat(final); err == nil {
		_ = r.Remove(tmpName)
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return storageFault("look up a stored file", err)
	}
	if err := r.MkdirAll(path.Dir(final), dirMode); err != nil {
		return storageFault("create a storage directory", err)
	}
	if err := r.Rename(tmpName, final); err != nil {
		// A concurrent store of the same content may have won the name.
		if _, serr := r.Lstat(final); serr == nil {
			_ = r.Remove(tmpName)
			return nil
		}
		return storageFault("store the file", err)
	}
	if err := syncDir(r, path.Dir(final)); err != nil {
		return storageFault("flush a storage directory", err)
	}
	return nil
}

// syncDir makes a rename into dir durable by syncing the directory. Windows
// cannot sync a directory (and journals the rename itself): there it does
// nothing.
var syncDir = func(r *os.Root, dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := r.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// open opens a stored blob for reading. The file stays open after the root that
// found it is closed.
func (b *blobStore) open(dir, key string) (*os.File, int64, error) {
	p, err := blobPath(dir, key)
	if err != nil {
		return nil, 0, err
	}
	r, err := b.openRoot()
	if err != nil {
		return nil, 0, err
	}
	defer r.Close()
	f, err := r.Open(p)
	if err != nil {
		return nil, 0, storageFault("read a stored file", err)
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, 0, storageFault("read a stored file", errors.New("not a regular file"))
	}
	return f, st.Size(), nil
}

// remove deletes a blob; absence is fine. An emptied directory is removed too
// (best effort: Remove fails on a directory that still has files).
func (b *blobStore) remove(dir, key string) error {
	p, err := blobPath(dir, key)
	if err != nil {
		return err
	}
	r, err := b.openRoot()
	if err != nil {
		return err
	}
	defer r.Close()
	if err := r.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return storageFault("remove a stored file", err)
	}
	_ = r.Remove(path.Dir(p))
	_ = r.Remove(dir)
	return nil
}

// exists reports whether the blob is on disk.
func (b *blobStore) exists(dir, key string) bool {
	p, err := blobPath(dir, key)
	if err != nil {
		return false
	}
	r, err := b.openRoot()
	if err != nil {
		return false
	}
	defer r.Close()
	_, err = r.Lstat(p)
	return err == nil
}

// sweepTmp removes upload files older than cutoff: the residue of an upload
// that was cut by a crash.
func (b *blobStore) sweepTmp(cutoff time.Time) (int, error) {
	r, err := b.openRoot()
	if err != nil {
		return 0, err
	}
	defer r.Close()
	entries, err := fs.ReadDir(r.FS(), tmpDirName)
	if err != nil {
		return 0, storageFault("list the upload directory", err)
	}
	n := 0
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
			continue
		}
		if r.Remove(path.Join(tmpDirName, e.Name())) == nil {
			n++
		}
	}
	return n, nil
}

// blobRef names a blob found on disk.
type blobRef struct{ dir, key string }

// walk visits every blob on disk older than cutoff, for the orphan sweep, in
// batches of at most batch blobs.
func (b *blobStore) walk(cutoff time.Time, batch int, visit func([]blobRef) error) error {
	r, err := b.openRoot()
	if err != nil {
		return err
	}
	defer r.Close()
	fsys := r.FS()
	dirs, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return storageFault("list the files directory", err)
	}
	var found []blobRef
	for _, d := range dirs {
		if !d.IsDir() || !dirRE.MatchString(d.Name()) {
			continue
		}
		subs, err := fs.ReadDir(fsys, d.Name())
		if err != nil {
			continue
		}
		for _, sub := range subs {
			if !sub.IsDir() || len(sub.Name()) != 2 {
				continue
			}
			files, err := fs.ReadDir(fsys, path.Join(d.Name(), sub.Name()))
			if err != nil {
				continue
			}
			for _, f := range files {
				info, err := f.Info()
				if err != nil || !info.Mode().IsRegular() || !keyRE.MatchString(f.Name()) || info.ModTime().After(cutoff) {
					continue
				}
				found = append(found, blobRef{dir: d.Name(), key: f.Name()})
			}
		}
	}
	// Visited after the walk, with the root closed, so a visit that removes a blob
	// opens its own.
	r.Close()
	for len(found) > 0 {
		n := min(batch, len(found))
		if err := visit(found[:n]); err != nil {
			return err
		}
		found = found[n:]
	}
	return nil
}

// storageFault is the error of a failed storage operation. The cause is kept for
// the server log with the path stripped, and the message names the operation and
// never a location (the model and the client never see a path).
func storageFault(op string, err error) error {
	return contracts.Wrap(contracts.ErrUnavailable, stripPath(err), "file storage failed: "+op)
}

// stripPath removes the file name an *fs.PathError or *os.LinkError carries, so
// an error that reaches a log or a client cannot reveal where files are kept.
func stripPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err
	}
	return err
}
