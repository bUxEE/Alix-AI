package exports

import (
	"bytes"
	"context"
	"log/slog"
	"strings"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/blocks"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/files"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// CapCreate is the capability the host puts in the context token to create an export.
const CapCreate = "exports.create"

// Format is one of the supported export formats.
type Format string

const (
	FormatCSV  Format = "csv"
	FormatXLSX Format = "xlsx"
	FormatHTML Format = "html"
	FormatMD   Format = "md"
)

// Valid reports whether f is a known format.
func (f Format) Valid() bool {
	switch f {
	case FormatCSV, FormatXLSX, FormatHTML, FormatMD:
		return true
	}
	return false
}

// Request is one export of verified blocks.
type Request struct {
	TurnID   string
	BlockIDs []string
	Format   Format
}

// FileStore stores a generated export file. *files.Service satisfies it via PutGenerated.
type FileStore interface {
	PutGenerated(ctx context.Context, scope contracts.Scope, in files.GeneratedInput) (conversation.FileRef, error)
}

// Auditor records the export (ids and sizes only).
type Auditor = audit.Appender

// Options configures New.
type Options struct {
	Source BlockSource
	Files  FileStore
	Audit  Auditor
	Clock  clock.Clock
	Config *Config
	Blocks *blocks.Validator
	Log    *slog.Logger
}

// Service creates downloadable files from verified blocks.
type Service struct {
	src BlockSource
	fs  FileStore
	aud Auditor
	clk clock.Clock
	cfg *Config
	san *blocks.Validator
	log *slog.Logger
	mem *MemorySource // optional; Remember delegates here when Source embeds one
}

// New builds a Service. Source and Files are required.
func New(opts Options) (*Service, error) {
	if opts.Source == nil {
		return nil, contracts.New(contracts.ErrInternal, "exports: a block source is required")
	}
	if opts.Files == nil {
		return nil, contracts.New(contracts.ErrInternal, "exports: a file store is required")
	}
	clk := opts.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	cfg := opts.Config
	if cfg == nil {
		cfg = ConfigSection()
	}
	if err := cfg.Validate(); err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "exports: invalid configuration")
	}
	san := opts.Blocks
	if san == nil {
		san = blocks.Default()
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Service{src: opts.Source, fs: opts.Files, aud: opts.Audit, clk: clk, cfg: cfg, san: san, log: log}
	if m, ok := opts.Source.(*MemorySource); ok {
		s.mem = m
	}
	return s, nil
}

// Remember stores verified blocks for a turn (composition roots / tests).
func (s *Service) Remember(scope contracts.Scope, sessionID, turnID string, blocks []conversation.RichBlock) {
	if s.mem != nil {
		s.mem.Remember(scope, sessionID, turnID, blocks)
	}
}

// Create generates a file from the turn's verified blocks and stores it as OriginGenerated.
func (s *Service) Create(ctx context.Context, scope contracts.Scope, sessionID string, req Request) (conversation.FileRef, error) {
	if err := scope.Validate(); err != nil {
		return conversation.FileRef{}, err
	}
	if !scope.Permits(CapCreate) {
		return conversation.FileRef{}, contracts.Newf(contracts.ErrPermissionDenied, "exports: the context does not permit %s", CapCreate)
	}
	if sessionID == "" {
		sessionID = scope.SessionID
	}
	if sessionID != scope.SessionID {
		return conversation.FileRef{}, contracts.New(contracts.ErrScopeViolation, "exports: the session is not the token's")
	}
	if strings.TrimSpace(req.TurnID) == "" {
		return conversation.FileRef{}, invalid("turn_id", "turn_id is required")
	}
	if !req.Format.Valid() {
		return conversation.FileRef{}, invalid("format", "format must be csv, xlsx, html or md")
	}

	all, err := s.src.Blocks(ctx, scope, sessionID, req.TurnID)
	if err != nil {
		return conversation.FileRef{}, err
	}
	selected, err := selectBlocks(all, req.BlockIDs)
	if err != nil {
		return conversation.FileRef{}, err
	}
	if len(selected) == 0 {
		return conversation.FileRef{}, notFound("turn", "the turn has no verified blocks to export")
	}

	cfg := s.cfg.withDefaults()
	body, name, mime, err := s.render(selected, req.Format, cfg)
	if err != nil {
		return conversation.FileRef{}, err
	}
	if len(body) > cfg.MaxBytes {
		return conversation.FileRef{}, exhausted("max_bytes", "the export exceeds the configured size limit")
	}

	ref, err := s.fs.PutGenerated(ctx, scope, files.GeneratedInput{
		Name: name, DeclaredMIME: mime, Content: bytes.NewReader(body), TTL: cfg.FileTTL,
	})
	if err != nil {
		return conversation.FileRef{}, err
	}

	if s.aud != nil {
		loc, lerr := audit.ForScope(scope)
		if lerr == nil {
			_, _ = eventCreated.Append(ctx, s.aud, loc, map[string]any{
				"turn_id": req.TurnID, "file_id": string(ref.FileID), "format": string(req.Format),
				"size_bytes": ref.SizeBytes, "blocks": len(selected),
			})
		}
	}
	return ref, nil
}

func (s *Service) render(selected []IdentifiedBlock, format Format, cfg Config) (body []byte, name, mime string, err error) {
	sets := datasetsFromBlocks(selected)
	rows, cells := 0, 0
	for _, set := range sets {
		rows += len(set.Rows)
		cells += len(set.Rows) * len(set.Headers)
	}
	switch format {
	case FormatCSV, FormatXLSX:
		if len(sets) == 0 {
			return nil, "", "", invalid("format_not_applicable", "csv and xlsx need a table or chart block")
		}
		if rows > cfg.MaxRows {
			return nil, "", "", exhausted("max_rows", "the export exceeds the configured row limit")
		}
		if cells > cfg.MaxCells {
			return nil, "", "", exhausted("max_cells", "the export exceeds the configured cell limit")
		}
	}

	stamp := s.clk.Now().UTC().Format("20060102T150405Z")
	switch format {
	case FormatCSV:
		bom := cfg.CSVExcelBOM != nil && *cfg.CSVExcelBOM
		body, err = renderCSV(sets, bom)
		name, mime = "export-"+stamp+".csv", "text/csv"
	case FormatXLSX:
		body, err = renderXLSX(sets)
		name, mime = "export-"+stamp+".xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case FormatHTML:
		body, err = renderHTML(selected, s.san)
		name, mime = "export-"+stamp+".html", "text/html"
	case FormatMD:
		body, err = renderMarkdown(selected)
		name, mime = "export-"+stamp+".md", "text/markdown"
	default:
		return nil, "", "", invalid("format", "format must be csv, xlsx, html or md")
	}
	if err != nil {
		return nil, "", "", err
	}
	if len(body) == 0 {
		return nil, "", "", invalid("empty", "the export produced no content")
	}
	return body, name, mime, nil
}
