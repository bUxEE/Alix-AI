package audit

import (
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// egressDecision stands for a payload type another package owns, such as the
// data-egress decision events of the external-provider milestone.
type egressDecision struct {
	DecisionID string `json:"decision_id"`
	Provider   string `json:"provider"`
	Outcome    string `json:"outcome"`
	APIKey     string `json:"api_key,omitempty"`
}

func TestRegisterEventType(t *testing.T) {
	reg := NewCoreRegistry()
	egress, err := RegisterIn[egressDecision](reg, TypeSpec{
		Name:        "dataegress.decision",
		Description: "a data-egress decision",
		Levels:      LevelTenant | LevelApplication,
		Required:    []string{"decision_id"},
	})
	if err != nil {
		t.Fatalf("RegisterIn: %v", err)
	}
	h := newHarness(t, func(o *Options) { o.Registry = reg })

	t.Run("another package's type is appended without editing this one", func(t *testing.T) {
		loc, err := ForScope(s1)
		if err != nil {
			t.Fatal(err)
		}
		e, err := egress.Append(ctx, h.log, loc, egressDecision{DecisionID: "edec_1", Provider: "provider-x", Outcome: "deny", APIKey: "sk-should-not-be-stored"})
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		if e.Type != "dataegress.decision" || !strings.Contains(string(e.Data), "edec_1") || strings.Contains(string(e.Data), "sk-should-not") {
			t.Errorf("event = %+v data %s", e, e.Data)
		}
		appLoc, _ := ForApplication("app-a", "")
		if _, err := egress.Append(ctx, h.log, appLoc, egressDecision{DecisionID: "edec_2"}); err != nil {
			t.Errorf("application level (Levels allows it): %v", err)
		}
		h.mustVerify(key1)
	})

	t.Run("a type missing a required key is refused", func(t *testing.T) {
		loc, _ := ForScope(s1)
		if _, err := h.log.AppendAt(ctx, loc, NewEvent{Type: egress.Name(), Data: map[string]any{"provider": "x"}}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("the same name cannot have two owners", func(t *testing.T) {
		if _, err := RegisterIn[egressDecision](reg, TypeSpec{Name: "dataegress.decision"}); contracts.CodeOf(err) != contracts.ErrConflict {
			t.Errorf("duplicate: %v", err)
		}
		if _, err := RegisterIn[struct{}](reg, TypeSpec{Name: EventPolicyDecision}); contracts.CodeOf(err) != contracts.ErrConflict {
			t.Errorf("a core name: %v", err)
		}
	})

	t.Run("a registry is separate from the default one", func(t *testing.T) {
		if _, ok := DefaultRegistry.Lookup("dataegress.decision"); ok {
			t.Error("a type registered in a private registry leaked into the default one")
		}
		if _, ok := NewCoreRegistry().Lookup("dataegress.decision"); ok {
			t.Error("core registries share state")
		}
	})

	t.Run("a type of another registry is unknown to this log", func(t *testing.T) {
		other := newHarness(t) // core registry only
		loc, _ := ForScope(s1)
		if _, err := egress.Append(ctx, other.log, loc, egressDecision{DecisionID: "x"}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("the zero handle appends nothing", func(t *testing.T) {
		var zero Type[egressDecision]
		loc, _ := ForScope(s1)
		if _, err := zero.Append(ctx, h.log, loc, egressDecision{}); err == nil {
			t.Error("a zero Type appended")
		}
	})
}

func TestEventTypeNames(t *testing.T) {
	good := []string{"a.b", "action.committing", "x_y.z_1", "a.b.c.d", "policy.decision"}
	bad := []string{"", "a", "A.b", "a.B", "a..b", ".a.b", "a.b.", "a b.c", "1a.b", "a.b.c.d.e", "a.b-c", strings.Repeat("a", 40) + "." + strings.Repeat("b", 40)}
	for _, n := range good {
		if !ValidTypeName(n) {
			t.Errorf("%q rejected", n)
		}
	}
	for _, n := range bad {
		if ValidTypeName(n) {
			t.Errorf("%q accepted", n)
		}
		if err := NewRegistry().Register(TypeSpec{Name: EventType(n)}); err == nil {
			t.Errorf("Register accepted %q", n)
		}
	}
	if err := NewRegistry().Register(TypeSpec{Name: "a.b", Levels: 8}); err == nil {
		t.Error("an unknown chain level was accepted")
	}
	if err := NewRegistry().Register(TypeSpec{Name: "a.b", Required: []string{""}}); err == nil {
		t.Error("an empty required key was accepted")
	}
}

func TestCoreEventTypes(t *testing.T) {
	r := NewCoreRegistry()
	for _, name := range []EventType{
		EventPolicyDecision,
		EventActionPrepared, EventActionConfirmed, EventActionCommitting, EventActionCommitted,
		EventActionRejected, EventActionExpired, EventActionInvalidated, EventActionUncertain,
		EventPackageInstalled, EventPackageQuarantined, EventPackageActivated, EventPackageDeactivated, EventPackageRemoved,
		EventAuditRunStarted, EventAuditRunCompleted, EventAuditRunWarningAccepted,
		EventSettingsChanged,
		EventDeletionTenant, EventDeletionSource, EventDeletionHistoryCleared,
		EventAuditExported, EventDeletionReceipts, EventAuditErased,
	} {
		spec, ok := r.Lookup(name)
		if !ok {
			t.Errorf("core type %s is not registered", name)
			continue
		}
		if spec.Description == "" {
			t.Errorf("core type %s has no description", name)
		}
	}
	if got := len(r.Types()); got != 24 {
		t.Errorf("%d core types, want 24", got)
	}
	// The default registry is the core types and is what New uses when given none.
	if _, ok := DefaultRegistry.Lookup(EventActionCommitting); !ok {
		t.Error("the default registry lacks the core types")
	}
}

// Tenant deletion is a tombstone on the tenant's own chain: the chain is not
// cut, and every earlier event is still there and still verifies.
func TestTenantDeletionTombstoneKeepsChain(t *testing.T) {
	h := newHarness(t)
	h.appendN(s1, 4)
	tomb, err := h.log.Append(ctx, s1, NewEvent{Type: EventDeletionTenant, Data: map[string]any{"reason": "operator request"}})
	if err != nil {
		t.Fatal(err)
	}
	if tomb.ChainSeq != 5 {
		t.Errorf("tombstone position %d", tomb.ChainSeq)
	}
	// Clearing conversation history is recorded and deletes no audit event.
	if _, err := h.log.Append(ctx, s1, NewEvent{Type: EventDeletionHistoryCleared, Data: map[string]any{"session_id": "ses_test"}}); err != nil {
		t.Fatal(err)
	}
	rep := h.mustVerify(key1)
	if rep.Events != 6 {
		t.Errorf("report = %+v", rep)
	}
	if n := h.count(`SELECT COUNT(*) FROM audit_events WHERE tenant_id = 'tenant-1'`); n != 6 {
		t.Errorf("%d events", n)
	}
	// The module offers no way to delete: the only writers are Append*, and
	// the table refuses UPDATE and DELETE.
	if _, err := h.db.ExecContext(ctx, `DELETE FROM audit_events`); err == nil {
		t.Error("audit events can be deleted")
	}
}

// The audit tables carry the three scope columns, TEXT and NOT NULL
// (master plan 10.2).
func TestAuditTablesAreScoped(t *testing.T) {
	reg := store.NewRegistry()
	if err := reg.Register("audit", migrations); err != nil {
		t.Fatal(err)
	}
	db := storetest.Open(t, reg)
	storetest.AssertScopedTables(t, db)
}
