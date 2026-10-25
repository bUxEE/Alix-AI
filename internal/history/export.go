package history

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// exportedAudit is the data of a history.exported event: what was asked for,
// never any text.
type exportedAudit struct {
	SessionID string `json:"session_id,omitempty"`
	Since     string `json:"since,omitempty"`
	Until     string `json:"until,omitempty"`
}

// eventExported records an export of conversation history, appended before any
// of it is written.
var eventExported = audit.MustRegister[exportedAudit](audit.TypeSpec{
	Name:        "history.exported",
	Description: "conversation history was exported by its owner",
	Levels:      audit.LevelTenant,
})

// ExportOptions narrows an export. The zero value exports every transcript row
// of the caller in the token's workspace.
type ExportOptions struct {
	// SessionID restricts the export to one of the caller's sessions.
	SessionID string
	// Since and Until, when not zero, bound created_at to [Since, Until).
	Since, Until time.Time
}

// ExportResult says what an export wrote.
type ExportResult struct {
	Rows int
}

// ExportRow is one line of an export: one transcript row as JSON.
type ExportRow struct {
	SessionID string    `json:"session_id"`
	TurnID    string    `json:"turn_id"`
	N         int64     `json:"n"`
	Role      string    `json:"role"`
	Act       string    `json:"act,omitempty"`
	Body      string    `json:"body"`
	Clipped   bool      `json:"clipped"`
	CreatedAt time.Time `json:"created_at"`
	// Withheld is true when Body is empty because the sanitiser refused the text of
	// an assistant row (see Service.display).
	Withheld bool `json:"withheld,omitempty"`
}

// Export writes the caller's own transcript to w as JSON lines, one ExportRow
// per line, oldest first. Only rows of the token's application, tenant and
// workspace whose principal is the caller are read; a session named in the
// options that belongs to another principal is scope_violation and nothing is
// written. The caller must hold CapExport.
//
// The export is recorded (history.exported) before any row is read, and a failed
// append refuses it: no history leaves without a record of it. Rows are read a
// page at a time, so memory does not grow with the transcript; if w fails part
// way the lines already written stay written and the error is returned.
func (s *Service) Export(ctx context.Context, scope contracts.Scope, w io.Writer, opts ExportOptions) (ExportResult, error) {
	if err := s.authorize(ctx, scope, ActionExport, CapExport); err != nil {
		return ExportResult{}, err
	}
	if w == nil {
		return ExportResult{}, contracts.New(contracts.ErrValidationFailed, "history: no writer to export to")
	}
	if !opts.Since.IsZero() && !opts.Until.IsZero() && !opts.Since.Before(opts.Until) {
		return ExportResult{}, contracts.New(contracts.ErrValidationFailed, "history: the export window is empty")
	}
	if opts.SessionID != "" {
		if !validID(opts.SessionID) {
			return ExportResult{}, badSessionID()
		}
		if err := s.checkOwner(ctx, scope, opts.SessionID); err != nil {
			return ExportResult{}, err
		}
	}
	loc, err := audit.ForScope(scope)
	if err != nil {
		return ExportResult{}, err
	}
	rec := exportedAudit{SessionID: opts.SessionID}
	if !opts.Since.IsZero() {
		rec.Since = opts.Since.UTC().Format(time.RFC3339Nano)
	}
	if !opts.Until.IsZero() {
		rec.Until = opts.Until.UTC().Format(time.RFC3339Nano)
	}
	if _, err := eventExported.Append(ctx, s.aud, loc, rec); err != nil {
		return ExportResult{}, err
	}

	where, args := exportFilter(scope.PrincipalID, opts)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	var res ExportResult
	after := int64(0)
	for {
		args[0] = after
		page, last, err := s.exportPage(ctx, scope, where, args)
		if err != nil {
			return res, err
		}
		for _, row := range page {
			if err := enc.Encode(row); err != nil {
				return res, contracts.Wrap(contracts.ErrUnavailable, err, "history: writing the export failed")
			}
			res.Rows++
		}
		if len(page) < s.cfg.ExportPage {
			return res, nil
		}
		after = last
	}
}

// exportFilter is the condition, after the scope, of the rows an export reads, with
// its arguments. The first argument is the seq to read after, set per page.
//
// Without a session, a page walks transcript_turns_principal, which yields the
// caller's rows in seq order. With one, the unary + keeps SQLite on the session's
// own index (transcript_turns_order) instead of walking all the caller's rows.
func exportFilter(principal string, opts ExportOptions) (string, []any) {
	where, args := "seq > ? AND principal_id = ?", []any{nil, principal}
	if opts.SessionID != "" {
		where = "seq > ? AND +principal_id = ? AND session_id = ?"
		args = append(args, opts.SessionID)
	}
	if !opts.Since.IsZero() {
		where += " AND created_at >= ?"
		args = append(args, toMS(opts.Since))
	}
	if !opts.Until.IsZero() {
		where += " AND created_at < ?"
		args = append(args, toMS(opts.Until))
	}
	return where, args
}

// exportStmt is the statement of one page of an export.
func (s *Service) exportStmt(where string, args []any) store.ScopedStmt {
	return store.ScopedStmt{
		SQL:       "SELECT seq, session_id, turn_id, n, role, act, body, clipped, created_at FROM transcript_turns",
		Where:     where,
		WhereArgs: args,
		Tail:      "ORDER BY seq LIMIT ?",
		TailArgs:  []any{s.cfg.ExportPage},
	}
}

// exportPage reads one page of rows in insertion order and the seq of the last.
func (s *Service) exportPage(ctx context.Context, scope contracts.Scope, where string, args []any) ([]ExportRow, int64, error) {
	rows, err := store.QueryScoped(ctx, s.db, scope, s.exportStmt(where, args))
	if err != nil {
		return nil, 0, wrapDB(err)
	}
	defer rows.Close()
	var (
		out  []ExportRow
		last int64
	)
	for rows.Next() {
		var (
			r       ExportRow
			clipped int64
			created int64
		)
		if err := rows.Scan(&last, &r.SessionID, &r.TurnID, &r.N, &r.Role, &r.Act, &r.Body, &clipped, &created); err != nil {
			return nil, 0, wrapDB(err)
		}
		r.Clipped, r.CreatedAt = clipped == 1, fromMS(created)
		r.Body, r.Withheld = s.display(r.Role, r.Body)
		out = append(out, r)
	}
	return out, last, wrapDB(rows.Err())
}
