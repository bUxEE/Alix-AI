package packs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/voice/profile"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Why a pack's files were refused. Each is wrapped in an unavailable error (the
// code models.Verify uses for the same findings, SEC-063), so errors.Is tells
// the reasons apart while the caller sees one fail-closed code.
var (
	ErrHashMismatch   = errors.New("sha256 does not match the manifest")
	ErrSizeMismatch   = errors.New("size does not match the manifest")
	ErrMissingFile    = errors.New("file is missing or is not a regular file")
	ErrUnexpectedFile = errors.New("file is not part of the manifest")
	ErrStaleInstall   = errors.New("installed from other pins than the approved manifest")
)

const (
	activeMarkerFile = ".active"
	maxRecordBytes   = 64 << 10
)

// Approval is the operator's explicit approval of one pack: its models, their
// licences and the disk they take. It names the pack, so an approval of one
// pack never covers another, and the zero value approves nothing.
type Approval struct {
	PackID string
}

func (a Approval) approves(id string) bool { return a.PackID != "" && a.PackID == id }

// Plan is what installing a pack would put on the machine: shown to the
// operator before approval and returned with a refusal.
type Plan struct {
	ID        string
	Name      string
	Language  string
	Status    string
	DiskBytes int64
	Models    []PlanModel
}

// PlanModel is one model of a Plan.
type PlanModel struct {
	Role    providers.Role
	Name    string
	Engine  string
	Licence Licence
	Files   []File
}

func planOf(m Manifest) Plan {
	p := Plan{ID: m.ID, Name: m.Name, Language: m.Language, Status: m.Status, DiskBytes: m.DiskBytes()}
	for _, r := range []struct {
		role providers.Role
		m    *Model
	}{{providers.RoleSTT, m.Models.STT}, {providers.RoleTTS, m.Models.TTS}, {providers.RoleVAD, m.Models.VAD}} {
		if r.m != nil {
			p.Models = append(p.Models, PlanModel{Role: r.role, Name: r.m.Name, Engine: r.m.Engine, Licence: r.m.Licence, Files: append([]File(nil), r.m.Files...)})
		}
	}
	return p
}

// InstallRequest asks for a pack to be installed from files the operator has
// already fetched. Nothing is downloaded: From is a directory holding each of
// the manifest's files under its manifest name.
type InstallRequest struct {
	PackID   string
	From     string
	Approval Approval
}

// InstallResult describes an install, or, with a refusal, what was refused.
type InstallResult struct {
	Plan Plan
	// Dir is where the pack's files are.
	Dir string
	// Replaced is true when an earlier install of the same pack was replaced.
	Replaced bool
}

// Installed is the record of an installed pack.
type Installed struct {
	Pack           profile.Pack
	ManifestSHA256 string
	InstalledAt    time.Time
	Dir            string
}

type installRecord struct {
	ID             string           `json:"id"`
	ManifestSHA256 string           `json:"manifest_sha256"`
	Version        string           `json:"version"`
	Language       string           `json:"language"`
	Roles          []providers.Role `json:"roles"`
	InstalledAt    time.Time        `json:"installed_at"`
}

type activeRecord struct {
	Pack           string    `json:"pack"`
	ManifestSHA256 string    `json:"manifest_sha256"`
	ActivatedAt    time.Time `json:"activated_at"`
}

// Store is the on-disk language-pack store below one directory (normally
// <data_dir>/voice-packs):
//
//	<root>/<id>/<model files>   the verified files of an installed pack
//	<root>/<id>/.installed.json the install record
//	<root>/.active              the activation marker: at most one exists
//
// Only packs of the approved manifest set can be installed. Directories are
// owner-only (0700) and files 0600 (SEC-081); Windows file modes carry no such
// bits, so there the data directory's ACL is the operator's to restrict.
type Store struct {
	root      string
	manifests Manifests
	clk       clock.Clock
}

// NewStore returns a store rooted at root over the approved manifests. It
// creates nothing: the directory appears with the first install.
func NewStore(root string, manifests Manifests, clk clock.Clock) (*Store, error) {
	if root == "" || clk == nil || len(manifests.byID) == 0 {
		return nil, contracts.New(contracts.ErrValidationFailed, "voice: a language pack store needs a directory, a clock and the approved manifests")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "voice: language pack directory")
	}
	return &Store{root: abs, manifests: manifests, clk: clk}, nil
}

// Root returns the store's directory.
func (s *Store) Root() string { return s.root }

// Dir returns the directory of the pack with the given id (it may not exist).
func (s *Store) Dir(id string) string { return filepath.Join(s.root, id) }

// Manifests returns the approved manifest set.
func (s *Store) Manifests() Manifests { return s.manifests }

// Plan returns what installing the pack would do.
func (s *Store) Plan(id string) (Plan, error) {
	m, err := s.manifest(id)
	if err != nil {
		return Plan{}, err
	}
	return planOf(m), nil
}

func (s *Store) manifest(id string) (Manifest, error) {
	if !profile.ValidPackID(id) {
		return Manifest{}, contracts.Newf(contracts.ErrValidationFailed, "voice: %q is not a language pack id", id)
	}
	m, ok := s.manifests.Lookup(id)
	if !ok {
		return Manifest{}, contracts.Newf(contracts.ErrValidationFailed, "voice: %q is not an approved language pack", id).WithDetails("pack", id)
	}
	return m, nil
}

func unavailable(id string, sentinel error, format string, args ...any) error {
	return contracts.Wrap(contracts.ErrUnavailable, sentinel, fmt.Sprintf("voice: language pack %q: %s", id, fmt.Sprintf(format, args...))).
		WithDetails("pack", id)
}

// Install installs an approved pack from the operator's files, and never
// activates it. It refuses first, before it reads or writes anything, unless
// req.Approval names this pack. Every file is checked while it is copied into a
// staging directory: its size and SHA-256 must be the manifest's, and a single
// mismatch discards the staging directory and installs nothing. Only a complete
// pack is moved into place, replacing an earlier install of the same pack, which
// is refused while that pack is the active one.
func (s *Store) Install(ctx context.Context, req InstallRequest) (InstallResult, error) {
	m, err := s.manifest(req.PackID)
	if err != nil {
		return InstallResult{}, err
	}
	res := InstallResult{Plan: planOf(m), Dir: s.Dir(m.ID)}
	if !req.Approval.approves(m.ID) {
		return res, contracts.Newf(contracts.ErrConfirmationRequired,
			"voice: installing language pack %q needs the operator's explicit approval of its models and licences", m.ID).
			WithDetails("pack", m.ID)
	}
	if err := ctx.Err(); err != nil {
		return res, providers.ContextError(err)
	}
	if req.From == "" {
		return res, contracts.New(contracts.ErrValidationFailed, "voice: install needs the directory holding the model files")
	}
	if cur, ok, err := s.ActiveID(); err != nil {
		return res, err
	} else if ok && cur == m.ID {
		return res, contracts.Newf(contracts.ErrConflict, "voice: language pack %q is active; deactivate it before reinstalling it", m.ID).
			WithDetails("active", cur)
	}

	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return res, contracts.Wrap(contracts.ErrUnavailable, err, "voice: cannot create the language pack directory")
	}
	staging, err := os.MkdirTemp(s.root, ".staging-"+m.ID+"-")
	if err != nil {
		return res, contracts.Wrap(contracts.ErrUnavailable, err, "voice: cannot create a staging directory")
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(staging) // our own staging directory, never a path from the caller
		}
	}()

	for _, rf := range m.Files() {
		if err := copyVerified(ctx, m.ID, filepath.Join(req.From, rf.File.Name), filepath.Join(staging, rf.File.Name), rf.File); err != nil {
			return res, err
		}
	}
	rec := installRecord{ID: m.ID, ManifestSHA256: m.Digest, Version: m.Version, Language: m.Language, Roles: m.Roles(), InstalledAt: s.clk.Now().UTC()}
	if err := writeJSON(filepath.Join(staging, installRecordFile), rec); err != nil {
		return res, err
	}

	final := s.Dir(m.ID)
	var parked string
	if _, err := os.Lstat(final); err == nil {
		// os.Rename cannot replace a directory on Windows: park the old one under a
		// name that does not exist, and put it back if the new one cannot be moved in.
		parked = filepath.Join(s.root, fmt.Sprintf(".old-%s-%d", m.ID, s.clk.Now().UnixNano()))
		if err := os.Rename(final, parked); err != nil {
			return res, contracts.Wrap(contracts.ErrUnavailable, err, "voice: cannot replace the installed pack")
		}
		res.Replaced = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return res, contracts.Wrap(contracts.ErrUnavailable, err, "voice: cannot inspect the pack directory")
	}
	if err := os.Rename(staging, final); err != nil {
		if parked != "" {
			_ = os.Rename(parked, final)
		}
		return res, contracts.Wrap(contracts.ErrConflict, err, "voice: cannot move the pack into place").WithDetails("pack", m.ID)
	}
	keep = true
	if parked != "" {
		_ = os.RemoveAll(parked) // a leftover is harmless: Installed skips names that start with a dot
	}
	return res, nil
}

// copyVerified copies src to dst and fails unless the bytes copied are the
// pinned file. The hash is of the bytes written, not of a second read of src.
func copyVerified(ctx context.Context, id, src, dst string, want File) error {
	in, err := os.Open(src)
	if err != nil {
		return unavailable(id, errors.Join(ErrMissingFile, err), "%s cannot be read from the install directory", want.Name)
	}
	defer in.Close()
	fi, err := in.Stat()
	switch {
	case err != nil:
		return unavailable(id, errors.Join(ErrMissingFile, err), "%s cannot be read from the install directory", want.Name)
	case !fi.Mode().IsRegular():
		return unavailable(id, ErrMissingFile, "%s is not a regular file", want.Name)
	case fi.Size() != want.SizeBytes:
		return unavailable(id, ErrSizeMismatch, "%s is %d bytes, the manifest pins %d", want.Name, fi.Size(), want.SizeBytes)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "voice: cannot write "+want.Name)
	}
	h := sha256.New()
	n, cerr := io.Copy(io.MultiWriter(out, h), io.LimitReader(ctxReader{ctx, in}, want.SizeBytes+1))
	if err := out.Close(); cerr == nil {
		cerr = err
	}
	switch {
	case cerr != nil && (errors.Is(cerr, context.Canceled) || errors.Is(cerr, context.DeadlineExceeded)):
		return providers.ContextError(cerr)
	case cerr != nil:
		return contracts.Wrap(contracts.ErrUnavailable, cerr, "voice: cannot copy "+want.Name)
	case n != want.SizeBytes:
		return unavailable(id, ErrSizeMismatch, "%s changed while it was copied (%d bytes read, %d pinned)", want.Name, n, want.SizeBytes)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want.SHA256 {
		return unavailable(id, ErrHashMismatch, "%s has sha256 %s, the manifest pins %s", want.Name, got, want.SHA256)
	}
	return nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return contracts.Wrap(contracts.ErrInternal, err, "voice: cannot encode a record")
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "voice: cannot write "+filepath.Base(path))
	}
	return nil
}

func readJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxRecordBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxRecordBytes {
		return errors.New("record is too large")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// record reads the install record of id.
func (s *Store) record(id string) (installRecord, error) {
	var rec installRecord
	if err := readJSON(filepath.Join(s.Dir(id), installRecordFile), &rec); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return rec, contracts.Newf(contracts.ErrValidationFailed, "voice: language pack %q is not installed", id).WithDetails("pack", id)
		}
		return rec, contracts.Wrap(contracts.ErrSchemaInvalid, err, fmt.Sprintf("voice: the install record of language pack %q is unreadable", id))
	}
	if rec.ID != id || !profile.ValidPackID(rec.ID) || rec.ManifestSHA256 == "" {
		return rec, contracts.Newf(contracts.ErrSchemaInvalid, "voice: the install record of language pack %q is malformed", id)
	}
	return rec, nil
}

// Installed lists the installed packs in id order from their install records
// alone: it hashes nothing and loads nothing (profile.PackCatalog). A directory
// with an unreadable record is an error, not a pack that is quietly absent.
func (s *Store) Installed(ctx context.Context) ([]Installed, error) {
	if err := ctx.Err(); err != nil {
		return nil, providers.ContextError(err)
	}
	des, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "voice: cannot read the language pack directory")
	}
	var out []Installed
	for _, de := range des {
		if !de.IsDir() || strings.HasPrefix(de.Name(), ".") {
			continue
		}
		rec, err := s.record(de.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, Installed{
			Pack:           profile.Pack{ID: rec.ID, Language: rec.Language, Version: rec.Version, Roles: append([]providers.Role(nil), rec.Roles...)},
			ManifestSHA256: rec.ManifestSHA256, InstalledAt: rec.InstalledAt, Dir: s.Dir(rec.ID),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pack.ID < out[j].Pack.ID })
	return out, nil
}

// Verify checks an installed pack against the approved manifest it is named
// after: it is installed from this manifest (not a stale one), nothing but the
// manifest's files is in its directory, and every file has the pinned size and
// SHA-256. A finding is unavailable (SEC-063); the pack must not be activated
// or served from. Run it at activation and again at startup.
func (s *Store) Verify(ctx context.Context, id string) error {
	if !profile.ValidPackID(id) {
		return contracts.Newf(contracts.ErrValidationFailed, "voice: %q is not a language pack id", id)
	}
	rec, err := s.record(id)
	if err != nil {
		return err
	}
	m, ok := s.manifests.Lookup(id)
	if !ok {
		return unavailable(id, ErrStaleInstall, "this build approves no manifest for it")
	}
	if rec.ManifestSHA256 != m.Digest {
		return unavailable(id, ErrStaleInstall, "it was installed from pins %.12s, this build approves %.12s; install it again", rec.ManifestSHA256, m.Digest)
	}
	dir := s.Dir(id)
	want := map[string]File{}
	for _, rf := range m.Files() {
		want[rf.File.Name] = rf.File
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		return unavailable(id, errors.Join(ErrMissingFile, err), "its directory cannot be read")
	}
	for _, de := range des {
		if _, ok := want[de.Name()]; !ok && de.Name() != installRecordFile {
			return unavailable(id, ErrUnexpectedFile, "%s is in the pack directory but not in the manifest", de.Name())
		}
	}
	for _, rf := range m.Files() {
		f := rf.File
		path := filepath.Join(dir, f.Name)
		if li, err := os.Lstat(path); err != nil || !li.Mode().IsRegular() {
			return unavailable(id, ErrMissingFile, "%s is missing or is not a regular file", f.Name)
		}
		sum, size, err := models.HashFile(ctx, path)
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return providers.ContextError(err)
		case err != nil:
			return unavailable(id, errors.Join(ErrMissingFile, err), "%s cannot be read", f.Name)
		case size != f.SizeBytes:
			return unavailable(id, ErrSizeMismatch, "%s is %d bytes, the manifest pins %d", f.Name, size, f.SizeBytes)
		case sum != f.SHA256:
			return unavailable(id, ErrHashMismatch, "%s has sha256 %s, the manifest pins %s", f.Name, sum, f.SHA256)
		}
	}
	return nil
}

// ActiveID returns the id of the pack the activation marker names. A marker that
// cannot be read is an error: it is never taken to mean that none is active.
func (s *Store) ActiveID() (string, bool, error) {
	var rec activeRecord
	err := readJSON(filepath.Join(s.root, activeMarkerFile), &rec)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", false, nil
	case err != nil || !profile.ValidPackID(rec.Pack):
		return "", false, contracts.New(contracts.ErrSchemaInvalid, "voice: the activation marker is unreadable; deactivate to clear it")
	}
	return rec.Pack, true, nil
}

func activeConflict(cur, id string) error {
	return contracts.Newf(contracts.ErrConflict, "voice: language pack %q is active; deactivate it before activating %q", cur, id).
		WithDetails("active", cur).WithDetails("requested", id)
}

// Activate makes an installed pack the active one, durably: the marker outlives
// the process, so `alixai serve` activates the pack at its next start. At most
// one pack is ever active. A second pack is refused with conflict while another
// is active (the operator deactivates first), and activating the active pack
// again only verifies it. The pack's files are verified first, and a pack that
// fails is not activated. The marker is published exclusively, so two concurrent
// activations cannot both succeed.
func (s *Store) Activate(ctx context.Context, id string) (profile.Pack, error) {
	if !profile.ValidPackID(id) {
		return profile.Pack{}, contracts.Newf(contracts.ErrValidationFailed, "voice: %q is not a language pack id", id)
	}
	if cur, ok, err := s.ActiveID(); err != nil {
		return profile.Pack{}, err
	} else if ok && cur != id {
		return profile.Pack{}, activeConflict(cur, id)
	}
	rec, err := s.record(id)
	if err != nil {
		return profile.Pack{}, err
	}
	if err := s.Verify(ctx, id); err != nil {
		return profile.Pack{}, err
	}
	pack := profile.Pack{ID: rec.ID, Language: rec.Language, Version: rec.Version, Roles: append([]providers.Role(nil), rec.Roles...)}

	// The marker is published whole and exclusively: its content is written to a
	// temporary file, which is then hard-linked to the marker name. The link fails
	// when a marker exists, so two concurrent activations cannot both succeed, and
	// no reader ever sees a marker that is empty or half written.
	marker := filepath.Join(s.root, activeMarkerFile)
	data, err := json.Marshal(activeRecord{Pack: id, ManifestSHA256: rec.ManifestSHA256, ActivatedAt: s.clk.Now().UTC()})
	if err != nil {
		return profile.Pack{}, contracts.Wrap(contracts.ErrInternal, err, "voice: cannot encode the activation")
	}
	tmp, err := os.CreateTemp(s.root, ".active-")
	if err != nil {
		return profile.Pack{}, contracts.Wrap(contracts.ErrUnavailable, err, "voice: cannot record the activation")
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(append(data, '\n'))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return profile.Pack{}, contracts.Wrap(contracts.ErrUnavailable, err, "voice: cannot record the activation")
	}
	err = os.Link(tmp.Name(), marker)
	if errors.Is(err, fs.ErrExist) {
		cur, ok, rerr := s.ActiveID()
		switch {
		case rerr != nil:
			return profile.Pack{}, rerr
		case ok && cur == id:
			return pack, nil
		case ok:
			return profile.Pack{}, activeConflict(cur, id)
		}
		return profile.Pack{}, contracts.New(contracts.ErrConflict, "voice: another activation is in progress")
	}
	if err != nil {
		return profile.Pack{}, contracts.Wrap(contracts.ErrUnavailable, err, "voice: cannot record the activation")
	}
	return pack, nil
}

// Deactivate clears the activation marker. With none active it is a no-op, and
// it clears a marker that cannot be read, which is how the operator recovers
// from one.
func (s *Store) Deactivate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return providers.ContextError(err)
	}
	err := os.Remove(filepath.Join(s.root, activeMarkerFile))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return contracts.Wrap(contracts.ErrUnavailable, err, "voice: cannot clear the activation marker")
	}
	return nil
}

// Catalog is the profile.PackCatalog over the store: metadata only.
func (s *Store) Catalog() profile.PackCatalog { return catalog{s} }

// Loader is the profile.PackLoader over the store: it verifies a pack's files
// before the profile marks it active, and holds nothing, so there is nothing to
// unload. The runtime adapter decides what loading the verified files means.
func (s *Store) Loader() profile.PackLoader { return loader{s} }

type catalog struct{ s *Store }

func (c catalog) List(ctx context.Context) ([]profile.Pack, error) {
	installed, err := c.s.Installed(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]profile.Pack, len(installed))
	for i, in := range installed {
		out[i] = in.Pack
	}
	return out, nil
}

type loader struct{ s *Store }

func (l loader) Load(ctx context.Context, p profile.Pack) error { return l.s.Verify(ctx, p.ID) }
func (loader) Unload(context.Context, profile.Pack) error       { return nil }
