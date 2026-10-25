package audit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The composition root's join: an audit sink for policy records satisfies
// policy.Sink without either package importing the other. (This test file is
// the one place the audit package's tests import internal/policy.)
var _ policy.Sink = (*RecordSink[policy.Record])(nil)

func policyLocator(r policy.Record) (Locator, error) {
	s := r.Subject
	return ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
}

func sampleDecision(subject policy.Subject) policy.Record {
	return policy.Record{
		Format:       policy.RecordFormat,
		DecisionID:   "pdec_01J9ZQ3K8V4W5X6Y7Z8A9B0C1D",
		DecidedAt:    t0,
		Phase:        policy.PhaseCommit,
		Outcome:      policy.OutcomeDeny,
		Code:         contracts.ErrConfirmationRequired,
		Subject:      subject,
		CapabilityID: "reservations.create",
		Package:      policy.PackageRef{ApplicationID: "app-a", Version: "1.0.0", Status: "active"},
		Policies:     []policy.PolicyRef{{PolicyID: "default-access", Digest: strings.Repeat("a", 64)}},
		Checks: []policy.Check{
			{Name: policy.CheckRequest, Passed: true, Reason: "ok"},
			{Name: policy.CheckToken, Passed: true, Reason: "capability_permitted"},
			{Name: policy.CheckEvidence, Passed: false, Code: contracts.ErrConfirmationRequired, Reason: "confirmation_missing"},
		},
		Risk:          policy.RiskRecord{Declared: contracts.RiskLevel(2), Effective: contracts.RiskLevel(2), EffectClass: "write", RaisedBy: []policy.RiskRaise{}},
		Requires:      policy.Requirements{Confirmation: true},
		VisibleFields: []string{"title"},
		WithheldFields: []policy.WithheldField{
			{Field: "member_note", Reason: policy.WithheldSensitive},
		},
		RulesApplied:  []policy.RuleApplied{},
		RulesDeferred: []string{},
	}
}

func TestPolicyRecordSink(t *testing.T) {
	h := newHarness(t)
	sink := NewRecordSink[policy.Record](h.log, EventPolicyDecision, policyLocator)

	t.Run("a decision is stored whole, in the chain of its subject", func(t *testing.T) {
		rec := sampleDecision(policy.Subject{
			ApplicationID: "app-a", TenantID: "tenant-1", WorkspaceID: "ws-1", PrincipalID: "usr_1", SessionID: "ses_1",
			Roles: []string{"member"}, CapabilityScopes: []string{"reservations.create"},
		})
		if err := sink.Emit(ctx, rec); err != nil {
			t.Fatalf("Emit: %v", err)
		}
		var typ, ws, principal, data string
		if err := h.db.QueryRowContext(ctx,
			`SELECT event_type, workspace_id, principal_id, data FROM audit_events WHERE application_id = 'app-a' AND tenant_id = 'tenant-1'`).
			Scan(&typ, &ws, &principal, &data); err != nil {
			t.Fatal(err)
		}
		if typ != string(EventPolicyDecision) || ws != "ws-1" || principal != "usr_1" {
			t.Errorf("stored %s %s %s", typ, ws, principal)
		}
		var back map[string]any
		if err := json.Unmarshal([]byte(data), &back); err != nil {
			t.Fatal(err)
		}
		// The token_scope CHECK NAME must not be mistaken for a secret: only a
		// credential-looking KEY or VALUE is redacted.
		for _, want := range []string{"pdec_01J9ZQ3K8V4W5X6Y7Z8A9B0C1D", "token_scope", "confirmation_missing", "member_note", "default-access"} {
			if !strings.Contains(data, want) {
				t.Errorf("the stored decision lost %q:\n%s", want, data)
			}
		}
		if strings.Contains(data, Redacted) {
			t.Errorf("a plain decision record was redacted:\n%s", data)
		}
		h.mustVerify(ChainKey{"app-a", "tenant-1"})
	})

	t.Run("a decision for a request without a usable scope still leaves a record", func(t *testing.T) {
		rec := sampleDecision(policy.Subject{TenantID: "tenant-1"}) // no application, workspace or principal
		rec.Code = contracts.ErrScopeMissing
		if err := sink.Emit(ctx, rec); err != nil {
			t.Fatalf("Emit: %v", err)
		}
		rep := h.mustVerify(ChainKey{trust.PlatformApplicationID, ""})
		if rep.Events != 1 {
			t.Errorf("platform chain = %+v", rep)
		}
	})

	t.Run("a decision that cannot be recorded is an error", func(t *testing.T) {
		failing := NewRecordSink[policy.Record](failingAppender{}, EventPolicyDecision, policyLocator)
		if err := failing.Emit(ctx, sampleDecision(policy.Subject{ApplicationID: "app-a", TenantID: "tenant-1", WorkspaceID: "ws-1"})); err == nil {
			t.Fatal("a sink whose append fails reported success")
		}
		if err := (*RecordSink[policy.Record])(nil).Emit(ctx, policy.Record{}); err == nil {
			t.Error("an unwired sink reported success")
		}
	})

	t.Run("a locator error that is not about scope is returned, not rerouted", func(t *testing.T) {
		boom := errors.New("boom")
		s := NewRecordSink[policy.Record](h.log, EventPolicyDecision, func(policy.Record) (Locator, error) { return Locator{}, boom })
		if err := s.Emit(ctx, sampleDecision(policy.Subject{})); !errors.Is(err, boom) {
			t.Errorf("err = %v", err)
		}
	})
}

// The gateway wired to the audit sink: every decision, allow or deny, is
// recorded, and a decision that cannot be recorded is a denial.
func TestPolicyGatewayWithAuditSink(t *testing.T) {
	h := newHarness(t)
	gw := policy.NewGateway(h.clk, policy.WithSink(NewRecordSink[policy.Record](h.log, EventPolicyDecision, policyLocator)))
	d := gw.Decide(ctx, policy.Request{}) // no scope, no package: a denial
	if d.Allowed {
		t.Fatal("an empty request was allowed")
	}
	var data string
	if err := h.db.QueryRowContext(ctx, `SELECT data FROM audit_events WHERE application_id = ?`, trust.PlatformApplicationID).Scan(&data); err != nil {
		t.Fatalf("the denial left no record: %v", err)
	}
	if !strings.Contains(data, d.Record.DecisionID) {
		t.Errorf("the stored record is not the decision's: %s", data)
	}

	gw2 := policy.NewGateway(h.clk, policy.WithSink(NewRecordSink[policy.Record](failingAppender{}, EventPolicyDecision, policyLocator)))
	if d := gw2.Decide(ctx, policy.Request{}); d.Allowed || d.Code != contracts.ErrUnavailable {
		t.Errorf("a decision that could not be recorded: allowed=%v code=%s", d.Allowed, d.Code)
	}
}

// failingAppender is an Appender whose store is down.
type failingAppender struct{}

func (failingAppender) AppendAt(context.Context, Locator, NewEvent) (Event, error) {
	return Event{}, contracts.New(contracts.ErrUnavailable, "store down")
}

func (failingAppender) AppendTx(context.Context, *store.Tx, Locator, NewEvent) (Event, error) {
	return Event{}, contracts.New(contracts.ErrUnavailable, "store down")
}
