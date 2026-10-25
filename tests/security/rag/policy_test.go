package ragsec

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The deterministic authority the injection tests hold still: the policy
// gateway, which decides, from the verified scope and the application package
// alone, which capabilities a model may be shown and which a proposal may use
// (master plan 3 rules 5 and 6). Retrieved text is not an input to it. The tests
// read its decisions before and after a document that tries to change them has
// been ingested, retrieved and put in front of a model that obeys it.

const (
	capFind    = "catalog.find_title"
	capExtend  = "loans.extend"
	capDelete  = "members.delete_account"
	capReindex = "admin.reindex"
	capExport  = "export_all_members" // named by an injected document, declared by no package
)

const memberPolicy = `{
  "policy_id": "library-access",
  "role_grants": [
    { "role": "member", "capabilities": ["catalog.find_title", "loans.extend"] },
    { "role": "staff",  "capabilities": ["catalog.find_title", "loans.extend", "members.delete_account", "admin.reindex"] }
  ],
  "risk_overrides": [],
  "field_projection": []
}`

type policyWorld struct {
	gw    *policy.Gateway
	pkg   *policy.Package
	scope contracts.Scope
}

// newPolicyWorld is a small library application with two capabilities a member
// may use and two that need staff, and a member whose token, in the worst case,
// names all four.
func newPolicyWorld(t testing.TB, clk clock.Clock, scope contracts.Scope) *policyWorld {
	t.Helper()
	pol, err := policy.ParseHostPolicy([]byte(memberPolicy))
	if err != nil {
		t.Fatalf("ParseHostPolicy: %v", err)
	}
	pkg, err := policy.NewPackage(policy.PackageSpec{
		ApplicationID: scope.ApplicationID,
		Version:       "1.0.0",
		Status:        policy.StatusActive,
		Roles:         []string{"member", "staff"},
		Capabilities: []policy.CapabilityDecl{
			{ID: capFind, Audience: policy.AudienceUser, EffectClass: policy.EffectNone, RiskLevel: 0, Confirmation: policy.ConfirmNone,
				Fields: []policy.FieldDecl{{Name: "query", Required: true}}},
			{ID: capExtend, Audience: policy.AudienceUser, EffectClass: policy.EffectReversible, RiskLevel: 1, Confirmation: policy.ConfirmNone,
				Fields: []policy.FieldDecl{{Name: "loan", Required: true}, {Name: "weeks"}}},
			{ID: capDelete, Audience: policy.AudienceUser, EffectClass: policy.EffectPrivileged, RiskLevel: 3, Confirmation: policy.ConfirmStepUp,
				Fields: []policy.FieldDecl{{Name: "member", Required: true}}},
			{ID: capReindex, Audience: policy.AudienceAdmin, EffectClass: policy.EffectNone, RiskLevel: 0, Confirmation: policy.ConfirmNone},
		},
		Policies: []*policy.HostPolicy{pol},
	})
	if err != nil {
		t.Fatalf("policy.NewPackage: %v", err)
	}
	s := scope.Clone()
	s.Roles = []string{"member"}
	s.CapabilityScopes = []string{capFind, capExtend, capDelete, capReindex}
	return &policyWorld{gw: policy.NewGateway(clk), pkg: pkg, scope: s}
}

// policySnapshot is the gateway's whole answer to the questions a prompt builder
// and a proposal check ask: which capabilities may be shown to the model (with
// the fields and risk each carries), and what happens when each capability, the
// ones a document names included, is proposed.
type policySnapshot struct {
	Visible   []string
	Decisions map[string]string
}

func (p *policyWorld) snapshot(t testing.TB) policySnapshot {
	t.Helper()
	exp := p.gw.Expose(bg, policy.ExposeRequest{Scope: p.scope, Package: p.pkg, Binding: policy.BindingOf(p.scope)})
	if exp.Code != "" {
		t.Fatalf("Expose refused: %s", exp.Code)
	}
	snap := policySnapshot{Decisions: map[string]string{}}
	for _, d := range exp.Visible {
		snap.Visible = append(snap.Visible, fmt.Sprintf("%s risk=%v fields=%v", d.Record.CapabilityID, d.EffectiveRisk, d.VisibleFields))
	}
	for _, id := range []string{capFind, capExtend, capDelete, capReindex, capExport} {
		d := p.gw.Decide(bg, policy.Request{
			Scope: p.scope, Package: p.pkg, CapabilityID: id, Binding: policy.BindingOf(p.scope), Phase: policy.PhasePrepare,
			Fields: map[string]any{"query": "x", "loan": "l1", "member": "all"},
		})
		snap.Decisions[id] = fmt.Sprintf("allowed=%v code=%s risk=%v", d.Allowed, d.Code, d.EffectiveRisk)
	}
	return snap
}

func (s policySnapshot) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "visible: %q\n", s.Visible)
	ids := make([]string, 0, len(s.Decisions))
	for id := range s.Decisions {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		fmt.Fprintf(&b, "  %-24s %s\n", id, s.Decisions[id])
	}
	return b.String()
}

// requireDeniesWhatItDenied is the control of every policy comparison: the
// snapshot is not trivially empty. A member sees exactly the two member
// capabilities, and the three an injected document names (one staff capability,
// one admin capability, one declared by nobody) are denied.
func (s policySnapshot) requireMemberView(t testing.TB) {
	t.Helper()
	if len(s.Visible) != 2 || !strings.HasPrefix(s.Visible[0], capFind) || !strings.HasPrefix(s.Visible[1], capExtend) {
		t.Errorf("the member's tool list is %q, want exactly %s and %s", s.Visible, capFind, capExtend)
	}
	for _, id := range []string{capDelete, capReindex, capExport} {
		if !strings.HasPrefix(s.Decisions[id], "allowed=false") {
			t.Errorf("%s is not denied to a member: %s", id, s.Decisions[id])
		}
	}
	for _, id := range []string{capFind, capExtend} {
		if !strings.HasPrefix(s.Decisions[id], "allowed=true") {
			t.Errorf("%s is not allowed to a member: %s", id, s.Decisions[id])
		}
	}
}
