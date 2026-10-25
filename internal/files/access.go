package files

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"regexp"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// The reads of a file. Every one of them resolves the reference from scratch:
// the scope is part of the query, the principal, the session, the expiry and the
// permission are checked in code, and nothing is cached, so a reference that
// stopped being valid a moment ago stops resolving a moment ago (master plan
// 14.1: "re-checks scope and expiry every time"; SEC-026).
//
// A reference that does not resolve in the caller's scope is one error
// (scope_violation) whatever the reason: another application, tenant or
// workspace, another principal's private file, another session's ephemeral
// file, a deleted file and an id that never existed are indistinguishable.
// Only once the caller has proved the file is theirs does the answer say more:
// expired (the reference has outlived its time) and permission_denied (the
// policy does not grant that use).

var idRE = regexp.MustCompile(`^` + IDPrefix + `_[0-9A-HJKMNP-TV-Z]{26}$`)

// ValidID reports whether s is the shape of a file id. The gateway uses it to
// answer a malformed path before it reaches a lookup.
func ValidID(s string) bool { return idRE.MatchString(s) }

func errNoSuchFile() error {
	return contracts.New(contracts.ErrScopeViolation, "the file reference does not resolve for this caller")
}

func errExpired() error {
	return contracts.New(contracts.ErrExpired, "the file reference has expired").WithDetails("reason", "expired")
}

// lookup finds the live row of id in the verified scope. Any miss is
// errNoSuchFile.
func lookup(ctx context.Context, q store.Reader, scope contracts.Scope, id string) (fileRow, error) {
	if err := scope.Validate(); err != nil {
		return fileRow{}, err
	}
	if !ValidID(id) {
		return fileRow{}, errNoSuchFile()
	}
	row, err := store.QueryRowScoped(ctx, q, scope, store.ScopedStmt{
		SQL:   "SELECT " + selectColumns + " FROM files",
		Where: "id = ? AND tombstoned_at IS NULL", WhereArgs: []any{id},
	})
	if err != nil {
		return fileRow{}, err
	}
	r, err := scanRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return fileRow{}, errNoSuchFile()
	}
	if err != nil {
		return fileRow{}, dbFault("resolve a file", err)
	}
	return r, nil
}

// authorize decides whether the caller may use the file now. perm is the use
// (empty for a plain lookup of the reference).
func (s *Service) authorize(scope contracts.Scope, r fileRow, perm Permission) error {
	if r.owner != scope.PrincipalID && r.visibility != VisibilityWorkspace {
		return errNoSuchFile()
	}
	if r.session != "" && r.session != scope.SessionID {
		return errNoSuchFile()
	}
	if !s.clk.Now().Before(r.expires) {
		return errExpired()
	}
	if perm != "" && !r.has(perm) {
		return contracts.New(contracts.ErrPermissionDenied, "the attachment policy does not grant that use of this file").
			WithDetails("reason", "permission").WithDetails("permission", string(perm))
	}
	return nil
}

// resolve is lookup plus authorize.
func (s *Service) resolve(ctx context.Context, scope contracts.Scope, id string, perm Permission) (fileRow, error) {
	r, err := lookup(ctx, s.db, scope, id)
	if err != nil {
		return fileRow{}, err
	}
	if err := s.authorize(scope, r, perm); err != nil {
		return fileRow{}, err
	}
	return r, nil
}

// Stat resolves a reference and returns it as the caller sees it.
func (s *Service) Stat(ctx context.Context, scope contracts.Scope, id string) (conversation.FileRef, error) {
	r, err := s.resolve(ctx, scope, id, "")
	if err != nil {
		return conversation.FileRef{}, err
	}
	return r.ref(), nil
}

// Content is an open file: its reference and its bytes. The caller closes Body.
type Content struct {
	Ref  conversation.FileRef
	Body io.ReadCloser
}

// Open resolves a reference for one use (read, preview or download) and opens
// its bytes. The permission is part of the check: a file whose policy does not
// grant download cannot be downloaded however it was reached.
func (s *Service) Open(ctx context.Context, scope contracts.Scope, id string, perm Permission) (*Content, error) {
	if !perm.Valid() {
		return nil, contracts.New(contracts.ErrInternal, "files: Open needs a permission")
	}
	r, err := s.resolve(ctx, scope, id, perm)
	if err != nil {
		return nil, err
	}
	return s.openRow(r)
}

func (s *Service) openRow(r fileRow) (*Content, error) {
	f, size, err := s.blobs.open(tenantDirName(r.app, r.tenant), r.blobKey)
	if err != nil {
		return nil, err
	}
	if size != r.size {
		f.Close()
		return nil, contracts.New(contracts.ErrUnavailable, "the stored file does not match its record").WithDetails("reason", "damaged")
	}
	return &Content{Ref: r.ref(), Body: f}, nil
}

// Preview is a safe rendering of a file: its bytes and the type to serve them
// as.
type Preview struct {
	Ref conversation.FileRef
	// ContentType is what to serve the body as: the image's own type for a raster
	// image, text/plain for any text.
	ContentType string
	// Truncated says a text preview was cut to its limit.
	Truncated bool
	// Size is the length of Body.
	Size int64
	Body io.ReadCloser
}

// Preview opens a file for preview. Only a type with a safe previewer has one:
// raster images, served as themselves, and text, served as text/plain. PDF,
// Office files and anything scriptable are not previewable (validation_failed,
// reason not_previewable). The caller serves the body with a sandboxing
// Content-Security-Policy and nosniff.
func (s *Service) Preview(ctx context.Context, scope contracts.Scope, id string) (*Preview, error) {
	r, err := s.resolve(ctx, scope, id, PermPreview)
	if err != nil {
		return nil, err
	}
	spec := typeTable[r.mime]
	if spec.preview == previewNone {
		return nil, invalid("not_previewable", "there is no safe previewer for this file type").WithDetails("mime_type", r.mime)
	}
	c, err := s.openRow(r)
	if err != nil {
		return nil, err
	}
	if spec.preview == previewImage {
		return &Preview{Ref: c.Ref, ContentType: r.mime, Size: r.size, Body: c.Body}, nil
	}
	defer c.Body.Close()
	limit := int64(s.config().PreviewTextBytes)
	buf, err := io.ReadAll(io.LimitReader(c.Body, limit+1))
	if err != nil {
		return nil, storageFault("read a stored file", err)
	}
	truncated := int64(len(buf)) > limit
	if truncated {
		buf = trimToRune(buf[:limit])
	}
	return &Preview{
		Ref: c.Ref, ContentType: "text/plain; charset=utf-8", Truncated: truncated,
		Size: int64(len(buf)), Body: io.NopCloser(bytes.NewReader(buf)),
	}, nil
}

// Delete deletes a file the caller owns. It stops resolving at once (a
// tombstone); the blob is removed by compaction, once no other reference of the
// scope uses it. A file registered as a RAG source is withdrawn from the RAG
// pipeline first, and if that fails the file stays (and the error says so):
// a deleted source must not stay retrievable.
func (s *Service) Delete(ctx context.Context, scope contracts.Scope, id string) error {
	r, err := s.resolve(ctx, scope, id, "")
	if err != nil {
		return err
	}
	if r.owner != scope.PrincipalID {
		return contracts.New(contracts.ErrPermissionDenied, "only the owner of a file can delete it").WithDetails("reason", "not_owner")
	}
	if r.treatment == TreatmentRAGSource && s.rag != nil {
		if err := s.rag.RemoveFile(ctx, Location{ApplicationID: r.app, TenantID: r.tenant, WorkspaceID: r.workspace}, r.id); err != nil {
			return ragFault(err)
		}
	}
	return s.tombstone(ctx, scope, r.id)
}
