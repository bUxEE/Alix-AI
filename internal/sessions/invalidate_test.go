package sessions

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

// choice is a candidate record the host offered.
func choice(id, label string) act.Choice {
	return act.Choice{ChoiceID: common.Identifier(id), Label: label, Value: act.CanonicalValue{Value: act.IdentifierValue{Value: common.Identifier(id)}}}
}

// service is the authenticated context of an event source: it names the
// application, tenant and workspace; the session it carries is not used.
func service(tenant string) contracts.Scope {
	sc := scopeCaps(tenant, "svc-events", "svc-1", "events.deliver")
	return sc
}

// preparedGoal is a goal with a preparation awaiting confirmation, a title the
// host resolved at the given version, and a quantity the user typed.
func preparedGoal(capID, prep, title, version string, now time.Time) *Goal {
	g := newGoal(capID, now, map[string]Slot{
		"title":    {Value: title, Origin: OriginEntityResolution},
		"quantity": {Value: "2", Origin: OriginUserTurn},
	})
	g.Entities = []ResolvedEntity{{Field: "title", EntityType: "title", EntityID: title, Version: version}}
	g.SetPrepared(prep, now.Add(10*time.Minute))
	return g
}

// A record the state relied on was revised, so the preparation built on the old
// version is voided, what was resolved from it is dropped, and what the user typed
// stays. Sessions of other workspaces and tenants are never touched.
//
// SEC-042
func TestInvalidationByRecordRevision(t *testing.T) {
	s, clk, db := newStore(t, Config{})
	ctx := context.Background()
	svc := service("tenant-a")

	scStale := scopeCaps("tenant-a", "user-1", "ses-1", "items.create")
	scCurrent := scopeCaps("tenant-a", "user-2", "ses-2", "items.create")
	scOther := scopeCaps("tenant-a", "user-3", "ses-3", "items.create")
	scWorkspace := inWorkspace(scStale, "ws-2")
	scTenant := scopeCaps("tenant-b", "user-1", "ses-1", "items.create")

	seed(t, s, scStale, func(st *State) {
		st.Goal = preparedGoal("items.create", "prep_1", "t1", "v3", clk.Now())
		st.Last = &DirectiveSummary{Act: "ask_confirm"}
		// A goal set aside on the same title, with the host's candidates for it.
		sus := newGoal("items.get", clk.Now(), nil)
		sus.Pending = &Clarification{Field: "title", Kind: ClarifyChoice, EntityType: "title", Version: "v3",
			Choices: []act.Choice{choice("t1", "One"), choice("t2", "Two")}}
		st.Suspended = []*Goal{sus}
		st.Results = []ResultSummary{
			{CapabilityID: "items.list", Kind: ResultRead, At: clk.Now(), Items: []ResultItem{
				{EntityType: "title", EntityID: "t1", Version: "v3", Label: "One"}, {EntityType: "title", EntityID: "t5", Version: "v1", Label: "Five"}}},
		}
	})
	seed(t, s, scCurrent, func(st *State) { st.Goal = preparedGoal("items.create", "prep_2", "t1", "v4", clk.Now()) })
	seed(t, s, scOther, func(st *State) { st.Goal = preparedGoal("items.create", "prep_3", "t9", "v1", clk.Now()) })
	seed(t, s, scWorkspace, func(st *State) { st.Goal = preparedGoal("items.create", "prep_4", "t1", "v3", clk.Now()) })
	seed(t, s, scTenant, func(st *State) { st.Goal = preparedGoal("items.create", "prep_5", "t1", "v3", clk.Now()) })
	before := map[string]int64{}
	for name, sc := range map[string]contracts.Scope{"current": scCurrent, "other": scOther, "workspace": scWorkspace, "tenant": scTenant} {
		before[name] = reopen(t, s, sc).Version
	}

	ivr := &scopedInvalidator{}
	inv := Invalidation{Kind: KindRecordRevision, Entities: []EntityRef{{EntityType: "title", EntityID: "t1", Version: "v4"}}}
	res, err := s.InvalidateWorkspace(ctx, svc, inv, ivr)
	if err != nil {
		t.Fatal(err)
	}
	if res.Sessions != 1 || res.Impact.EntitiesDropped != 1 || res.Impact.PendingCleared != 1 || res.Impact.ResultsDropped != 1 {
		t.Fatalf("sweep = %+v", res)
	}

	// The preparation was voided under the identity it was prepared under, for the right reason.
	if len(ivr.calls) != 1 {
		t.Fatalf("invalidator calls = %+v, want exactly the stale session's preparation", ivr.calls)
	}
	c := ivr.calls[0]
	if c.id != "prep_1" || c.reason != act.PreparedActionInvalidationConditionsItemDataVersionChanged ||
		c.scope.PrincipalID != "user-1" || c.scope.SessionID != "ses-1" || c.scope.TenantID != "tenant-a" || c.scope.WorkspaceID != "ws-1" {
		t.Fatalf("invalidator call = %+v", c)
	}

	// Its state: nothing built on v3 survives, what the user typed does.
	got := reopen(t, s, scStale).State
	g := got.Goal
	if g == nil || g.PreparedID != "" || g.Phase != capabilities.PhaseOpen || g.Confirmed || !g.PreparedExpiresAt.IsZero() {
		t.Fatalf("goal = %+v", g)
	}
	if len(g.Entities) != 0 {
		t.Fatalf("entities = %+v", g.Entities)
	}
	if _, kept := g.Slots["title"]; kept {
		t.Fatal("the title resolved from the old version must be resolved again")
	}
	if g.Slots["quantity"].Value != "2" {
		t.Fatalf("a value the user typed was lost: %+v", g.Slots)
	}
	if len(got.Suspended) != 1 || got.Suspended[0].Pending != nil {
		t.Fatalf("the candidates offered from the old version must go: %+v", got.Suspended)
	}
	if len(got.Results) != 1 || len(got.Results[0].Items) != 1 || got.Results[0].Items[0].EntityID != "t5" {
		t.Fatalf("results = %+v, want the list without the revised record", got.Results)
	}
	if got.Last != nil {
		t.Fatalf("the directive that asked for the old confirmation must go: %+v", got.Last)
	}

	// Everything that does not concern the revision is as it was.
	for name, sc := range map[string]contracts.Scope{"current": scCurrent, "other": scOther, "workspace": scWorkspace, "tenant": scTenant} {
		sess := reopen(t, s, sc)
		if sess.Version != before[name] || sess.State.Goal.PreparedID == "" {
			t.Errorf("%s session was touched: version %d (was %d), goal %+v", name, sess.Version, before[name], sess.State.Goal)
		}
	}

	// Applying it again changes nothing.
	again, err := s.InvalidateWorkspace(ctx, svc, inv, ivr)
	if err != nil || again.Sessions != 0 || len(ivr.calls) != 1 {
		t.Fatalf("second sweep = %+v %v, calls %d", again, err, len(ivr.calls))
	}

	// A host that does not say the new version means every version relied on is stale.
	unversioned := Invalidation{Kind: KindRecordRevision, Entities: []EntityRef{{EntityType: "title", EntityID: "t1"}}}
	res, err = s.InvalidateWorkspace(ctx, svc, unversioned, ivr)
	if err != nil || res.Sessions != 1 || !slices.Equal(ivr.ids(), []string{"prep_1", "prep_2"}) {
		t.Fatalf("unversioned sweep = %+v %v, calls %v", res, err, ivr.ids())
	}
	if reopen(t, s, scTenant).State.Goal.PreparedID == "" || reopen(t, s, scWorkspace).State.Goal.PreparedID == "" {
		t.Fatal("a revision in tenant-a ws-1 reached another tenant or workspace")
	}

	// The lookup rows follow the state: the revised record is no longer referenced.
	if n := countRows(t, db, `SELECT COUNT(*) FROM session_refs WHERE tenant_id = 'tenant-a' AND workspace_id = 'ws-1' AND session_id = 'ses-1' AND ref_id = 't1'`); n != 0 {
		t.Fatalf("%d stale lookup rows", n)
	}

	// A revision names its records.
	if _, err := s.InvalidateWorkspace(ctx, svc, Invalidation{Kind: KindRecordRevision}, ivr); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("a revision of nothing: %v", err)
	}
	if _, err := s.InvalidateWorkspace(ctx, svc, Invalidation{Kind: KindRecordRevision, Entities: []EntityRef{{EntityType: "title"}}}, ivr); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("a revision of a record with no ID: %v", err)
	}
}

// If the preparation cannot be invalidated, the state is left as it was so the
// event can be retried: nothing is left confirmable behind a state that forgot it.
func TestInvalidationFailsClosedWhenTheCoordinatorFails(t *testing.T) {
	s, clk, _ := newStore(t, Config{})
	ctx := context.Background()
	sc := scopeCaps("tenant-a", "user-1", "ses-1", "items.create")
	seed(t, s, sc, func(st *State) { st.Goal = preparedGoal("items.create", "prep_1", "t1", "v3", clk.Now()) })
	inv := Invalidation{Kind: KindApplicationEvent, Entities: []EntityRef{{EntityType: "title", EntityID: "t1"}}}

	down := &scopedInvalidator{err: contracts.New(contracts.ErrUnavailable, "actions down")}
	res, err := s.InvalidateWorkspace(ctx, service("tenant-a"), inv, down)
	if contracts.CodeOf(err) != contracts.ErrUnavailable || res.Sessions != 0 {
		t.Fatalf("sweep with the coordinator down = %+v %v", res, err)
	}
	if g := reopen(t, s, sc).State.Goal; g.PreparedID != "prep_1" || g.Phase != capabilities.PhasePrepared {
		t.Fatalf("the state changed although the preparation was not voided: %+v", g)
	}
	if _, err := s.Invalidate(ctx, sc, inv, nil); contracts.CodeOf(err) != contracts.ErrInternal {
		t.Fatalf("invalidating a prepared goal with no invalidator: %v", err)
	}

	// The coordinator back: the same event applies. A preparation that already left
	// the confirmable states is not an obstacle.
	done := &scopedInvalidator{err: contracts.New(contracts.ErrConflict, "already committed")}
	imp, err := s.Invalidate(ctx, sc, inv, done)
	if err != nil || len(imp.Voided) != 1 || done.calls[0].reason != act.PreparedActionInvalidationConditionsItemApplicationEvent {
		t.Fatalf("retry = %+v %v, calls %+v", imp, err, done.calls)
	}
	if g := reopen(t, s, sc).State.Goal; g.PreparedID != "" {
		t.Fatalf("goal = %+v", g)
	}
}

// An application event concerns what it names, or everything when it says so.
func TestApplicationEventSelectors(t *testing.T) {
	s, clk, _ := newStore(t, Config{})
	ctx := context.Background()
	scCap := scopeCaps("tenant-a", "user-1", "ses-1", "items.create", "items.get")
	scOther := scopeCaps("tenant-a", "user-2", "ses-2", "items.create", "items.get")
	for i, sc := range []contracts.Scope{scCap, scOther} {
		capID := []string{"items.create", "items.get"}[i]
		seed(t, s, sc, func(st *State) {
			st.Goal = preparedGoal(capID, "prep_"+capID, "t1", "v1", clk.Now())
			st.Results = []ResultSummary{{CapabilityID: capID, Kind: ResultRead, At: clk.Now()}}
		})
	}
	ivr := &scopedInvalidator{}
	svc := service("tenant-a")

	res, err := s.InvalidateWorkspace(ctx, svc, Invalidation{Kind: KindApplicationEvent, Capabilities: []string{"items.create"}}, ivr)
	if err != nil || res.Sessions != 1 || !slices.Equal(ivr.ids(), []string{"prep_items.create"}) || res.Impact.ResultsDropped != 1 {
		t.Fatalf("by capability = %+v %v %v", res, err, ivr.ids())
	}
	g := reopen(t, s, scCap).State.Goal
	if g == nil || g.PreparedID != "" || g.Slots["title"].Value != "t1" {
		t.Fatalf("the goal and what was collected stay; only the preparation goes: %+v", g)
	}
	if reopen(t, s, scOther).State.Goal.PreparedID == "" {
		t.Fatal("an event about items.create voided an items.get preparation")
	}

	res, err = s.InvalidateWorkspace(ctx, svc, Invalidation{Kind: KindApplicationEvent, Everything: true}, ivr)
	if err != nil || res.Sessions != 1 || !slices.Equal(ivr.ids(), []string{"prep_items.create", "prep_items.get"}) {
		t.Fatalf("everything = %+v %v %v", res, err, ivr.ids())
	}
	if _, err := s.InvalidateWorkspace(ctx, svc, Invalidation{Kind: KindApplicationEvent}, ivr); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("an event that names nothing must be refused, not read as everything: %v", err)
	}
	if _, err := s.InvalidateWorkspace(ctx, svc, Invalidation{Kind: "whatever", Everything: true}, ivr); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("unknown kind: %v", err)
	}
}

// A permission taken away voids what was prepared under it and discards the goals
// that need it; a token that only widens changes nothing.
//
// SEC-042
func TestPermissionChangeInvalidatesPending(t *testing.T) {
	s, clk, db := newStore(t, Config{})
	ctx := context.Background()
	full := scopeCaps("tenant-a", "user-1", "ses-1", "items.create", "items.get")

	seed(t, s, full, func(st *State) {
		st.Goal = preparedGoal("items.create", "prep_P", "t1", "v1", clk.Now())
		st.Last = &DirectiveSummary{Act: "ask_confirm"}
		sus := newGoal("items.get", clk.Now(), map[string]Slot{"item_id": {Value: "i1", Origin: OriginUserTurn}})
		sus.Pending = &Clarification{Field: "colour", Kind: ClarifyText}
		st.Suspended = []*Goal{sus}
		st.Results = []ResultSummary{{CapabilityID: "items.create", Kind: ResultPrepare, At: clk.Now()}, {CapabilityID: "items.get", Kind: ResultRead, At: clk.Now()}}
		st.Capsules = []Capsule{{CapabilityID: "items.create", Outcome: OutcomeSuccess, CompletedAt: clk.Now()}, {CapabilityID: "items.get", Outcome: OutcomeSuccess, CompletedAt: clk.Now()}}
		st.SetAllowedNext([]string{"items.create", "items.get"})
	})

	// The next token no longer carries items.create.
	narrow := scopeCaps("tenant-a", "user-1", "ses-1", "items.get")
	ivr := &scopedInvalidator{}
	sess, imp, err := s.Reconcile(ctx, narrow, nil, ivr)
	if err != nil {
		t.Fatal(err)
	}
	if len(ivr.calls) != 1 || ivr.calls[0].id != "prep_P" || ivr.calls[0].reason != act.PreparedActionInvalidationConditionsItemPermissionChanged {
		t.Fatalf("invalidator calls = %+v", ivr.calls)
	}
	// The preparation is voided under the session's bound identity, not the presented token's.
	if sc := ivr.calls[0].scope; sc.PrincipalID != "user-1" || sc.SessionID != "ses-1" || !slices.Equal(sc.CapabilityScopes, full.CapabilityScopes) {
		t.Fatalf("invalidated under %+v", sc)
	}
	if imp.GoalsEnded != 1 || len(imp.Voided) != 1 {
		t.Fatalf("impact = %+v", imp)
	}
	st := reopen(t, s, narrow).State
	if st.Goal != nil || st.Last != nil {
		t.Fatalf("the goal that needs the lost permission must go: %+v %+v", st.Goal, st.Last)
	}
	if len(st.Suspended) != 1 || st.Suspended[0].CapabilityID != "items.get" || st.Suspended[0].Pending == nil {
		t.Fatalf("a goal on a permitted capability is kept: %+v", st.Suspended)
	}
	if len(st.Results) != 1 || st.Results[0].CapabilityID != "items.get" || len(st.Capsules) != 1 || st.Capsules[0].CapabilityID != "items.get" {
		t.Fatalf("results %+v, capsules %+v", st.Results, st.Capsules)
	}
	if !slices.Equal(st.AllowedNext, []string{"items.get"}) {
		t.Fatalf("allowed next = %v", st.AllowedNext)
	}
	if st.Perms == nil || !slices.Equal(st.Perms.Capabilities, []string{"items.get"}) {
		t.Fatalf("the narrower permissions are remembered: %+v", st.Perms)
	}
	if sess.State.Goal != nil {
		t.Fatalf("returned session = %+v", sess.State)
	}

	// A token that only widens: nothing is voided or discarded, the permissions are remembered once.
	calls := len(ivr.calls)
	wide := scopeCaps("tenant-a", "user-1", "ses-1", "items.get", "items.create", "items.list")
	if _, imp, err = s.Reconcile(ctx, wide, nil, ivr); err != nil || imp.Changed() || len(ivr.calls) != calls {
		t.Fatalf("widening = %+v %v, calls %d", imp, err, len(ivr.calls))
	}
	v := reopen(t, s, wide).Version
	if _, _, err = s.Reconcile(ctx, wide, nil, ivr); err != nil || reopen(t, s, wide).Version != v {
		t.Fatalf("an unchanged token wrote the session again: %v", err)
	}
	if p := reopen(t, s, wide).State.Perms; !slices.Equal(p.Capabilities, []string{"items.create", "items.get", "items.list"}) {
		t.Fatalf("perms = %+v", p)
	}

	// A role the session had and the token lacks: which capability it gated is not
	// known here, so every preparation is voided and every question dropped, and
	// the goals stay for policy to recheck.
	scRole := scopeCaps("tenant-a", "user-2", "ses-2", "items.get")
	seed(t, s, scRole, func(st *State) {
		st.Goal = preparedGoal("items.get", "prep_R", "t1", "v1", clk.Now())
		sus := newGoal("items.get", clk.Now(), nil)
		sus.Pending = &Clarification{Field: "colour", Kind: ClarifyText}
		st.Suspended = []*Goal{sus}
	})
	visitor := scRole.Clone()
	visitor.Roles = []string{"visitor"}
	ivr = &scopedInvalidator{}
	if _, imp, err = s.Reconcile(ctx, visitor, nil, ivr); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ivr.ids(), []string{"prep_R"}) || imp.PendingCleared != 1 || imp.GoalsEnded != 0 {
		t.Fatalf("role lost: calls %v impact %+v", ivr.ids(), imp)
	}
	st = reopen(t, s, scRole).State
	if st.Goal == nil || st.Goal.PreparedID != "" || st.Suspended[0].Pending != nil {
		t.Fatalf("role lost: state = %+v / %+v", st.Goal, st.Suspended)
	}

	// The policy gateway's view overrides the token's list: a capability it rejects is gone.
	scPol := scopeCaps("tenant-a", "user-3", "ses-3", "items.get", "items.create")
	seed(t, s, scPol, func(st *State) { st.Goal = preparedGoal("items.get", "prep_G", "t1", "v1", clk.Now()) })
	ivr = &scopedInvalidator{}
	_, imp, err = s.Reconcile(ctx, scPol, func(id string) bool { return id != "items.get" }, ivr)
	if err != nil || imp.GoalsEnded != 1 || !slices.Equal(ivr.ids(), []string{"prep_G"}) || reopen(t, s, scPol).State.Goal != nil {
		t.Fatalf("policy rejection = %+v %v %v", imp, err, ivr.ids())
	}

	// The host says a capability was revoked for a principal: that principal's
	// sessions in the workspace lose their goals on it; another principal's do not.
	scMine := scopeCaps("tenant-a", "user-4", "ses-4", "items.create")
	scTheirs := scopeCaps("tenant-a", "user-5", "ses-5", "items.create")
	seed(t, s, scMine, func(st *State) { st.Goal = preparedGoal("items.create", "prep_M", "t1", "v1", clk.Now()) })
	seed(t, s, scTheirs, func(st *State) { st.Goal = preparedGoal("items.create", "prep_T", "t1", "v1", clk.Now()) })
	ivr = &scopedInvalidator{}
	res, err := s.InvalidateWorkspace(ctx, service("tenant-a"),
		Invalidation{Kind: KindPermissionChange, Capabilities: []string{"items.create"}, PrincipalID: "user-4"}, ivr)
	if err != nil || res.Sessions != 1 || !slices.Equal(ivr.ids(), []string{"prep_M"}) {
		t.Fatalf("revoked for user-4 = %+v %v %v", res, err, ivr.ids())
	}
	if reopen(t, s, scTheirs).State.Goal == nil {
		t.Fatal("another principal's goal was discarded")
	}

	// Fail closed: the coordinator down leaves the state as it was; a prepared
	// goal with nobody to invalidate it is an error.
	scDown := scopeCaps("tenant-a", "user-6", "ses-6", "items.create", "items.get")
	seed(t, s, scDown, func(st *State) { st.Goal = preparedGoal("items.create", "prep_D", "t1", "v1", clk.Now()) })
	narrowDown := scopeCaps("tenant-a", "user-6", "ses-6", "items.get")
	if _, _, err = s.Reconcile(ctx, narrowDown, nil, &scopedInvalidator{err: contracts.New(contracts.ErrUnavailable, "down")}); contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Fatalf("coordinator down: %v", err)
	}
	if _, _, err = s.Reconcile(ctx, narrowDown, nil, nil); contracts.CodeOf(err) != contracts.ErrInternal {
		t.Fatalf("no invalidator: %v", err)
	}
	if g := reopen(t, s, scDown).State.Goal; g == nil || g.PreparedID != "prep_D" {
		t.Fatalf("the state changed although the preparation was not voided: %+v", g)
	}

	// A different principal presenting the session is not a permission change but a refusal.
	intruder := scopeCaps("tenant-a", "user-9", "ses-6", "items.get")
	if _, _, err = s.Reconcile(ctx, intruder, nil, &scopedInvalidator{}); contracts.CodeOf(err) != contracts.ErrScopeViolation {
		t.Fatalf("another principal on the session: %v", err)
	}
	if g := reopen(t, s, scDown).State.Goal; g == nil || g.PreparedID != "prep_D" {
		t.Fatal("a refused presentation changed the session")
	}
	_ = db
}

// The user behind the session changed: everything pending is voided and the
// session ends, because a session never changes principal.
func TestPrincipalChangeEndsTheSession(t *testing.T) {
	s, clk, db := newStore(t, Config{})
	ctx := context.Background()
	scOld := scopeCaps("tenant-a", "user-1", "ses-1", "items.create")
	scKeep := scopeCaps("tenant-a", "user-2", "ses-2", "items.create")
	seed(t, s, scOld, func(st *State) {
		st.Goal = preparedGoal("items.create", "prep_O", "t1", "v1", clk.Now())
		st.Suspended = []*Goal{newGoal("items.get", clk.Now(), nil)}
		st.Results = []ResultSummary{{CapabilityID: "items.get", Kind: ResultRead, At: clk.Now()}}
	})
	seed(t, s, scKeep, func(st *State) { st.Goal = preparedGoal("items.create", "prep_K", "t1", "v1", clk.Now()) })

	if _, err := s.InvalidateWorkspace(ctx, service("tenant-a"), Invalidation{Kind: KindPrincipalChange}, &scopedInvalidator{}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("a principal change that names nobody: %v", err)
	}
	ivr := &scopedInvalidator{}
	res, err := s.InvalidateWorkspace(ctx, service("tenant-a"), Invalidation{Kind: KindPrincipalChange, PrincipalID: "user-1"}, ivr)
	if err != nil || res.Sessions != 1 || !res.Impact.Ended {
		t.Fatalf("sweep = %+v %v", res, err)
	}
	if len(ivr.calls) != 1 || ivr.calls[0].id != "prep_O" || ivr.calls[0].reason != act.PreparedActionInvalidationConditionsItemPrincipalChanged {
		t.Fatalf("calls = %+v", ivr.calls)
	}
	if _, err := s.Open(ctx, scOld); contracts.CodeOf(err) != contracts.ErrExpired {
		t.Fatalf("the session of the old user must be ended: %v", err)
	}
	if reopen(t, s, scKeep).State.Goal.PreparedID != "prep_K" {
		t.Fatal("another principal's session was touched")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM session_refs WHERE session_id = 'ses-1'`); n != 0 {
		t.Fatalf("%d lookup rows left for an ended session", n)
	}
	// Through the session-scoped call: the same effect on the verified scope's own session.
	imp, err := s.Invalidate(ctx, scKeep, Invalidation{Kind: KindPrincipalChange}, &scopedInvalidator{})
	if err != nil || !imp.Ended {
		t.Fatalf("Invalidate = %+v %v", imp, err)
	}
	if _, err := s.Open(ctx, scKeep); contracts.CodeOf(err) != contracts.ErrExpired {
		t.Fatalf("open after end: %v", err)
	}
}

// Deadlines pass. A question nobody answered, a preparation past its own expiry, an
// idle goal and a goal left suspended are each discarded, and only an idle goal's
// preparation is voided (the other has expired on its own).
//
// SEC-042
func TestTimeoutExpiresPending(t *testing.T) {
	cfg := Config{GoalTTL: 10 * time.Minute, PendingTTL: 2 * time.Minute, SuspendedTTL: 30 * time.Minute}
	s, clk, db := newStore(t, cfg)
	ctx := context.Background()
	svc := service("tenant-a")
	now := clk.Now()
	sweep := func(ivr *scopedInvalidator) SweepResult {
		t.Helper()
		res, err := s.InvalidateWorkspace(ctx, svc, Invalidation{Kind: KindTimeout}, ivr)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	deadline := func(session string) int64 {
		t.Helper()
		var d sql.NullInt64
		if err := db.QueryRowContext(ctx, `SELECT next_deadline FROM sessions WHERE tenant_id = 'tenant-a' AND id = ?`, session).Scan(&d); err != nil {
			t.Fatal(err)
		}
		if !d.Valid {
			t.Fatalf("session %s has no deadline", session)
		}
		return d.Int64
	}

	scAsk := scopeCaps("tenant-a", "user-1", "ses-ask", "items.create")
	scPrep := scopeCaps("tenant-a", "user-1", "ses-prep", "items.create")
	scIdle := scopeCaps("tenant-a", "user-1", "ses-idle", "items.create")
	scSusp := scopeCaps("tenant-a", "user-1", "ses-susp", "items.create")
	scLive := scopeCaps("tenant-a", "user-1", "ses-live", "items.create")

	seed(t, s, scAsk, func(st *State) {
		st.Goal = newGoal("items.create", now, map[string]Slot{"name": {Value: "w", Origin: OriginUserTurn}})
		st.Goal.ExpiresAt = now.Add(10 * time.Minute)
		st.Goal.Pending = &Clarification{Field: "quantity", Kind: ClarifyText}
		st.Last = &DirectiveSummary{Act: "ask_missing", Question: &Question{Field: "quantity", AnswerKind: "text"}}
	})
	seed(t, s, scPrep, func(st *State) {
		st.Goal = preparedGoal("items.create", "prep_Q", "t1", "v1", now)
		st.Goal.ExpiresAt = now.Add(10 * time.Minute)
		st.Goal.PreparedExpiresAt = now.Add(5 * time.Minute)
	})
	seed(t, s, scIdle, func(st *State) {
		st.Goal = preparedGoal("items.create", "prep_I", "t1", "v1", now)
		st.Goal.ExpiresAt = now.Add(10 * time.Minute)
		st.Goal.PreparedExpiresAt = time.Time{} // the writer did not record an expiry
		st.Results = []ResultSummary{{CapabilityID: "items.list", Kind: ResultRead, At: now}}
	})
	seed(t, s, scSusp, func(st *State) {
		st.Goal = newGoal("items.create", now, nil)
		if _, err := st.Suspend(now, SuspendedTopicSwitch, s.Config()); err != nil {
			t.Fatal(err)
		}
		st.Goal = newGoal("items.get", now, nil)
		st.Goal.ExpiresAt = now.Add(time.Hour)
	})
	seed(t, s, scLive, func(st *State) {
		st.Goal = newGoal("items.get", now, nil)
		st.Goal.ExpiresAt = now.Add(time.Hour)
	})

	// The earliest deadline of each session is stored for the sweep to find.
	for session, want := range map[string]time.Duration{"ses-ask": 2 * time.Minute, "ses-prep": 5 * time.Minute, "ses-idle": 10 * time.Minute, "ses-susp": 30 * time.Minute, "ses-live": time.Hour} {
		if got := deadline(session); got != toMS(now.Add(want)) {
			t.Errorf("%s: next deadline = %v, want %v", session, fromMS(got), now.Add(want))
		}
	}

	// Nothing has lapsed: nothing is read, nothing voided.
	ivr := &scopedInvalidator{}
	if res := sweep(ivr); res.Sessions != 0 || len(ivr.calls) != 0 {
		t.Fatalf("early sweep = %+v, calls %v", res, ivr.ids())
	}

	// Three minutes: the unanswered question lapses, the goal that asked it lives on.
	clk.Advance(3 * time.Minute)
	res := sweep(ivr)
	if res.Sessions != 1 || res.Impact.PendingCleared != 1 || len(ivr.calls) != 0 {
		t.Fatalf("sweep at 3m = %+v, calls %v", res, ivr.ids())
	}
	st := reopen(t, s, scAsk).State
	if st.Goal == nil || st.Goal.Pending != nil || st.Last != nil || st.Goal.Slots["name"].Value != "w" {
		t.Fatalf("after the question lapsed: %+v / %+v", st.Goal, st.Last)
	}
	if got := deadline("ses-ask"); got != toMS(now.Add(10*time.Minute)) {
		t.Fatalf("the deadline of the session that asked = %v", fromMS(got))
	}

	// Six minutes: a preparation past its own expiry is no longer offered. It has
	// expired in the coordinator already, so there is nothing to void.
	clk.Advance(3 * time.Minute)
	res = sweep(ivr)
	if res.Sessions != 1 || res.Impact.Lapsed != 1 || len(ivr.calls) != 0 {
		t.Fatalf("sweep at 6m = %+v, calls %v", res, ivr.ids())
	}
	if g := reopen(t, s, scPrep).State.Goal; g == nil || g.PreparedID != "" || g.Phase != capabilities.PhaseOpen {
		t.Fatalf("goal after the preparation lapsed = %+v", g)
	}

	// Eleven minutes: the idle goal goes and its preparation is voided; a result older than the goal TTL goes too.
	clk.Advance(5 * time.Minute)
	res = sweep(ivr)
	// Three sessions lapse (the goal that was asked-and-waiting, the one whose
	// preparation lapsed earlier, and the idle one); only the last held a preparation
	// the coordinator might still confirm.
	if res.Sessions != 3 || res.Impact.GoalsEnded != 3 || res.Impact.ResultsDropped != 1 || !slices.Equal(ivr.ids(), []string{"prep_I"}) ||
		ivr.calls[0].reason != act.PreparedActionInvalidationConditionsItemGoalChanged || ivr.calls[0].scope.SessionID != "ses-idle" {
		t.Fatalf("sweep at 11m = %+v, calls %+v", res, ivr.calls)
	}
	idle := reopen(t, s, scIdle).State
	if idle.Goal != nil || len(idle.Results) != 0 {
		t.Fatalf("idle session = %+v", idle)
	}

	// Past the suspension: the goal left waiting is gone, the active goal of that session is not.
	clk.Advance(20 * time.Minute)
	if res = sweep(ivr); res.Sessions != 1 {
		t.Fatalf("sweep at 31m = %+v", res)
	}
	susp := reopen(t, s, scSusp).State
	if len(susp.Suspended) != 0 || susp.Goal == nil || susp.Goal.CapabilityID != "items.get" {
		t.Fatalf("suspended session = %+v", susp)
	}
	if reopen(t, s, scLive).State.Goal == nil {
		t.Fatal("a live goal was expired")
	}
	// Idempotent.
	if res = sweep(ivr); res.Sessions != 0 {
		t.Fatalf("repeated sweep = %+v", res)
	}

	// The same through the session-scoped call.
	clk.Advance(time.Hour)
	imp, err := s.Invalidate(ctx, scLive, Invalidation{Kind: KindTimeout}, ivr)
	if err != nil || imp.GoalsEnded != 1 || reopen(t, s, scLive).State.Goal != nil {
		t.Fatalf("Invalidate timeout = %+v %v", imp, err)
	}
}

// A clarification takes its expiry from the config when Save first stores it, and
// without PendingTTL it lives as long as its goal.
func TestPendingExpiryIsStampedOnce(t *testing.T) {
	s, clk, _ := newStore(t, Config{PendingTTL: time.Minute})
	sc := scopeCaps("tenant-a", "user-1", "ses-1", "items.create")
	sess := seed(t, s, sc, func(st *State) {
		st.Goal = newGoal("items.create", clk.Now(), nil)
		st.Goal.Pending = &Clarification{Field: "q", Kind: ClarifyText}
	})
	first := sess.State.Goal.Pending.ExpiresAt
	if !first.Equal(clk.Now().Add(time.Minute)) || !sess.State.Goal.Pending.AskedAt.Equal(clk.Now()) {
		t.Fatalf("pending = %+v", sess.State.Goal.Pending)
	}
	clk.Advance(30 * time.Second)
	again, err := s.Save(context.Background(), sess, nil)
	if err != nil || !again.State.Goal.Pending.ExpiresAt.Equal(first) {
		t.Fatalf("saving the same question again moved its expiry: %v %v", again.State.Goal.Pending.ExpiresAt, err)
	}

	s2, clk2, _ := newStore(t, Config{})
	sess2 := seed(t, s2, sc, func(st *State) {
		st.Goal = newGoal("items.create", clk2.Now(), nil)
		st.Goal.Pending = &Clarification{Field: "q", Kind: ClarifyText}
	})
	if !sess2.State.Goal.Pending.ExpiresAt.IsZero() {
		t.Fatalf("with no PendingTTL the question has no expiry of its own: %+v", sess2.State.Goal.Pending)
	}
}

func TestInvalidationValidation(t *testing.T) {
	cases := map[string]struct {
		inv Invalidation
		ok  bool
	}{
		"revision":            {Invalidation{Kind: KindRecordRevision, Entities: []EntityRef{{"t", "1", "v"}}}, true},
		"revision of nothing": {Invalidation{Kind: KindRecordRevision}, false},
		"event everything":    {Invalidation{Kind: KindApplicationEvent, Everything: true}, true},
		"event nothing":       {Invalidation{Kind: KindApplicationEvent}, false},
		"permission caps":     {Invalidation{Kind: KindPermissionChange, Capabilities: []string{"a"}}, true},
		"permission nothing":  {Invalidation{Kind: KindPermissionChange}, false},
		"permission func":     {Invalidation{Kind: KindPermissionChange, Permitted: func(string) bool { return true }}, true},
		"principal":           {Invalidation{Kind: KindPrincipalChange}, true},
		"timeout":             {Invalidation{Kind: KindTimeout}, true},
		"unknown":             {Invalidation{Kind: "x"}, false},
		"empty capability":    {Invalidation{Kind: KindApplicationEvent, Capabilities: []string{" "}}, false},
		"too many records": {Invalidation{Kind: KindRecordRevision, Entities: func() []EntityRef {
			out := make([]EntityRef, MaxInvalidationEntities+1)
			for i := range out {
				out[i] = EntityRef{"t", "x", ""}
			}
			return out
		}()}, false},
	}
	for name, c := range cases {
		err := c.inv.Validate()
		if c.ok && err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if !c.ok && contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("%s: error = %v, want validation_failed", name, err)
		}
	}
}

// A sweep larger than one page still reaches every session.
func TestWorkspaceSweepPages(t *testing.T) {
	s, clk, _ := newStore(t, Config{SweepBatch: 2})
	ctx := context.Background()
	var scopes []contracts.Scope
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		sc := scopeCaps("tenant-a", "user-1", "ses-"+id, "items.create")
		scopes = append(scopes, sc)
		seed(t, s, sc, func(st *State) { st.Goal = preparedGoal("items.create", "prep_"+id, "t1", "v1", clk.Now()) })
	}
	ivr := &scopedInvalidator{}
	res, err := s.InvalidateWorkspace(ctx, service("tenant-a"), Invalidation{Kind: KindApplicationEvent, Everything: true}, ivr)
	if err != nil || res.Sessions != 5 || len(ivr.calls) != 5 {
		t.Fatalf("sweep = %+v %v, calls %d", res, err, len(ivr.calls))
	}
	for _, sc := range scopes {
		if reopen(t, s, sc).State.Goal.PreparedID != "" {
			t.Fatalf("%s kept its preparation", sc.SessionID)
		}
	}
}

// A writer that saves between the sweep's read and its write makes the sweep read
// again and apply to the newer state; neither writer's work is lost.
func TestInvalidationRetriesAfterAConcurrentSave(t *testing.T) {
	s, clk, _ := newStore(t, Config{})
	ctx := context.Background()
	sc := scopeCaps("tenant-a", "user-1", "ses-1", "items.create")
	stale := seed(t, s, sc, func(st *State) { st.Goal = preparedGoal("items.create", "prep_1", "t1", "v1", clk.Now()) })

	// Another turn saves first (a note in the state), moving the version.
	other := stale
	other.State.AllowedNext = []string{"items.get"}
	if _, err := s.Save(ctx, other, nil); err != nil {
		t.Fatal(err)
	}
	// The sweep holds the old read; its write conflicts, it reads again and applies.
	ivr := &scopedInvalidator{}
	out, imp, err := s.apply(ctx, stale, Invalidation{Kind: KindApplicationEvent, Everything: true}, ivr, applyOpts{})
	if err != nil || len(imp.Voided) != 1 {
		t.Fatalf("apply = %+v %v", imp, err)
	}
	got := reopen(t, s, sc)
	if got.Version != out.Version || got.State.Goal.PreparedID != "" || !slices.Equal(got.State.AllowedNext, []string{"items.get"}) {
		t.Fatalf("the other writer's change or the invalidation was lost: %+v", got.State)
	}
}

// A workspace sweep pages through the active sessions of a workspace in seq order. When it
// concerns every session, each page must walk the active sessions in seq order, not sort every
// session of the workspace in a temporary B-tree; when it names a principal, records or a
// deadline, the page keeps the narrower index that finds just those. No page scans the table.
func TestSweepPagePlans(t *testing.T) {
	s, clk, db := newStore(t, Config{})
	ctx := context.Background()
	for i := range 30 {
		sc := scopeCaps("tenant-a", "user-1", fmt.Sprintf("ses-%02d", i), "items.create")
		seed(t, s, sc, func(st *State) {
			st.Goal = preparedGoal("items.create", "prep_"+sc.SessionID, "t1", "v1", clk.Now())
		})
	}
	// want is the index the page reads sessions through ("" when either is fine).
	for name, tc := range map[string]struct {
		inv  Invalidation
		want string
	}{
		"everything":       {Invalidation{Kind: KindApplicationEvent, Everything: true}, "sessions_active"},
		"permission check": {Invalidation{Kind: KindPermissionChange, Permitted: func(string) bool { return false }}, "sessions_active"},
		"one principal":    {Invalidation{Kind: KindPrincipalChange, PrincipalID: "user-1"}, "sessions_principal"},
		"a record":         {Invalidation{Kind: KindRecordRevision, Entities: []EntityRef{{EntityType: "title", EntityID: "t1"}}}, "sessions_key"},
		"timeouts":         {Invalidation{Kind: KindTimeout}, "sessions_deadline"},
		"a capability too": {Invalidation{Kind: KindApplicationEvent, Entities: []EntityRef{{EntityType: "title", EntityID: "t1"}}, Capabilities: []string{"items.create"}}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			query, qargs, err := s.sweepPage(s.candidates(service("tenant-a"), tc.inv), 0).Build(service("tenant-a"))
			if err != nil {
				t.Fatal(err)
			}
			rows, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, qargs...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var plan []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plan = append(plan, detail)
			}
			joined := strings.Join(plan, "\n")
			t.Logf("plan:\n%s", joined)
			if strings.Contains(joined, "SCAN sessions") || (tc.want != "" && !strings.Contains(joined, "SEARCH sessions USING INDEX "+tc.want+" ")) {
				t.Errorf("the sweep page does not read sessions through %q:\n%s", tc.want, joined)
			}
			if tc.want == "sessions_active" && strings.Contains(joined, "TEMP B-TREE") {
				t.Errorf("the sweep page sorts the workspace:\n%s", joined)
			}
		})
	}
}
