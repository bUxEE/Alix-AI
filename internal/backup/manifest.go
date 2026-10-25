package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Names and constants of the backup format.
const (
	// ManifestFormat is the format string of manifest.json.
	ManifestFormat = "alixai.backup-manifest.v1"
	// ManifestKind is inside the signed part, so a signature made for another
	// kind of payload never verifies as a backup.
	ManifestKind = "backup-manifest"
	// ManifestFile is the manifest's file name inside a backup directory.
	ManifestFile = "manifest.json"
	// DatabaseFile is the snapshot's name in a backup and in the data directory.
	DatabaseFile = store.DefaultFileName
	// TreeRoot is the directory of a backup that holds the copied file trees.
	TreeRoot = "tree"

	hashAlgorithm = "sha256"
	// DefaultValidity is how long a backup signature stays valid.
	DefaultValidity = 10 * 366 * 24 * time.Hour

	maxManifestBytes = 64 << 20
	timeLayout       = "2006-01-02T15:04:05Z"
)

// DefaultTrees are the directories of the data directory that hang off the
// database: content-addressed file blobs (internal/files) and installed
// application packages (internal/apppkg).
var DefaultTrees = []string{"files", "packages"}

// restorableTrees are the only trees a backup may hold, and so the only
// directories of the data directory a restore replaces (L4). A signed manifest
// naming any other tree is refused: whoever holds the backup key must not be
// able to replace keys/ (the checkpoint and backup signing keys), external/
// (the external-mode secrets) or upgrade/ through a restore.
var restorableTrees = map[string]bool{"files": true, "packages": true}

func checkRestorableTree(t string) error {
	if !restorableTrees[t] {
		return fmt.Errorf("%q is not a tree a backup holds (only files and packages)", t)
	}
	return nil
}

var (
	backupIDRE = regexp.MustCompile(`^bkp_[0-9A-Z]{26}$`)
	treeNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	hexRE      = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Manifest is manifest.json.
type Manifest struct {
	Format     string            `json:"format"`
	Signed     Signed            `json:"signed"`
	Signatures []trust.Signature `json:"signatures"`
}

// Signed is the part the signatures cover (canonical JSON, RFC 8785).
type Signed struct {
	Kind          string `json:"kind"`
	ApplicationID string `json:"application_id"`
	BackupID      string `json:"backup_id"`
	CreatedAt     string `json:"created_at"`
	Version       string `json:"version"`
	HashAlgorithm string `json:"hash_algorithm"`
	// Trees are the file trees that were in scope, existing or not: a restore
	// replaces exactly these, so a tree that was empty then is empty after.
	Trees []string    `json:"trees"`
	Files []FileEntry `json:"files"`
}

// FileEntry is one file of the backup, by its path inside the backup directory.
type FileEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func manifestErr(format string, a ...any) error {
	return contracts.Newf(contracts.ErrSchemaInvalid, "backup manifest: "+format, a...)
}

// validate checks the signed part against the rules of the format. A manifest
// that lists a path outside the grammar is refused here, before anything is
// read from or written to the paths it names.
func (s Signed) validate() error {
	if s.Kind != ManifestKind {
		return manifestErr("kind is %q, want %q", s.Kind, ManifestKind)
	}
	if s.ApplicationID != trust.PlatformApplicationID {
		return manifestErr("application_id is %q, want %q", s.ApplicationID, trust.PlatformApplicationID)
	}
	if !backupIDRE.MatchString(s.BackupID) {
		return manifestErr("backup_id %q is malformed", s.BackupID)
	}
	if t, err := time.Parse(timeLayout, s.CreatedAt); err != nil || t.Format(timeLayout) != s.CreatedAt {
		return manifestErr("created_at %q is not a UTC timestamp like 2026-10-03T12:00:00Z", s.CreatedAt)
	}
	if s.HashAlgorithm != hashAlgorithm {
		return manifestErr("hash_algorithm is %q, want %q", s.HashAlgorithm, hashAlgorithm)
	}
	trees := map[string]bool{}
	prev := ""
	for i, t := range s.Trees {
		if !treeNameRE.MatchString(t) {
			return manifestErr("trees[%d] %q is not a plain tree name", i, t)
		}
		if err := checkRestorableTree(t); err != nil {
			return manifestErr("trees[%d]: %v", i, err)
		}
		if i > 0 && t <= prev {
			return manifestErr("trees is not sorted strictly ascending at %q", t)
		}
		prev = t
		trees[t] = true
	}
	sawDB := false
	prev = ""
	for i, f := range s.Files {
		if err := trust.ValidateListedPath(f.Path); err != nil {
			return manifestErr("files[%d]: %v", i, err)
		}
		if i > 0 && f.Path <= prev {
			return manifestErr("files is not sorted strictly ascending at %q", f.Path)
		}
		prev = f.Path
		switch {
		case f.Path == DatabaseFile:
			sawDB = true
		case strings.HasPrefix(f.Path, TreeRoot+"/"):
			name, rest, _ := strings.Cut(strings.TrimPrefix(f.Path, TreeRoot+"/"), "/")
			if !trees[name] || rest == "" {
				return manifestErr("files[%d] %q is not inside a listed tree", i, f.Path)
			}
		default:
			return manifestErr("files[%d] %q is neither the database nor inside a tree", i, f.Path)
		}
		if !hexRE.MatchString(f.SHA256) {
			return manifestErr("files[%d] (%s): sha256 is not 64 lower-case hex characters", i, f.Path)
		}
		if f.Size < 0 {
			return manifestErr("files[%d] (%s): negative size", i, f.Path)
		}
	}
	if !sawDB {
		return manifestErr("the database %s is not listed", DatabaseFile)
	}
	return nil
}

// parseManifest parses raw as strictly as internal/trust parses a package
// signature manifest: acceptable canonical JSON (no duplicate key, no float,
// valid UTF-8), no unknown, missing or differently cased property, and the
// rules of the format. It checks no signature.
func parseManifest(raw []byte) (*Manifest, error) {
	canon, err := trust.Canonicalize(raw)
	if err != nil {
		return nil, err
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(canon))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, contracts.Wrap(contracts.ErrSchemaInvalid, err, "backup manifest does not match the format")
	}
	again, err := trust.CanonicalJSON(m)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(canon, again) {
		return nil, manifestErr("a property is missing, null or spelt with other letter case")
	}
	if m.Format != ManifestFormat {
		return nil, manifestErr("format is %q, want %q", m.Format, ManifestFormat)
	}
	if err := m.Signed.validate(); err != nil {
		return nil, err
	}
	if len(m.Signatures) > trust.MaxSignatures {
		return nil, manifestErr("%d signatures, at most %d are allowed", len(m.Signatures), trust.MaxSignatures)
	}
	return &m, nil
}

// loadManifest reads manifest.json from a backup directory, parses it,
// verifies a signature over its signed part with keys and checks that the
// directory holds exactly the listed files and nothing else. It fails closed:
// no signature, an unknown, revoked or expired key, one changed byte under
// signed, a missing, extra, linked or special file are all errors. It does not
// read the listed files' content (stage does, while copying).
func loadManifest(ctx context.Context, dir string, keys trust.KeyLookup, clk clock.Clock) (*Manifest, error) {
	if keys == nil {
		return nil, contracts.New(contracts.ErrUnauthenticated, "backup: no keys to verify the manifest with")
	}
	path := filepath.Join(dir, ManifestFile)
	st, err := os.Lstat(path)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "backup manifest cannot be read")
	}
	if !st.Mode().IsRegular() || st.Size() > maxManifestBytes {
		return nil, manifestErr("%s is not a regular file of acceptable size", ManifestFile)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "backup manifest cannot be read")
	}
	m, err := parseManifest(raw)
	if err != nil {
		return nil, err
	}
	if _, err := trust.NewVerifier(keys, clk).VerifyPayload(ctx, m.Signed, m.Signatures, trust.PayloadOptions{ApplicationID: trust.PlatformApplicationID}); err != nil {
		return nil, err
	}
	if err := checkListing(dir, m); err != nil {
		return nil, err
	}
	return m, nil
}

// checkListing compares the files of dir with the manifest: every listed file
// exists as a regular file of the listed size, and nothing else is there.
func checkListing(dir string, m *Manifest) error {
	listed := make(map[string]FileEntry, len(m.Signed.Files))
	for _, f := range m.Signed.Files {
		listed[f.Path] = f
	}
	seen := map[string]bool{}
	var problems []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case rel == ".":
			return nil
		case d.IsDir():
			return nil
		case !d.Type().IsRegular():
			problems = append(problems, fmt.Sprintf("%s is a link or special file", rel))
		case rel == ManifestFile:
		default:
			f, ok := listed[rel]
			if !ok {
				problems = append(problems, fmt.Sprintf("%s is in the backup but not in the manifest", rel))
				return nil
			}
			seen[rel] = true
			if info, err := d.Info(); err != nil || info.Size() != f.Size {
				problems = append(problems, fmt.Sprintf("%s has a different size from the manifest (truncated or modified)", rel))
			}
		}
		return nil
	})
	if err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "backup directory cannot be read")
	}
	for _, f := range m.Signed.Files {
		if !seen[f.Path] {
			problems = append(problems, fmt.Sprintf("%s is listed but missing", f.Path))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		if len(problems) > 10 {
			problems = problems[:10]
		}
		return contracts.Newf(contracts.ErrValidationFailed, "backup is not what its manifest says: %s", strings.Join(problems, "; ")).
			WithDetails("problems", problems)
	}
	return nil
}
