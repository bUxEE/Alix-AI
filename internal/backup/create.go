package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/files"
	"github.com/buxee/alix-ai/internal/fsperm"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Options configures Create.
type Options struct {
	// Dir receives the backup: <Dir>/<backup id>/. Created owner only if missing;
	// the backup directory itself is always owner only (internal/fsperm).
	Dir string
	// DataDir is the data directory whose file trees are copied. It may be
	// empty only when Trees is empty.
	DataDir string
	// Trees names the directories below DataDir to include, among files and
	// packages (no other tree is restorable). Nil means DefaultTrees; an empty
	// non-nil slice means database only. A tree that
	// does not exist is recorded as empty.
	Trees []string
	// Signer signs the manifest. Its public half is what Restore needs.
	Signer trust.SigningKey
	// Clock stamps created_at and the signature. Nil means the wall clock.
	Clock clock.Clock
	// Version is the software version recorded in the manifest.
	Version string
	// Validity is how long the signature is valid; zero means DefaultValidity.
	Validity time.Duration
}

// ErrBlobMissing: a live row of the database snapshot names a blob that is not
// in the backup's copy of the files tree (compaction removed it while the trees
// were copied). The backup is refused; taking it again sees the row tombstoned.
var ErrBlobMissing = errors.New("backup: a file the database snapshot names is missing from the copied files tree")

// afterSnapshot, when set, runs between the database snapshot and the copy of
// the trees: a test changes the trees there.
var afterSnapshot func()

// Result describes a finished backup.
type Result struct {
	ID       string
	Dir      string
	Manifest *Manifest
	// LostBlobs lists the blobs live rows name that the data directory had
	// already lost before the backup: the backup holds them missing too.
	LostBlobs []string
}

// Create takes a consistent online backup of db and the file trees beside it.
// The database is copied first (store.DB.Backup: VACUUM INTO, a snapshot that
// does not block readers or writers, integrity-checked before it is
// published), the trees second; blobs added meanwhile are harmless extras, and
// a blob a live row of the snapshot names that is missing from the copy
// (compaction removed it meanwhile) refuses the backup with ErrBlobMissing. The
// result is written as <id>.partial and renamed once the signed manifest is in it.
func Create(ctx context.Context, db *store.DB, o Options) (Result, error) {
	if db == nil {
		return Result{}, contracts.New(contracts.ErrValidationFailed, "backup: nil store")
	}
	if strings.TrimSpace(o.Dir) == "" {
		return Result{}, contracts.New(contracts.ErrValidationFailed, "backup: no destination directory")
	}
	if o.Signer.KeyID == "" || len(o.Signer.Key) == 0 {
		return Result{}, contracts.New(contracts.ErrValidationFailed, "backup: no signing key")
	}
	trees, err := normalizeTrees(o.Trees)
	if err != nil {
		return Result{}, err
	}
	clk := o.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	validity := o.Validity
	if validity <= 0 {
		validity = DefaultValidity
	}
	dir, err := filepath.Abs(o.Dir)
	if err != nil {
		return Result{}, contracts.Wrap(contracts.ErrValidationFailed, err, "backup: destination invalid")
	}
	var dataDir string
	if len(trees) > 0 {
		if strings.TrimSpace(o.DataDir) == "" {
			return Result{}, contracts.New(contracts.ErrValidationFailed, "backup: file trees named but no data directory")
		}
		if dataDir, err = filepath.Abs(o.DataDir); err != nil {
			return Result{}, contracts.Wrap(contracts.ErrValidationFailed, err, "backup: data directory invalid")
		}
		for _, t := range trees {
			if within(filepath.Join(dataDir, t), dir) {
				return Result{}, contracts.Newf(contracts.ErrValidationFailed, "backup: the destination is inside the %s tree it would copy", t)
			}
		}
	}

	now := clk.Now().UTC().Truncate(time.Second)
	id := ids.NewGenerator(clk).New("bkp")
	final := filepath.Join(dir, id)
	partial := final + ".partial"
	// Owner only (internal/fsperm), on Windows too, where a new directory would
	// otherwise inherit whatever its parent grants: a backup holds the whole
	// database. The working directory is restricted before anything is copied
	// into it, and what is created below inherits that.
	if _, err := fsperm.MkdirOwnerOnly(dir); err != nil {
		return Result{}, unavailable(err, "backup directory cannot be created")
	}
	if err := os.Mkdir(partial, 0o700); err != nil {
		return Result{}, unavailable(err, "backup working directory cannot be created")
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(partial)
		}
	}()
	if err := fsperm.Restrict(partial, true); err != nil {
		return Result{}, unavailable(err, "backup working directory cannot be made owner only")
	}

	files := []FileEntry{}
	dbCopy := filepath.Join(partial, DatabaseFile)
	if err := db.Backup(ctx, dbCopy); err != nil {
		return Result{}, err
	}
	if afterSnapshot != nil {
		afterSnapshot()
	}
	for _, t := range trees {
		entries, err := copyTree(ctx, filepath.Join(dataDir, t), filepath.Join(partial, TreeRoot, t), TreeRoot+"/"+t)
		if err != nil {
			return Result{}, err
		}
		files = append(files, entries...)
	}
	var lost []string
	if slices.Contains(trees, filesTree) {
		if lost, err = checkBlobs(ctx, db, dbCopy, filepath.Join(partial, TreeRoot, filesTree), filepath.Join(dataDir, filesTree)); err != nil {
			return Result{}, err
		}
	}
	sum, size, err := hashFile(dbCopy)
	if err != nil {
		return Result{}, unavailable(err, "backup database copy cannot be read")
	}
	files = append(files, FileEntry{Path: DatabaseFile, Size: size, SHA256: sum})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	version := o.Version
	if version == "" {
		version = "unknown"
	}
	m := Manifest{
		Format: ManifestFormat,
		Signed: Signed{
			Kind: ManifestKind, ApplicationID: trust.PlatformApplicationID, BackupID: id,
			CreatedAt: now.Format(timeLayout), Version: version, HashAlgorithm: hashAlgorithm,
			Trees: trees, Files: files,
		},
		Signatures: []trust.Signature{},
	}
	if err := m.Signed.validate(); err != nil {
		return Result{}, err
	}
	sig, err := trust.SignPayload(m.Signed, o.Signer.KeyID, o.Signer.Key, now, now.Add(validity))
	if err != nil {
		return Result{}, err
	}
	m.Signatures = append(m.Signatures, sig)
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Result{}, contracts.Wrap(contracts.ErrInternal, err, "backup manifest cannot be encoded")
	}
	if _, err := parseManifest(raw); err != nil {
		return Result{}, err
	}
	if err := writeFileAtomic(filepath.Join(partial, ManifestFile), append(raw, '\n')); err != nil {
		return Result{}, unavailable(err, "backup manifest cannot be written")
	}
	if err := os.Rename(partial, final); err != nil {
		return Result{}, unavailable(err, "backup cannot be moved into place")
	}
	ok = true
	return Result{ID: id, Dir: final, Manifest: &m, LostBlobs: lost}, nil
}

// filesTree is the tree of internal/files blobs.
const filesTree = "files"

// checkBlobs refuses the backup when its copy of the files tree (copied) lacks a
// blob that a live row of the database snapshot names and that compaction
// removed while the trees were copied (the snapshot is taken first): a restore
// of such a backup would hold live rows without their bytes. A blob the data
// directory had already lost (its row is still live in db and the file is not
// in liveTree) is no race: the backup holds the data directory as it is, and
// the blob is returned so the caller can say so. The snapshot is only read.
func checkBlobs(ctx context.Context, db *store.DB, snapshot, copied, liveTree string) (lost []string, err error) {
	missing := map[string]bool{}
	err = store.ViewFile(ctx, snapshot, func(q store.Reader) error {
		return files.LiveBlobPaths(ctx, q, func(p string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !regularFile(filepath.Join(copied, filepath.FromSlash(p))) {
				missing[p] = true
			}
			return nil
		})
	})
	if err == nil && len(missing) > 0 {
		stillLive := map[string]bool{}
		err = db.View(ctx, func(tx *store.Tx) error {
			return files.LiveBlobPaths(ctx, tx, func(p string) error {
				if missing[p] {
					stillLive[p] = true
				}
				return nil
			})
		})
		var raced []string
		for p := range missing {
			if stillLive[p] && !regularFile(filepath.Join(liveTree, filepath.FromSlash(p))) {
				lost = append(lost, TreeRoot+"/"+filesTree+"/"+p)
			} else {
				raced = append(raced, TreeRoot+"/"+filesTree+"/"+p)
			}
		}
		sort.Strings(lost)
		sort.Strings(raced)
		if err == nil && len(raced) > 0 {
			return nil, contracts.Wrap(contracts.ErrUnavailable, ErrBlobMissing,
				fmt.Sprintf("backup: %d file(s) the database snapshot still names were removed while the trees were copied (compaction); take the backup again", len(raced))).
				WithDetails("missing", raced[:min(len(raced), 10)])
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, contracts.Wrap(contracts.ErrTimeout, ctx.Err(), "backup cancelled")
		}
		return nil, err
	}
	return lost, nil
}

func regularFile(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.Mode().IsRegular()
}

// normalizeTrees validates, de-duplicates and sorts tree names; nil means the defaults.
func normalizeTrees(in []string) ([]string, error) {
	if in == nil {
		in = DefaultTrees
	}
	seen := map[string]bool{}
	out := []string{}
	for _, t := range in {
		if !treeNameRE.MatchString(t) {
			return nil, contracts.Newf(contracts.ErrValidationFailed, "backup: %q is not a plain tree name", t)
		}
		if err := checkRestorableTree(t); err != nil {
			return nil, contracts.Newf(contracts.ErrValidationFailed, "backup: %v", err)
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out, nil
}

// within reports whether path is dir or below it.
func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// transientDirs are the scratch directories of the default trees, as listed
// paths: an upload being received (internal/files) and a package install being
// staged (internal/apppkg). No committed row names anything in them (each is
// renamed into place inside the transaction that records it), and their names
// are outside the path grammar, so a backup leaves them out.
var transientDirs = []string{TreeRoot + "/files/.tmp", TreeRoot + "/packages/*/.staging-*"}

func transientDir(listed string) bool {
	for _, pattern := range transientDirs {
		if ok, _ := path.Match(pattern, listed); ok {
			return true
		}
	}
	return false
}

// copyTree copies the regular files below src into dst and lists them with the
// prefix (the path inside the backup). A missing src is an empty tree. A link
// or special file, or a name outside the safe path grammar, refuses the backup:
// a backup that silently left something out would be a false statement. The
// transientDirs are not copied. A file that vanishes while it is being copied
// (compaction removing a tombstoned blob) is skipped; checkBlobs refuses the
// backup if a live row of the snapshot still names it.
func copyTree(ctx context.Context, src, dst, prefix string) ([]FileEntry, error) {
	st, err := os.Lstat(src)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, unavailable(err, "backup cannot read a file tree")
	}
	if !st.IsDir() {
		return nil, contracts.Newf(contracts.ErrValidationFailed, "backup: %s is not a directory", src)
	}
	var out []FileEntry
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		listed := prefix + "/" + filepath.ToSlash(rel)
		if d.IsDir() {
			if transientDir(listed) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return contracts.Newf(contracts.ErrValidationFailed, "backup: %s is a link or special file", listed)
		}
		if err := trust.ValidateListedPath(listed); err != nil {
			return err
		}
		sum, size, err := copyHashed(p, filepath.Join(dst, rel))
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		out = append(out, FileEntry{Path: listed, Size: size, SHA256: sum})
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, contracts.Wrap(contracts.ErrTimeout, ctx.Err(), "backup cancelled")
		}
		var ce *contracts.Error
		if errors.As(err, &ce) {
			return nil, err
		}
		return nil, unavailable(err, "backup cannot copy a file tree")
	}
	return out, nil
}
