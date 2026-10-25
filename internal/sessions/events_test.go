package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// eventRecorder is a Notifier that keeps every notice and can run a hook at the
// moment it is delivered or be told to refuse.
type eventRecorder struct {
	mu      sync.Mutex
	notices []Notice
	hook    func(Notice)
	err     error
}

func (r *eventRecorder) Notify(_ context.Context, n Notice) error {
	if r.hook != nil {
		r.hook(n)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.notices = append(r.notices, n)
	return nil
}

func (r *eventRecorder) sessionIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, n := range r.notices {
		out = append(out, n.Scope.SessionID)
	}
	slices.Sort(out)
	return out
}

// eventCoordinator is a coordinator that can do far more than the session layer
// is supposed to ask of it, and any such call fails the test. Only Invalidate is
// legitimate. Commit, Reject and Reconcile have the real coordinator's
// signatures (internal/actions), so a type assertion of the Invalidator to an
// interface built from them would succeed and be caught here; Prepare and Confirm
// take actions types this package's tests do not import, so theirs are loosely
// shaped and only count a direct call. What actually keeps an event from
// committing is the type (it is handed an Invalidator) and the import check in
// TestEventNeverCommits.
type eventCoordinator struct {
	t     *testing.T
	mu    sync.Mutex
	calls []invCall
	err   error
}

func (c *eventCoordinator) Invalidate(_ context.Context, sc contracts.Scope, id string, reason act.PreparedActionInvalidationConditionsItem) (contracts.PreparedAction, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, invCall{scope: sc, id: id, reason: reason})
	return contracts.PreparedAction{}, c.err
}

func (c *eventCoordinator) Commit(context.Context, contracts.Scope, string) (contracts.ActionReceipt, error) {
	c.t.Error("an event committed an action")
	return contracts.ActionReceipt{}, nil
}

func (c *eventCoordinator) Reject(context.Context, contracts.Scope, string) (contracts.PreparedAction, error) {
	c.t.Error("an event rejected an action")
	return contracts.PreparedAction{}, nil
}

func (c *eventCoordinator) Reconcile(context.Context, contracts.Scope, string) (contracts.ActionReceipt, error) {
	c.t.Error("an event reconciled an uncertain commit")
	return contracts.ActionReceipt{}, nil
}

func (c *eventCoordinator) Prepare(...any) { c.t.Error("an event prepared an action") }
func (c *eventCoordinator) Confirm(...any) { c.t.Error("an event confirmed an action") }
func (c *eventCoordinator) Call(...any)    { c.t.Error("an event called a tool") }

var (
	eventSchemaOnce sync.Once
	eventSchemaComp *jsonschema.Compiler
	eventSchemaErr  error
)

// eventSchemas compiles the conversation contract the notice events must satisfy:
// the JSON Schema, which also holds the cross-field rules the generated types
// cannot express.
func eventSchemas(t *testing.T) *jsonschema.Compiler {
	t.Helper()
	eventSchemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		for _, area := range []string{"common", "actions", "conversation"} {
			files, err := filepath.Glob(filepath.Join("..", "..", "schemas", area, "*.schema.json"))
			if err != nil || len(files) == 0 {
				eventSchemaErr = errors.Join(err, errors.New("no schemas in "+area))
				return
			}
			for _, f := range files {
				raw, err := os.ReadFile(f)
				if err != nil {
					eventSchemaErr = err
					return
				}
				doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
				if err != nil {
					eventSchemaErr = err
					return
				}
				if err := c.AddResource(doc.(map[string]any)["$id"].(string), doc); err != nil {
					eventSchemaErr = err
					return
				}
			}
		}
		eventSchemaComp = c
	})
	if eventSchemaErr != nil {
		t.Fatal(eventSchemaErr)
	}
	return eventSchemaComp
}

var eventTurnIDRE = regexp.MustCompile(`^trn_[0-9A-HJKMNP-TV-Z]{26}$`)

// checkNoticeEvents asserts a notice is a valid turn of its own and returns the
// error event's details: every event satisfies the TurnEvent schema, one turn_id
// throughout, seq from 0 without gaps, an error event then a failed done.
func checkNoticeEvents(t *testing.T, n Notice) map[string]any {
	t.Helper()
	schema, err := eventSchemas(t).Compile("urn:alixai:schema:conversation:turn-event:v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Events) != 2 {
		t.Fatalf("notice has %d events, want an error and a done", len(n.Events))
	}
	var details map[string]any
	var turn string
	for i, ev := range n.Events {
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(inst); err != nil {
			t.Fatalf("event %d is not a TurnEvent: %v: %s", i, err, raw)
		}
		var back conversation.TurnEvent
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("event %d does not decode strictly: %v", i, err)
		}
		switch e := ev.Value.(type) {
		case conversation.ErrorEvent:
			if i != 0 || e.Seq != 0 || e.Error.Code != common.ErrorCodeStale || e.Error.Retryable {
				t.Fatalf("event %d = %+v, want the first, a non-retryable stale error", i, e)
			}
			if !strings.Contains(e.Error.Message, "Nothing was committed") {
				t.Fatalf("notice text = %q", e.Error.Message)
			}
			turn, details = string(e.TurnID), e.Error.Details
		case conversation.DoneEvent:
			if i != 1 || e.Seq != 1 || e.FinishReason != conversation.DoneEventFinishReasonFailed || e.PreparationID != nil {
				t.Fatalf("event %d = %+v, want the second, a failed done with no preparation", i, e)
			}
			if string(e.TurnID) != turn || !eventTurnIDRE.MatchString(turn) {
				t.Fatalf("turn ids %q, %q", turn, e.TurnID)
			}
		default:
			t.Fatalf("event %d is a %T: a notice carries no prepared action, receipt, directive, block or progress", i, ev.Value)
		}
	}
	return details
}

// An application event that concerns a record a session relied on voids the
// preparation, drops what was derived from the record, keeps what the user typed
// and tells that session, and no other. The preparation is voided through the
// coordinator under the identity it was prepared under, for the right reason, and
// the session is told only after its new state is saved.
//
// SEC-042
func TestEventInvalidatesPreparedAction(t *testing.T) {
	s, clk, _ := newStore(t, Config{})
	ctx := context.Background()
	svc := service("tenant-a")

	scStale := scopeCaps("tenant-a", "user-1", "ses-1", "items.create")
	scOther := scopeCaps("tenant-a", "user-2", "ses-2", "items.create")
	scWorkspace := inWorkspace(scStale, "ws-2")
	scTenant := scopeCaps("tenant-b", "user-1", "ses-1", "items.create")
	seed(t, s, scStale, func(st *State) {
		st.Goal = preparedGoal("items.create", "prep_1", "t1", "v3", clk.Now())
		st.Last = &DirectiveSummary{Act: "ask_confirm"}
	})
	seed(t, s, scOther, func(st *State) { st.Goal = preparedGoal("items.create", "prep_2", "t9", "v1", clk.Now()) })
	seed(t, s, scWorkspace, func(st *State) { st.Goal = preparedGoal("items.create", "prep_3", "t1", "v3", clk.Now()) })
	seed(t, s, scTenant, func(st *State) { st.Goal = preparedGoal("items.create", "prep_4", "t1", "v3", clk.Now()) })
	before := map[string]int64{}
	for name, sc := range map[string]contracts.Scope{"other": scOther, "workspace": scWorkspace, "tenant": scTenant} {
		before[name] = reopen(t, s, sc).Version
	}

	ivr := &scopedInvalidator{}
	rec := &eventRecorder{}
	// The notice arrives after the state is saved: reading the session from inside
	// the delivery already shows it invalidated.
	rec.hook = func(n Notice) {
		g := reopen(t, s, n.Scope).State.Goal
		if g == nil || g.PreparedID != "" {
			t.Errorf("notified before the new state was saved: %+v", g)
		}
	}
	ev := ApplicationEvent{ID: "evt-1", Type: "title.merged", Kind: KindApplicationEvent,
		Entities: []EntityRef{{EntityType: "title", EntityID: "t1"}}}
	res, err := s.ApplyEvent(ctx, svc, ev, ivr, rec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Sessions != 1 || res.Impact.EntitiesDropped != 1 || len(res.Impact.Voided) != 1 || res.Notified != 1 || res.NotifyFailed != 0 {
		t.Fatalf("result = %+v", res)
	}

	if len(ivr.calls) != 1 {
		t.Fatalf("invalidator calls = %+v, want only the concerned session's preparation", ivr.calls)
	}
	c := ivr.calls[0]
	if c.id != "prep_1" || c.reason != act.PreparedActionInvalidationConditionsItemApplicationEvent ||
		c.scope.PrincipalID != "user-1" || c.scope.SessionID != "ses-1" || c.scope.TenantID != "tenant-a" || c.scope.WorkspaceID != "ws-1" {
		t.Fatalf("invalidator call = %+v", c)
	}

	got := reopen(t, s, scStale).State
	g := got.Goal
	if g == nil || g.PreparedID != "" || g.Phase != capabilities.PhaseOpen || g.Confirmed || !g.PreparedExpiresAt.IsZero() {
		t.Fatalf("goal = %+v", g)
	}
	if _, kept := g.Slots["title"]; kept || len(g.Entities) != 0 {
		t.Fatalf("what the host resolved must be resolved again: %+v", g)
	}
	if g.Slots["quantity"].Value != "2" {
		t.Fatalf("a value the user typed was lost: %+v", g.Slots)
	}
	if got.Last != nil {
		t.Fatalf("the directive that asked for the old confirmation must go: %+v", got.Last)
	}

	// One notice, for the session that changed, under the scope it is bound to.
	if !slices.Equal(rec.sessionIDs(), []string{"ses-1"}) {
		t.Fatalf("notified %v", rec.sessionIDs())
	}
	n := rec.notices[0]
	if n.Scope.PrincipalID != "user-1" || n.Scope.TenantID != "tenant-a" || n.Scope.WorkspaceID != "ws-1" ||
		n.EventID != "evt-1" || n.EventType != "title.merged" || n.Kind != KindApplicationEvent || len(n.Impact.Voided) != 1 {
		t.Fatalf("notice = %+v", n)
	}
	details := checkNoticeEvents(t, n)
	if details["event_id"] != "evt-1" || details["kind"] != "application_event" ||
		!slices.Equal(details["preparation_ids"].([]any), []any{"prep_1"}) || details["entities_dropped"] != 1 {
		t.Fatalf("details = %+v", details)
	}

	// Sessions the event does not concern, in this workspace, another workspace
	// and another tenant, are untouched.
	for name, sc := range map[string]contracts.Scope{"other": scOther, "workspace": scWorkspace, "tenant": scTenant} {
		sess := reopen(t, s, sc)
		if sess.Version != before[name] || sess.State.Goal.PreparedID == "" {
			t.Errorf("%s session was touched: version %d (was %d), goal %+v", name, sess.Version, before[name], sess.State.Goal)
		}
	}

	// Delivered again, it changes nothing and tells nobody: there is nothing left
	// to invalidate, and a preparation made since is not touched by an old record.
	again, err := s.ApplyEvent(ctx, svc, ev, ivr, rec)
	if err != nil || again.Sessions != 0 || again.Notified != 0 || len(ivr.calls) != 1 || len(rec.notices) != 1 {
		t.Fatalf("second delivery = %+v %v, calls %d, notices %d", again, err, len(ivr.calls), len(rec.notices))
	}

	// The selectors an event can use: a capability, and everything.
	capEvent := ApplicationEvent{ID: "evt-2", Type: "items.disabled", Kind: KindApplicationEvent, Capabilities: []string{"items.create"}}
	res, err = s.ApplyEvent(ctx, svc, capEvent, ivr, rec)
	if err != nil || res.Sessions != 1 || res.Notified != 1 || !slices.Equal(ivr.ids(), []string{"prep_1", "prep_2"}) {
		t.Fatalf("by capability = %+v %v %v", res, err, ivr.ids())
	}
	if reopen(t, s, scWorkspace).State.Goal.PreparedID == "" || reopen(t, s, scTenant).State.Goal.PreparedID == "" {
		t.Fatal("an event in tenant-a ws-1 reached another workspace or tenant")
	}
}

// An event only invalidates. Whatever the kind, the coordinator is asked to void
// preparations and nothing else; a confirmed preparation is voided, not
// committed; the session's goal never reaches success; and the notice is no
// confirmation, receipt or tool call. The code that does this imports nothing
// that could commit.
//
// SEC-045
func TestEventNeverCommits(t *testing.T) {
	t.Run("runtime", func(t *testing.T) {
		kinds := map[string]ApplicationEvent{
			"event":      {ID: "e1", Type: "invoice.paid", Kind: KindApplicationEvent, Everything: true},
			"revision":   {ID: "e2", Type: "invoice.revised", Kind: KindRecordRevision, Entities: []EntityRef{{EntityType: "title", EntityID: "t1", Version: "v9"}}},
			"permission": {ID: "e3", Type: "role.revoked", Kind: KindPermissionChange, Everything: true},
			"principal":  {ID: "e4", Type: "user.removed", Kind: KindPrincipalChange, PrincipalID: "user-1"},
		}
		for name, ev := range kinds {
			t.Run(name, func(t *testing.T) {
				s, clk, _ := newStore(t, Config{})
				sc := scopeCaps("tenant-a", "user-1", "ses-1", "items.create")
				seed(t, s, sc, func(st *State) {
					g := preparedGoal("items.create", "prep_1", "t1", "v3", clk.Now())
					g.Confirmed = true // the user said yes; the commit has not happened
					st.Goal = g
					st.Last = &DirectiveSummary{Act: "ask_confirm"}
				})
				coord := &eventCoordinator{t: t}
				rec := &eventRecorder{}
				res, err := s.ApplyEvent(context.Background(), service("tenant-a"), ev, coord, rec)
				if err != nil || res.Sessions != 1 {
					t.Fatalf("result = %+v, %v", res, err)
				}

				// The only thing asked of the coordinator was to void the preparation.
				if len(coord.calls) != 1 || coord.calls[0].id != "prep_1" {
					t.Fatalf("coordinator calls = %+v", coord.calls)
				}
				switch r := coord.calls[0].reason; r {
				case act.PreparedActionInvalidationConditionsItemApplicationEvent, act.PreparedActionInvalidationConditionsItemDataVersionChanged,
					act.PreparedActionInvalidationConditionsItemPermissionChanged, act.PreparedActionInvalidationConditionsItemPrincipalChanged:
				default:
					t.Fatalf("reason = %s", r)
				}

				// The confirmed preparation no longer hangs off the session, and nothing
				// in the session says anything was done.
				sess, status, err := s.get(context.Background(), s.db, sc)
				if err != nil {
					t.Fatal(err)
				}
				if g := sess.State.Goal; g != nil && (g.PreparedID != "" || g.Confirmed || g.Phase == capabilities.PhaseSuccess || g.Phase == capabilities.PhaseFailure) {
					t.Fatalf("goal after the event = %+v", g)
				}
				if name == "principal" && status != "ended" {
					t.Fatalf("status = %s, want ended", status)
				}
				for _, c := range sess.State.Capsules {
					t.Fatalf("an event completed a goal into a capsule: %+v", c)
				}

				// The notice is a typed stale error and a done: no receipt, no prepared
				// action to confirm, no directive, no progress of a commit or a tool call.
				if len(rec.notices) != 1 {
					t.Fatalf("notices = %d", len(rec.notices))
				}
				checkNoticeEvents(t, rec.notices[0])
			})
		}
	})

	// The type system keeps it so: the one method an event can reach is
	// Invalidate, and the implementation imports no package that commits, calls a
	// tool, decides policy, runs a turn or touches the network.
	t.Run("imports", func(t *testing.T) {
		const mod = "github.com/buxee/alix-ai/"
		forbidden := []string{
			mod + "internal/actions", mod + "internal/adapters", mod + "internal/policy", mod + "internal/dialogue",
			mod + "internal/rag", mod + "internal/jobs", mod + "internal/stream", mod + "adapters/",
			"net/http", "net", "os/exec", "plugin",
		}
		f, err := parser.ParseFile(token.NewFileSet(), "events.go", nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		if len(f.Imports) == 0 {
			t.Fatal("events.go imports nothing: the check is not reading it")
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			for _, bad := range forbidden {
				if p == bad || strings.HasPrefix(p, bad) && strings.HasSuffix(bad, "/") || strings.HasPrefix(p, bad+"/") {
					t.Errorf("events.go imports %s: an event must have no way to commit or call a tool", p)
				}
			}
		}
		// The API takes the narrow Invalidator, which has one method.
		var _ Invalidator = (*eventCoordinator)(nil)
	})
}

// A malformed event is refused before anything is read, never taken for
// "everything" or "nothing", and an event a host cannot report (a timeout) is not
// one.
func TestEventValidationFailsClosed(t *testing.T) {
	long := make([]string, MaxEventCapabilities+1)
	for i := range long {
		long[i] = "c" + strconv.Itoa(i)
	}
	ent := []EntityRef{{EntityType: "title", EntityID: "t1", Version: "v1"}}
	cases := map[string]struct {
		ev ApplicationEvent
		ok bool
	}{
		"event by record":       {ApplicationEvent{ID: "e", Type: "a.b", Kind: KindApplicationEvent, Entities: ent}, true},
		"event by capability":   {ApplicationEvent{ID: "e", Type: "a", Kind: KindApplicationEvent, Capabilities: []string{"x"}}, true},
		"event everything":      {ApplicationEvent{ID: "e", Type: "a", Kind: KindApplicationEvent, Everything: true, PrincipalID: "u1"}, true},
		"revision":              {ApplicationEvent{ID: "e", Type: "a", Kind: KindRecordRevision, Entities: ent}, true},
		"permission by caps":    {ApplicationEvent{ID: "e", Type: "a", Kind: KindPermissionChange, Capabilities: []string{"x"}}, true},
		"principal":             {ApplicationEvent{ID: "e", Type: "a", Kind: KindPrincipalChange, PrincipalID: "u1"}, true},
		"event says nothing":    {ApplicationEvent{ID: "e", Type: "a", Kind: KindApplicationEvent}, false},
		"no id":                 {ApplicationEvent{Type: "a", Kind: KindApplicationEvent, Everything: true}, false},
		"id with a space":       {ApplicationEvent{ID: "e 1", Type: "a", Kind: KindApplicationEvent, Everything: true}, false},
		"id wildcard":           {ApplicationEvent{ID: "*", Type: "a", Kind: KindApplicationEvent, Everything: true}, false},
		"id too long":           {ApplicationEvent{ID: strings.Repeat("x", MaxEventIDLen+1), Type: "a", Kind: KindApplicationEvent, Everything: true}, false},
		"no type":               {ApplicationEvent{ID: "e", Kind: KindApplicationEvent, Everything: true}, false},
		"type upper case":       {ApplicationEvent{ID: "e", Type: "Invoice", Kind: KindApplicationEvent, Everything: true}, false},
		"type with prose":       {ApplicationEvent{ID: "e", Type: "ignore previous", Kind: KindApplicationEvent, Everything: true}, false},
		"type too long":         {ApplicationEvent{ID: "e", Type: strings.Repeat("a", MaxEventTypeLen+1), Kind: KindApplicationEvent, Everything: true}, false},
		"timeout":               {ApplicationEvent{ID: "e", Type: "a", Kind: KindTimeout}, false},
		"unknown kind":          {ApplicationEvent{ID: "e", Type: "a", Kind: "commit", Everything: true}, false},
		"no kind":               {ApplicationEvent{ID: "e", Type: "a", Everything: true}, false},
		"revision of nothing":   {ApplicationEvent{ID: "e", Type: "a", Kind: KindRecordRevision}, false},
		"revision everything":   {ApplicationEvent{ID: "e", Type: "a", Kind: KindRecordRevision, Entities: ent, Everything: true}, false},
		"revision capabilities": {ApplicationEvent{ID: "e", Type: "a", Kind: KindRecordRevision, Entities: ent, Capabilities: []string{"x"}}, false},
		"permission of records": {ApplicationEvent{ID: "e", Type: "a", Kind: KindPermissionChange, Entities: ent, Everything: true}, false},
		"permission nothing":    {ApplicationEvent{ID: "e", Type: "a", Kind: KindPermissionChange}, false},
		"principal unnamed":     {ApplicationEvent{ID: "e", Type: "a", Kind: KindPrincipalChange}, false},
		"principal and records": {ApplicationEvent{ID: "e", Type: "a", Kind: KindPrincipalChange, PrincipalID: "u", Entities: ent}, false},
		"record without an id":  {ApplicationEvent{ID: "e", Type: "a", Kind: KindApplicationEvent, Entities: []EntityRef{{EntityType: "title"}}}, false},
		"empty capability":      {ApplicationEvent{ID: "e", Type: "a", Kind: KindApplicationEvent, Capabilities: []string{" "}}, false},
		"too many capabilities": {ApplicationEvent{ID: "e", Type: "a", Kind: KindApplicationEvent, Capabilities: long}, false},
		"malformed principal":   {ApplicationEvent{ID: "e", Type: "a", Kind: KindApplicationEvent, Everything: true, PrincipalID: "a b"}, false},
	}
	for name, c := range cases {
		err := c.ev.Validate()
		if c.ok && err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if !c.ok && contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("%s: error = %v, want validation_failed", name, err)
		}
	}
}

// The service identity decides whether an event may be applied at all: a scope
// that is invalid or does not carry the event capability, and a missing
// coordinator, are refused with every session left as it was.
func TestEventRequiresAnEventSource(t *testing.T) {
	s, clk, _ := newStore(t, Config{})
	ctx := context.Background()
	sc := scopeCaps("tenant-a", "user-1", "ses-1", "items.create")
	seed(t, s, sc, func(st *State) { st.Goal = preparedGoal("items.create", "prep_1", "t1", "v1", clk.Now()) })
	version := reopen(t, s, sc).Version
	ev := ApplicationEvent{ID: "evt-1", Type: "any", Kind: KindApplicationEvent, Everything: true}
	ivr := &scopedInvalidator{}
	rec := &eventRecorder{}

	intruder := scopeCaps("tenant-a", "user-9", "ses-9", "items.create")
	_, err := s.ApplyEvent(ctx, intruder, ev, ivr, rec)
	wantCode(t, err, contracts.ErrPermissionDenied)
	_, err = s.ApplyEvent(ctx, contracts.Scope{}, ev, ivr, rec)
	wantCode(t, err, contracts.ErrScopeMissing)
	wild := service("tenant-a")
	wild.TenantID = "*"
	_, err = s.ApplyEvent(ctx, wild, ev, ivr, rec)
	wantCode(t, err, contracts.ErrScopeViolation)
	_, err = s.ApplyEvent(ctx, service("tenant-a"), ev, nil, rec)
	wantCode(t, err, contracts.ErrInternal)

	if got := reopen(t, s, sc); got.Version != version || got.State.Goal.PreparedID == "" || len(ivr.calls) != 0 || len(rec.notices) != 0 {
		t.Fatalf("a refused event touched something: version %d (was %d), goal %+v, calls %d, notices %d",
			got.Version, version, got.State.Goal, len(ivr.calls), len(rec.notices))
	}
}

// If the preparation cannot be invalidated the session is left as it was and is
// not told; delivered again once the coordinator works, the event does its job.
// A session that cannot be told is still invalidated: the notice is best effort
// and the state is what counts.
func TestEventRedeliveryAndNoticeFailure(t *testing.T) {
	s, clk, _ := newStore(t, Config{})
	ctx := context.Background()
	svc := service("tenant-a")
	sc1 := scopeCaps("tenant-a", "user-1", "ses-1", "items.create")
	sc2 := scopeCaps("tenant-a", "user-2", "ses-2", "items.create")
	for i, sc := range []contracts.Scope{sc1, sc2} {
		seed(t, s, sc, func(st *State) {
			st.Goal = preparedGoal("items.create", "prep_"+strconv.Itoa(i+1), "t1", "v1", clk.Now())
		})
	}
	ev := ApplicationEvent{ID: "evt-1", Type: "any", Kind: KindApplicationEvent, Everything: true}

	coord := &eventCoordinator{t: t, err: contracts.New(contracts.ErrUnavailable, "coordinator down")}
	rec := &eventRecorder{}
	res, err := s.ApplyEvent(ctx, svc, ev, coord, rec)
	wantCode(t, err, contracts.ErrUnavailable)
	if res.Sessions != 0 || len(rec.notices) != 0 {
		t.Fatalf("a failed invalidation reported work: %+v, notices %d", res, len(rec.notices))
	}
	for _, sc := range []contracts.Scope{sc1, sc2} {
		if reopen(t, s, sc).State.Goal.PreparedID == "" {
			t.Fatalf("%s forgot a preparation the coordinator could not void", sc.SessionID)
		}
	}

	coord.err = nil
	rec.err = errors.New("stream is down")
	res, err = s.ApplyEvent(ctx, svc, ev, coord, rec)
	if err != nil || res.Sessions != 2 || res.Notified != 0 || res.NotifyFailed != 2 {
		t.Fatalf("redelivery = %+v, %v: a notice that cannot be delivered must not fail the invalidation", res, err)
	}
	for _, sc := range []contracts.Scope{sc1, sc2} {
		if reopen(t, s, sc).State.Goal.PreparedID != "" {
			t.Fatalf("%s kept its preparation", sc.SessionID)
		}
	}

	// No notifier at all is fine too.
	seed(t, s, sc1, func(st *State) { st.Goal = preparedGoal("items.create", "prep_9", "t1", "v1", clk.Now()) })
	if res, err := s.ApplyEvent(ctx, svc, ev, coord, nil); err != nil || res.Sessions != 1 {
		t.Fatalf("without a notifier = %+v, %v", res, err)
	}
}

// When the user behind a session changes, the session ends, and the notice says
// nothing about what it held: whoever is on the other end is not that user.
func TestEventPrincipalChangeNoticeRevealsNothing(t *testing.T) {
	s, clk, _ := newStore(t, Config{})
	ctx := context.Background()
	scGone := scopeCaps("tenant-a", "user-1", "ses-1", "items.create")
	scStays := scopeCaps("tenant-a", "user-2", "ses-2", "items.create")
	for i, sc := range []contracts.Scope{scGone, scStays} {
		seed(t, s, sc, func(st *State) {
			st.Goal = preparedGoal("items.create", "prep_"+strconv.Itoa(i+1), "t1", "v1", clk.Now())
		})
	}
	rec := &eventRecorder{}
	ev := ApplicationEvent{ID: "evt-1", Type: "user.removed", Kind: KindPrincipalChange, PrincipalID: "user-1"}
	res, err := s.ApplyEvent(ctx, service("tenant-a"), ev, &eventCoordinator{t: t}, rec)
	if err != nil || res.Sessions != 1 || !res.Impact.Ended || !slices.Equal(rec.sessionIDs(), []string{"ses-1"}) {
		t.Fatalf("result = %+v %v, notified %v", res, err, rec.sessionIDs())
	}
	details := checkNoticeEvents(t, rec.notices[0])
	if details["ended"] != true || details["preparation_ids"] != nil || details["entities_dropped"] != nil {
		t.Fatalf("details of an ended session = %+v", details)
	}
	if reopen(t, s, scStays).State.Goal.PreparedID == "" {
		t.Fatal("another user's session was ended")
	}
}

// A sweep larger than a page tells every session it changed, once.
func TestEventSweepPagesAndNotifiesOnce(t *testing.T) {
	s, clk, _ := newStore(t, Config{SweepBatch: 2})
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		sc := scopeCaps("tenant-a", "user-1", "ses-"+id, "items.create")
		seed(t, s, sc, func(st *State) { st.Goal = preparedGoal("items.create", "prep_"+id, "t1", "v1", clk.Now()) })
	}
	rec := &eventRecorder{}
	ev := ApplicationEvent{ID: "evt-1", Type: "any", Kind: KindApplicationEvent, Everything: true}
	res, err := s.ApplyEvent(context.Background(), service("tenant-a"), ev, &eventCoordinator{t: t}, rec)
	if err != nil || res.Sessions != 5 || res.Notified != 5 {
		t.Fatalf("result = %+v %v", res, err)
	}
	want := []string{"ses-a", "ses-b", "ses-c", "ses-d", "ses-e"}
	if got := rec.sessionIDs(); !slices.Equal(got, want) {
		t.Fatalf("notified %v, want %v", got, want)
	}
	turns := map[string]bool{}
	for _, n := range rec.notices {
		turns[string(n.Events[0].Value.(conversation.ErrorEvent).TurnID)] = true
	}
	if len(turns) != 5 {
		t.Fatalf("every session's notice is a turn of its own, got %d turn ids", len(turns))
	}
}
