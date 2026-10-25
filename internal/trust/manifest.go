package trust

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Fixed names and limits of the signature manifest
// (schemas/application-package/signature-manifest.schema.json and
// docs/integration/application-package.md section 13).
const (
	// FormatV1 is the value of the manifest's "format" property.
	FormatV1 = "alixai.signature-manifest/v1"
	// SignaturesDir is the package-root directory that holds the signature
	// material. Everything under it is excluded from the signed file list: the
	// signature cannot cover itself. It may hold nothing else than
	// SignatureManifestFile.
	SignaturesDir = "signatures"
	// SignatureManifestFile is the file name inside SignaturesDir.
	SignatureManifestFile = "signature-manifest.json"
	// SignatureManifestPath is the fixed package-relative path of the manifest.
	SignatureManifestPath = SignaturesDir + "/" + SignatureManifestFile
	// ManifestFile is the package's own manifest, always part of the file list.
	ManifestFile = "alixai.yaml"

	// HashSHA256 and AlgEd25519 are the only digest and signature algorithms.
	HashSHA256 = "sha256"
	AlgEd25519 = "ed25519"

	// MaxFiles bounds the file list (schema maxItems).
	MaxFiles = 100000
	// MaxPathLen bounds one path (schema maxLength).
	MaxPathLen = 240
	// MaxFileSize is the largest file the manifest can describe (schema maximum).
	MaxFileSize = int64(1) << 40
	// MaxSignatures bounds the signatures array (schema maxItems).
	MaxSignatures = 4
	// MaxSignatureManifestBytes bounds the manifest file read from a package,
	// so a hostile package cannot make the verifier read without limit. A full
	// 100000-entry list is about 25 MB.
	MaxSignatureManifestBytes = 32 << 20
)

var (
	// pathRE is defs.schema.json relativePath.
	pathRE    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*(/[A-Za-z0-9_][A-Za-z0-9._-]*)*$`)
	segmentRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
	sha256RE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// semverRE is defs.schema.json semver.
	semverRE = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
	// appIDRE is defs.schema.json applicationId (the identifier part is
	// checked by validIdentifier as well).
	appIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	// timestampRE is timestamps.schema.json timestamp: UTC, literal Z.
	timestampRE = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]{1,9})?Z$`)
)

// FileEntry is one file of the integrity manifest.
type FileEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Signed is the signed payload of a package: the integrity manifest. Every
// signature covers CanonicalJSON(Signed).
type Signed struct {
	ApplicationID  string      `json:"application_id"`
	PackageVersion string      `json:"package_version"`
	CreatedAt      string      `json:"created_at"`
	HashAlgorithm  string      `json:"hash_algorithm"`
	Files          []FileEntry `json:"files"`
}

// Signature is one detached Ed25519 signature over the canonical JSON of a
// payload. The same record signs packages, audit reports and release manifests.
type Signature struct {
	KeyID     string `json:"key_id"`
	Algorithm string `json:"algorithm"`
	Signature string `json:"signature"`
	SignedAt  string `json:"signed_at"`
	ExpiresAt string `json:"expires_at"`
}

// SignatureManifest is the content of signatures/signature-manifest.json.
type SignatureManifest struct {
	Format     string      `json:"format"`
	Signed     Signed      `json:"signed"`
	Signatures []Signature `json:"signatures"`
}

// Canonical returns CanonicalJSON(s), the exact bytes every signature covers.
func (s Signed) Canonical() ([]byte, error) { return CanonicalJSON(s) }

// ValidatePath checks p against the package path grammar (defs.schema.json
// relativePath): forward slashes only, no leading slash, no drive letter, no
// empty, "." or ".." segment, no segment starting with a dot, each segment
// made of letters, digits, '.', '_' and '-', at most 240 characters in all.
// It does not forbid the signatures/ prefix: ValidateListedPath does.
func ValidatePath(p string) error {
	if len(p) == 0 || len(p) > MaxPathLen || !pathRE.MatchString(p) {
		return fail(contracts.ErrValidationFailed, ErrUnsafePackage, "path %q is outside the safe path grammar", clip(p))
	}
	return nil
}

// ValidateListedPath is ValidatePath plus the rule that nothing under
// signatures/ is ever listed.
func ValidateListedPath(p string) error {
	if err := ValidatePath(p); err != nil {
		return err
	}
	if p == SignaturesDir || strings.HasPrefix(p, SignaturesDir+"/") {
		return fail(contracts.ErrValidationFailed, ErrUnsafePackage, "path %q is under %s/, which is never listed", p, SignaturesDir)
	}
	return nil
}

func clip(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

// PackageIdentity names what a manifest is built for.
type PackageIdentity struct {
	ApplicationID  string
	PackageVersion string
}

func (id PackageIdentity) validate() error {
	if !appIDRE.MatchString(id.ApplicationID) {
		return fail(contracts.ErrValidationFailed, nil, "application id %q is not a valid application id", clip(id.ApplicationID))
	}
	if len(id.PackageVersion) > 64 || !semverRE.MatchString(id.PackageVersion) {
		return fail(contracts.ErrValidationFailed, nil, "package version %q is not a semantic version", clip(id.PackageVersion))
	}
	return nil
}

// formatTime renders t as the schema's timestamp: UTC, whole seconds, Z.
func formatTime(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(time.RFC3339Nano)
}

// parseTimestamp parses the schema's timestamp grammar (UTC, literal Z).
func parseTimestamp(s string) (time.Time, error) {
	if !timestampRE.MatchString(s) {
		return time.Time{}, fmt.Errorf("%q is not a UTC timestamp ending in Z", clip(s))
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a valid timestamp: %v", clip(s), err)
	}
	return t.UTC(), nil
}

// Problem is an entry of a package that cannot be listed.
type problem struct {
	path   string
	detail string
}

// listing is what scanFS found.
type listing struct {
	// files are the regular files outside signatures/, sorted by byte order.
	files []string
	// problems are entries that must fail the package. Nothing is skipped.
	problems []problem
}

// scanFS lists the package. It follows these fixed rules and has no skip list:
//
//   - only directories and regular files are accepted; a symbolic link, a
//     Windows junction or other reparse point, a device, a pipe or a socket
//     is a problem (and is never opened or followed);
//   - every path segment must satisfy the safe grammar, so a hidden entry
//     such as .git or .DS_Store is a problem, not something skipped;
//   - the top-level signatures directory is not listed; it may contain
//     nothing but signature-manifest.json (a regular file), anything else is
//     a problem, because an unlisted file there would be unsigned content;
//   - two paths that differ only by letter case are a problem, because they
//     collide on a case-insensitive file system.
func scanFS(fsys fs.FS) (*listing, error) {
	l := &listing{}
	if err := scanDir(fsys, ".", l); err != nil {
		return nil, err
	}
	sort.Strings(l.files)
	seen := map[string]string{}
	for _, f := range l.files {
		k := strings.ToLower(f)
		if prev, dup := seen[k]; dup {
			l.problems = append(l.problems, problem{f, fmt.Sprintf("differs only by case from %q", prev)})
			continue
		}
		seen[k] = f
	}
	sort.Slice(l.problems, func(i, j int) bool { return l.problems[i].path < l.problems[j].path })
	return l, nil
}

func scanDir(fsys fs.FS, dir string, l *listing) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return fail(contracts.ErrUnavailable, err, "cannot read directory %q", dir)
	}
	for _, e := range entries {
		name := e.Name()
		rel := name
		if dir != "." {
			rel = dir + "/" + name
		}
		if len(l.files)+len(l.problems) > MaxFiles {
			return fail(contracts.ErrResourceExhausted, nil, "package has more than %d entries", MaxFiles)
		}
		if !segmentRE.MatchString(name) || len(rel) > MaxPathLen {
			l.problems = append(l.problems, problem{rel, "path is outside the safe path grammar"})
			continue
		}
		typ := e.Type()
		if dir == "." && strings.EqualFold(name, SignaturesDir) {
			scanSignaturesEntry(fsys, e, rel, l)
			continue
		}
		switch {
		case typ.IsDir():
			if err := scanDir(fsys, rel, l); err != nil {
				return err
			}
		case typ.IsRegular():
			l.files = append(l.files, rel)
		default:
			l.problems = append(l.problems, problem{rel, fmt.Sprintf("is not a regular file or directory (%s)", describeMode(typ))})
		}
	}
	return nil
}

func scanSignaturesEntry(fsys fs.FS, e fs.DirEntry, rel string, l *listing) {
	if rel != SignaturesDir {
		l.problems = append(l.problems, problem{rel, "name differs only by case from the reserved signatures directory"})
		return
	}
	if !e.Type().IsDir() {
		l.problems = append(l.problems, problem{rel, "the reserved name signatures must be a directory"})
		return
	}
	inner, err := fs.ReadDir(fsys, SignaturesDir)
	if err != nil {
		l.problems = append(l.problems, problem{rel, "cannot be read: " + err.Error()})
		return
	}
	for _, ie := range inner {
		p := SignaturesDir + "/" + ie.Name()
		switch {
		case ie.Name() != SignatureManifestFile:
			l.problems = append(l.problems, problem{p, "signatures/ may hold only " + SignatureManifestFile})
		case !ie.Type().IsRegular():
			l.problems = append(l.problems, problem{p, "is not a regular file (" + describeMode(ie.Type()) + ")"})
		}
	}
}

func describeMode(m fs.FileMode) string {
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
	default:
		return "mode " + m.String()
	}
}

// problemsError turns the problems of a scan into the error of a manifest
// build: contracts.ErrValidationFailed, cause ErrUnsafePackage.
func problemsError(ps []problem) error {
	list := make([]map[string]string, 0, len(ps))
	for i, p := range ps {
		if i == maxReportedDifferences {
			break
		}
		list = append(list, map[string]string{"path": p.path, "problem": p.detail})
	}
	first := ps[0]
	return fail(contracts.ErrValidationFailed, ErrUnsafePackage, "package cannot be listed: %q %s (%d problem(s))", clip(first.path), first.detail, len(ps)).
		WithDetails("problems", list)
}

// hashFile returns the entry of one regular file. It opens the file, requires
// the opened object to be a regular file again (a path swapped for a link
// after the scan fails here) and requires the bytes read to match the size.
func hashFile(fsys fs.FS, p string) (FileEntry, error) {
	f, err := fsys.Open(p)
	if err != nil {
		return FileEntry{}, fail(contracts.ErrUnavailable, err, "cannot open %q", p)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return FileEntry{}, fail(contracts.ErrUnavailable, err, "cannot stat %q", p)
	}
	if !st.Mode().IsRegular() {
		return FileEntry{}, fail(contracts.ErrValidationFailed, ErrUnsafePackage, "%q is not a regular file (%s)", p, describeMode(st.Mode().Type()))
	}
	if st.Size() > MaxFileSize {
		return FileEntry{}, fail(contracts.ErrResourceExhausted, nil, "%q is larger than %d bytes", p, MaxFileSize)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return FileEntry{}, fail(contracts.ErrUnavailable, err, "cannot read %q", p)
	}
	if n != st.Size() {
		return FileEntry{}, fail(contracts.ErrConflict, nil, "%q changed while it was being hashed", p)
	}
	return FileEntry{Path: p, SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}

// BuildSigned builds the integrity manifest of the package in fsys: every
// regular file outside signatures/, sorted by path, with SHA-256 and size,
// created_at taken from now (UTC, whole seconds). It fails, naming the entry,
// on anything scanFS refuses, and when alixai.yaml is absent. Nothing is
// skipped silently.
//
// BuildSigned reads each file once; it does not guard against the package
// being modified concurrently by someone else with write access (a file
// swapped between the scan and the hash is detected for links and size, not
// for same-size content). Sign and verify a directory that nobody else can
// write to, or work on a private copy.
func BuildSigned(fsys fs.FS, id PackageIdentity, now time.Time) (Signed, error) {
	if err := id.validate(); err != nil {
		return Signed{}, err
	}
	l, err := scanFS(fsys)
	if err != nil {
		return Signed{}, err
	}
	if len(l.problems) > 0 {
		return Signed{}, problemsError(l.problems)
	}
	files, err := hashFiles(fsys, l.files)
	if err != nil {
		return Signed{}, err
	}
	hasManifest := false
	for _, f := range files {
		if f.Path == ManifestFile {
			hasManifest = true
		}
	}
	if !hasManifest {
		return Signed{}, fail(contracts.ErrValidationFailed, nil, "package has no %s", ManifestFile)
	}
	return Signed{
		ApplicationID:  id.ApplicationID,
		PackageVersion: id.PackageVersion,
		CreatedAt:      formatTime(now),
		HashAlgorithm:  HashSHA256,
		Files:          files,
	}, nil
}

func hashFiles(fsys fs.FS, paths []string) ([]FileEntry, error) {
	out := make([]FileEntry, 0, len(paths))
	for _, p := range paths {
		e, err := hashFile(fsys, p)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// DirFS returns an fs.FS for the package directory root. The root itself must
// be a real directory: a symbolic link (or junction) as the package root is
// refused. Entries below it are checked by the scan.
func DirFS(root string) (fs.FS, error) {
	st, err := os.Lstat(root)
	if err != nil {
		return nil, fail(contracts.ErrValidationFailed, err, "package directory %q cannot be read", root)
	}
	if !st.IsDir() {
		return nil, fail(contracts.ErrValidationFailed, ErrUnsafePackage, "package root %q is not a directory (%s)", root, describeMode(st.Mode().Type()))
	}
	return os.DirFS(root), nil
}

// BuildSignedDir is BuildSigned for a directory on disk.
func BuildSignedDir(root string, id PackageIdentity, now time.Time) (Signed, error) {
	fsys, err := DirFS(root)
	if err != nil {
		return Signed{}, err
	}
	return BuildSigned(fsys, id, now)
}
