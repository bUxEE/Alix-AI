package probes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// This file is the test host of the step-up probes: the level 3 capability of the
// facility-maintenance application (assets.decommission), written in Go to behave
// as protocol.py and stepup.py do (the order of the checks of a commit, the staged
// step-up request that Prepare returns idempotently, a Status that never has a record,
// an interface that applies the effect itself) and to break in exactly one chosen way.
// With the approval knob it behaves as the ledger does instead: the interface records
// an approval and the commit that follows applies the effect. Identifiers come from
// counters of the whole deployment, not of a tenant, as in the application, so the
// identifiers of two audit worlds differ.

const (
	facilityDir = "../../../examples/reference-apps/facility-maintenance/package"
	suCap       = "assets.decommission"
	suAppID     = "facility-maintenance"
	suStepUpRef = "step_up_reference"
)

type suKnobs struct {
	approval       bool // the interface records an approval and the commit applies the effect (the ledger's design)
	bypass         bool // a commit without the step-up is applied
	applyOnRefusal bool // a refused commit applies the effect all the same, and records nothing
	wrongCode      bool // a commit without the step-up is refused as permission_denied
	idleUI         bool // completing the step-up changes nothing
	failUI         bool // the step-up cannot be completed
	oddStatus      bool // Status of a key without a receipt is rejected, but not as the no-record marker
}

type suAsset struct {
	id, ws, name, tag string
	rev               int
	status            string
}

type suStep struct {
	ref, principal, asset, reason, status string
	ws                                    string
	rev                                   int
	expires                               time.Time
}

type suPrincipal struct {
	ws   string
	role string
}

type suTenant struct {
	principals map[string]suPrincipal
	workspaces map[string]bool
	assets     map[string]*suAsset
	steps      map[string]*suStep
	approvals  map[string]bool
	receipts   map[string]map[string]any
}

type suHost struct {
	mu          sync.Mutex
	clk         *clock.Fake
	k           suKnobs
	tenants     map[string]*suTenant
	nextAsset   int
	nextStep    int
	effects     int
	completions int
}

func newSUHost(k ...func(*suKnobs)) *suHost {
	h := &suHost{clk: clock.NewFake(simT0), tenants: map[string]*suTenant{}}
	for _, f := range k {
		f(&h.k)
	}
	return h
}

func (h *suHost) body(op adapters.Operation, key string, m map[string]any) (act.DeterministicResult, error) {
	return adapters.Normalise(mustJSON(m), adapters.Expect{Operation: op, CapabilityID: suCap, IdempotencyKey: key})
}

func (h *suHost) fresh(version string) map[string]any {
	return map[string]any{"as_of": h.clk.Now().UTC().Format("2006-01-02T15:04:05Z"), "version": version}
}

func suOK(fresh map[string]any) map[string]any {
	return map[string]any{
		"status": "ok", "identifiers": []any{}, "values": []any{}, "errors": []any{}, "allowed_next_actions": []any{},
		"freshness": fresh,
	}
}

// authz resolves the caller from the application's own directory.
func (h *suHost) authz(sc contracts.Scope) (*suTenant, suPrincipal, map[string]any) {
	t := h.tenants[sc.TenantID]
	if t == nil {
		return nil, suPrincipal{}, rejected("scope_violation", "", "outside the caller's scope")
	}
	p, ok := t.principals[sc.PrincipalID]
	if !ok || p.ws != sc.WorkspaceID {
		return nil, suPrincipal{}, rejected("scope_violation", "", "outside the caller's scope")
	}
	if p.role != "supervisor" {
		return nil, suPrincipal{}, rejected("permission_denied", "", "the role may not decommission assets")
	}
	return t, p, nil
}

func (h *suHost) load(t *suTenant, sc contracts.Scope, f map[string]any) (*suAsset, string, map[string]any) {
	id, ok := stringField(f, "asset")
	if !ok || id == "" {
		return nil, "", rejected("missing_fields", "asset", "asset is required")
	}
	reason, ok := stringField(f, "reason")
	if !ok {
		return nil, "", rejected("missing_fields", "reason", "reason is required")
	}
	if n := len([]rune(reason)); n < 10 || n > 300 {
		return nil, "", rejected("validation_failed", "reason", "the reason must be 10 to 300 characters")
	}
	a := t.assets[id]
	if a == nil || a.ws != sc.WorkspaceID {
		return nil, "", rejected("scope_violation", "asset", "the asset is outside the caller's scope or does not exist")
	}
	if a.status != "active" {
		return nil, "", rejected("conflict", "asset", "the asset is not active")
	}
	return a, reason, nil
}

func suVersion(a *suAsset) string { return fmt.Sprintf("r%d", a.rev) }

func (h *suHost) Read(_ context.Context, _ contracts.Scope, r adapters.ReadRequest) (act.DeterministicResult, error) {
	return adapters.Normalise(mustJSON(rejected("unknown_capability", "", "no such operation")), adapters.Expect{Operation: adapters.OpRead, CapabilityID: r.CapabilityID})
}

func (h *suHost) Validate(_ context.Context, sc contracts.Scope, r adapters.ValidateRequest) (act.DeterministicResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, _, rej := h.authz(sc)
	if rej != nil {
		return h.body(adapters.OpValidate, "", rej)
	}
	a, _, rej := h.load(t, sc, r.Fields)
	if rej != nil {
		return h.body(adapters.OpValidate, "", rej)
	}
	return h.body(adapters.OpValidate, "", suOK(h.fresh(suVersion(a))))
}

func (h *suHost) Prepare(_ context.Context, sc contracts.Scope, r adapters.PrepareRequest) (act.DeterministicResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, p, rej := h.authz(sc)
	if rej != nil {
		return h.body(adapters.OpPrepare, "", rej)
	}
	a, reason, rej := h.load(t, sc, r.Fields)
	if rej != nil {
		return h.body(adapters.OpPrepare, "", rej)
	}
	// the step-up request is staged idempotently: the same request, the same reference
	var step *suStep
	for _, s := range t.steps {
		if s.status == "pending" && s.principal == sc.PrincipalID && s.asset == a.id && s.rev == a.rev && s.reason == reason {
			step = s
		}
	}
	if step == nil {
		h.nextStep++
		step = &suStep{
			ref: fmt.Sprintf("SU-%08d", h.nextStep), principal: sc.PrincipalID, ws: p.ws, asset: a.id, reason: reason, status: "pending",
			rev: a.rev, expires: h.clk.Now().Add(900 * time.Second),
		}
		t.steps[step.ref] = step
	}
	ver := suVersion(a)
	b := suOK(h.fresh(ver))
	b["values"] = []any{textFact("asset_name", a.name), textFact("asset_tag", a.tag), textFact(suStepUpRef, step.ref)}
	b["preparation"] = map[string]any{
		"summary": "Decommission " + a.name,
		"effects": []any{map[string]any{
			"operation": "update", "entity_type": "asset", "entity_id": a.id,
			"description": "Retire the asset.", "changes": []any{textFact("status", "decommissioned")}, "external": false,
		}},
		"data_versions":       []any{map[string]any{"entity_type": "asset", "entity_id": a.id, "version": ver}},
		"declared_risk_level": 3,
		"valid_for_seconds":   900,
	}
	return h.body(adapters.OpPrepare, "", b)
}

func (h *suHost) apply(t *suTenant, a *suAsset, key, ws string) map[string]any {
	a.status = "decommissioned"
	a.rev++
	h.effects++
	b := suOK(h.fresh(suVersion(a)))
	b["identifiers"] = []any{rec("asset", a.id)}
	b["values"] = []any{textFact("asset_name", a.name), textFact("status", "decommissioned")}
	t.receipts[ws+"|"+key] = b
	return b
}

func (h *suHost) Commit(_ context.Context, sc contracts.Scope, r adapters.CommitRequest) (act.DeterministicResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := string(r.IdempotencyKey)
	t, _, rej := h.authz(sc)
	if rej != nil {
		return h.body(adapters.OpCommit, key, rej)
	}
	if prev, seen := t.receipts[sc.WorkspaceID+"|"+key]; seen {
		return h.body(adapters.OpCommit, key, prev)
	}
	var assetID, dvVersion string
	for _, e := range r.Prepared.Effects {
		if e.EntityID != nil {
			assetID = string(*e.EntityID)
		}
	}
	for _, dv := range r.Prepared.DataVersions {
		dvVersion = string(dv.Version)
	}
	if exp, err := time.Parse(time.RFC3339, string(r.Prepared.ExpiresAt)); err == nil && !h.clk.Now().Before(exp) {
		return h.body(adapters.OpCommit, key, rejected("expired", "prepared", "the preparation expired"))
	}
	a := t.assets[assetID]
	if a == nil || a.ws != sc.WorkspaceID {
		return h.body(adapters.OpCommit, key, rejected("scope_violation", "asset", "the asset is outside the caller's scope or does not exist"))
	}
	if dvVersion != suVersion(a) {
		return h.body(adapters.OpCommit, key, rejected("stale", "", "the asset changed after it was prepared"))
	}
	if a.status != "active" {
		return h.body(adapters.OpCommit, key, rejected("conflict", "asset", "the asset is not active"))
	}
	switch {
	case h.k.bypass:
		return h.body(adapters.OpCommit, key, h.apply(t, a, key, sc.WorkspaceID))
	case h.k.approval && t.approvals[a.id+"|"+suVersion(a)]:
		return h.body(adapters.OpCommit, key, h.apply(t, a, key, sc.WorkspaceID))
	}
	if h.k.applyOnRefusal {
		a.status = "decommissioned"
		a.rev++
		h.effects++
	}
	code := "step_up_required"
	if h.k.wrongCode {
		code = "permission_denied"
	}
	return h.body(adapters.OpCommit, key, rejected(code, "", "this action must be completed in the application's own authenticated interface"))
}

func (h *suHost) Status(_ context.Context, sc contracts.Scope, r adapters.StatusRequest) (act.DeterministicResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := string(r.IdempotencyKey)
	t, _, rej := h.authz(sc)
	if rej != nil {
		return h.body(adapters.OpStatus, key, rej)
	}
	if b, ok := t.receipts[sc.WorkspaceID+"|"+key]; ok {
		return h.body(adapters.OpStatus, key, b)
	}
	if h.k.oddStatus {
		return h.body(adapters.OpStatus, key, rejected("validation_failed", "other_field", "unknown"))
	}
	return h.body(adapters.OpStatus, key, rejected("validation_failed", "idempotency_key", "the application has no record of this key"))
}

func (h *suHost) Cancel(_ context.Context, _ contracts.Scope, r adapters.CancelRequest) (act.DeterministicResult, error) {
	return h.body(adapters.OpCancel, string(r.IdempotencyKey), rejected("validation_failed", "idempotency_key", "nothing is pending for this key"))
}

func (h *suHost) Describe(context.Context) (adapters.Description, error) {
	return adapters.Description{
		Transport: "fake", ContractVersions: []int{adapters.ContractVersion}, ApplicationVersion: "1.0.0",
		Capabilities: []adapters.CapabilityOps{{CapabilityID: suCap, Operations: []adapters.Operation{
			adapters.OpValidate, adapters.OpPrepare, adapters.OpCommit, adapters.OpStatus,
		}}},
	}, nil
}

func (h *suHost) Health(context.Context) (adapters.HealthReport, error) {
	return adapters.HealthReport{Status: adapters.HealthOK}, nil
}

func (h *suHost) Inspect(_ context.Context, _ contracts.Scope, req adapters.InspectRequest) (adapters.Inspection, error) {
	out := adapters.Inspection{}
	aspects := req.Aspects
	if len(aspects) == 0 {
		aspects = adapters.AllAspects()
	}
	for _, a := range aspects {
		out.Reports = append(out.Reports, adapters.AspectReport{Aspect: a, State: adapters.AspectUnsupported, Facts: []act.Fact{}})
	}
	return out, nil
}

// ---- provisioning, and the application's own interface ----

func (h *suHost) CreateTenant(_ context.Context, t Tenant) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.tenants[t.ID]; ok {
		return contracts.New(contracts.ErrConflict, "tenant exists")
	}
	tt := &suTenant{
		principals: map[string]suPrincipal{}, workspaces: map[string]bool{}, assets: map[string]*suAsset{},
		steps: map[string]*suStep{}, approvals: map[string]bool{}, receipts: map[string]map[string]any{},
	}
	for _, w := range t.Workspaces {
		tt.workspaces[w] = true
	}
	for _, p := range t.Principals {
		role := ""
		if len(p.Roles) > 0 {
			role = p.Roles[0]
		}
		tt.principals[p.ID] = suPrincipal{ws: p.WorkspaceID, role: role}
	}
	h.tenants[t.ID] = tt
	return nil
}

func (h *suHost) Seed(_ context.Context, sd Seed) (SeedResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.tenants[sd.TenantID]
	if t == nil {
		return SeedResult{}, contracts.New(contracts.ErrScopeViolation, "no such tenant")
	}
	res := SeedResult{Entities: map[string]string{}}
	for _, r := range sd.Records {
		// one counter for the whole deployment: two tenants never hold the same identifier
		h.nextAsset++
		id := fmt.Sprintf("rec-%06d", h.nextAsset)
		if r.EntityType == "asset" {
			id = fmt.Sprintf("ast-%06d", h.nextAsset)
			t.assets[id] = &suAsset{id: id, ws: r.WorkspaceID, name: "Synthetic " + r.Ref, tag: strings.ToUpper(r.Ref), rev: 1, status: "active"}
		}
		res.Entities[r.Ref] = id
	}
	return res, nil
}

func (h *suHost) Cleanup(_ context.Context, tenantID string) (CleanupReport, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.tenants[tenantID] == nil {
		return CleanupReport{Existed: false, Remaining: map[string]int{}}, nil
	}
	delete(h.tenants, tenantID)
	return CleanupReport{Existed: true, Remaining: map[string]int{}}, nil
}

// CompleteStepUp is the supervisor in the application's own interface.
func (h *suHost) CompleteStepUp(_ context.Context, su StepUp) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.completions++
	if h.k.failUI {
		return contracts.New(contracts.ErrPermissionDenied, "the step-up was refused")
	}
	ref := ""
	for _, f := range su.Result.Values {
		if tv, ok := f.Value.Value.(act.TextValue); ok && string(f.Field) == suStepUpRef {
			ref = tv.Value
		}
	}
	t := h.tenants[su.Scope.TenantID]
	if t == nil || ref == "" {
		return contracts.New(contracts.ErrScopeViolation, "no such request")
	}
	step := t.steps[ref]
	switch {
	case step == nil || step.principal != su.Scope.PrincipalID:
		return contracts.New(contracts.ErrScopeViolation, "no such request")
	case step.status != "pending" || !h.clk.Now().Before(step.expires):
		return contracts.New(contracts.ErrExpired, "the request is no longer valid")
	}
	a := t.assets[step.asset]
	if a == nil || a.rev != step.rev {
		return contracts.New(contracts.ErrStale, "the asset changed")
	}
	if h.k.idleUI {
		return nil
	}
	step.status = "done"
	if h.k.approval {
		t.approvals[a.id+"|"+suVersion(a)] = true
		return nil
	}
	a.status = "decommissioned"
	a.rev++
	h.effects++
	return nil
}

func (h *suHost) effectCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.effects
}

// ---- the package: the real facility-maintenance package, held to its step-up capability ----

func loadFacility(t testing.TB) *apppkg.Package {
	t.Helper()
	res, err := apppkg.Load(context.Background(), apppkg.FromDir(facilityDir), apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
	if err != nil {
		t.Fatal(err)
	}
	if res.Package == nil {
		t.Fatalf("the facility-maintenance package did not load: %v", res.Findings)
	}
	return res.Package
}

// facilityStepUpPackage is the shipped package, unchanged but for the capabilities the
// host does not implement: the audit then runs over its level 3 capability only.
func facilityStepUpPackage(t testing.TB) *apppkg.Package {
	t.Helper()
	return prunedTo(t, loadFacility(t), suCap)
}

// prunedTo keeps one capability of a loaded package, and of its policies, and checks that
// it is a level 3 step-up capability: nothing else of the package is touched.
func prunedTo(t testing.TB, full *apppkg.Package, capID string) *apppkg.Package {
	t.Helper()
	c, ok := full.Capabilities[capID]
	if !ok {
		t.Fatalf("the package declares no capability %s", capID)
	}
	if c.Confirmation.Mode != "step_up" || c.RiskLevel != 3 {
		t.Fatalf("%s is no longer a level 3 step-up capability (mode %s, level %d)", capID, c.Confirmation.Mode, c.RiskLevel)
	}
	pruned := *full
	pruned.Capabilities = map[string]ap.Capability{capID: c}
	pruned.Policies = nil
	for _, hp := range full.Policies {
		kept := *hp
		kept.RoleGrants, kept.RiskOverrides, kept.FieldProjection = nil, nil, nil
		for _, g := range hp.RoleGrants {
			if slices.Contains(g.Capabilities, capID) {
				kept.RoleGrants = append(kept.RoleGrants, policy.RoleGrant{Role: g.Role, Capabilities: []string{capID}})
			}
		}
		for _, o := range hp.RiskOverrides {
			if o.CapabilityID == capID {
				kept.RiskOverrides = append(kept.RiskOverrides, o)
			}
		}
		for _, f := range hp.FieldProjection {
			if f.CapabilityID == capID {
				kept.FieldProjection = append(kept.FieldProjection, f)
			}
		}
		pruned.Policies = append(pruned.Policies, &kept)
	}
	return &pruned
}

// ---- the input of a check ----

// noStepUp hides the step-up of a provisioner that has one.
type noStepUp struct{ Provisioner }

type suEnv struct {
	t    *testing.T
	host *suHost
	pkg  *apppkg.Package
	in   *integrationaudit.Input
	prov Provisioner
}

func newSUEnv(t *testing.T, pkg *apppkg.Package, h *suHost, withStepUp bool) *suEnv {
	t.Helper()
	guard, err := adapters.Wrap(h, adapters.Options{ApplicationID: suAppID, DefaultTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	desc, err := h.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e := &suEnv{t: t, host: h, pkg: pkg}
	e.prov = h
	if !withStepUp {
		e.prov = noStepUp{h}
	}
	e.in = &integrationaudit.Input{
		Description: &desc, Package: pkg, Adapter: guard, Clock: h.clk, CoreVersion: apppkg.DevCoreVersion,
		Scope: contracts.Scope{
			DeploymentID: "alixai-audit", ApplicationID: suAppID, TenantID: "alixai-audit", WorkspaceID: "alixai-audit",
			PrincipalID: "alixai-audit", Roles: []string{"auditor"}, CapabilityScopes: []string{"audit.inspect"}, SessionID: "audit-run",
		},
	}
	return e
}

func (e *suEnv) run(section int) *integrationaudit.Outcome {
	e.t.Helper()
	opts := Options{
		Source:     func(context.Context, *integrationaudit.Input) (Provisioner, error) { return e.prov, nil },
		ScratchDir: e.t.TempDir(),
	}
	for _, c := range Checks(opts) {
		if c.Section() == section {
			out, err := c.Run(context.Background(), e.in)
			if err != nil {
				e.t.Fatalf("check %d returned an error: %v", section, err)
			}
			return out
		}
	}
	e.t.Fatalf("no check for section %d", section)
	return nil
}

// The three checks that commit through the adapter.
var suSections = []int{integrationaudit.SectionDeterministic, integrationaudit.SectionSafeExecution, integrationaudit.SectionDegradation}

func covered(o *integrationaudit.Outcome, id string) bool {
	for _, c := range o.Covered {
		if c == id {
			return true
		}
	}
	return false
}

func probePassed(o *integrationaudit.Outcome, subject string) (found, passed bool) {
	for _, p := range o.Probes {
		if p.Subject == subject {
			return true, p.Passed
		}
	}
	return false, false
}

func hasUnverified(o *integrationaudit.Outcome, subject string) bool {
	for _, u := range o.Unverified {
		if u.Subject == subject {
			return true
		}
	}
	return false
}

func criticalIDs(o *integrationaudit.Outcome) []string {
	return findingIDs(o, contracts.SeverityCritical)
}

func mustJSON(m map[string]any) []byte {
	raw, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return raw
}

// ---- the tests ----

func TestStepUpCapabilityIsProbedNotRecordedUnprobed(t *testing.T) {
	const effect = "the effect is there after the application's own step-up"
	cases := []struct {
		name       string
		knobs      func(*suKnobs)
		completer  bool
		wantCovers bool     // the capability counts as probed by checks 2, 8 and 9
		critical   []string // the critical findings, over the three checks
		unverified string   // a subject the audit records as unverified
		effectPass *bool    // the probe that shows the effect is there only after the step-up
	}{
		{name: "the interface applies the effect itself", completer: true, wantCovers: true, effectPass: ptr(true)},
		{name: "the interface records an approval and the commit applies it", knobs: func(k *suKnobs) { k.approval = true }, completer: true, wantCovers: true, effectPass: ptr(true)},
		{name: "no way to complete a step-up is verified as far as it goes", completer: false, wantCovers: true, unverified: "safety:step_up_completion"},
		{name: "a commit without the step-up applies the effect", knobs: func(k *suKnobs) { k.bypass = true }, completer: true, wantCovers: false, critical: []string{"safety.step_up_bypassed"}},
		{name: "a refused commit applies the effect all the same", knobs: func(k *suKnobs) { k.applyOnRefusal = true }, completer: true, wantCovers: false, critical: []string{"safety.step_up_bypassed"}},
		{name: "the step-up changes nothing", knobs: func(k *suKnobs) { k.idleUI = true }, completer: true, wantCovers: false, critical: []string{"safety.step_up_without_effect"}, effectPass: ptr(false)},
		{name: "a commit is refused for another reason than the step-up", knobs: func(k *suKnobs) { k.wrongCode = true }, completer: true, wantCovers: false, critical: []string{"probes.capability_not_probed"}},
		{name: "the step-up cannot be completed", knobs: func(k *suKnobs) { k.failUI = true }, completer: true, wantCovers: false, critical: []string{"probes.capability_not_probed"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var knobs []func(*suKnobs)
			if tc.knobs != nil {
				knobs = append(knobs, tc.knobs)
			}
			h := newSUHost(knobs...)
			e := newSUEnv(t, facilityStepUpPackage(t), h, tc.completer)
			var crit []string
			var all []*integrationaudit.Outcome
			for _, s := range suSections {
				o := e.run(s)
				all = append(all, o)
				crit = append(crit, criticalIDs(o)...)
				if tc.wantCovers && !covered(o, suCap) {
					t.Errorf("check %d did not count %s as probed:\n%s", s, suCap, dump(o))
				}
				if tc.wantCovers && hasFinding(o, "probes.capability_not_probed") {
					t.Errorf("check %d recorded %s as not probed:\n%s", s, suCap, dump(o))
				}
			}
			crit = dedup(crit)
			if !equalStrings(crit, tc.critical) {
				t.Errorf("critical findings = %v, want %v\n%s", crit, tc.critical, dump(all[1]))
			}
			if tc.unverified != "" && !hasUnverified(all[1], tc.unverified) {
				t.Errorf("the audit does not record %q as unverified:\n%s", tc.unverified, dump(all[1]))
			}
			if tc.unverified == "" && hasUnverified(all[1], "safety:step_up_completion") {
				t.Errorf("the step-up was completed, yet the audit records it as unverified")
			}
			if tc.effectPass != nil {
				found, passed := probePassed(all[1], effect)
				if !found || passed != *tc.effectPass {
					t.Errorf("probe %q: found %v passed %v, want passed %v\n%s", effect, found, passed, *tc.effectPass, dump(all[1]))
				}
			}
			if tc.critical == nil && h.effectCount() != h.completions {
				t.Errorf("effects applied = %d, step-ups completed = %d: the effect must happen once per completed step-up and never otherwise", h.effectCount(), h.completions)
			}
		})
	}
}

// A failed record probe of check 9 is a finding, never a silent failed probe: after a
// refused step-up commit, Status that is not the no-record marker is said to be that.
func TestFailureProbeOfAStepUpCommitWithoutTheMarkerIsAFinding(t *testing.T) {
	e := newSUEnv(t, facilityStepUpPackage(t), newSUHost(func(k *suKnobs) { k.oddStatus = true }), true)
	out := e.run(integrationaudit.SectionDegradation)
	if found, passed := probePassed(out, "a refused commit of a step-up action leaves no record"); !found || passed {
		t.Fatalf("the record probe: found %v passed %v, want it failed:\n%s", found, passed, dump(out))
	}
	found := false
	for _, f := range out.Findings {
		found = found || (f.CheckID == "degradation.no_record_not_marked" && f.Capability == suCap && strings.Contains(f.Message, "step-up"))
	}
	if !found {
		t.Errorf("the failed record probe has no finding:\n%s", dump(out))
	}
}

func TestExpiredProbeAcceptsStepUpRequired(t *testing.T) {
	stepUp, explicit, none := act.ConfirmationRequirementModeStepUp, act.ConfirmationRequirementModeExplicit, act.ConfirmationRequirementModeNone
	for _, tc := range []struct {
		name string
		mode act.ConfirmationRequirementMode
		err  error
		want bool
	}{
		{"expired for a step-up action", stepUp, contracts.New(contracts.ErrExpired, "expired"), true},
		{"step_up_required for a step-up action", stepUp, contracts.New(contracts.ErrStepUpRequired, "step-up"), true},
		{"step_up_required for an action that needs none", explicit, contracts.New(contracts.ErrStepUpRequired, "step-up"), false},
		{"expired for an explicit action", explicit, contracts.New(contracts.ErrExpired, "expired"), true},
		{"expired for an action that needs no confirmation", none, contracts.New(contracts.ErrExpired, "expired"), true},
		{"another refusal of a step-up action", stepUp, contracts.New(contracts.ErrConflict, "conflict"), false},
		{"no refusal at all", stepUp, nil, false},
		{"a wrapped step_up_required", stepUp, fmt.Errorf("commit: %w", contracts.New(contracts.ErrStepUpRequired, "step-up")), true},
		{"an untyped error", stepUp, errors.New("boom"), false},
	} {
		if got := coreExpiryRefusal(tc.mode, tc.err); got != tc.want {
			t.Errorf("%s: coreExpiryRefusal = %v, want %v", tc.name, got, tc.want)
		}
	}

	// and through the real coordinator: whichever of the two it answers for an expired
	// step-up preparation, the check accepts it and finds nothing sent to the application
	h := newSUHost()
	e := newSUEnv(t, facilityStepUpPackage(t), h, true)
	out := e.run(integrationaudit.SectionSafeExecution)
	if hasFinding(out, "safety.core_let_commit_through") {
		t.Fatalf("the expired-preparation probe rejects the coordinator's refusal:\n%s", dump(out))
	}
	found, passed := probePassed(out, "the coordinator refuses an expired preparation")
	if !found || !passed {
		t.Fatalf("the coordinator's refusal of an expired step-up preparation was not recorded as passed (found %v):\n%s", found, dump(out))
	}
	if !covered(out, suCap) {
		t.Fatalf("%s was not probed:\n%s", suCap, dump(out))
	}
	t.Logf("the coordinator answers %q for a commit of an expired step-up preparation", coordinatorExpiredCode(t))
}

// coordinatorExpiredCode asks the platform's own coordinator what it answers for the
// commit of an expired step-up preparation.
func coordinatorExpiredCode(t *testing.T) contracts.ErrCode {
	t.Helper()
	h := newSUHost()
	e := newSUEnv(t, facilityStepUpPackage(t), h, true)
	ctx := context.Background()
	opts := Options{ScratchDir: t.TempDir()}
	g, err := newRig(ctx, opts, e.pkg, e.in.Adapter, h.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer g.close()
	suite := e.pkg.Tests[0]
	base := contracts.Scope{DeploymentID: "alixai-audit", ApplicationID: suAppID}
	w, _, err := provision(ctx, h, suite, base, e.pkg.SortedCapabilityIDs(), clock.NewFake(simT0))
	if err != nil {
		t.Fatal(err)
	}
	defer w.cleanup(ctx)
	scope, p, _ := w.scopeOf("sup-a1")
	c := e.pkg.Capabilities[suCap]
	fields, err := w.fieldsFor(c, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	pr, err := g.prepare(ctx, scope, c, fields)
	if err != nil {
		t.Fatal(err)
	}
	if !isStepUp(pr.action) {
		t.Fatalf("the coordinator prepared %s with mode %s", suCap, pr.action.Confirmation.Mode)
	}
	g.advance(time.Hour)
	_, err = g.commit(ctx, scope, pr)
	if !coreExpiryRefusal(pr.action.Confirmation.Mode, err) {
		t.Fatalf("the coordinator's answer %v is not accepted", err)
	}
	return contracts.CodeOf(err)
}

func TestFacilityMaintenanceAuditsWithoutCriticalStepUpFindings(t *testing.T) {
	pkg := facilityStepUpPackage(t)
	h := newSUHost()
	e := newSUEnv(t, pkg, h, true)
	for _, s := range suSections {
		out := e.run(s)
		for _, f := range out.Findings {
			if f.Severity == contracts.SeverityCritical {
				t.Errorf("check %d: critical finding %s for %q: %s", s, f.CheckID, f.Capability, f.Message)
			}
		}
		if !covered(out, suCap) {
			t.Errorf("check %d did not probe %s:\n%s", s, suCap, dump(out))
		}
	}
	// the seven kinds of safe-execution probe, each of the package's declared outcomes
	safety := e.run(integrationaudit.SectionSafeExecution)
	for _, p := range safety.Probes {
		if !p.Passed {
			t.Errorf("probe %s %q failed: want %s, got %s", p.Kind, p.Subject, p.Expected, p.Observed)
		}
	}
	for _, subject := range []string{
		"a commit without the step-up applies nothing",
		"the effect is there after the application's own step-up",
		"the coordinator refuses an expired preparation",
	} {
		if found, passed := probePassed(safety, subject); !found || !passed {
			t.Errorf("probe %q: found %v passed %v", subject, found, passed)
		}
	}
	for _, f := range safety.Findings {
		if f.Capability == suCap {
			t.Errorf("finding %s (%s) for %s: %s", f.CheckID, f.Severity, suCap, f.Message)
		}
	}
	if got := h.effectCount(); got != h.completions {
		t.Errorf("the application applied %d effects for %d completed step-ups", got, h.completions)
	}
	// the identifiers of two audit worlds differ: the application numbers them for the whole deployment
	if h.nextAsset < 2*len(pkg.Tests[0].Fixtures.Records) {
		t.Errorf("only %d records were seeded: the audit did not use several worlds", h.nextAsset)
	}
}

func dedup(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
