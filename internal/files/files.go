package files

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

//go:embed migrations/*.sql
var migrations embed.FS

func init() { store.RegisterMigrations("files", migrations) }

// IDPrefix is the prefix of a file id ("fil_" plus a ULID).
const IDPrefix = "fil"

// timestampLayout is the spelling of common.Timestamp.
const timestampLayout = "2006-01-02T15:04:05.000Z"

// RAGSources is the seam to the RAG ingestion for the rag_source treatment
// (master plan 14.1: "an explicitly registered RAG source"). The files module
// stores the bytes and the scoped reference; whether and how the file becomes an
// indexed source is the RAG pipeline's. Nothing here parses or indexes.
//
// The pipeline reads the content with Service.Open under the scope it was
// handed, so it never receives a path either.
type RAGSources interface {
	// RegisterFile hands a stored file to the pipeline as a source of the scope.
	// An error means the file is not a source: the upload is rolled back.
	RegisterFile(ctx context.Context, scope contracts.Scope, f RAGFile) error
	// RemoveFile withdraws it, immediately and everywhere (index, caches). It is
	// called when the file is deleted, expires or its tenant is deleted, and the
	// file is not tombstoned until it succeeds (SEC-028: a deleted source is never
	// retrievable).
	RemoveFile(ctx context.Context, loc Location, fileID string) error
}

// RAGFile describes a file registered as a RAG source: metadata only.
type RAGFile struct {
	FileID      string
	Name        string
	MIMEType    string
	SizeBytes   int64
	ContentHash string
}

// Location is a workspace, the part of a Scope a background sweep still knows.
type Location struct {
	ApplicationID string
	TenantID      string
	WorkspaceID   string
}

// VisionGate says whether a vision-capable provider is installed. The files
// module never imports the providers: the composition root answers.
type VisionGate interface {
	VisionAvailable(ctx context.Context) bool
}

// VisionFunc adapts a function to a VisionGate.
type VisionFunc func(ctx context.Context) bool

// VisionAvailable implements VisionGate.
func (f VisionFunc) VisionAvailable(ctx context.Context) bool { return f(ctx) }

// Options configures New.
type Options struct {
	// DB is the store the metadata lives in. Required.
	DB *store.DB
	// DataDir is the data directory; blobs are kept in <DataDir>/files, a bind
	// mount in a container deployment. Required.
	DataDir string
	// Clock is the only source of time. Nil means clock.Real.
	Clock clock.Clock
	// Config is the "files" section. Nil means the registered section, read on
	// every call so a reloaded configuration takes effect.
	Config *Config
	// Policy is the host's attachment policy. Nil means the policy declared in
	// the "files" section (ConfigPolicy).
	Policy PolicySource
	// RAG ingests rag_source files. Nil means that treatment is unavailable: an
	// upload that asks for it is refused before anything is stored.
	RAG RAGSources
	// Vision says whether a vision-capable provider is installed. Nil means none:
	// image bytes are never handed to a model.
	Vision VisionGate
	// Charge charges the tenant's storage (and, for a rag_source file, its
	// ingestion) in the transaction that stores the file. Nil means no volume is
	// charged by this module (a deployment without quotas).
	Charge Charger
	// IDs mints file ids. Nil means a generator on Clock.
	IDs *ids.Generator
	// Log receives operational lines. Nil discards them.
	Log *slog.Logger
}

// Service stores files and resolves scoped references to them. It is safe for
// concurrent use.
type Service struct {
	db     *store.DB
	clk    clock.Clock
	ids    *ids.Generator
	blobs  *blobStore
	cfg    *Config
	policy PolicySource
	rag    RAGSources
	vision VisionGate
	charge Charger
	log    *slog.Logger
}

// New builds a Service and creates <DataDir>/files owner-only.
func New(opts Options) (*Service, error) {
	if opts.DB == nil {
		return nil, contracts.New(contracts.ErrInternal, "files: a store is required")
	}
	if strings.TrimSpace(opts.DataDir) == "" {
		return nil, contracts.New(contracts.ErrInternal, "files: a data directory is required")
	}
	clk := opts.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	s := &Service{db: opts.DB, clk: clk, ids: opts.IDs, cfg: opts.Config, policy: opts.Policy, rag: opts.RAG, vision: opts.Vision, charge: opts.Charge, log: opts.Log}
	if s.ids == nil {
		s.ids = ids.NewGenerator(clk)
	}
	if s.cfg == nil {
		s.cfg = ConfigSection()
	}
	if s.policy == nil {
		s.policy = ConfigPolicy(s.config)
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if err := s.config().Validate(); err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "files: invalid configuration")
	}
	b, err := openBlobStore(opts.DataDir)
	if err != nil {
		return nil, err
	}
	s.blobs = b
	return s, nil
}

// Close releases the service. It holds no open handle (the files directory is
// opened for each operation and closed again), so there is nothing to release and
// a service that is never closed leaves nothing locked; Close exists so a caller
// can treat the service like the other stores. It is safe to call more than once.
func (s *Service) Close() error { return nil }

// config is the section with its defaults applied, read through the pointer
// each time.
func (s *Service) config() Config { return s.cfg.withDefaults() }

// PutInput is one file to store.
type PutInput struct {
	// Name is the display name; it is reduced to a bare file name and is never a
	// path.
	Name string
	// DeclaredMIME is the media type the sender claims. It must match what the
	// bytes are (SEC-019).
	DeclaredMIME string
	// Treatment is how the file is to be treated. Empty means the policy's
	// default. A treatment the host's policy does not accept is refused.
	Treatment Treatment
	// Origin is where the file came from. Empty means upload. The HTTP API only
	// ever stores uploads; the others are for the core and the host's adapter.
	Origin Origin
	// Content is the file. It is read once, in a stream, never held in memory.
	Content io.Reader
}

// Put stores a file for the principal of scope and returns its reference.
//
// The policy decides: the treatment must have an enabled rule, the declared
// type must be allowed by it, the size is capped by it and the tenant's quota
// must have room. The bytes must be the declared type (the first bytes are
// sniffed, an Office package is opened to its directory) or nothing is stored.
// The blob is addressed by scope and content (SEC-023), so storing the same
// bytes twice in one scope keeps one blob and storing them in another scope
// keeps another.
func (s *Service) Put(ctx context.Context, scope contracts.Scope, in PutInput) (conversation.FileRef, error) {
	if err := scope.Validate(); err != nil {
		return conversation.FileRef{}, err
	}
	if in.Content == nil {
		return conversation.FileRef{}, invalid("empty", "the upload has no content")
	}
	origin := in.Origin
	if origin == "" {
		origin = OriginUpload
	}
	if !origin.Valid() {
		return conversation.FileRef{}, invalid("origin", "the origin is not upload, generated or application")
	}
	cfg := s.config()
	treatment, rule, err := s.ruleFor(ctx, scope, in.Treatment, cfg)
	if err != nil {
		return conversation.FileRef{}, err
	}
	declared, err := declaredType(in.DeclaredMIME)
	if err != nil {
		return conversation.FileRef{}, err
	}
	spec, known := typeTable[declared]
	if !known || !rule.allows(declared) {
		return conversation.FileRef{}, invalid("mime_not_allowed", "this attachment policy does not accept that file type").
			WithDetails("declared", declared)
	}
	if treatment == TreatmentRAGSource && s.rag == nil {
		return conversation.FileRef{}, contracts.New(contracts.ErrUnavailable, "no RAG ingestion is available to register a source").
			WithDetails("reason", "rag_unavailable")
	}
	// A tenant that is already at its quota is refused before a byte is read.
	now := s.clk.Now()
	if use, err := s.usage(ctx, s.db, scope, now); err != nil {
		return conversation.FileRef{}, err
	} else if err := checkQuota(cfg, use.Files+1, use.Bytes+1); err != nil {
		return conversation.FileRef{}, err
	}

	rcv, err := s.receive(ctx, in.Content, spec, rule.MaxBytes)
	if err != nil {
		return conversation.FileRef{}, err
	}
	defer s.blobs.discard(rcv.tmpName)

	row := fileRow{
		id:         s.ids.New(IDPrefix),
		app:        scope.ApplicationID,
		tenant:     scope.TenantID,
		workspace:  scope.WorkspaceID,
		owner:      scope.PrincipalID,
		name:       cleanName(in.Name),
		mime:       declared,
		size:       rcv.size,
		hash:       "sha256:" + hex.EncodeToString(rcv.sum[:]),
		blobKey:    blobName(scope.ApplicationID, scope.TenantID, scope.WorkspaceID, rcv.sum),
		origin:     origin,
		treatment:  treatment,
		visibility: rule.Visibility,
		perms:      rule.Permissions,
		created:    now,
		expires:    now.Add(rule.TTL),
	}
	if treatment == TreatmentEphemeral {
		row.session = scope.SessionID
	}
	dir := tenantDirName(scope.ApplicationID, scope.TenantID)
	err = s.db.Tx(ctx, func(tx *store.Tx) error {
		// Quota and insert are one transaction on the single writer, so two
		// concurrent uploads cannot both slip under the limit.
		use, err := s.usage(ctx, tx, scope, now)
		if err != nil {
			return err
		}
		if err := checkQuota(cfg, use.Files+1, use.Bytes+rcv.size); err != nil {
			return err
		}
		// The tenant's volume quotas, charged by the write that stores the data: a refusal aborts this
		// transaction before the blob is placed, a failure after it takes the charge back with the row.
		if err := s.chargeTx(ctx, tx, scope, treatment, rcv.size); err != nil {
			return err
		}
		if err := s.blobs.place(rcv.tmpName, dir, row.blobKey); err != nil {
			return err
		}
		_, err = store.InsertScoped(ctx, tx, scope, "files", insertColumns, row.insertValues()...)
		return err
	})
	if err != nil {
		return conversation.FileRef{}, err
	}

	if treatment == TreatmentRAGSource {
		if err := s.rag.RegisterFile(ctx, scope, RAGFile{FileID: row.id, Name: row.name, MIMEType: row.mime, SizeBytes: row.size, ContentHash: row.hash}); err != nil {
			// Not a source: the file does not stay as one (fail closed).
			if terr := s.tombstone(ctx, scope, row.id); terr != nil {
				s.log.Error("files: could not roll back a file the RAG ingestion refused", slog.String("file_id", row.id))
			} else {
				// The file is gone, so what it was charged is given back (a file that stayed keeps its charge).
				s.refundCharge(ctx, scope, treatment, row.size)
			}
			return conversation.FileRef{}, ragFault(err)
		}
	}
	return row.ref(), nil
}

// ruleFor resolves the treatment of an upload and its rule from the host's
// policy. A policy that cannot be read, or that has no rule for the treatment,
// refuses the upload: nothing is stored by default.
func (s *Service) ruleFor(ctx context.Context, scope contracts.Scope, want Treatment, cfg Config) (Treatment, Rule, error) {
	pol, err := s.policy.AttachmentPolicy(ctx, scope)
	if err != nil {
		if contracts.CodeOf(err) != contracts.ErrInternal {
			return "", Rule{}, err
		}
		return "", Rule{}, contracts.Wrap(contracts.ErrUnavailable, err, "the attachment policy cannot be read")
	}
	t := want
	if t == "" {
		t = pol.Default
	}
	if t == "" {
		return "", Rule{}, contracts.New(contracts.ErrPermissionDenied, "the application's attachment policy accepts no attachment").
			WithDetails("reason", "no_policy")
	}
	if !t.Valid() {
		return "", Rule{}, invalid("treatment", "the treatment is not ephemeral_context, rag_source or application_object")
	}
	rule, ok := pol.Rules[t]
	if !ok {
		return "", Rule{}, contracts.New(contracts.ErrPermissionDenied, "the application's attachment policy does not accept that treatment").
			WithDetails("reason", "treatment_not_allowed").WithDetails("treatment", string(t))
	}
	rule, err = rule.resolve(t, cfg)
	if err != nil {
		return "", Rule{}, err
	}
	return t, rule, nil
}

// received is an upload on disk, verified and hashed.
type received struct {
	tmpName string
	size    int64
	sum     [sha256.Size]byte
}

// receive streams content into a temporary file, hashing it and refusing it as
// soon as it is the wrong type or too large. Nothing it leaves behind is a
// blob: the caller places it or discards it.
func (s *Service) receive(ctx context.Context, content io.Reader, spec typeSpec, limit int64) (received, error) {
	head := make([]byte, sniffLen)
	n, err := io.ReadFull(content, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return received{}, readFault(ctx, err)
	}
	head = head[:n]
	if n == 0 {
		return received{}, invalid("empty", "the file is empty")
	}
	if err := verifyHead(spec, head); err != nil {
		return received{}, err
	}
	if int64(n) > limit {
		return received{}, tooLarge(limit)
	}
	f, name, err := s.blobs.tmpFile()
	if err != nil {
		return received{}, err
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
			s.blobs.discard(name)
		}
	}()
	h := sha256.New()
	if _, err := f.Write(head); err != nil {
		return received{}, storageFault("write an upload", err)
	}
	h.Write(head)
	size := int64(n)
	if n == sniffLen {
		w, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(content, limit-size+1))
		size += w
		if err != nil {
			return received{}, copyFault(ctx, err)
		}
		if size > limit {
			return received{}, tooLarge(limit)
		}
	}
	if err := verifyContainer(spec, f, size); err != nil {
		return received{}, err
	}
	if err := f.Sync(); err != nil {
		return received{}, storageFault("flush an upload", err)
	}
	// Closed before it is renamed: Windows will not rename an open file.
	if err := f.Close(); err != nil {
		return received{}, storageFault("close an upload", err)
	}
	ok = true
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return received{tmpName: name, size: size, sum: sum}, nil
}

// copyFault classifies a failure of io.Copy: a failed read of the sender's
// stream is the upload's fault (and keeps its cause, so a body limit is still
// recognised); a failed write is storage's.
func copyFault(ctx context.Context, err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return storageFault("write an upload", err)
	}
	return readFault(ctx, err)
}

func readFault(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return contracts.Wrap(contracts.ErrValidationFailed, err, "the upload could not be read").WithDetails("reason", "read_failed")
}

func tooLarge(limit int64) error {
	return invalid("too_large", "the file is larger than this attachment policy allows").WithDetails("limit", limit)
}

func ragFault(err error) error {
	if contracts.CodeOf(err) != contracts.ErrInternal {
		return err
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, "the file could not be registered as a RAG source").WithDetails("reason", "rag_refused")
}

// ---- quota ----

// Usage is what one tenant holds: its live files, expired and deleted ones
// excluded (those wait for compaction and no longer count).
type Usage struct {
	Files int
	Bytes int64
}

// checkQuota refuses a tenant that would hold more than its quota: files and
// bytes are the totals AFTER the upload.
func checkQuota(cfg Config, files int, bytes int64) error {
	if files > cfg.QuotaFiles {
		return contracts.New(contracts.ErrResourceExhausted, "the tenant has reached its file count quota").
			WithDetails("reason", "quota_files").WithDetails("limit", cfg.QuotaFiles)
	}
	if bytes > cfg.QuotaBytes {
		return contracts.New(contracts.ErrResourceExhausted, "the tenant has reached its storage quota").
			WithDetails("reason", "quota_bytes").WithDetails("limit", cfg.QuotaBytes)
	}
	return nil
}

// usageSQL is deliberately TENANT-wide, not workspace-wide: the quota is the
// tenant's. It still never leaves the application and tenant of the verified
// scope, so another tenant's files neither count against this one nor are
// visible to it.
const usageSQL = `SELECT COUNT(*), COALESCE(SUM(size_bytes), 0) FROM files
 WHERE application_id = ? AND tenant_id = ? AND tombstoned_at IS NULL AND expires_at > ?`

func (s *Service) usage(ctx context.Context, q store.Reader, scope contracts.Scope, now time.Time) (Usage, error) {
	var u Usage
	err := q.QueryRowContext(ctx, usageSQL, scope.ApplicationID, scope.TenantID, now.UnixMilli()).Scan(&u.Files, &u.Bytes)
	if err != nil {
		return Usage{}, dbFault("read the tenant's usage", err)
	}
	return u, nil
}

// Usage reports what the tenant of scope holds now.
func (s *Service) Usage(ctx context.Context, scope contracts.Scope) (Usage, error) {
	if err := scope.Validate(); err != nil {
		return Usage{}, err
	}
	return s.usage(ctx, s.db, scope, s.clk.Now())
}

// dbFault is a store failure that is not a typed error already.
func dbFault(op string, err error) error {
	if contracts.CodeOf(err) != contracts.ErrInternal {
		return err
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, "file metadata could not be read or written: "+op)
}

// ---- rows ----

// fileRow is one row of the files table.
type fileRow struct {
	seq        int64
	id         string
	app        string
	tenant     string
	workspace  string
	owner      string
	session    string
	name       string
	mime       string
	size       int64
	hash       string
	blobKey    string
	origin     Origin
	treatment  Treatment
	visibility Visibility
	perms      []Permission
	created    time.Time
	expires    time.Time
}

const selectColumns = `seq, id, application_id, tenant_id, workspace_id, owner_id, session_id, name, mime_type, size_bytes,
 content_hash, blob_key, origin, treatment, visibility, permissions, created_at, expires_at`

var insertColumns = []string{
	"id", "owner_id", "session_id", "name", "mime_type", "size_bytes", "content_hash", "blob_key", "origin", "treatment",
	"visibility", "permissions", "created_at", "expires_at",
}

func (r fileRow) insertValues() []any {
	return []any{
		r.id, r.owner, r.session, r.name, r.mime, r.size, r.hash, r.blobKey, string(r.origin), string(r.treatment),
		string(r.visibility), joinPerms(r.perms), r.created.UnixMilli(), r.expires.UnixMilli(),
	}
}

type rowScanner interface{ Scan(dest ...any) error }

func scanRow(sc rowScanner) (fileRow, error) {
	var r fileRow
	var origin, treatment, vis, perms string
	var created, expires int64
	if err := sc.Scan(&r.seq, &r.id, &r.app, &r.tenant, &r.workspace, &r.owner, &r.session, &r.name, &r.mime, &r.size,
		&r.hash, &r.blobKey, &origin, &treatment, &vis, &perms, &created, &expires); err != nil {
		return fileRow{}, err
	}
	r.origin, r.treatment, r.visibility = Origin(origin), Treatment(treatment), Visibility(vis)
	for _, p := range strings.Split(perms, ",") {
		if Permission(p).Valid() {
			r.perms = append(r.perms, Permission(p))
		}
	}
	r.created, r.expires = time.UnixMilli(created).UTC(), time.UnixMilli(expires).UTC()
	return r, nil
}

func joinPerms(p []Permission) string {
	s := make([]string, len(p))
	for i, x := range p {
		s[i] = string(x)
	}
	return strings.Join(s, ",")
}

func (r fileRow) has(p Permission) bool {
	for _, x := range r.perms {
		if x == p {
			return true
		}
	}
	return false
}

// ref is the public reference of the row: no path, no URL, no storage key.
func (r fileRow) ref() conversation.FileRef {
	hash := r.hash
	treatment := r.treatment
	perms := append([]Permission{}, r.perms...)
	return conversation.FileRef{
		FileID:      actions.FileID(r.id),
		Name:        r.name,
		MimeType:    r.mime,
		SizeBytes:   r.size,
		ContentHash: &hash,
		Owner:       common.Identifier(r.owner),
		Origin:      r.origin,
		Treatment:   &treatment,
		Permissions: perms,
		Previewable: previewable(r.mime),
		ExpiresAt:   common.Timestamp(r.expires.UTC().Format(timestampLayout)),
	}
}

// tombstone makes a file stop resolving at once; compaction removes it later.
func (s *Service) tombstone(ctx context.Context, scope contracts.Scope, id string) error {
	_, err := store.ExecScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL: "UPDATE files SET tombstoned_at = ?", Args: []any{s.clk.Now().UnixMilli()},
		Where: "id = ? AND tombstoned_at IS NULL", WhereArgs: []any{id},
	})
	if err != nil {
		return dbFault("tombstone a file", err)
	}
	return nil
}
