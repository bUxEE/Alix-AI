package history

import (
	"context"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Reasons recorded with a deletion.
const (
	reasonUserRequest = "user_request"
	reasonCleanup     = "cleanup"
	reasonRetention   = "retention"
)

// ClearResult says what a deletion removed.
type ClearResult struct {
	// Rows is how many transcript rows went.
	Rows int64
}

// clearedAudit is the data of a deletion.history_cleared event: counts and times,
// never text.
type clearedAudit struct {
	// Scope is "session" or "workspace".
	Scope      string `json:"scope"`
	Reason     string `json:"reason"`
	Rows       int64  `json:"rows"`
	Before     string `json:"before,omitempty"`
	RetainDays int    `json:"retain_days,omitempty"`
}

// ClearSession deletes the transcript of one of the caller's own sessions and
// reports how many rows went. Only the transcript goes (and, with it, its search
// index entries and, if the session has ended, the session store's copy of its
// last turns, which nothing reads any more): the session state, its prepared
// actions, the committed receipts, the audit events and the application's records
// and sources are not touched (master plan 19). The session need not be the session of the token,
// but it must be the caller's: another principal's session is scope_violation.
// A session with no transcript is cleared of nothing and nothing is recorded.
//
// The deletion is recorded in the audit log first and refused if that fails.
func (s *Service) ClearSession(ctx context.Context, scope contracts.Scope, sessionID string) (ClearResult, error) {
	if err := s.authorize(ctx, scope, ActionClear, CapClear); err != nil {
		return ClearResult{}, err
	}
	if !validID(sessionID) {
		return ClearResult{}, badSessionID()
	}
	if err := s.checkOwner(ctx, scope, sessionID); err != nil {
		return ClearResult{}, err
	}
	target := forSession(scope, sessionID)
	n, err := s.tr.TranscriptLen(ctx, target)
	if err != nil {
		return ClearResult{}, err
	}
	if n == 0 {
		return ClearResult{}, nil
	}
	if err := s.record(ctx, target, clearedAudit{Scope: "session", Reason: reasonUserRequest, Rows: n}); err != nil {
		return ClearResult{}, err
	}
	rows, err := s.tr.ClearTranscript(ctx, target)
	if err != nil {
		return ClearResult{}, err
	}
	s.log.Info("history cleared", "scope", "session", "rows", rows)
	return ClearResult{Rows: rows}, nil
}

// Cleanup deletes every transcript turn of the caller's WORKSPACE (all its
// principals, all its sessions) that is older than before, and reports how many
// rows went. It reaches beyond the caller's own history, so it needs its own
// capability and is authorised before the session store's PruneTranscript, which
// deletes across the whole workspace, is called. before must not be in the
// future: a cutoff past now would delete everything, and a clock mistake must not
// do that. Like ClearSession it leaves the session state, receipts, audit events,
// records and sources alone and is recorded first.
func (s *Service) Cleanup(ctx context.Context, scope contracts.Scope, before time.Time) (ClearResult, error) {
	if err := s.authorize(ctx, scope, ActionCleanup, CapCleanup); err != nil {
		return ClearResult{}, err
	}
	switch {
	case before.IsZero():
		return ClearResult{}, contracts.New(contracts.ErrValidationFailed, "history: a cleanup needs a cutoff time")
	case before.After(s.now()):
		return ClearResult{}, contracts.New(contracts.ErrValidationFailed, "history: the cutoff of a cleanup cannot be in the future")
	}
	return s.prune(ctx, scope, before, reasonCleanup, 0)
}

// prune deletes the transcript of scope's workspace older than cutoff. Its
// callers have already authorised the caller (Cleanup) or are the retention job
// under its service identity. Nothing is recorded or deleted when no row is that
// old.
func (s *Service) prune(ctx context.Context, scope contracts.Scope, cutoff time.Time, reason string, retainDays int) (ClearResult, error) {
	n, err := s.countOlder(ctx, scope, cutoff)
	if err != nil {
		return ClearResult{}, err
	}
	if n == 0 {
		return ClearResult{}, nil
	}
	rec := clearedAudit{Scope: "workspace", Reason: reason, Rows: n, Before: cutoff.UTC().Format(time.RFC3339Nano), RetainDays: retainDays}
	if err := s.record(ctx, scope, rec); err != nil {
		return ClearResult{}, err
	}
	rows, err := s.tr.PruneTranscript(ctx, scope, cutoff)
	if err != nil {
		return ClearResult{}, err
	}
	s.log.Info("history pruned", "reason", reason, "rows", rows)
	return ClearResult{Rows: rows}, nil
}

// countOlder counts the transcript rows of scope's workspace older than cutoff.
func (s *Service) countOlder(ctx context.Context, scope contracts.Scope, cutoff time.Time) (int64, error) {
	row, err := store.QueryRowScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL: "SELECT COUNT(*) FROM transcript_turns", Where: "created_at < ?", WhereArgs: []any{toMS(cutoff)},
	})
	if err != nil {
		return 0, err
	}
	var n int64
	if err := row.Scan(&n); err != nil {
		return 0, wrapDB(err)
	}
	return n, nil
}

// record appends a deletion.history_cleared event for scope. An error means the
// deletion must not happen.
func (s *Service) record(ctx context.Context, scope contracts.Scope, data clearedAudit) error {
	loc, err := audit.ForScope(scope)
	if err != nil {
		return err
	}
	_, err = s.aud.AppendAt(ctx, loc, audit.NewEvent{Type: audit.EventDeletionHistoryCleared, Data: data})
	return err
}
