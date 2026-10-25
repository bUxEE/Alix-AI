package sessions

import (
	"context"
	"time"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Erasure of the conversation history of a tenant, or of one principal in it, on an
// operator's request (`alixai retention erase`, ADR-0036). Clearing history and
// retention leave the session state alone; an erasure takes all of it: the
// transcript (deleted through transcript_turns, so the history index's triggers drop
// every row from search in the same statement), the recent windows, the lookup rows
// and the session rows. Nothing else references a session row, so it is removed, not
// reduced: its identity columns cannot be rewritten anyway (sessions_bound_for_life).
//
// An ACTIVE session is ended first, the way End leaves one (status ended, state
// emptied, its lookup rows dropped), and then erased with the ended ones: once ended,
// no turn can be saved to it, so nothing is added behind the erasure. End would first
// void the preparation its goal holds; nothing is voided here, because the caller
// refuses the whole erasure, in the same transaction, while any pending or uncertain
// action of the tenant or principal exists (actions.EraseFinished does), so an ended
// session never leaves a confirmable preparation behind. A host that presents the
// erased session's id again with a still-valid token opens a new, empty session.
//
// Like the receipts', an erasure spans the tenant's workspaces and never another
// tenant. The order is transcript, windows, lookup rows, sessions: the windows and
// the lookup rows carry no principal and are found through their session, so a
// session row goes only after them.

// eraseBatch bounds the rows one EraseHistory call changes (like pruneBatch), so an
// erasure never holds the single writer, and the per-row index trigger, for long.
var eraseBatch = 1000

// EraseSpec says whose conversation history is erased. ApplicationID and TenantID
// are required.
type EraseSpec struct {
	ApplicationID string
	TenantID      string
	// PrincipalID, when set, narrows to the sessions of that principal and their
	// transcript.
	PrincipalID string
	// Now is when an active session is ended (EraseHistory only).
	Now time.Time
}

// EraseOutcome is what EraseHistory changed, or what CountHistory found.
type EraseOutcome struct {
	// TranscriptTurns and SessionTurns are rows of the transcript and of the recent
	// windows, Sessions session rows; Ended is how many of the sessions were active
	// and were ended first (each is counted in Sessions too once it is removed).
	TranscriptTurns, SessionTurns, Sessions, Ended int
	// More is set when the batch was full: call again.
	More bool
}

func (s EraseSpec) validate() error {
	if s.ApplicationID == "" || s.TenantID == "" {
		return contracts.New(contracts.ErrScopeMissing, "sessions: an erasure names an application and a tenant")
	}
	return nil
}

// owned is the condition that a row of sessions or transcript_turns (both carry the
// principal) is the spec's.
func (s EraseSpec) owned() (string, []any) {
	where, args := "application_id = ? AND tenant_id = ?", []any{s.ApplicationID, s.TenantID}
	if s.PrincipalID != "" {
		where += " AND principal_id = ?"
		args = append(args, s.PrincipalID)
	}
	return where, args
}

// ofSession is the condition that a row aliased a of a table keyed by session
// (session_turns, session_refs) belongs to a session of the spec, an ended one with
// ended. As in endedTurn, the session is matched on application, tenant, workspace
// and id: a session id is unique only inside its workspace.
func (s EraseSpec) ofSession(a string, ended bool) (string, []any) {
	where := a + ".application_id = ? AND " + a + ".tenant_id = ? AND EXISTS (SELECT 1 FROM sessions se WHERE se.application_id = " + a +
		".application_id AND se.tenant_id = " + a + ".tenant_id AND se.workspace_id = " + a + ".workspace_id AND se.id = " + a + ".session_id"
	args := []any{s.ApplicationID, s.TenantID}
	if ended {
		where += " AND se.status = 'ended'"
	}
	if s.PrincipalID != "" {
		where += " AND se.principal_id = ?"
		args = append(args, s.PrincipalID)
	}
	return where + ")", args
}

// CountHistory counts what erasing spec removes, with the conditions EraseHistory
// deletes by: a dry run reports it.
func CountHistory(ctx context.Context, q store.Reader, spec EraseSpec) (EraseOutcome, error) {
	var out EraseOutcome
	if err := spec.validate(); err != nil {
		return out, err
	}
	owned, args := spec.owned()
	if err := q.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(status = 'active'), 0) FROM sessions WHERE "+owned, args...).Scan(&out.Sessions, &out.Ended); err != nil {
		return EraseOutcome{}, wrap(err)
	}
	if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM transcript_turns WHERE "+owned, args...).Scan(&out.TranscriptTurns); err != nil {
		return EraseOutcome{}, wrap(err)
	}
	turns, targs := spec.ofSession("st", false)
	if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM session_turns st WHERE "+turns, targs...).Scan(&out.SessionTurns); err != nil {
		return EraseOutcome{}, wrap(err)
	}
	return out, nil
}

// EraseHistory erases one batch of spec's conversation history inside tx, at most
// eraseBatch rows in all: it ends the active sessions, then deletes the transcript,
// the recent windows, the lookup rows and the ended sessions, each only once the one
// before it has nothing left. A caller that gets More calls again in a new
// transaction. The caller records the erasure in the same transaction and must
// refuse it there while a pending or uncertain action of the spec exists (see above).
func EraseHistory(ctx context.Context, tx *store.Tx, spec EraseSpec) (EraseOutcome, error) {
	var out EraseOutcome
	switch {
	case tx == nil:
		return out, contracts.New(contracts.ErrInternal, "sessions: EraseHistory needs a transaction")
	case spec.Now.IsZero():
		return out, contracts.New(contracts.ErrValidationFailed, "sessions: an erasure needs the time")
	}
	if err := spec.validate(); err != nil {
		return out, err
	}
	empty, err := State{}.encode()
	if err != nil {
		return out, err
	}
	left := eraseBatch
	// step runs a statement whose last placeholder is the LIMIT of what is left of
	// the batch, and counts what it changed.
	step := func(n *int, query string, args ...any) error {
		if left == 0 {
			return nil
		}
		res, err := tx.ExecContext(ctx, query, append(args, left)...)
		if err != nil {
			return wrap(err)
		}
		k, _ := res.RowsAffected()
		*n += int(k)
		left -= int(k)
		return nil
	}
	owned, args := spec.owned()
	turns, targs := spec.ofSession("st", true)
	refs, rargs := spec.ofSession("sr", true)
	var refsGone int
	for _, s := range []struct {
		n     *int
		query string
		args  []any
	}{
		{&out.Ended, "UPDATE sessions SET status = 'ended', state_json = ?, version = version + 1, updated_at = ?, next_deadline = NULL" +
			" WHERE seq IN (SELECT seq FROM sessions WHERE " + owned + " AND status = 'active' LIMIT ?)",
			append([]any{string(empty), toMS(spec.Now)}, args...)},
		{&out.TranscriptTurns, "DELETE FROM transcript_turns WHERE seq IN (SELECT seq FROM transcript_turns WHERE " + owned + " LIMIT ?)", args},
		{&out.SessionTurns, "DELETE FROM session_turns WHERE seq IN (SELECT st.seq FROM session_turns st WHERE " + turns + " LIMIT ?)", targs},
		{&refsGone, "DELETE FROM session_refs WHERE seq IN (SELECT sr.seq FROM session_refs sr WHERE " + refs + " LIMIT ?)", rargs},
		{&out.Sessions, "DELETE FROM sessions WHERE seq IN (SELECT seq FROM sessions WHERE " + owned + " AND status = 'ended' LIMIT ?)", args},
	} {
		if err := step(s.n, s.query, append([]any(nil), s.args...)...); err != nil {
			return EraseOutcome{}, err
		}
	}
	out.More = left == 0
	return out, nil
}
