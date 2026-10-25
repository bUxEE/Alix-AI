package sessions

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"slices"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

//go:embed migrations/*.sql
var migrations embed.FS

// transcriptMigrations holds the migrations of the separate "transcript" module.
// They live under transcript/migrations so the module's tables are versioned and
// checksummed on their own, even though the code that appends to them is here.
//
//go:embed transcript/migrations/*.sql
var transcriptMigrations embed.FS

func init() {
	store.RegisterMigrations("sessions", migrations)
	store.RegisterMigrations("transcript", transcriptFS())
}

// transcriptFS is the transcript module's migrations with the module directory as
// root, which is the layout store.Registry expects.
func transcriptFS() fs.FS {
	sub, err := fs.Sub(transcriptMigrations, "transcript")
	if err != nil {
		panic("sessions: transcript migrations: " + err.Error())
	}
	return sub
}

// Defaults of Config.
const (
	// DefaultGoalTTL is how long an idle goal is kept. It is longer than the
	// platform limit on a preparation (ten minutes), so a prepared action's own
	// expiry, not the goal's, decides when it lapses.
	DefaultGoalTTL = 30 * time.Minute
	// DefaultSuspendedTTL is how long a suspended goal waits to be resumed.
	DefaultSuspendedTTL = 2 * time.Hour
	// DefaultMaxSuspended is how many goals can be suspended at once.
	DefaultMaxSuspended = 3
	// DefaultMaxCapsules is how many completed-goal capsules a session keeps.
	DefaultMaxCapsules = 8
	// DefaultMaxResults is how many deterministic results a session keeps.
	DefaultMaxResults = 4
	// DefaultKeepTurns is how many turn rows a session keeps (two per turn).
	DefaultKeepTurns = 40
	// DefaultMaxTurnRunes clips a stored turn of the recent window.
	DefaultMaxTurnRunes = 2000
	// DefaultMaxTranscriptRunes clips a turn of the transcript. It is far above the
	// window's bound: the transcript is what the user reads back, but it is still
	// bounded so one turn cannot grow the table without limit.
	DefaultMaxTranscriptRunes = 8000
	// DefaultSweepBatch is how many sessions a workspace-wide invalidation reads at
	// a time.
	DefaultSweepBatch = 200
)

// Bounds that are not worth a knob.
const (
	maxResultItems = 10
	maxLabelRunes  = 80
	maxAllowedNext = 8
	maxFactRunes   = 80
	maxCapsuleFact = 8
	maxCapsuleEnts = 8
	maxSlotRunes   = 4000
	maxSlotList    = 64
)

// Config bounds the store. A zero field takes its default.
type Config struct {
	// GoalTTL is how long an idle goal lives; every turn that touches the goal
	// moves its expiry.
	GoalTTL time.Duration
	// PendingTTL is how long a pending clarification waits for its answer. Zero
	// means no limit of its own: it lives as long as its goal.
	PendingTTL time.Duration
	// SuspendedTTL is how long a suspended goal waits to be resumed.
	SuspendedTTL time.Duration
	// MaxSuspended is how many goals can be suspended at once; suspending one more
	// discards the one suspended longest ago.
	MaxSuspended int
	// MaxCapsules is how many completed-goal capsules are kept; older ones are
	// dropped.
	MaxCapsules int
	// MaxResults is how many deterministic results are kept; older ones are
	// dropped. A result also lapses after GoalTTL.
	MaxResults int
	// KeepTurns is how many stored turn rows (a turn is a user row and an
	// assistant row) a session keeps in its recent window; older ones are pruned.
	KeepTurns int
	// MaxTurnRunes clips each turn of the recent window.
	MaxTurnRunes int
	// MaxTranscriptRunes clips each turn of the transcript.
	MaxTranscriptRunes int
	// SweepBatch is how many sessions a workspace-wide invalidation reads per page.
	SweepBatch int
}

func (c Config) withDefaults() Config {
	if c.GoalTTL <= 0 {
		c.GoalTTL = DefaultGoalTTL
	}
	if c.PendingTTL < 0 {
		c.PendingTTL = 0
	}
	if c.SuspendedTTL <= 0 {
		c.SuspendedTTL = DefaultSuspendedTTL
	}
	if c.MaxSuspended <= 0 {
		c.MaxSuspended = DefaultMaxSuspended
	}
	if c.MaxCapsules <= 0 {
		c.MaxCapsules = DefaultMaxCapsules
	}
	if c.MaxResults <= 0 {
		c.MaxResults = DefaultMaxResults
	}
	if c.KeepTurns <= 0 {
		c.KeepTurns = DefaultKeepTurns
	}
	if c.MaxTurnRunes <= 0 {
		c.MaxTurnRunes = DefaultMaxTurnRunes
	}
	if c.MaxTranscriptRunes <= 0 {
		c.MaxTranscriptRunes = DefaultMaxTranscriptRunes
	}
	if c.SweepBatch <= 0 {
		c.SweepBatch = DefaultSweepBatch
	}
	return c
}

// Store is the durable session store. It is safe for concurrent use.
type Store struct {
	db  *store.DB
	clk clock.Clock
	cfg Config
}

// New returns a Store over db. A nil clock means clock.Real.
func New(db *store.DB, clk clock.Clock, cfg Config) (*Store, error) {
	if db == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "sessions: a store needs a database")
	}
	if clk == nil {
		clk = clock.Real{}
	}
	return &Store{db: db, clk: clk, cfg: cfg.withDefaults()}, nil
}

// GoalTTL is how long an idle goal lives.
func (s *Store) GoalTTL() time.Duration { return s.cfg.GoalTTL }

// Config returns the effective configuration, defaults filled in. The state
// methods that need a bound (Suspend, Complete, Expire, ...) take it.
func (s *Store) Config() Config { return s.cfg }

const sessionCols = "seq, id, principal_id, scope_json, status, state_json, version, turn_count, created_at, updated_at"

func toMS(t time.Time) int64    { return t.UTC().UnixMilli() }
func fromMS(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

type rowScanner interface{ Scan(dest ...any) error }

// sessionRow is a row of sessions (sessionCols) as read, before it is decoded.
type sessionRow struct {
	seq                                         int64
	id, principal, scopeJSON, status, stateJSON string
	version, turns, created, updated            int64
}

func scanSessionRow(r rowScanner) (sessionRow, error) {
	var w sessionRow
	err := r.Scan(&w.seq, &w.id, &w.principal, &w.scopeJSON, &w.status, &w.stateJSON, &w.version, &w.turns, &w.created, &w.updated)
	return w, err
}

func scanSession(r rowScanner) (Session, string, error) {
	w, err := scanSessionRow(r)
	if err != nil {
		return Session{}, "", err
	}
	return w.decode()
}

// decode returns the session of the row and its status.
func (w sessionRow) decode() (Session, string, error) {
	var sc contracts.Scope
	if err := json.Unmarshal([]byte(w.scopeJSON), &sc); err != nil {
		return Session{}, "", contracts.New(contracts.ErrInternal, "sessions: the stored scope is unreadable")
	}
	if sc.SessionID != w.id || sc.PrincipalID != w.principal {
		return Session{}, "", contracts.New(contracts.ErrInternal, "sessions: the stored scope differs from its columns")
	}
	st, err := decodeState([]byte(w.stateJSON))
	if err != nil {
		return Session{}, "", err
	}
	return Session{
		ID: w.id, Scope: sc, State: st, Version: w.version, TurnCount: w.turns,
		CreatedAt: fromMS(w.created), UpdatedAt: fromMS(w.updated), seq: w.seq,
	}, w.status, nil
}

// get reads the session of the scope's session ID, or sql.ErrNoRows.
func (s *Store) get(ctx context.Context, q store.Reader, scope contracts.Scope) (Session, string, error) {
	row, err := store.QueryRowScoped(ctx, q, scope, store.ScopedStmt{
		SQL:       "SELECT " + sessionCols + " FROM sessions",
		Where:     "id = ?",
		WhereArgs: []any{scope.SessionID},
	})
	if err != nil {
		return Session{}, "", err
	}
	return scanSession(row)
}

// Open returns the session of the verified scope, creating it on the first turn.
// The session ID is the one the token carries. An existing session must be the
// same authenticated context it was bound to (identity.CheckContinuity); an ended
// session cannot be reopened. A session is found only through the application,
// tenant and workspace of the scope, so the same session ID presented under
// another tenant or workspace is another, empty session: a session never changes
// tenant or workspace (master plan 10.2). Open changes nothing in an existing
// session; Reconcile is the call that also applies a narrower token.
func (s *Store) Open(ctx context.Context, scope contracts.Scope) (Session, error) {
	if err := scope.Validate(); err != nil {
		return Session{}, err
	}
	for range 2 {
		sess, status, err := s.get(ctx, s.db, scope)
		switch {
		case err == nil:
			if err := identity.CheckContinuity(sess.Scope, scope); err != nil {
				return Session{}, err
			}
			if status == "ended" {
				return Session{}, contracts.New(contracts.ErrExpired, "the session has ended; a new session needs a new authenticated context")
			}
			return sess, nil
		case !errors.Is(err, sql.ErrNoRows):
			return Session{}, wrap(err)
		}
		created, err := s.create(ctx, scope)
		if err == nil {
			return created, nil
		}
		if !store.IsUniqueViolation(err) {
			return Session{}, wrap(err)
		}
		// Another writer created it between the read and the insert: read again.
	}
	return Session{}, contracts.New(contracts.ErrConflict, "sessions: the session keeps changing; try again")
}

func (s *Store) create(ctx context.Context, scope contracts.Scope) (Session, error) {
	now := s.clk.Now()
	state := State{Perms: permsOf(scope.Roles, scope.CapabilityScopes)}
	stateJSON, err := state.encode()
	if err != nil {
		return Session{}, err
	}
	bound := scope.Clone()
	scopeJSON, err := json.Marshal(bound)
	if err != nil {
		return Session{}, contracts.Wrap(contracts.ErrInternal, err, "sessions: the scope cannot be encoded")
	}
	var seq int64
	err = s.db.Tx(ctx, func(tx *store.Tx) error {
		res, err := store.InsertScoped(ctx, tx, scope, "sessions",
			[]string{"id", "principal_id", "scope_json", "status", "state_json", "version", "turn_count", "created_at", "updated_at"},
			scope.SessionID, scope.PrincipalID, string(scopeJSON), "active", string(stateJSON), 1, 0, toMS(now), toMS(now))
		if err != nil {
			return err
		}
		seq, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return Session{}, err
	}
	return Session{ID: scope.SessionID, Scope: bound, State: state, Version: 1, CreatedAt: now.UTC(), UpdatedAt: now.UTC(), seq: seq}, nil
}

// permsOf is a sorted copy of what a token allowed.
func permsOf(roles, caps []string) *Permissions {
	p := &Permissions{Roles: slices.Clone(roles), Capabilities: slices.Clone(caps)}
	slices.Sort(p.Roles)
	slices.Sort(p.Capabilities)
	return p
}

// wrap types a store failure that is not already typed.
func wrap(err error) error {
	if err == nil {
		return nil
	}
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return err
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, "sessions: the session store failed")
}

// Save writes the state of a session, and the turn it belongs to when rec is not
// nil, in one transaction. It compares the version the session was read at: if
// another writer saved first, nothing is written and the error is conflict. The
// returned session carries the new version.
//
// Save writes only the row the session was read from, and never rewrites its
// identity: a Session value whose scope was edited to another tenant cannot
// overwrite that tenant's session. The state is first bounded (goal IDs assigned,
// suspended goals, results, capsules and the allowed-next list clipped to their
// limits, a clarification's expiry stamped) and the lookup rows and the earliest
// deadline are rewritten with it. With rec the turn goes to the recent window
// (clipped and pruned) and to the transcript (kept apart, clipped far higher).
func (s *Store) Save(ctx context.Context, sess Session, rec *TurnRecord) (Session, error) {
	if err := sess.Scope.Validate(); err != nil {
		return Session{}, err
	}
	if sess.seq == 0 || sess.ID != sess.Scope.SessionID {
		return Session{}, contracts.New(contracts.ErrValidationFailed, "sessions: a session can be saved only as it was read from the store")
	}
	now := s.clk.Now()
	next := sess
	s.normalize(&next.State, now)
	stateJSON, err := next.State.encode()
	if err != nil {
		return Session{}, err
	}
	var deadline any
	if d, ok := next.State.nextDeadline(s.cfg); ok {
		deadline = toMS(d)
	}
	refs := next.State.refs()
	next.Version = sess.Version + 1
	next.UpdatedAt = now.UTC()
	if rec != nil {
		next.TurnCount = sess.TurnCount + 1
	}
	err = s.db.Tx(ctx, func(tx *store.Tx) error {
		res, err := store.ExecScoped(ctx, tx, sess.Scope, store.ScopedStmt{
			SQL:       "UPDATE sessions SET state_json = ?, version = ?, turn_count = ?, updated_at = ?, next_deadline = ?",
			Args:      []any{string(stateJSON), next.Version, next.TurnCount, toMS(now), deadline},
			Where:     "seq = ? AND id = ? AND version = ? AND status = 'active'",
			WhereArgs: []any{sess.seq, sess.ID, sess.Version},
		})
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errMoved
		}
		if err := s.writeRefs(ctx, tx, sess.Scope, sess.ID, refs); err != nil {
			return err
		}
		if rec == nil {
			return nil
		}
		if err := s.writeTurns(ctx, tx, sess.Scope, sess.ID, next.TurnCount, *rec, now); err != nil {
			return err
		}
		return s.appendTranscript(ctx, tx, sess.Scope, next.TurnCount, *rec, now)
	})
	switch {
	case errors.Is(err, errMoved):
		return Session{}, contracts.New(contracts.ErrConflict, "the session changed while the turn was being processed; try again")
	case err != nil:
		return Session{}, wrap(err)
	}
	return next, nil
}

var errMoved = errors.New("sessions: the session moved")

// writeRefs makes the lookup rows of a session those of its new state. Most saves
// change no reference, so only the rows that differ are deleted or inserted.
func (s *Store) writeRefs(ctx context.Context, tx *store.Tx, scope contracts.Scope, sessionID string, refs []ref) error {
	if len(refs) == 0 {
		_, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
			SQL:       "DELETE FROM session_refs",
			Where:     "session_id = ?",
			WhereArgs: []any{sessionID},
		})
		return err
	}
	rows, err := store.QueryScoped(ctx, tx, scope, store.ScopedStmt{
		SQL:       "SELECT kind, ref_type, ref_id FROM session_refs",
		Where:     "session_id = ?",
		WhereArgs: []any{sessionID},
	})
	if err != nil {
		return err
	}
	stored := map[ref]bool{}
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.kind, &r.typ, &r.id); err != nil {
			rows.Close()
			return err
		}
		stored[r] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range refs {
		if stored[r] {
			delete(stored, r)
			continue
		}
		if _, err := store.InsertScoped(ctx, tx, scope, "session_refs",
			[]string{"session_id", "kind", "ref_type", "ref_id"}, sessionID, r.kind, r.typ, r.id); err != nil {
			return err
		}
	}
	for r := range stored {
		if _, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
			SQL:       "DELETE FROM session_refs",
			Where:     "session_id = ? AND kind = ? AND ref_type = ? AND ref_id = ?",
			WhereArgs: []any{sessionID, r.kind, r.typ, r.id},
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) writeTurns(ctx context.Context, tx *store.Tx, scope contracts.Scope, sessionID string, n int64, rec TurnRecord, now time.Time) error {
	cols := []string{"session_id", "turn_id", "n", "role", "act", "body", "created_at"}
	if rec.User != "" {
		if _, err := store.InsertScoped(ctx, tx, scope, "session_turns", cols,
			sessionID, rec.TurnID, n, "user", "", clip(rec.User, s.cfg.MaxTurnRunes), toMS(now)); err != nil {
			return err
		}
	}
	if rec.Assistant != "" {
		if _, err := store.InsertScoped(ctx, tx, scope, "session_turns", cols,
			sessionID, rec.TurnID, n, "assistant", rec.Act, clip(rec.Assistant, s.cfg.MaxTurnRunes), toMS(now)); err != nil {
			return err
		}
	}
	keep := int64(s.cfg.KeepTurns / 2)
	if keep < 1 {
		keep = 1
	}
	if n > keep {
		if _, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
			SQL:       "DELETE FROM session_turns",
			Where:     "session_id = ? AND n <= ?",
			WhereArgs: []any{sessionID, n - keep},
		}); err != nil {
			return err
		}
	}
	return nil
}

// RecentTurns returns the last rows of the session's recent window, oldest
// first. At most limit rows are returned (a turn is up to two rows). It reads the
// bounded window only; the transcript is never consulted to build model context.
func (s *Store) RecentTurns(ctx context.Context, sess Session, limit int) ([]Turn, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := store.QueryScoped(ctx, s.db, sess.Scope, store.ScopedStmt{
		SQL:       "SELECT turn_id, n, role, act, body, created_at FROM session_turns",
		Where:     "session_id = ?",
		WhereArgs: []any{sess.ID},
		Tail:      "ORDER BY n DESC, CASE role WHEN 'assistant' THEN 1 ELSE 0 END DESC LIMIT ?",
		TailArgs:  []any{limit},
	})
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()
	var out []Turn
	for rows.Next() {
		var t Turn
		var created int64
		if err := rows.Scan(&t.TurnID, &t.N, &t.Role, &t.Act, &t.Body, &created); err != nil {
			return nil, wrap(err)
		}
		t.CreatedAt = fromMS(created)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap(err)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// Invalidator is the part of the action coordinator the session layer needs.
// *actions.Coordinator implements it.
type Invalidator interface {
	Invalidate(ctx context.Context, scope contracts.Scope, preparationID string, reason act.PreparedActionInvalidationConditionsItem) (contracts.PreparedAction, error)
}

// End closes the session of the verified scope. The preparation the goal
// references is invalidated first (session_ended); if that fails, the session
// stays open so the event can be retried and a prepared action is never left
// confirmable in a closed session. A suspended goal never holds a preparation
// (suspending voids it), so only the active goal's is considered. If a turn saved
// the session in between, it is read again and its preparation voided in turn.
// End never commits anything, and it leaves the transcript alone: clearing
// history is a separate decision (ClearTranscript).
func (s *Store) End(ctx context.Context, scope contracts.Scope, inv Invalidator) error {
	sess, err := s.Open(ctx, scope)
	if err != nil {
		return err
	}
	for range maxApplyTries {
		if g := sess.State.Goal; g != nil && g.PreparedID != "" {
			if inv == nil {
				return contracts.New(contracts.ErrInternal, "sessions: a session with a prepared action cannot be ended without an invalidator")
			}
			if err := s.void(ctx, inv, sess.Scope, Void{PreparedID: g.PreparedID, Reason: act.PreparedActionInvalidationConditionsItemSessionEnded}); err != nil {
				return err
			}
		}
		err := s.endRow(ctx, sess, true)
		if contracts.CodeOf(err) != contracts.ErrConflict {
			return err
		}
		fresh, status, gerr := s.get(ctx, s.db, sess.Scope)
		if gerr != nil {
			return wrap(gerr)
		}
		if status == "ended" {
			return nil
		}
		sess = fresh
	}
	return contracts.New(contracts.ErrConflict, "sessions: the session keeps changing; try again")
}

// endRow marks the session ended and empties its state. With checkVersion the
// write fails with conflict if the session was saved since it was read.
func (s *Store) endRow(ctx context.Context, sess Session, checkVersion bool) error {
	stateJSON, err := State{}.encode()
	if err != nil {
		return err
	}
	now := s.clk.Now()
	err = s.db.Tx(ctx, func(tx *store.Tx) error {
		stmt := store.ScopedStmt{
			SQL:       "UPDATE sessions SET status = 'ended', state_json = ?, version = version + 1, updated_at = ?, next_deadline = NULL",
			Args:      []any{string(stateJSON), toMS(now)},
			Where:     "seq = ? AND id = ?",
			WhereArgs: []any{sess.seq, sess.ID},
		}
		if checkVersion {
			stmt.Where += " AND version = ? AND status = 'active'"
			stmt.WhereArgs = append(stmt.WhereArgs, sess.Version)
		}
		res, err := store.ExecScoped(ctx, tx, sess.Scope, stmt)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errMoved
		}
		return s.writeRefs(ctx, tx, sess.Scope, sess.ID, nil)
	})
	if errors.Is(err, errMoved) {
		return contracts.New(contracts.ErrConflict, "the session changed while it was being ended; try again")
	}
	return wrap(err)
}

// void invalidates one preparation through the coordinator. It must never run
// inside a database transaction: the coordinator writes to the same database. A
// preparation that already left the confirmable states is not an obstacle (that is
// the outcome wanted), nor is one the coordinator no longer holds under the
// session's own scope (retention removed it once it was over); any other failure
// is returned and the caller leaves the state as it was, so the event can be
// retried and a preparation is never left confirmable behind a state that no
// longer mentions it.
func (s *Store) void(ctx context.Context, inv Invalidator, scope contracts.Scope, v Void) error {
	reasons := []act.PreparedActionInvalidationConditionsItem{v.Reason}
	if v.Reason != act.PreparedActionInvalidationConditionsItemGoalChanged {
		// A preparation lists the conditions it honours; if it does not list this
		// one, goal_changed (which every preparation lists) still voids it.
		reasons = append(reasons, act.PreparedActionInvalidationConditionsItemGoalChanged)
	}
	var err error
	for _, reason := range reasons {
		_, err = inv.Invalidate(ctx, scope, v.PreparedID, reason)
		if err == nil {
			return nil
		}
		switch contracts.CodeOf(err) {
		case contracts.ErrConflict, contracts.ErrExpired, contracts.ErrStale:
			return nil
		case contracts.ErrScopeViolation:
			if notStored(err) {
				return nil
			}
		case contracts.ErrValidationFailed:
			continue
		}
		return err
	}
	return err
}

// notStored reports the coordinator's "no such preparation in this scope".
func notStored(err error) bool {
	var ce *contracts.Error
	return errors.As(err, &ce) && ce.Code == contracts.ErrScopeViolation && ce.Details["reason"] == "not_found"
}
