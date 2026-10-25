package sessions

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// InvalidationKind says what happened that can make pending state stale (master
// plan 11.3).
type InvalidationKind string

// Kinds of invalidation.
const (
	// KindRecordRevision: the host application revised records the state relied
	// on. Preparations built on an older version are voided (data_version_changed).
	KindRecordRevision InvalidationKind = "record_revision"
	// KindApplicationEvent: the host application reported something that may
	// concern pending state (application_event).
	KindApplicationEvent InvalidationKind = "application_event"
	// KindPermissionChange: what the principal may do narrowed (permission_changed).
	KindPermissionChange InvalidationKind = "permission_change"
	// KindPrincipalChange: the user behind the session changed (principal_changed).
	// The session ends.
	KindPrincipalChange InvalidationKind = "principal_change"
	// KindTimeout: deadlines passed. Nothing is invalidated through the
	// coordinator for a lapsed preparation; an idle goal's is voided (goal_changed).
	KindTimeout InvalidationKind = "timeout"
)

// MaxInvalidationEntities bounds the records one invalidation may name.
const MaxInvalidationEntities = 100

// EntityRef names a record and, for a revision, the version it is now at. An
// empty Version means the host did not say, and every version the state relied on
// counts as stale.
type EntityRef struct {
	EntityType string
	EntityID   string
	Version    string
}

// Invalidation describes one event that can invalidate pending state. It carries
// no scope and no authority: it can only void, discard or clear, never prepare,
// confirm, commit or call a tool. The caller authenticates the source of the
// event; the store scopes the sessions it touches.
type Invalidation struct {
	Kind InvalidationKind
	// Entities are the records concerned. Required for a record revision; a
	// selector for an application event.
	Entities []EntityRef
	// Capabilities are the capabilities concerned: a selector for an application
	// event, the capabilities no longer permitted for a permission change.
	Capabilities []string
	// Permitted is for a permission change inside the process: a capability it
	// rejects is no longer permitted. It is not serialisable and carries no
	// authority of its own; the caller builds it from the policy gateway.
	Permitted func(capabilityID string) bool
	// Everything applies an application event or a permission change to all the
	// pending state of every session considered, when the host cannot say more.
	Everything bool
	// PrincipalID restricts a workspace-wide invalidation to one principal's
	// sessions; required for a principal change.
	PrincipalID string
}

// Validate reports whether the invalidation is well formed. A malformed one is
// refused, never read as "everything" or "nothing".
func (inv Invalidation) Validate() error {
	bad := func(msg string) error { return contracts.New(contracts.ErrValidationFailed, "sessions: "+msg) }
	if len(inv.Entities) > MaxInvalidationEntities {
		return bad("an invalidation names too many records")
	}
	for _, e := range inv.Entities {
		if strings.TrimSpace(e.EntityType) == "" || strings.TrimSpace(e.EntityID) == "" {
			return bad("an invalidation names a record without its type and ID")
		}
	}
	for _, c := range inv.Capabilities {
		if strings.TrimSpace(c) == "" {
			return bad("an invalidation names an empty capability")
		}
	}
	switch inv.Kind {
	case KindRecordRevision:
		if len(inv.Entities) == 0 {
			return bad("a record revision names the records it revised")
		}
	case KindApplicationEvent:
		if len(inv.Entities) == 0 && len(inv.Capabilities) == 0 && !inv.Everything {
			return bad("an application event says what it concerns, or that it concerns everything")
		}
	case KindPermissionChange:
		if len(inv.Capabilities) == 0 && inv.Permitted == nil && !inv.Everything {
			return bad("a permission change says which capabilities are gone, or that everything must be rechecked")
		}
	case KindPrincipalChange, KindTimeout:
	default:
		return bad("unknown invalidation kind")
	}
	return nil
}

// reason is the invalidation condition of a preparation this kind voids.
func (k InvalidationKind) reason() act.PreparedActionInvalidationConditionsItem {
	switch k {
	case KindRecordRevision:
		return act.PreparedActionInvalidationConditionsItemDataVersionChanged
	case KindApplicationEvent:
		return act.PreparedActionInvalidationConditionsItemApplicationEvent
	case KindPermissionChange:
		return act.PreparedActionInvalidationConditionsItemPermissionChanged
	case KindPrincipalChange:
		return act.PreparedActionInvalidationConditionsItemPrincipalChanged
	}
	return act.PreparedActionInvalidationConditionsItemGoalChanged
}

// revoked reports whether a permission change took the capability away.
func (inv Invalidation) revoked(capabilityID string) bool {
	return slices.Contains(inv.Capabilities, capabilityID) || (inv.Permitted != nil && !inv.Permitted(capabilityID))
}

// Invalidate applies an invalidation to the state in memory and reports what it
// did; the caller invalidates Impact.Voided through the coordinator and saves.
// Store.Invalidate does all of that for a stored session.
func (s *State) Invalidate(inv Invalidation, now time.Time, cfg Config) (Impact, error) {
	if err := inv.Validate(); err != nil {
		return Impact{}, err
	}
	cfg = cfg.withDefaults()
	switch inv.Kind {
	case KindTimeout:
		return s.Expire(now, cfg), nil
	case KindPrincipalChange:
		return s.endEverything(), nil
	}
	var imp Impact
	if inv.Kind == KindPermissionChange {
		imp = s.revokeCapabilities(inv)
	} else {
		imp = s.touchRecords(inv)
	}
	s.forgetLastIfVoided(imp)
	return imp, nil
}

// forgetLastIfVoided drops the summary of the last directive when it belonged to
// the confirmation of a preparation that was just voided: the next turn must not
// treat the stale question as still standing.
func (s *State) forgetLastIfVoided(imp Impact) {
	if s.Goal == nil {
		return
	}
	for _, v := range imp.Voided {
		if v.GoalID == s.Goal.ID {
			s.Last = nil
			return
		}
	}
}

// goals returns the active goal and the suspended ones.
func (s *State) goals() []*Goal {
	var out []*Goal
	if s.Goal != nil {
		out = append(out, s.Goal)
	}
	return append(out, s.Suspended...)
}

// void clears the preparation of a goal and reports it.
func (g *Goal) void(imp *Impact, reason act.PreparedActionInvalidationConditionsItem) {
	if id := g.ClearPrepared(); id != "" {
		imp.Voided = append(imp.Voided, Void{PreparedID: id, GoalID: g.ID, Reason: reason})
	}
}

// endEverything discards all state: the user behind the session changed.
func (s *State) endEverything() Impact {
	imp := Impact{Ended: true}
	for _, g := range s.goals() {
		g.void(&imp, act.PreparedActionInvalidationConditionsItemPrincipalChanged)
		imp.GoalsEnded++
	}
	imp.ResultsDropped = len(s.Results)
	*s = State{}
	return imp
}

// touchRecords applies a record revision or an application event: the goals that
// depend on the named records, capabilities, or on everything, lose what was
// derived from facts that may no longer hold. Slots a person supplied stay.
func (s *State) touchRecords(inv Invalidation) Impact {
	var imp Impact
	reason := inv.Kind.reason()
	// A record revision compares versions; an event naming a record has none to
	// compare and concerns whatever relied on it.
	stale := func(relied string, e EntityRef) bool {
		if inv.Kind == KindApplicationEvent {
			return true
		}
		return e.Version == "" || e.Version != relied
	}
	for _, g := range s.goals() {
		hit := inv.Everything || slices.Contains(inv.Capabilities, g.CapabilityID)
		keep := g.Entities[:0:0]
		for _, e := range g.Entities {
			dropped := false
			for _, r := range inv.Entities {
				if r.EntityType == e.EntityType && r.EntityID == e.EntityID && stale(e.Version, r) {
					dropped = true
					break
				}
			}
			if !dropped {
				keep = append(keep, e)
				continue
			}
			hit = true
			imp.EntitiesDropped++
			// A value the host resolved or the user picked from the host's candidates
			// was fixed by those facts; it is asked or resolved again. A value the
			// user typed stays, and is resolved again by the dialogue.
			if sl, ok := g.Slots[e.Field]; ok && (sl.Origin == OriginEntityResolution || sl.Origin == OriginChoice) {
				delete(g.Slots, e.Field)
			}
		}
		g.Entities = keep
		if p := g.Pending; p != nil && p.EntityType != "" {
		choices:
			for _, c := range p.Choices {
				for _, r := range inv.Entities {
					if r.EntityType == p.EntityType && r.EntityID == string(c.ChoiceID) && stale(p.Version, r) {
						hit = true
						break choices
					}
				}
			}
		}
		if !hit {
			continue
		}
		g.void(&imp, reason)
		if g.Pending != nil {
			g.Pending = nil
			imp.PendingCleared++
			if g == s.Goal {
				s.Last = nil
			}
		}
	}
	s.dropResults(&imp, func(r ResultSummary) (drop bool, items func(ResultItem) bool) {
		if inv.Everything || slices.Contains(inv.Capabilities, r.CapabilityID) {
			return true, nil
		}
		return false, func(it ResultItem) bool {
			for _, e := range inv.Entities {
				if e.EntityType == it.EntityType && e.EntityID == it.EntityID && stale(it.Version, e) {
					return true
				}
			}
			return false
		}
	})
	return imp
}

// dropResults removes results (or the presented records of a result) the filter
// names. A result whose records were all removed goes too: a list of nothing is
// not something "the second one" can refer to.
func (s *State) dropResults(imp *Impact, decide func(ResultSummary) (bool, func(ResultItem) bool)) {
	var keep []ResultSummary
	for _, r := range s.Results {
		whole, item := decide(r)
		if whole {
			imp.ResultsDropped++
			continue
		}
		if item != nil && len(r.Items) > 0 {
			var left []ResultItem
			for _, it := range r.Items {
				if item(it) {
					imp.ResultsDropped++
					continue
				}
				left = append(left, it)
			}
			if len(left) == 0 {
				continue
			}
			r.Items = left
		}
		keep = append(keep, r)
	}
	s.Results = keep
}

// revokeCapabilities applies a permission change. A goal on a capability that is
// no longer permitted is discarded with everything built on it: its slots and
// entities were gathered under a permission that is gone. Where the host could
// not say which capabilities changed (Everything), every preparation is voided and
// every question dropped, and the goals themselves stay to be rechecked by policy
// on the next turn.
func (s *State) revokeCapabilities(inv Invalidation) Impact {
	var imp Impact
	reason := inv.Kind.reason()
	discard := func(g *Goal) {
		g.void(&imp, reason)
		imp.GoalsEnded++
	}
	if g := s.Goal; g != nil && inv.revoked(g.CapabilityID) {
		discard(g)
		s.Goal, s.Last = nil, nil
	}
	var kept []*Goal
	for _, g := range s.Suspended {
		if inv.revoked(g.CapabilityID) {
			discard(g)
			continue
		}
		kept = append(kept, g)
	}
	s.Suspended = kept
	if inv.Everything {
		for _, g := range s.goals() {
			g.void(&imp, reason)
			if g.Pending != nil {
				g.Pending = nil
				imp.PendingCleared++
				if g == s.Goal {
					s.Last = nil
				}
			}
		}
		imp.ResultsDropped += len(s.Results)
		s.Results = nil
		s.AllowedNext = nil
	}
	s.dropResults(&imp, func(r ResultSummary) (bool, func(ResultItem) bool) { return inv.revoked(r.CapabilityID), nil })
	s.Capsules = slices.DeleteFunc(slices.Clone(s.Capsules), func(c Capsule) bool { return inv.revoked(c.CapabilityID) })
	s.AllowedNext = slices.DeleteFunc(slices.Clone(s.AllowedNext), inv.revoked)
	return imp
}

// Expire discards what has lapsed at now. An idle active goal is discarded with
// the preparation it referenced voided (goal_changed), leaving a capsule if it had
// finished; a clarification past its expiry is dropped; a prepared-action
// reference past the preparation's own expiry is dropped (the coordinator already
// refuses it; there is nothing to void); suspended goals past their expiry and
// results older than cfg.GoalTTL are discarded. Expired state is never reused.
func (s *State) Expire(now time.Time, cfg Config) Impact {
	cfg = cfg.withDefaults()
	var imp Impact
	lapse := func(t time.Time) bool { return !t.IsZero() && !now.Before(t) }
	parts := func(g *Goal) {
		if p := g.Pending; p != nil && lapse(p.ExpiresAt) {
			g.Pending = nil
			imp.PendingCleared++
			if g == s.Goal {
				s.Last = nil
			}
		}
		if g.PreparedID != "" && lapse(g.PreparedExpiresAt) {
			g.ClearPrepared()
			imp.Lapsed++
			if g == s.Goal {
				s.Last = nil
			}
		}
	}
	if g := s.Goal; g != nil {
		if lapse(g.ExpiresAt) {
			g.void(&imp, act.PreparedActionInvalidationConditionsItemGoalChanged)
			s.keepCapsuleOfFinished(g, now, cfg.MaxCapsules)
			s.Goal, s.Last = nil, nil
			imp.GoalsEnded++
		} else {
			parts(g)
		}
	}
	var kept []*Goal
	for _, g := range s.Suspended {
		if lapse(g.ExpiresAt) {
			g.void(&imp, act.PreparedActionInvalidationConditionsItemGoalChanged)
			imp.GoalsEnded++
			continue
		}
		parts(g)
		kept = append(kept, g)
	}
	s.Suspended = kept
	s.dropResults(&imp, func(r ResultSummary) (bool, func(ResultItem) bool) {
		return !r.At.IsZero() && lapse(r.At.Add(cfg.GoalTTL)), nil
	})
	return imp
}

// Prepared reports whether a goal has a preparation that can still be offered for
// confirmation at now. It is a convenience over the reference and its recorded
// expiry; the coordinator decides authoritatively.
func (g *Goal) Prepared(now time.Time) bool {
	return g != nil && g.PreparedID != "" && g.Phase == capabilities.PhasePrepared &&
		(g.PreparedExpiresAt.IsZero() || now.Before(g.PreparedExpiresAt))
}

// ---- stored sessions ----

const maxApplyTries = 4

// applyOpts adds to an invalidation of one stored session.
type applyOpts struct {
	// after runs on the state after the invalidation, before it is saved, and
	// reports whether it changed anything that must be saved even if the
	// invalidation did nothing.
	after func(*State) bool
}

// apply invalidates one stored session and saves the result. The preparations the
// state loses are invalidated through the coordinator first, outside any
// transaction, and the state is saved only if that worked: a preparation is never
// left confirmable behind a session that no longer mentions it. If another writer
// saved in between, the session is read again and the invalidation re-applied
// (voiding an already voided preparation is a no-op).
func (s *Store) apply(ctx context.Context, sess Session, inv Invalidation, ivr Invalidator, o applyOpts) (Session, Impact, error) {
	for range maxApplyTries {
		imp, err := sess.State.Invalidate(inv, s.clk.Now(), s.cfg)
		if err != nil {
			return sess, Impact{}, err
		}
		dirty := imp.Changed()
		if o.after != nil && o.after(&sess.State) {
			dirty = true
		}
		if !dirty {
			return sess, imp, nil
		}
		if len(imp.Voided) > 0 && ivr == nil {
			return sess, Impact{}, contracts.New(contracts.ErrInternal, "sessions: a session with a prepared action cannot be invalidated without an invalidator")
		}
		for _, v := range imp.Voided {
			if err := s.void(ctx, ivr, sess.Scope, v); err != nil {
				return sess, Impact{}, err
			}
		}
		var saved Session
		if imp.Ended {
			err = s.endRow(ctx, sess, true)
			saved = sess
		} else {
			saved, err = s.Save(ctx, sess, nil)
		}
		if err == nil {
			return saved, imp, nil
		}
		if contracts.CodeOf(err) != contracts.ErrConflict {
			return sess, Impact{}, err
		}
		fresh, status, gerr := s.get(ctx, s.db, sess.Scope)
		if gerr != nil {
			return sess, Impact{}, wrap(gerr)
		}
		if status == "ended" {
			return fresh, Impact{}, nil
		}
		sess = fresh
	}
	return sess, Impact{}, contracts.New(contracts.ErrConflict, "sessions: the session keeps changing; try again")
}

// Invalidate applies an invalidation to the session of the verified scope: the
// preparations that lose their goal are invalidated through ivr first, then the
// state is saved. A session that does not exist has nothing to invalidate. The
// session must be the same authenticated context it was bound to. Nothing here
// commits anything or calls a tool.
func (s *Store) Invalidate(ctx context.Context, scope contracts.Scope, inv Invalidation, ivr Invalidator) (Impact, error) {
	if err := scope.Validate(); err != nil {
		return Impact{}, err
	}
	if err := inv.Validate(); err != nil {
		return Impact{}, err
	}
	sess, status, err := s.get(ctx, s.db, scope)
	if err != nil {
		if isNoRows(err) {
			return Impact{}, nil
		}
		return Impact{}, wrap(err)
	}
	if err := checkBound(sess, scope); err != nil {
		return Impact{}, err
	}
	if status == "ended" {
		return Impact{}, nil
	}
	_, imp, err := s.apply(ctx, sess, inv, ivr, applyOpts{})
	return imp, err
}

// Reconcile opens the session of the verified scope and applies the token it was
// presented with: a capability the session's goals rely on that the token no
// longer carries (or permitted rejects) discards those goals, and a role the last
// token had and this one lacks voids every preparation and drops every question,
// because which capability a role gated is not known here. permitted is the
// policy gateway's view when the caller has one; nil means membership in the
// token's capability scopes, the fail-closed default. A token that only widens
// what was allowed changes nothing but the permissions remembered.
//
// The dialogue engine calls this where it would call Open.
func (s *Store) Reconcile(ctx context.Context, scope contracts.Scope, permitted func(capabilityID string) bool, ivr Invalidator) (Session, Impact, error) {
	sess, err := s.Open(ctx, scope)
	if err != nil {
		return Session{}, Impact{}, err
	}
	base := sess.State.Perms
	if base == nil {
		base = permsOf(sess.Scope.Roles, sess.Scope.CapabilityScopes)
	}
	now := permsOf(scope.Roles, scope.CapabilityScopes)
	if permitted == nil {
		caps := now.Capabilities
		permitted = func(id string) bool { return slices.Contains(caps, id) }
	}
	rolesLost := false
	for _, r := range base.Roles {
		if !slices.Contains(now.Roles, r) {
			rolesLost = true
		}
	}
	inv := Invalidation{Kind: KindPermissionChange, Permitted: permitted, Everything: rolesLost}
	out, imp, err := s.apply(ctx, sess, inv, ivr, applyOpts{after: func(st *State) bool {
		if st.Perms != nil && slices.Equal(st.Perms.Roles, now.Roles) && slices.Equal(st.Perms.Capabilities, now.Capabilities) {
			return false
		}
		st.Perms = now
		return true
	}})
	return out, imp, err
}

// SweepResult is the outcome of a workspace-wide invalidation.
type SweepResult struct {
	// Sessions is how many sessions were changed.
	Sessions int
	// Impact is the sum of what happened in them.
	Impact Impact
}

// InvalidateWorkspace applies an invalidation to every active session of the
// scope's application, tenant and workspace that it can concern: the sessions
// whose goals reference the named records or capabilities (found through an
// index), the sessions of a principal, those with a lapsed deadline, or all of
// them. scope is the authenticated context of the caller (a declared service
// identity, for events); its session is not used, only its application, tenant and
// workspace. Each session is invalidated as Store.Invalidate does, with the
// session's own bound scope, so a preparation is voided under the identity it was
// prepared under. A failure in one session does not stop the others; the first is
// returned after the sweep. Other tenants' sessions are never touched.
func (s *Store) InvalidateWorkspace(ctx context.Context, scope contracts.Scope, inv Invalidation, ivr Invalidator) (SweepResult, error) {
	var out SweepResult
	if err := scope.Validate(); err != nil {
		return out, err
	}
	if err := inv.Validate(); err != nil {
		return out, err
	}
	if inv.Kind == KindPrincipalChange && inv.PrincipalID == "" {
		return out, contracts.New(contracts.ErrValidationFailed, "sessions: a principal change names the principal")
	}
	sw := s.candidates(scope, inv)
	var first error
	var after int64
	for {
		if err := ctx.Err(); err != nil {
			return out, contracts.Wrap(contracts.ErrTimeout, err, "sessions: the sweep was interrupted")
		}
		page, err := s.sweepRows(ctx, scope, sw, after)
		if err != nil {
			return out, wrap(err)
		}
		for _, c := range page {
			after = c.seq
			sess, _, err := c.decode()
			if err != nil {
				if first == nil {
					first = wrap(err)
				}
				continue
			}
			_, imp, err := s.apply(ctx, sess, inv, ivr, applyOpts{})
			if err != nil {
				if first == nil {
					first = err
				}
				continue
			}
			if imp.Changed() {
				out.Sessions++
				out.Impact.merge(imp)
			}
		}
		if len(page) < s.cfg.SweepBatch {
			return out, first
		}
	}
}

// sweep selects the sessions a workspace sweep concerns: a condition (constant
// text, values as arguments) and whether it can select every active session.
type sweep struct {
	cond string
	args []any
	all  bool
}

// sweepPage is the statement of one page of a workspace sweep: the active sessions
// of the scope after seq `after` that the sweep selects, in seq order, read whole.
//
// The status term is written two ways on purpose. A sweep that can select every
// active session walks the partial index sessions_active, which yields them in
// seq order (SQLite uses a partial index only when the query repeats its
// condition literally). Any other sweep names its sessions through a narrower
// index (the lookup rows, the principal, the deadline): there the unary + keeps
// SQLite from preferring a walk of every active session of the workspace.
func (s *Store) sweepPage(sw sweep, after int64) store.ScopedStmt {
	status := "+status = 'active'"
	if sw.all {
		status = "status = 'active'"
	}
	return store.ScopedStmt{
		SQL:       "SELECT " + sessionCols + " FROM sessions",
		Where:     status + " AND seq > ? AND (" + sw.cond + ")",
		WhereArgs: append([]any{after}, sw.args...),
		Tail:      "ORDER BY seq LIMIT ?",
		TailArgs:  []any{s.cfg.SweepBatch},
	}
}

// sweepRows reads one page of a workspace sweep. The rows are read whole, so no
// session is read a second time, and closed before any session of the page is
// changed. A session changed since it was read is caught by the version every
// write compares (apply reads it again on a conflict).
func (s *Store) sweepRows(ctx context.Context, scope contracts.Scope, sw sweep, after int64) ([]sessionRow, error) {
	rows, err := store.QueryScoped(ctx, s.db, scope, s.sweepPage(sw, after))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var page []sessionRow
	for rows.Next() {
		w, err := scanSessionRow(rows)
		if err != nil {
			return nil, err
		}
		page = append(page, w)
	}
	return page, rows.Err()
}

// candidates selects the sessions an invalidation can concern.
func (s *Store) candidates(scope contracts.Scope, inv Invalidation) sweep {
	var parts []string
	var args []any
	all := false
	sub := "id IN (SELECT session_id FROM session_refs WHERE application_id = ? AND tenant_id = ? AND workspace_id = ? AND kind = ? AND ref_type = ? AND ref_id = ?)"
	byRef := func(kind, typ, id string) {
		parts = append(parts, sub)
		args = append(args, scope.ApplicationID, scope.TenantID, scope.WorkspaceID, kind, typ, id)
	}
	switch inv.Kind {
	case KindTimeout:
		parts = append(parts, "next_deadline IS NOT NULL AND next_deadline <= ?")
		args = append(args, toMS(s.clk.Now()))
	case KindPrincipalChange:
		parts, all = append(parts, "1 = 1"), true
	default:
		for _, e := range inv.Entities {
			byRef(refEntity, e.EntityType, e.EntityID)
		}
		for _, c := range inv.Capabilities {
			if inv.Kind == KindApplicationEvent || inv.Kind == KindPermissionChange {
				byRef(refCapability, "", c)
			}
		}
		if inv.Everything || inv.Permitted != nil {
			parts, all = append(parts, "1 = 1"), true
		}
	}
	cond := "(" + strings.Join(parts, " OR ") + ")"
	if inv.PrincipalID != "" {
		cond += " AND principal_id = ?"
		args = append(args, inv.PrincipalID)
	}
	return sweep{cond: cond, args: args, all: all}
}
