package dataegress

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func TestKillSwitchBlocksEverything(t *testing.T) {
	f := newFixture(t)
	f.enableAll()
	src := f.newSource(providers.EgressModeAllowExcerpt, providers.DataClassificationInternal, nil)
	candidate := func() Candidate { return f.candidate(excerptPart(src, "body", "T")) }
	f.mustAllow(candidate()) // the control: everything is enabled and the request goes

	if err := f.svc.EngageKillSwitch(testCtx, f.op, ""); err != nil {
		t.Fatal(err)
	}
	// Every evaluation is denied first, with nothing else consulted, whatever the policies say.
	res, ce := f.mustDeny(candidate(), providers.EgressDecisionReasonKillSwitch, providers.EgressDecisionCheckNameKillSwitch)
	if len(res.Decision.Checks) != 1 || len(res.Decision.Levels) != 0 || res.Decision.Checks[0].Reason != "global_external_disable_on" {
		t.Errorf("the kill switch stops evaluation at once: %+v %+v", res.Decision.Checks, res.Decision.Levels)
	}
	if ce.Details["check"] != "kill_switch" {
		t.Errorf("details = %v", ce.Details)
	}
	// ... for every scope of the deployment, tenants that never configured anything included ...
	other := scopeOf(tDeployment, tOtherApp, tTenantB, tWsp2, "usr_bob", allCaps...)
	f.enableAllFor(other)
	g := f.candidate()
	g.Scope = other
	f.mustDeny(g, providers.EgressDecisionReasonKillSwitch, providers.EgressDecisionCheckNameKillSwitch)
	// ... and every request, whatever it carries.
	f.mustDeny(f.candidate(), providers.EgressDecisionReasonKillSwitch, providers.EgressDecisionCheckNameKillSwitch)
	// A host showing the disclosure is told nothing may leave.
	d, err := f.svc.Disclose(testCtx, f.scope, f.prof, DisclosureInput{Role: providers.RoleConversationalController, Sources: []providers.SourceID{src}})
	if err != nil {
		t.Fatal(err)
	}
	if d.ExternalEnabled || len(d.DataClasses) != 0 || len(d.DataCategories) != 0 || d.RAGExcerptsIncluded {
		t.Errorf("disclosure under the kill switch: %+v", d)
	}
	// Another deployment is unaffected.
	foreign := scopeOf("dep_other", tApp, tTenantA, tWsp1, tUser, allCaps...)
	f.putAsDeployment(foreign)
	h := f.candidate()
	h.Scope = foreign
	f.mustAllow(h)

	// The switch and its audit trail: engaged, with who and when.
	st, err := f.svc.KillSwitch(testCtx, tDeployment)
	if err != nil || !st.Engaged || st.ChangedBy != tOperator || st.Reason != ReasonOperatorEmergency || !st.ChangedAt.Equal(tNow) {
		t.Errorf("state = %+v, %v", st, err)
	}
	ev := f.eventsOf(string(EventKillSwitch.Name()))
	if len(ev) != 1 || ev[0] != `{"changed_by":"`+tOperator+`","deployment_id":"`+tDeployment+`","engaged":true,"reason":"operator_emergency"}` {
		t.Errorf("kill switch events = %v", ev)
	}

	// Releasing it restores nothing by itself except the policy chain's own answer.
	if err := f.svc.ReleaseKillSwitch(testCtx, f.op); err != nil {
		t.Fatal(err)
	}
	f.mustAllow(candidate())
	if st, _ := f.svc.KillSwitch(testCtx, tDeployment); st.Engaged || st.Reason != ReasonOperatorReleased {
		t.Errorf("released state = %+v", st)
	}
	// Engaging again after a release works and is idempotent.
	for i := 0; i < 2; i++ {
		if err := f.svc.EngageKillSwitch(testCtx, f.op, "legal_hold"); err != nil {
			t.Fatal(err)
		}
	}
	f.mustDeny(candidate(), providers.EgressDecisionReasonKillSwitch, providers.EgressDecisionCheckNameKillSwitch)
}

// putAsDeployment enables the baseline for a scope of another deployment.
func (f *fixture) putAsDeployment(scope contracts.Scope) {
	f.t.Helper()
	op := Operator{DeploymentID: scope.DeploymentID, PrincipalID: tOperator}
	in := basePolicy(providers.EgressLevelDeployment)
	if _, err := f.svc.PutDeploymentPolicy(testCtx, op, in); err != nil {
		f.t.Fatal(err)
	}
	for _, lv := range []providers.EgressLevel{providers.EgressLevelApplication, providers.EgressLevelTenant, providers.EgressLevelWorkspace} {
		f.putAs(scope, lv, "", nil)
	}
}

func (f *fixture) eventsOf(typ string) []string {
	f.t.Helper()
	rows, err := f.db.QueryContext(testCtx, "SELECT data FROM audit_events WHERE event_type = ? ORDER BY application_id, tenant_id, chain_seq", typ)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func TestKillSwitchSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "egress.db")
	open := func() (*store.DB, *Service) {
		db, err := store.Open(testCtx, store.Options{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		clk := clock.NewFake(tNow)
		log, err := audit.New(db, audit.Options{Clock: clk})
		if err != nil {
			t.Fatal(err)
		}
		svc, err := New(Deps{DB: db, Clock: clk, Audit: log, IDs: ids.NewGenerator(clk)})
		if err != nil {
			t.Fatal(err)
		}
		return db, svc
	}
	op := Operator{DeploymentID: tDeployment, PrincipalID: tOperator}
	db, svc := open()
	if err := svc.EngageKillSwitch(testCtx, op, ReasonOperatorEmergency); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// A new process over the same store: the switch is still engaged, with no in-memory help.
	db2, svc2 := open()
	defer db2.Close()
	if on, err := svc2.killSwitchOn(testCtx, tDeployment); err != nil || !on {
		t.Fatalf("after a restart: engaged = %v, %v", on, err)
	}
	if on, _ := svc2.killSwitchOn(testCtx, "dep_other"); on {
		t.Error("the switch is per deployment")
	}
	if err := svc2.ReleaseKillSwitch(testCtx, op); err != nil {
		t.Fatal(err)
	}
	if on, _ := svc2.killSwitchOn(testCtx, tDeployment); on {
		t.Error("released")
	}
}

func TestKillSwitchFailsSafe(t *testing.T) {
	f := newFixture(t)
	f.enableAll()
	f.mustAllow(f.candidate())

	// An audit log that cannot record the change does not stop the process from stopping:
	// the switch is reported as failed, and this process stays shut.
	f.audit.set(errors.New("disk full"))
	if err := f.svc.EngageKillSwitch(testCtx, f.op, ""); err == nil {
		t.Fatal("engaging with a failing audit log must report the failure")
	}
	f.audit.set(nil)
	f.mustDeny(f.candidate(), providers.EgressDecisionReasonKillSwitch, providers.EgressDecisionCheckNameKillSwitch)
	// A failed release leaves it engaged.
	f.audit.set(errors.New("disk full"))
	if err := f.svc.ReleaseKillSwitch(testCtx, f.op); err == nil {
		t.Fatal("releasing with a failing audit log must report the failure")
	}
	f.audit.set(nil)
	f.mustDeny(f.candidate(), providers.EgressDecisionReasonKillSwitch, providers.EgressDecisionCheckNameKillSwitch)
	if err := f.svc.ReleaseKillSwitch(testCtx, f.op); err != nil {
		t.Fatal(err)
	}
	f.mustAllow(f.candidate())

	// A store that cannot be read is an engaged switch: the guard denies, as a typed denial that wraps the cause.
	ctx, cancel := context.WithCancel(testCtx)
	cancel()
	res, err := f.svc.Evaluate(ctx, f.candidate())
	if contracts.CodeOf(err) != contracts.ErrPermissionDenied || res.Envelope != nil {
		t.Fatalf("an unreadable store must deny: %v", err)
	}
	var ce *contracts.Error
	if !asContractsError(err, &ce) || ce.Details["rule"] != "guard_failure" {
		t.Errorf("details = %v", ce)
	}

	// Operators are validated: no operator, no switch.
	for _, op := range []Operator{{}, {DeploymentID: tDeployment}, {PrincipalID: tOperator}, {DeploymentID: "*", PrincipalID: tOperator}, {DeploymentID: tDeployment, PrincipalID: "a b"}} {
		if err := f.svc.EngageKillSwitch(testCtx, op, ""); err == nil {
			t.Errorf("operator %+v accepted", op)
		}
	}
	if err := f.svc.EngageKillSwitch(testCtx, f.op, "Free text reason!"); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("a reason is a fixed code, not text: %v", err)
	}
}
