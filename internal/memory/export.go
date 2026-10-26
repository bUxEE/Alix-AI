package memory

import (
	"context"
	"encoding/json"
	"io"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	mem "github.com/buxee/alix-ai/pkg/contracts/gen/memory"
)

// Exporter appends memory rows onto an export stream (history export hook).
type Exporter interface {
	AppendExport(ctx context.Context, scope contracts.Scope, w io.Writer) (ExportResult, error)
}

// ExportRow is one JSONL line of a memory export (alongside history when wired).
type ExportRow struct {
	Kind string         `json:"kind"`
	Item mem.MemoryItem `json:"item"`
}

// ExportResult says how many rows were written.
type ExportResult struct {
	Rows int
}

// Export writes the caller's memory items as JSON lines (kind=memory). Audited first.
func (s *Service) Export(ctx context.Context, scope contracts.Scope, w io.Writer) (ExportResult, error) {
	if err := s.authorize(ctx, scope, ActionExport, CapExport); err != nil {
		return ExportResult{}, err
	}
	if w == nil {
		return ExportResult{}, contracts.New(contracts.ErrValidationFailed, "memory: no writer to export to")
	}
	if err := s.audit(ctx, scope, EventExported, map[string]any{}); err != nil {
		return ExportResult{}, err
	}
	rows, err := store.QueryScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL:   "SELECT " + itemCols + " FROM memory_items",
		Where: "principal_id = ?", WhereArgs: []any{scope.PrincipalID},
		Tail: "ORDER BY created_at ASC, id ASC",
	})
	if err != nil {
		return ExportResult{}, wrapDB(err)
	}
	defer rows.Close()
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	var res ExportResult
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return res, wrapDB(err)
		}
		if err := enc.Encode(ExportRow{Kind: "memory", Item: r.item()}); err != nil {
			return res, contracts.Wrap(contracts.ErrUnavailable, err, "memory: writing the export failed")
		}
		res.Rows++
	}
	return res, wrapDB(rows.Err())
}

// AppendExport writes memory rows for scope without a capability check — used when
// history export includes memory after it has already authorised CapExport-equivalent.
func (s *Service) AppendExport(ctx context.Context, scope contracts.Scope, w io.Writer) (ExportResult, error) {
	if err := scope.Validate(); err != nil {
		return ExportResult{}, err
	}
	if w == nil {
		return ExportResult{}, contracts.New(contracts.ErrValidationFailed, "memory: no writer to export to")
	}
	rows, err := store.QueryScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL:   "SELECT " + itemCols + " FROM memory_items",
		Where: "principal_id = ?", WhereArgs: []any{scope.PrincipalID},
		Tail: "ORDER BY created_at ASC, id ASC",
	})
	if err != nil {
		return ExportResult{}, wrapDB(err)
	}
	defer rows.Close()
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	var res ExportResult
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return res, wrapDB(err)
		}
		if err := enc.Encode(ExportRow{Kind: "memory", Item: r.item()}); err != nil {
			return res, contracts.Wrap(contracts.ErrUnavailable, err, "memory: writing the export failed")
		}
		res.Rows++
	}
	return res, wrapDB(rows.Err())
}
