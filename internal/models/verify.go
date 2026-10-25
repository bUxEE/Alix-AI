package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// RuntimeInfo describes the runtime that is about to load an artefact: the
// serving runtime (for example "localai") with its version, and the inference
// backend inside it (for example "llama-cpp") with its version or image digest.
type RuntimeInfo struct {
	Runtime        string
	Version        string
	Backend        string
	BackendVersion string
}

// RuntimeCheck is the hook for a deployment-level allowlist: it returns nil to
// allow the entry to run on rt and an error to refuse. Use it for what the
// registry cannot know, such as the runtime and backend versions a release
// manifest approves (master plan 16.3).
type RuntimeCheck func(entry Entry, rt RuntimeInfo) error

// Allowlist is a ready-made RuntimeCheck: the runtime versions and backend
// versions a deployment approves, each keyed by name. A runtime or backend
// that is missing from the maps, or has an empty list, allows nothing.
type Allowlist struct {
	// Runtime maps a runtime name to the approved versions.
	Runtime map[string][]string
	// Backend maps a backend name to the approved versions or image digests.
	Backend map[string][]string
}

// Check implements RuntimeCheck.
func (a Allowlist) Check(_ Entry, rt RuntimeInfo) error {
	if !slices.Contains(a.Runtime[rt.Runtime], rt.Version) || rt.Version == "" {
		return fmt.Errorf("runtime %s version %q is not on the allowlist", rt.Runtime, rt.Version)
	}
	if !slices.Contains(a.Backend[rt.Backend], rt.BackendVersion) || rt.BackendVersion == "" {
		return fmt.Errorf("backend %s version %q is not on the allowlist", rt.Backend, rt.BackendVersion)
	}
	return nil
}

// VerifyOption tunes Verify.
type VerifyOption func(*verifyConfig)

type verifyConfig struct {
	ctx    context.Context
	rt     *RuntimeInfo
	checks []RuntimeCheck
}

// WithContext lets the caller cancel hashing of a large file.
func WithContext(ctx context.Context) VerifyOption {
	return func(c *verifyConfig) { c.ctx = ctx }
}

// WithRuntime also checks the runtime the artefact will be loaded by. The
// entry's own list always applies: the runtime and backend names must match the
// entry's backend, and rt.Version must be in backend.allowed_runtime_versions
// (an empty list allows none). Each extra check, typically Allowlist.Check,
// must also return nil.
func WithRuntime(rt RuntimeInfo, extra ...RuntimeCheck) VerifyOption {
	return func(c *verifyConfig) {
		c.rt = &rt
		c.checks = append(c.checks, extra...)
	}
}

// unavailable builds the error Verify returns: code unavailable (master plan
// 3 rule 14, SEC-063: a mismatch refuses activation and reports unavailable)
// wrapping the specific sentinel so errors.Is can tell the reasons apart.
func unavailable(entry Entry, sentinel error, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	return contracts.Wrap(contracts.ErrUnavailable, sentinel, fmt.Sprintf("model %s: %s", entry.ID, msg)).
		WithDetails("model", entry.ID)
}

// Verify checks that the installed artefact at path is the one the registry
// entry names, before the inference path may use it (SEC-063, master plan
// 16.3). It returns nil only when every check passes; otherwise it returns a
// *contracts.Error with code unavailable that wraps one of ErrInvalidEntry,
// ErrNotActivatable, ErrNotPinned, ErrRuntimeNotAllowed, ErrArtefactUnreadable,
// ErrSizeMismatch or ErrHashMismatch.
//
// The checks, in order:
//
//  1. the entry passes its own rules (Entry.Validate) and is not rejected or
//     retired;
//  2. the entry pins a sha256. A candidate without one has nothing to be
//     compared with, so it is refused: there is no unverified-but-allowed path;
//  3. with WithRuntime, the runtime and backend version are allowed;
//  4. path is a regular file whose size equals the entry's recorded size;
//  5. the SHA-256 of the file's content, streamed rather than read into memory,
//     equals the entry's.
//
// path may be a symlink: the file it resolves to is the one that is opened,
// statted and hashed, so the check applies to the content that was read. A
// file replaced after Verify returns is outside what a point-in-time check can
// cover; the caller should verify at startup and again at activation, and keep
// the models directory read-only to the runtime (hardening checklist HC-24).
func Verify(entry Entry, path string, opts ...VerifyOption) error {
	cfg := verifyConfig{ctx: context.Background()}
	for _, o := range opts {
		o(&cfg)
	}

	if vs := entry.Validate(); len(vs) > 0 {
		return unavailable(entry, ErrInvalidEntry, "entry is invalid: %s", vs[0].Error())
	}
	if entry.Status == StatusRejected || entry.Status == StatusRetired {
		return unavailable(entry, ErrNotActivatable, "status is %s", entry.Status)
	}
	if entry.Artifact.SHA256 == nil {
		return unavailable(entry, ErrNotPinned, "the registry pins no sha256 for %s, so the file cannot be verified", entry.ID)
	}
	want := *entry.Artifact.SHA256

	if cfg.rt != nil {
		if err := checkRuntime(entry, *cfg.rt, cfg.checks); err != nil {
			return unavailable(entry, ErrRuntimeNotAllowed, "%v", err)
		}
	}

	got, size, err := hashRegularFile(cfg.ctx, path, entry.Artifact.SizeBytes)
	if err != nil {
		var sizeErr *sizeMismatchError
		switch {
		case errors.As(err, &sizeErr):
			return unavailable(entry, ErrSizeMismatch, "%s is %d bytes, the registry records %d", filepath.Base(path), sizeErr.got, sizeErr.want)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return contracts.Wrap(contracts.ErrTimeout, err, fmt.Sprintf("model %s: verification was cancelled", entry.ID)).
				WithDetails("model", entry.ID)
		default:
			return unavailable(entry, errors.Join(ErrArtefactUnreadable, err), "%s cannot be read", filepath.Base(path))
		}
	}
	if got != want {
		return unavailable(entry, ErrHashMismatch, "%s (%d bytes) has sha256 %s, the registry pins %s", filepath.Base(path), size, got, want)
	}
	return nil
}

// checkRuntime applies the entry's own backend list and then each extra check.
func checkRuntime(entry Entry, rt RuntimeInfo, extra []RuntimeCheck) error {
	b := entry.Backend
	if rt.Runtime != b.Runtime || rt.Backend != b.Name {
		return fmt.Errorf("entry is for %s/%s, not %s/%s", b.Runtime, b.Name, rt.Runtime, rt.Backend)
	}
	if rt.Version == "" || !slices.Contains(b.AllowedRuntimeVersions, rt.Version) {
		return fmt.Errorf("%s version %q is not among the versions this entry was qualified with %v", rt.Runtime, rt.Version, b.AllowedRuntimeVersions)
	}
	for _, check := range extra {
		if check == nil {
			return errors.New("a nil runtime check is not an allow")
		}
		if err := check(entry, rt); err != nil {
			return err
		}
	}
	return nil
}

type sizeMismatchError struct{ got, want int64 }

func (e *sizeMismatchError) Error() string {
	return fmt.Sprintf("size %d does not match recorded size %d", e.got, e.want)
}

// HashFile returns the lower-case hex SHA-256 and the size of the regular file
// at path. It is for tools that pin a new entry (the M0-13 spike); Verify uses
// the same code.
func HashFile(ctx context.Context, path string) (sum string, size int64, err error) {
	return hashRegularFile(ctx, path, nil)
}

// hashRegularFile hashes the file at path. It refuses anything that is not a
// regular file before opening it (opening a FIFO or device can block forever),
// stats the opened handle again so what is hashed is what was checked, and, if
// wantSize is set, rejects a size difference before reading the content.
func hashRegularFile(ctx context.Context, path string, wantSize *int64) (string, int64, error) {
	if path == "" {
		return "", 0, errors.New("empty path")
	}
	li, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if li.Mode()&os.ModeSymlink == 0 && !li.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !fi.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	if wantSize != nil && fi.Size() != *wantSize {
		return "", 0, &sizeMismatchError{got: fi.Size(), want: *wantSize}
	}

	h := sha256.New()
	buf := make([]byte, 1<<20)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return "", n, err
		}
		k, rerr := f.Read(buf)
		if k > 0 {
			h.Write(buf[:k])
			n += int64(k)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", n, rerr
		}
	}
	if n != fi.Size() {
		// The file changed while it was being read: what was hashed is not
		// a consistent snapshot.
		return "", n, fmt.Errorf("file changed during verification (read %d bytes, expected %d)", n, fi.Size())
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
