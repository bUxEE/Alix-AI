package audit

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// CapabilityExport is the capability a caller's context token must permit
// (contracts.Scope.CapabilityScopes) to export audit events. The rule is
// exact: no wildcard, no role stands in for it.
const CapabilityExport = "audit.export"

// exportPage is how many events one read of Export fetches.
const exportPage = 500

// ExportOptions narrows an export. The zero value exports every event of the
// caller's workspace.
type ExportOptions struct {
	// Types, when not empty, keeps only these event types.
	Types []EventType
	// Since and Until, when not zero, bound occurred_at to [Since, Until).
	Since, Until time.Time
}

// ExportResult says what an export wrote.
type ExportResult struct {
	Events int
}

// Export writes the audit events of the caller's own application, tenant and
// workspace to w as JSON lines, oldest first, one Event per line. The data
// is the redacted data that was stored; nothing is added or re-derived, so a
// line can be checked with Event.HashValid.
//
// The caller's scope must be valid and permit CapabilityExport, and
// Options.ExportAuthorizer, when set, must accept it; otherwise the error is
// permission_denied (scope_missing or scope_violation for a bad scope) and
// nothing is written. The export is itself recorded (EventAuditExported) before
// any event is read, and a failed append refuses the export: no data leaves
// without a record of it. Every read filters on application, tenant and
// workspace through store.ScopedWhere.
//
// Output is written in pages; if w fails part-way the lines already written
// stay written and the error is returned.
func (l *Log) Export(ctx context.Context, scope contracts.Scope, w io.Writer, opts ExportOptions) (ExportResult, error) {
	loc, err := ForScope(scope)
	if err != nil {
		return ExportResult{}, err
	}
	if !scope.Permits(CapabilityExport) {
		return ExportResult{}, contracts.Newf(contracts.ErrPermissionDenied, "audit: the context does not permit %s", CapabilityExport)
	}
	if l.authz != nil {
		if aerr := l.authz(ctx, scope); aerr != nil {
			if contracts.CodeOf(aerr) == contracts.ErrPermissionDenied {
				return ExportResult{}, aerr
			}
			return ExportResult{}, contracts.Wrap(contracts.ErrPermissionDenied, aerr, "audit: the export was not authorised")
		}
	}
	for _, t := range opts.Types {
		if !ValidTypeName(string(t)) {
			return ExportResult{}, contracts.New(contracts.ErrValidationFailed, "audit: an export filter names a malformed event type")
		}
	}
	if w == nil {
		return ExportResult{}, contracts.New(contracts.ErrValidationFailed, "audit: no writer to export to")
	}

	record := map[string]any{"types": exportTypeNames(opts.Types)}
	if !opts.Since.IsZero() {
		record["since"] = opts.Since.UTC().Format(time.RFC3339Nano)
	}
	if !opts.Until.IsZero() {
		record["until"] = opts.Until.UTC().Format(time.RFC3339Nano)
	}
	if _, err := l.AppendAt(ctx, loc, NewEvent{Type: EventAuditExported, Data: record}); err != nil {
		return ExportResult{}, err
	}

	where, args := exportFilter(opts)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	var res ExportResult
	after := int64(0)
	for {
		var page []Event
		var last int64
		err := l.db.View(ctx, func(tx *store.Tx) error {
			page, last = page[:0], 0
			rows, err := store.QueryScoped(ctx, tx, scope, store.ScopedStmt{
				SQL:       "SELECT seq, id, application_id, tenant_id, workspace_id, chain_seq, event_type, occurred_at, principal_id, session_id, data, prev_hash, hash FROM audit_events",
				Where:     "seq > ?" + where,
				WhereArgs: append([]any{after}, args...),
				Tail:      "ORDER BY seq LIMIT ?",
				TailArgs:  []any{exportPage},
			})
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var (
					e    Event
					ms   int64
					typ  string
					data string
				)
				if err := rows.Scan(&last, &e.ID, &e.ApplicationID, &e.TenantID, &e.WorkspaceID, &e.ChainSeq, &typ, &ms,
					&e.PrincipalID, &e.SessionID, &data, &e.PrevHash, &e.Hash); err != nil {
					return err
				}
				e.Type = EventType(typ)
				e.OccurredAt = time.UnixMilli(ms).UTC()
				e.Data = json.RawMessage(data)
				page = append(page, e)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if err := rows.Close(); err != nil {
				return err
			}
			// An erased event goes out with its signed tombstone: its hash cannot
			// be recomputed from what is left, so the recipient verifies that. A
			// database from before tombstones holds none, which is asked once per
			// page, and only for a page with an erased event.
			checked, tombs := false, false
			for i := range page {
				if string(page[i].Data) != ErasedMarker {
					continue
				}
				if !checked {
					if tombs, err = tombstoneTableExists(ctx, tx); err != nil {
						return err
					}
					checked = true
				}
				if !tombs {
					break
				}
				if page[i].Tombstone, err = loadTombstoneOf(ctx, tx, page[i].ID); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return res, wrapDB(err)
		}
		for _, e := range page {
			if err := enc.Encode(e); err != nil {
				return res, contracts.Wrap(contracts.ErrUnavailable, err, "audit: writing the export failed")
			}
			res.Events++
		}
		if len(page) < exportPage {
			return res, nil
		}
		after = last
	}
}

func exportTypeNames(ts []EventType) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = string(t)
	}
	return out
}

// exportFilter builds the extra conditions (each starting with " AND ") and
// their arguments. Values are bound, never concatenated.
func exportFilter(opts ExportOptions) (string, []any) {
	var b strings.Builder
	var args []any
	if len(opts.Types) > 0 {
		b.WriteString(" AND event_type IN (")
		for i, t := range opts.Types {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString("?")
			args = append(args, string(t))
		}
		b.WriteString(")")
	}
	if !opts.Since.IsZero() {
		b.WriteString(" AND occurred_at >= ?")
		args = append(args, opts.Since.UnixMilli())
	}
	if !opts.Until.IsZero() {
		b.WriteString(" AND occurred_at < ?")
		args = append(args, opts.Until.UnixMilli())
	}
	return b.String(), args
}
