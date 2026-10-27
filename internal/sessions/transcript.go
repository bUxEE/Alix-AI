package sessions

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The transcript is the conversation as the user reads it back (master plan 11.3
// and 19). It lives in its own module and tables ("transcript"), apart from the
// state the dialogue engine reads each turn: building model context never reads a
// transcript row, which is what keeps old turns from being replayed. A turn is
// appended in the same transaction as the state it produced; deleting history
// leaves the session state, prepared actions, receipts and audit evidence alone.

const (
	// DefaultTranscriptPage and MaxTranscriptPage bound one read of the transcript.
	DefaultTranscriptPage = 50
	MaxTranscriptPage     = 500
)

// TranscriptTurn is one stored row of the transcript.
type TranscriptTurn struct {
	TurnID string
	N      int64
	// Role is "user" or "assistant".
	Role string
	// Act is the directive act of an assistant turn.
	Act  string
	Body string
	// Clipped is true if Body was cut at Config.MaxTranscriptRunes.
	Clipped   bool
	CreatedAt time.Time
}

// appendTranscript writes the turn's rows. It runs inside Save's transaction.
func (s *Store) appendTranscript(ctx context.Context, tx *store.Tx, scope contracts.Scope, n int64, rec TurnRecord, now time.Time) error {
	cols := []string{"session_id", "principal_id", "turn_id", "n", "role", "act", "body", "clipped", "created_at"}
	put := func(role, actName, text string) error {
		body := clip(text, s.cfg.MaxTranscriptRunes)
		clipped := 0
		if body != text {
			clipped = 1
		}
		_, err := store.InsertScoped(ctx, tx, scope, "transcript_turns", cols,
			scope.SessionID, scope.PrincipalID, rec.TurnID, n, role, actName, body, clipped, toMS(now))
		return err
	}
	if rec.User != "" {
		if err := put("user", "", rec.User); err != nil {
			return err
		}
	}
	if text := rec.DisplayAssistant(); text != "" {
		return put("assistant", rec.Act, text)
	}
	return nil
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// checkBound reports whether the scope is the authenticated context the stored
// session was bound to.
func checkBound(sess Session, scope contracts.Scope) error {
	return identity.CheckContinuity(sess.Scope, scope)
}

// Transcript returns up to limit rows of the verified scope's session after turn
// afterN, oldest first (afterN 0 starts at the beginning). The session must be the
// same authenticated context it was bound to; a session that does not exist has an
// empty transcript. An ended session's transcript stays readable until it is
// cleared.
func (s *Store) Transcript(ctx context.Context, scope contracts.Scope, afterN int64, limit int) ([]TranscriptTurn, error) {
	if err := s.checkReader(ctx, scope); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = DefaultTranscriptPage
	}
	if limit > MaxTranscriptPage {
		limit = MaxTranscriptPage
	}
	rows, err := store.QueryScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL:       "SELECT turn_id, n, role, act, body, clipped, created_at FROM transcript_turns",
		Where:     "session_id = ? AND n > ?",
		WhereArgs: []any{scope.SessionID, afterN},
		Tail:      "ORDER BY n, CASE role WHEN 'user' THEN 0 ELSE 1 END LIMIT ?",
		TailArgs:  []any{limit},
	})
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()
	var out []TranscriptTurn
	for rows.Next() {
		var t TranscriptTurn
		var clipped, created int64
		if err := rows.Scan(&t.TurnID, &t.N, &t.Role, &t.Act, &t.Body, &clipped, &created); err != nil {
			return nil, wrap(err)
		}
		t.Clipped, t.CreatedAt = clipped == 1, fromMS(created)
		out = append(out, t)
	}
	return out, wrap(rows.Err())
}

// TranscriptLen is how many rows the session's transcript holds.
func (s *Store) TranscriptLen(ctx context.Context, scope contracts.Scope) (int64, error) {
	if err := s.checkReader(ctx, scope); err != nil {
		return 0, err
	}
	row, err := store.QueryRowScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL: "SELECT COUNT(*) FROM transcript_turns", Where: "session_id = ?", WhereArgs: []any{scope.SessionID},
	})
	if err != nil {
		return 0, err
	}
	var n int64
	if err := row.Scan(&n); err != nil {
		return 0, wrap(err)
	}
	return n, nil
}

// checkReader checks that the scope may read the transcript of its session: the
// scope is valid and, if the session exists, is the context it was bound to.
func (s *Store) checkReader(ctx context.Context, scope contracts.Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	sess, _, err := s.get(ctx, s.db, scope)
	switch {
	case err == nil:
		return checkBound(sess, scope)
	case isNoRows(err):
		return nil
	}
	return wrap(err)
}

// endedTurn is the condition, on a session_turns row aliased st, that its session
// has ended. A session ID is unique only inside its application, tenant and
// workspace, so the session is matched on all four.
const endedTurn = "EXISTS (SELECT 1 FROM sessions se WHERE se.application_id = st.application_id AND se.tenant_id = st.tenant_id" +
	" AND se.workspace_id = st.workspace_id AND se.id = st.session_id AND se.status = 'ended')"

// ClearTranscript deletes the transcript of the verified scope's session and
// returns how many rows went. The session state, its prepared actions, receipts
// and audit evidence are not touched (master plan 19).
//
// The recent window (session_turns) of an ENDED session goes with it, in the same
// transaction: nothing reads it once the session has ended, and it holds the same
// text. The window of an ACTIVE session is session state and stays, as the
// transcript migration says of clearing history: the dialogue engine reads it
// every turn.
func (s *Store) ClearTranscript(ctx context.Context, scope contracts.Scope) (int64, error) {
	if err := s.checkReader(ctx, scope); err != nil {
		return 0, err
	}
	var n int64
	err := s.db.Tx(ctx, func(tx *store.Tx) error {
		res, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
			SQL: "DELETE FROM transcript_turns", Where: "session_id = ?", WhereArgs: []any{scope.SessionID},
		})
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		_, err = store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
			SQL: "DELETE FROM session_turns AS st", Alias: "st", Where: "st.session_id = ? AND " + endedTurn, WhereArgs: []any{scope.SessionID},
		})
		return err
	})
	if err != nil {
		return 0, wrap(err)
	}
	return n, nil
}

// pruneBatch bounds the rows one transaction of PruneTranscript deletes.
var pruneBatch = 1000

// PruneTranscript deletes the transcript rows of the scope's application, tenant
// and workspace that are older than cutoff, for retention. scope is the caller's
// authenticated context; its session is not used. It returns how many rows went.
//
// The recent-window rows (session_turns) of the scope's ENDED sessions that are
// older than cutoff go too, after the transcript: the same text, read by nothing
// once a session has ended. An ACTIVE session's window stays whatever its age (it
// is session state, which clearing history leaves alone); it is pruned by the
// first prune after the session ends.
func (s *Store) PruneTranscript(ctx context.Context, scope contracts.Scope, cutoff time.Time) (int64, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	// One batch per transaction, so a large prune never holds the single writer
	// (and the per-row FTS trigger) for the whole delete. The batch is scoped
	// too: other scopes' old rows must not fill its LIMIT.
	pred, args, err := store.ScopedWhere(scope)
	if err != nil {
		return 0, err
	}
	total, err := s.pruneBatches(ctx, scope, store.ScopedStmt{
		SQL:       "DELETE FROM transcript_turns",
		Where:     "seq IN (SELECT seq FROM transcript_turns WHERE " + pred + " AND created_at < ? LIMIT ?)",
		WhereArgs: append(args, toMS(cutoff), pruneBatch),
	})
	if err != nil {
		return 0, err
	}
	stPred, stArgs, err := store.ScopedWhereAs(scope, "st")
	if err != nil {
		return 0, err
	}
	if _, err := s.pruneBatches(ctx, scope, store.ScopedStmt{
		SQL:       "DELETE FROM session_turns",
		Where:     "seq IN (SELECT st.seq FROM session_turns st WHERE " + stPred + " AND st.created_at < ? AND " + endedTurn + " LIMIT ?)",
		WhereArgs: append(stArgs, toMS(cutoff), pruneBatch),
	}); err != nil {
		return 0, err
	}
	return total, nil
}

// pruneBatches runs a batched delete until a batch takes fewer than pruneBatch
// rows, and returns how many rows went.
func (s *Store) pruneBatches(ctx context.Context, scope contracts.Scope, batch store.ScopedStmt) (int64, error) {
	var total int64
	for {
		res, err := store.ExecScoped(ctx, s.db, scope, batch)
		if err != nil {
			return 0, wrap(err)
		}
		n, _ := res.RowsAffected()
		total += n
		if n < int64(pruneBatch) {
			return total, nil
		}
	}
}
