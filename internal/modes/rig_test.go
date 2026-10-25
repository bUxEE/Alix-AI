package modes

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

var testScope = contracts.Scope{
	DeploymentID: "dep1", ApplicationID: "app1", TenantID: "ten1", WorkspaceID: "ws1",
	PrincipalID: "user1", Roles: []string{"member"}, CapabilityScopes: []string{"cap.read"}, SessionID: "ses1",
}

const (
	profA pv.ProfileID = "pprf_A"
	profB pv.ProfileID = "pprf_B"
)

func reasonErr(code contracts.ErrCode, reason providerreg.Reason) error {
	return contracts.New(code, "fake").WithDetails("reason", string(reason))
}

type fakeKS struct {
	mu      sync.Mutex
	engaged bool
	err     error
}

func (k *fakeKS) KillSwitch(context.Context, string) (dataegress.KillSwitchState, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return dataegress.KillSwitchState{Engaged: k.engaged}, k.err
}

func (k *fakeKS) set(engaged bool) { k.mu.Lock(); k.engaged = engaged; k.mu.Unlock() }

type fakeLocal map[Role]bool

func (l fakeLocal) Usable(r Role) bool { return l[r] }

type fakeProfiles struct {
	rig           *rig
	checkErr      map[pv.ProfileID]error
	lifecycle     map[pv.ProfileID]pv.ProfileLifecycle
	fallbackTo    pv.ProfileID
	fallbackCalls int
}

func profileOf(id pv.ProfileID) pv.ProviderProfile {
	return pv.ProviderProfile{
		ProfileID: id, ProviderID: "acme", ModelID: "m1", ModelVersion: "v1",
		Lifecycle: pv.ProfileLifecycleQualified,
		Roles:     []pv.Role{pv.RoleConversationalController, pv.RoleLanguageRealization, pv.RoleVision},
	}
}

func (f *fakeProfiles) Get(_ context.Context, _ contracts.Scope, id pv.ProfileID) (pv.ProviderProfile, error) {
	p := profileOf(id)
	if lc, ok := f.lifecycle[id]; ok {
		p.Lifecycle = lc
	}
	return p, nil
}

func (f *fakeProfiles) Check(_ context.Context, _ contracts.Scope, id pv.ProfileID, req providerreg.Requirement) (providerreg.Admission, error) {
	f.rig.note("check:" + string(id))
	if err := f.checkErr[id]; err != nil {
		return providerreg.Admission{}, err
	}
	return providerreg.Admission{Profile: profileOf(id)}, nil
}

func (f *fakeProfiles) Fallback(_ context.Context, _ contracts.Scope, from pv.ProfileID, cond providerreg.FallbackCondition, _ providerreg.Requirement) (providerreg.FallbackRecord, providerreg.Admission, error) {
	f.fallbackCalls++
	f.rig.note("fallback:" + string(cond))
	if f.fallbackTo == "" {
		return providerreg.FallbackRecord{}, providerreg.Admission{}, reasonErr(contracts.ErrPermissionDenied, providerreg.ReasonFallbackNotNamed)
	}
	return providerreg.FallbackRecord{From: from, To: f.fallbackTo, Condition: providerreg.FallbackCondition(cond)}, providerreg.Admission{Profile: profileOf(f.fallbackTo)}, nil
}

type fakeGuard struct {
	rig  *rig
	deny error
	last dataegress.Candidate
}

func (g *fakeGuard) Evaluate(_ context.Context, c dataegress.Candidate) (dataegress.Result, error) {
	g.rig.note("guard:" + string(c.Profile.ProfileID))
	g.last = c
	if g.deny != nil {
		return dataegress.Result{}, g.deny
	}
	return dataegress.Result{
		Decision: pv.EgressDecision{DecisionID: "edec_1"},
		Envelope: &pv.RequestEnvelope{EnvelopeID: "envl_1"},
	}, nil
}

type fakeBudgets struct {
	rig       *rig
	reserve   error
	states    []pv.BudgetState
	calls     []providerbudget.Call
	settled   []providerbudget.Actual
	settleErr error
}

func (b *fakeBudgets) Reserve(_ context.Context, _ contracts.Scope, c providerbudget.Call) (providerbudget.Reservation, error) {
	b.rig.note("reserve:" + string(c.Provider.ProfileID))
	b.calls = append(b.calls, c)
	if b.reserve != nil {
		return providerbudget.Reservation{}, b.reserve
	}
	return providerbudget.Reservation{ID: "rsv_1"}, nil
}

func (b *fakeBudgets) Settle(_ context.Context, _ contracts.Scope, _ pv.ReservationID, a providerbudget.Actual) (providerbudget.Settlement, error) {
	b.rig.note("settle:" + string(a.Outcome))
	b.settled = append(b.settled, a)
	return providerbudget.Settlement{}, b.settleErr
}

func (b *fakeBudgets) States(context.Context, contracts.Scope, pv.ProviderID, pv.Channel) ([]pv.BudgetState, error) {
	return b.states, nil
}

// rig wires a Router over fakes that record the order of every call.
type rig struct {
	t        *testing.T
	log      []string
	ks       *fakeKS
	modes    *MemoryModes
	policy   StaticPolicy
	local    fakeLocal
	profiles *fakeProfiles
	guard    *fakeGuard
	budgets  *fakeBudgets
	router   *Router
	sendErr  map[pv.ProfileID]error
	localErr error
}

func (r *rig) note(s string) { r.log = append(r.log, s) }

func (r *rig) count(prefix string) int {
	n := 0
	for _, l := range r.log {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

func (r *rig) index(entry string) int { return slices.Index(r.log, entry) }

// newRig builds a rig with every level set to mode, a controller route to profA and a usable local
// controller. Mode Local leaves the levels unset (the default).
func newRig(t *testing.T, mode Mode) *rig {
	t.Helper()
	r := &rig{t: t, ks: &fakeKS{}, modes: NewMemoryModes(), local: fakeLocal{RoleController: true, RoleDecisionScorer: true, RoleEmbedder: true},
		policy: StaticPolicy{RoleController: {Profiles: []pv.ProfileID{profA}}}, sendErr: map[pv.ProfileID]error{}}
	r.profiles = &fakeProfiles{rig: r, checkErr: map[pv.ProfileID]error{}, lifecycle: map[pv.ProfileID]pv.ProfileLifecycle{}}
	r.guard = &fakeGuard{rig: r}
	r.budgets = &fakeBudgets{rig: r, states: []pv.BudgetState{{}}}
	if mode != ModeLocal {
		for _, lv := range []Level{LevelDeployment, LevelApplication, LevelTenant} {
			if err := r.modes.Set(lv, testScope, mode); err != nil {
				t.Fatal(err)
			}
		}
	}
	r.build()
	return r
}

func (r *rig) build() {
	rt, err := New(Config{Modes: r.modes, KillSwitch: r.ks, Policy: r.policy, Local: r.local, Profiles: r.profiles, Guard: r.guard, Budgets: r.budgets})
	if err != nil {
		r.t.Fatal(err)
	}
	r.router = rt
}

func (r *rig) request(role Role) Request {
	return Request{
		Scope: testScope, Role: role,
		Requirement: providerreg.Requirement{DataClass: pv.DataClassificationInternal},
		Estimate: func(pv.ProviderProfile) (providerbudget.Units, pv.MicroUnits, error) {
			return providerbudget.Units{ToksIn: 10, ToksOut: 5}, 100, nil
		},
	}
}

func (r *rig) execute(req Request) (Outcome, error) {
	r.t.Helper()
	return r.router.Execute(context.Background(), req, Executors{
		Local: func(context.Context) error {
			r.note("local")
			return r.localErr
		},
		External: func(_ context.Context, c ExternalCall) (ExternalResult, error) {
			r.note("send:" + string(c.Admission.Profile.ProfileID))
			if err := r.sendErr[c.Admission.Profile.ProfileID]; err != nil {
				return ExternalResult{}, err
			}
			return ExternalResult{Metering: providerbudget.Units{ToksIn: 8, ToksOut: 4}}, nil
		},
	})
}

func mustKind(t *testing.T, o Outcome, err error, want Kind) {
	t.Helper()
	if o.Kind != want {
		t.Fatalf("outcome = %q (%s), want %q", o.Kind, o.Reason, want)
	}
	if want == KindServed {
		if err != nil {
			t.Fatalf("served outcome returned an error: %v", err)
		}
		return
	}
	if err == nil || OutcomeOf(err) != want {
		t.Fatalf("a %q outcome must come with an error carrying it, got %v", want, err)
	}
}

func callErr(kind pv.ProviderErrorKind, sent bool) error {
	return &CallError{Kind: kind, Sent: sent, Err: errors.New("raw provider text must not leak")}
}
