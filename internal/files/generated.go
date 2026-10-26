package files

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// GeneratedInput is a file the core produced (exports, multimodal replies).
type GeneratedInput struct {
	Name         string
	DeclaredMIME string
	Content      io.Reader
	// TTL overrides the default generated lifetime; zero means the default (1 hour).
	TTL time.Duration
}

// generatedAllow is the closed set of media types a generated file may use.
// text/html is allowed only here (never for uploads): the producer must emit a
// script-free document (exports HTML does).
var generatedAllow = map[string]bool{
	"text/plain":       true,
	"text/markdown":    true,
	"text/csv":         true,
	"text/html":        true,
	"application/json": true,
	"application/pdf":  true,
	"image/png":        true,
	"image/jpeg":       true,
	"image/gif":        true,
	"image/webp":       true,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":       true,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": true,
}

const defaultGeneratedTTL = time.Hour

// PutGenerated stores a core-produced file as OriginGenerated with read and
// download permissions for the owner. It does not use the host upload allowlist
// (so CSV/XLSX/HTML exports work under the default ephemeral policy) but still
// sniffs bytes and refuses types outside generatedAllow.
func (s *Service) PutGenerated(ctx context.Context, scope contracts.Scope, in GeneratedInput) (conversation.FileRef, error) {
	if err := scope.Validate(); err != nil {
		return conversation.FileRef{}, err
	}
	if in.Content == nil {
		return conversation.FileRef{}, invalid("empty", "the generated file has no content")
	}
	declared, err := declaredType(in.DeclaredMIME)
	if err != nil {
		return conversation.FileRef{}, err
	}
	if !generatedAllow[declared] {
		return conversation.FileRef{}, invalid("mime_not_allowed", "this generated file type is not accepted").
			WithDetails("declared", declared)
	}
	// text/html is never on KnownTypes / upload allowlists; only PutGenerated may store a
	// script-free report the core produced (exports).
	spec, known := typeTable[declared]
	if !known && declared == "text/html" {
		spec, known = typeSpec{mime: "text/html", kind: kindText, sniffed: []string{"text/html", "text/plain"}, preview: previewText}, true
	}
	if !known {
		return conversation.FileRef{}, invalid("mime_not_allowed", "this generated file type is not accepted").
			WithDetails("declared", declared)
	}
	cfg := s.config()
	content := in.Content
	if declared == "text/html" {
		raw, err := io.ReadAll(io.LimitReader(in.Content, cfg.MaxFileBytes+1))
		if err != nil {
			return conversation.FileRef{}, readFault(ctx, err)
		}
		if int64(len(raw)) > cfg.MaxFileBytes {
			return conversation.FileRef{}, invalid("too_large", "the generated file is larger than the limit")
		}
		lower := strings.ToLower(string(raw))
		if strings.Contains(lower, "<script") || strings.Contains(lower, "javascript:") {
			return conversation.FileRef{}, invalid("html_unsafe", "generated HTML must not contain script")
		}
		content = bytes.NewReader(raw)
	}
	ttl := in.TTL
	if ttl <= 0 {
		ttl = defaultGeneratedTTL
	}
	if ttl > cfg.MaxTTL {
		ttl = cfg.MaxTTL
	}
	now := s.clk.Now()
	if use, err := s.usage(ctx, s.db, scope, now); err != nil {
		return conversation.FileRef{}, err
	} else if err := checkQuota(cfg, use.Files+1, use.Bytes+1); err != nil {
		return conversation.FileRef{}, err
	}

	rcv, err := s.receive(ctx, content, spec, cfg.MaxFileBytes)
	if err != nil {
		return conversation.FileRef{}, err
	}
	defer s.blobs.discard(rcv.tmpName)

	row := fileRow{
		id: s.ids.New(IDPrefix), app: scope.ApplicationID, tenant: scope.TenantID, workspace: scope.WorkspaceID,
		owner: scope.PrincipalID, name: cleanName(in.Name), mime: declared, size: rcv.size,
		hash: "sha256:" + hex.EncodeToString(rcv.sum[:]), blobKey: blobName(scope.ApplicationID, scope.TenantID, scope.WorkspaceID, rcv.sum),
		origin: OriginGenerated, treatment: TreatmentEphemeral, visibility: VisibilityOwner,
		perms: []Permission{PermRead, PermDownload, PermPreview}, created: now, expires: now.Add(ttl),
		session: scope.SessionID,
	}
	dir := tenantDirName(scope.ApplicationID, scope.TenantID)
	err = s.db.Tx(ctx, func(tx *store.Tx) error {
		use, err := s.usage(ctx, tx, scope, now)
		if err != nil {
			return err
		}
		if err := checkQuota(cfg, use.Files+1, use.Bytes+rcv.size); err != nil {
			return err
		}
		if err := s.chargeTx(ctx, tx, scope, TreatmentEphemeral, rcv.size); err != nil {
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
	return row.ref(), nil
}
