package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"unicode"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Binding is the application, tenant and workspace a decision is about: the
// session a tool is being exposed in, or the prepared action being prepared or
// committed. It must be supplied from stored state, not copied from the Scope,
// so that a token for another tenant or workspace presented against a session
// or a prepared action is caught here (master plan 10.2: a session cannot
// change tenant or workspace without a new authenticated context). All three
// are required; there is no default.
type Binding struct {
	ApplicationID string
	TenantID      string
	WorkspaceID   string
}

// BindingOf returns the binding of a Scope. Use it only when the thing the
// decision is about carries no stored binding of its own.
func BindingOf(s contracts.Scope) Binding {
	return Binding{ApplicationID: s.ApplicationID, TenantID: s.TenantID, WorkspaceID: s.WorkspaceID}
}

// Evidence is what the caller has obtained for a commit. The gateway never
// obtains it; it only refuses to commit without it. Both default to false, so a
// caller that forgets to pass it is denied, never allowed.
type Evidence struct {
	// Confirmed: the user explicitly confirmed this exact prepared action.
	Confirmed bool
	// StepUp: the host application's own authenticated UI (or stronger step-up)
	// completed the action. The AI path cannot set it.
	StepUp bool
}

// Request is one question put to the gateway.
type Request struct {
	// Scope is the verified context. It must be valid; the gateway never fills
	// a missing level in.
	Scope contracts.Scope
	// Package is the application package decisions are made against. Anything
	// but an active package exposes nothing.
	Package ActivePackage
	// CapabilityID is the capability asked about (Decide).
	CapabilityID string
	// Binding is the application, tenant and workspace the decision is bound
	// to; see Binding.
	Binding Binding
	// Phase says where in the turn this decision falls.
	Phase Phase
	// Fields are the proposed, validated field values (prepare and commit).
	// They are read by host rules with field conditions and are never copied
	// into the record.
	Fields map[string]any
	// TaskFields are the fields the current task needs. Sensitive fields stay
	// withheld from the model unless the projection names them or they are
	// listed here.
	TaskFields []string
	// Evidence is the confirmation and step-up obtained so far (commit).
	Evidence Evidence
}

// ExposeRequest asks which capabilities a model may be shown.
type ExposeRequest struct {
	Scope      contracts.Scope
	Package    ActivePackage
	Binding    Binding
	TaskFields []string
}

// Decision is the gateway's answer. The zero Decision is a denial.
type Decision struct {
	// Allowed is true only when every check passed. Callers must test it, or
	// call Err, before doing anything with the capability.
	Allowed bool
	// Code is the typed error code of a denial; empty when Allowed.
	Code contracts.ErrCode
	// Reasons lists the checks that decided the outcome: the failing check of a
	// denial, or the grants and rules that shaped an allow.
	Reasons []Check
	// EffectiveRisk is the level the capability is treated as having: the
	// highest of the declared level, the platform floor and every host raise.
	// On a denial that stopped before the level was known it is
	// contracts.RiskPrivileged, never the zero value. At exposure it is an
	// upper bound; the prepare and commit decisions are authoritative.
	EffectiveRisk contracts.RiskLevel
	// Requires is what the level obliges the caller to obtain before a commit.
	Requires Requirements
	// VisibleFields are the only fields the model may see or propose, in the
	// capability's declaration order. Empty on a denial.
	VisibleFields []string
	// Record is the audit record of this decision.
	Record Record
}

// Err returns nil for an allowed decision and a typed *contracts.Error for any
// other, carrying the decision ID so the denial can be found in the audit log.
func (d Decision) Err() error {
	if d.Allowed {
		return nil
	}
	code := d.Code
	if !code.Valid() {
		code = contracts.ErrPermissionDenied
	}
	reason := "denied"
	if n := len(d.Reasons); n > 0 {
		reason = d.Reasons[n-1].Reason
	}
	e := contracts.New(code, "policy denied the capability: "+reason).
		WithDetails("reason", reason)
	if d.Record.DecisionID != "" {
		e = e.WithDetails("decision_id", d.Record.DecisionID)
	}
	return e
}

// FieldVisible reports whether the model may see or propose a field. It is
// false for every field of a denied decision.
func (d Decision) FieldVisible(name string) bool {
	if !d.Allowed {
		return false
	}
	for _, f := range d.VisibleFields {
		if f == name {
			return true
		}
	}
	return false
}

// Project returns the part of values the model may see, and the sorted names of
// the fields it removed. It is how a record read from the application, or a set
// of proposed values, is reduced to the fields the current task needs before it
// reaches a prompt (master plan 3 rule 6). It never returns anything for a
// denied decision.
func (d Decision) Project(values map[string]any) (kept map[string]any, dropped []string) {
	kept = make(map[string]any, len(d.VisibleFields))
	for _, name := range sortedKeys(values) {
		if d.FieldVisible(name) {
			kept[name] = values[name]
		} else {
			dropped = append(dropped, name)
		}
	}
	return kept, dropped
}

// Exposure is the set of capabilities a model may be shown.
type Exposure struct {
	// Visible holds the allowed decisions, one per capability, sorted by
	// capability ID. It is the only list the prompt builder may show.
	Visible []Decision
	// Considered is how many capabilities were evaluated. The denials are in
	// the audit log; they are deliberately not returned, so that nothing but
	// Visible can reach a prompt.
	Considered int
	// Code is set when the whole exposure was refused: the package-level
	// denial (a quarantined package, a wrong application or tenant) or an
	// audit record that could not be written (unavailable). Visible is empty
	// whenever Code is set.
	Code contracts.ErrCode
}

// Sink receives the record of every decision. Decide and the package-level
// denial of Expose hand it a decision record (RecordFormat); the rest of Expose
// hands it exposure records (ExposureFormat), one per call unless the catalogue
// is too large for one (see MaxExposureRecordBytes). A sink that stores records
// as they come, such as audit.RecordSink, needs no change to carry both.
type Sink interface {
	Emit(ctx context.Context, r Record) error
}

// SinkFunc adapts a function to a Sink.
type SinkFunc func(ctx context.Context, r Record) error

// Emit calls f.
func (f SinkFunc) Emit(ctx context.Context, r Record) error { return f(ctx, r) }

// Option configures a Gateway.
type Option func(*Gateway)

// WithSink installs the sink that receives every decision record, allow or
// deny. If the sink fails, the decision is turned into a denial with code
// unavailable: a decision that cannot be audited is not made.
func WithSink(s Sink) Option { return func(g *Gateway) { g.sink = s } }

// Gateway makes policy decisions. It holds no state beyond its clock, ID
// generator and sink, and is safe for concurrent use.
type Gateway struct {
	clk  clock.Clock
	ids  *ids.Generator
	sink Sink
}

// NewGateway returns a Gateway that stamps records with clk (nil means the
// real clock).
func NewGateway(clk clock.Clock, opts ...Option) *Gateway {
	if clk == nil {
		clk = clock.Real{}
	}
	g := &Gateway{clk: clk, ids: ids.NewGenerator(clk)}
	for _, o := range opts {
		o(g)
	}
	return g
}

// Decide decides whether the verified context may use one capability in the
// given phase. It never returns an error: a failure of any kind is a denial
// with a typed code and a record. See the package comment for the order of
// checks.
func (g *Gateway) Decide(ctx context.Context, req Request) Decision {
	ev := g.evaluate(ctx, req, false)
	return g.finish(ctx, ev)
}

// Expose decides, for every capability the package declares, whether the model
// may be shown it, and returns the ones it may. Package-level failures (a
// quarantined package, a wrong application or tenant) are decided once and
// expose nothing.
//
// Every capability's decision, allow or deny, is recorded, in one exposure
// record for the call (ExposureFormat): the capability, its outcome, the reason
// code of a denial and the effective risk level. The decisions Expose returns
// are the full ones, as Decide would make them; what the record leaves out of
// them (checks, field lists, rules) follows from the package, the policy
// digests it names and the subject, and is recorded in full whenever the
// capability is used (Decide at prepare and commit). An exposure whose record
// cannot be written exposes nothing: a decision that cannot be recorded is not
// made. The record's DecisionID is the DecisionID of every decision it holds.
func (g *Gateway) Expose(ctx context.Context, req ExposeRequest) Exposure {
	base := Request{
		Scope: req.Scope, Package: req.Package, Binding: req.Binding,
		Phase: PhaseExpose, TaskFields: req.TaskFields,
	}
	pre := g.evaluate(ctx, base, true)
	if !pre.allowed {
		d := g.finish(ctx, pre)
		return Exposure{Code: d.Code}
	}
	capIDs := declaredCapabilityIDs(req.Package)
	ds := make([]Decision, 0, len(capIDs))
	for _, id := range capIDs {
		r := base
		r.CapabilityID = id
		ds = append(ds, g.evaluate(ctx, r, false).decision())
	}
	sort.SliceStable(ds, func(i, j int) bool { return ds[i].Record.CapabilityID < ds[j].Record.CapabilityID })
	out := Exposure{Considered: len(ds)}
	if g.sink != nil {
		if err := g.recordExposure(ctx, pre, ds); err != nil {
			out.Code = contracts.ErrUnavailable
			return out
		}
	}
	for _, d := range ds {
		if d.Allowed {
			out.Visible = append(out.Visible, d)
		}
	}
	return out
}

// recordExposure writes the exposure record or records of one Expose call and
// stamps each decision with the ID of the record that holds it. It stops at the
// first record the sink refuses; the ones already written stay, and say by Part
// and Parts that the call is incomplete (it exposed nothing, whatever they say).
func (g *Gateway) recordExposure(ctx context.Context, pre *evaluation, ds []Decision) error {
	recs, err := g.exposureRecords(pre.rec, ds)
	if err != nil {
		return err
	}
	for _, r := range recs {
		if err := g.emit(ctx, r.rec); err != nil {
			return err
		}
	}
	for _, r := range recs {
		for i := r.from; i < r.to; i++ {
			ds[i].Record.DecisionID = r.rec.DecisionID
		}
	}
	return nil
}

// exposureRec is one exposure record and the decisions ds[from:to] it holds.
type exposureRec struct {
	rec      Record
	from, to int
}

// Upper bound of the encoded size of one outcome entry beyond its variable
// strings: the keys, punctuation and the longest outcome and risk values.
const exposureEntryOverhead = 96

// exposureRecords builds the exposure records of the decisions ds (sorted by
// capability ID) of an Expose call whose package-level checks produced pre. It
// splits ds so that no record holds more than MaxExposureOutcomes entries or
// encodes to more than MaxExposureRecordBytes; an empty ds is one record with no
// entries (the package declares nothing, which is a recorded exposure of
// nothing). It fails when even one entry cannot fit, which only a record with an
// absurdly large subject could cause.
func (g *Gateway) exposureRecords(pre Record, ds []Decision) ([]exposureRec, error) {
	head := pre
	head.Format = ExposureFormat
	head.Phase = PhaseExpose
	head.CapabilityID = ""
	head.Exposure = &ExposureBatch{}
	fixed, err := json.Marshal(head)
	if err != nil {
		return nil, err
	}
	outcomes := make([]CapabilityOutcome, len(ds))
	sizes := make([]int, len(ds))
	for i, d := range ds {
		outcomes[i] = exposureOutcome(d)
		sizes[i] = len(outcomes[i].CapabilityID) + len(outcomes[i].Code) + len(outcomes[i].Reason) + exposureEntryOverhead
	}

	var spans [][2]int
	for from := 0; ; {
		to, used := from, len(fixed)+exposureWireOverhead
		for to < len(ds) && to-from < MaxExposureOutcomes && used+sizes[to] <= MaxExposureRecordBytes {
			used += sizes[to]
			to++
		}
		if to == from && from < len(ds) {
			return nil, contracts.New(contracts.ErrInternal, "policy: an exposure record cannot hold even one outcome")
		}
		spans = append(spans, [2]int{from, to})
		if from = to; from >= len(ds) {
			break
		}
	}

	now := g.clk.Now().UTC()
	recs := make([]exposureRec, len(spans))
	for k, sp := range spans {
		rec := pre // the subject, package and policies of the package-level evaluation
		rec.Format = ExposureFormat
		rec.DecisionID = g.ids.New(RecordIDPrefix)
		rec.DecidedAt = now
		rec.Phase = PhaseExpose
		rec.CapabilityID = ""
		rec.Outcome, rec.Code = OutcomeDeny, contracts.ErrPermissionDenied
		for _, o := range outcomes[sp[0]:sp[1]] {
			if o.Outcome == OutcomeAllow {
				rec.Outcome, rec.Code = OutcomeAllow, ""
				break
			}
		}
		rec.Exposure = &ExposureBatch{
			Considered: len(ds), Part: k + 1, Parts: len(spans),
			Outcomes: outcomes[sp[0]:sp[1]:sp[1]],
		}
		recs[k] = exposureRec{rec: rec, from: sp[0], to: sp[1]}
	}
	return recs, nil
}

// exposureWireOverhead covers the members of an exposure record that vary in
// size and are not in the measured head: the counters, the outcomes array and
// the decision ID and timestamp of the later records.
const exposureWireOverhead = 256

// exposureOutcome is the compact outcome of one decision.
func exposureOutcome(d Decision) CapabilityOutcome {
	o := CapabilityOutcome{CapabilityID: d.Record.CapabilityID, Outcome: OutcomeDeny, Risk: d.EffectiveRisk}
	if d.Allowed {
		o.Outcome = OutcomeAllow
		return o
	}
	o.Code = d.Code
	if !o.Code.Valid() {
		o.Code = contracts.ErrPermissionDenied
	}
	o.Reason = "denied"
	if n := len(d.Reasons); n > 0 {
		o.Reason = d.Reasons[n-1].Reason
	}
	return o
}

// declaredCapabilityIDs lists a package's capabilities, treating a package
// that panics as one that declares none.
func declaredCapabilityIDs(pkg ActivePackage) (ids []string) {
	defer func() {
		if recover() != nil {
			ids = nil
		}
	}()
	return pkg.CapabilityIDs()
}

// ---- evaluation ----

type evaluation struct {
	g   *Gateway
	req Request
	rec Record

	allowed bool
	code    contracts.ErrCode
	risk    contracts.RiskLevel
	visible []string
	notable []Check
}

func (e *evaluation) pass(name CheckName, reason, ruleID string) {
	c := Check{Name: name, Passed: true, Reason: reason, RuleID: ruleID}
	e.rec.Checks = append(e.rec.Checks, c)
	if ruleID != "" {
		e.notable = append(e.notable, c)
	}
}

// deny records the failing check and ends the evaluation as a denial.
func (e *evaluation) deny(name CheckName, code contracts.ErrCode, reason, ruleID string) {
	c := Check{Name: name, Passed: false, Code: code, Reason: reason, RuleID: ruleID}
	e.rec.Checks = append(e.rec.Checks, c)
	e.notable = append(e.notable, c)
	e.allowed = false
	e.code = code
}

func (g *Gateway) newRecord(req Request) Record {
	rec := Record{
		Format:         RecordFormat,
		DecisionID:     g.ids.New(RecordIDPrefix),
		DecidedAt:      g.clk.Now().UTC(),
		Phase:          req.Phase,
		Outcome:        OutcomeDeny,
		CapabilityID:   recordCapability(req.CapabilityID),
		Policies:       []PolicyRef{},
		Checks:         []Check{},
		Risk:           unknownRisk(),
		Requires:       Requirements{Confirmation: true, StepUp: true},
		VisibleFields:  []string{},
		WithheldFields: []WithheldField{},
		RulesApplied:   []RuleApplied{},
		RulesDeferred:  []string{},
		Subject: Subject{
			ApplicationID:    recordIdentifier(req.Scope.ApplicationID),
			TenantID:         recordIdentifier(req.Scope.TenantID),
			WorkspaceID:      recordIdentifier(req.Scope.WorkspaceID),
			PrincipalID:      recordIdentifier(req.Scope.PrincipalID),
			SessionID:        recordIdentifier(req.Scope.SessionID),
			Roles:            recordIdentifiers(req.Scope.Roles),
			CapabilityScopes: recordIdentifiers(req.Scope.CapabilityScopes),
		},
	}
	return rec
}

// unknownRisk is the risk of a decision that stopped before the capability was
// known: the highest level, so that nothing downstream can read it as low.
func unknownRisk() RiskRecord {
	return RiskRecord{
		Declared:      contracts.RiskPrivileged,
		PlatformFloor: contracts.RiskPrivileged,
		HostMinimum:   contracts.RiskRead,
		Effective:     contracts.RiskPrivileged,
		EffectClass:   "unknown",
		RaisedBy:      []RiskRaise{},
	}
}

// The record is written before the request is trusted, and some of what is in
// it came from a model (the capability ID) or from a token that failed
// validation. Nothing that is not a well-formed identifier is copied into the
// record: a level that fails the grammar is recorded as empty and the failing
// check says why. Free text from a user therefore cannot reach the audit log
// through a policy decision.
func recordIdentifier(s string) string {
	if validIdentifier(s) {
		return s
	}
	return ""
}

func recordCapability(s string) string {
	if validCapabilityID(s) {
		return s
	}
	return ""
}

func recordIdentifiers(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if validIdentifier(s) {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// recordText bounds a string taken from a package (version, status): at most
// 64 characters and no control characters, else empty.
func recordText(s string) string {
	if utf8.RuneCountInString(s) > 64 || !utf8.ValidString(s) {
		return ""
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return ""
		}
	}
	return s
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

// evaluate runs the checks. With preludeOnly it stops after the package-level
// checks and needs no capability.
func (g *Gateway) evaluate(ctx context.Context, req Request, preludeOnly bool) (ev *evaluation) {
	ev = &evaluation{g: g, req: req, rec: g.newRecord(req), risk: contracts.RiskPrivileged, code: contracts.ErrPermissionDenied}
	defer func() {
		// A panicking package implementation must not become an allow or take
		// the process down.
		if r := recover(); r != nil {
			ev.reset()
			ev.deny(CheckRequest, contracts.ErrInternal, "internal_fault", "")
		}
	}()
	ev.run(ctx, preludeOnly)
	return ev
}

// reset drops everything derived so far after an unexpected fault.
func (e *evaluation) reset() {
	e.allowed = false
	e.visible = nil
	e.risk = contracts.RiskPrivileged
	e.rec.Risk = unknownRisk()
	e.rec.VisibleFields = []string{}
	e.rec.WithheldFields = []WithheldField{}
	e.rec.RulesApplied = []RuleApplied{}
	e.rec.RulesDeferred = []string{}
	e.rec.Requires = Requirements{Confirmation: true, StepUp: true}
}

func (e *evaluation) run(ctx context.Context, preludeOnly bool) {
	req := e.req

	// 1. request
	switch {
	case ctx == nil:
		e.deny(CheckRequest, contracts.ErrInternal, "context_missing", "")
		return
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		e.deny(CheckRequest, contracts.ErrTimeout, "context_deadline_exceeded", "")
		return
	case ctx.Err() != nil:
		e.deny(CheckRequest, contracts.ErrUnavailable, "context_canceled", "")
		return
	case !req.Phase.Valid():
		e.deny(CheckRequest, contracts.ErrValidationFailed, "phase_invalid", "")
		return
	case !preludeOnly && req.CapabilityID == "":
		e.deny(CheckRequest, contracts.ErrUnknownCapability, "capability_id_missing", "")
		return
	}
	e.pass(CheckRequest, "well_formed", "")

	// 2. scope
	if err := req.Scope.Validate(); err != nil {
		code := contracts.CodeOf(err)
		reason := "scope_level_malformed"
		if code == contracts.ErrScopeMissing {
			reason = "scope_level_missing"
		} else {
			code = contracts.ErrScopeViolation
		}
		e.deny(CheckScope, code, reason, "")
		return
	}
	e.pass(CheckScope, "scope_complete", "")

	// 3. package
	pkg := req.Package
	if pkg == nil {
		e.deny(CheckPackage, contracts.ErrQuarantined, "no_active_package", "")
		return
	}
	status := pkg.Status()
	e.rec.Package = PackageRef{
		ApplicationID: recordIdentifier(pkg.ApplicationID()),
		Version:       recordText(pkg.PackageVersion()),
		Status:        recordText(string(status)),
	}
	if status != StatusActive {
		e.deny(CheckPackage, contracts.ErrQuarantined, "package_not_active", "")
		return
	}
	pol := pkg.Policy()
	if refs := pol.Refs(); refs != nil {
		e.rec.Policies = refs
	}
	e.pass(CheckPackage, "package_active", "")

	// 4. application
	switch {
	case req.Binding.ApplicationID == "":
		e.deny(CheckApplication, contracts.ErrScopeMissing, "binding_missing", "")
		return
	case req.Scope.ApplicationID != pkg.ApplicationID() || req.Binding.ApplicationID != req.Scope.ApplicationID:
		e.deny(CheckApplication, contracts.ErrScopeViolation, "application_mismatch", "")
		return
	}
	e.pass(CheckApplication, "application_matches", "")

	// 5. tenant and workspace
	switch {
	case req.Binding.TenantID == "" || req.Binding.WorkspaceID == "":
		e.deny(CheckTenant, contracts.ErrScopeMissing, "binding_missing", "")
		return
	case req.Binding.TenantID != req.Scope.TenantID:
		e.deny(CheckTenant, contracts.ErrScopeViolation, "tenant_mismatch", "")
		return
	case req.Binding.WorkspaceID != req.Scope.WorkspaceID:
		e.deny(CheckTenant, contracts.ErrScopeViolation, "workspace_mismatch", "")
		return
	}
	e.pass(CheckTenant, "tenant_and_workspace_match", "")
	if preludeOnly {
		e.allowed = true
		return
	}

	// 6. declared
	decl, ok := pkg.Capability(req.CapabilityID)
	if !ok {
		e.deny(CheckDeclared, contracts.ErrUnknownCapability, "capability_not_declared", "")
		return
	}
	if decl.ID != req.CapabilityID || decl.validate() != nil {
		e.deny(CheckDeclared, contracts.ErrSchemaInvalid, "capability_declaration_invalid", "")
		return
	}
	e.pass(CheckDeclared, "capability_declared", "")

	// 7. audience
	if decl.Audience != AudienceUser {
		e.deny(CheckAudience, contracts.ErrPermissionDenied, "admin_capability_not_model_visible", "")
		return
	}
	e.pass(CheckAudience, "audience_user", "")

	// 8. token scope
	if !req.Scope.Permits(decl.ID) {
		e.deny(CheckToken, contracts.ErrScopeViolation, "capability_not_in_token_scope", "")
		return
	}
	e.pass(CheckToken, "capability_in_token_scope", "")

	// 9. roles
	granting, grantPolicy, reason := grantingRoles(req.Scope.Roles, pkg.Roles(), pol.grantedRoles(decl.ID), pol != nil)
	if len(granting) == 0 {
		e.deny(CheckRole, contracts.ErrPermissionDenied, reason, "")
		return
	}
	e.pass(CheckRole, "role_granted", grantPolicy)

	// 10. host rules and risk
	applied, deferred := e.applicableRules(pol, decl)
	e.rec.RulesDeferred = deferred
	e.rec.RulesApplied = summariseRules(applied)
	e.rec.Risk = computeRisk(decl, pol, applied)
	e.risk = e.rec.Risk.Effective
	e.rec.Requires = requirements(e.risk, decl)
	for _, r := range applied {
		if r.rule.Then.Deny {
			e.deny(CheckRules, contracts.ErrPermissionDenied, "host_rule_denied", r.rule.ID)
			return
		}
	}
	e.pass(CheckRules, "no_rule_denied", "")

	// 11. fields
	visible, withheld, hiddenRequired, ruleID := visibleFields(decl, pol, granting, applied, req.TaskFields)
	if hiddenRequired {
		e.deny(CheckFields, contracts.ErrPermissionDenied, "required_field_not_visible", ruleID)
		return
	}
	e.pass(CheckFields, "required_fields_visible", "")
	e.visible = visible
	e.rec.VisibleFields = append([]string{}, visible...)
	e.rec.WithheldFields = withheld

	// 12. evidence
	if req.Phase == PhaseCommit {
		switch {
		case e.rec.Requires.StepUp && !req.Evidence.StepUp:
			e.deny(CheckEvidence, contracts.ErrStepUpRequired, "step_up_not_obtained", "")
			e.dropVisible()
			return
		case e.rec.Requires.Confirmation && !req.Evidence.Confirmed:
			e.deny(CheckEvidence, contracts.ErrConfirmationRequired, "confirmation_not_obtained", "")
			e.dropVisible()
			return
		}
		e.pass(CheckEvidence, "evidence_sufficient", "")
	}

	e.allowed = true
	e.code = ""
}

// dropVisible empties the field lists of a decision that was denied after they
// were computed.
func (e *evaluation) dropVisible() {
	e.visible = nil
	e.rec.VisibleFields = []string{}
	e.rec.WithheldFields = []WithheldField{}
}

// grantingRoles returns the principal's roles that the package declares and
// that role_grants grant the capability, sorted, with the policy that grants
// the first of them; otherwise the reason nothing is granted.
func grantingRoles(roles, packageRoles []string, grants map[string]string, hasPolicy bool) (granting []string, policyID, reason string) {
	if !hasPolicy {
		return nil, "", "no_host_policy"
	}
	declared := toSet(packageRoles)
	anyDeclared := false
	for _, r := range sortedCopy(roles) {
		if !declared[r] {
			continue
		}
		anyDeclared = true
		if pid, ok := grants[r]; ok {
			granting = append(granting, r)
			if policyID == "" {
				policyID = pid
			}
		}
	}
	if len(granting) > 0 {
		return granting, policyID, ""
	}
	if !anyDeclared {
		return nil, "", "no_declared_role"
	}
	return nil, "", "role_not_granted"
}

// applicableRules returns the rules of the capability whose condition holds for
// this request, in policy order, and the IDs of the rules whose field
// conditions could not be decided at exposure.
//
// At exposure no field values exist. A rule with field conditions that denies
// is deferred (it is evaluated, with the real values, at prepare and commit); a
// rule with field conditions that raises the level or hides fields is applied
// as if its conditions held, so that the model is never shown a tool labelled
// lower, or with more fields, than prepare will enforce.
func (e *evaluation) applicableRules(pol *Policy, decl CapabilityDecl) (applied []*compiledRule, deferred []string) {
	deferred = []string{}
	rules := pol.rulesFor(decl.ID)
	if len(rules) == 0 {
		return nil, deferred
	}
	roles := toSet(e.req.Scope.Roles)
	scopes := toSet(e.req.Scope.CapabilityScopes)
	for _, r := range rules {
		c := r.rule.When
		if !identityMatches(c, roles, scopes, e.req.Scope.TenantID, e.req.Scope.WorkspaceID) {
			continue
		}
		if len(c.Fields) > 0 {
			if e.req.Phase == PhaseExpose {
				deferred = append(deferred, r.rule.ID)
				if r.rule.Then.Deny {
					continue
				}
			} else if !fieldsMatch(c.Fields, e.req.Fields) {
				continue
			}
		}
		applied = append(applied, r)
	}
	return applied, deferred
}

func summariseRules(applied []*compiledRule) []RuleApplied {
	out := make([]RuleApplied, 0, len(applied))
	for _, r := range applied {
		var effects []string
		if r.rule.Then.Deny {
			effects = append(effects, "deny")
		}
		if r.rule.Then.RiskMin != nil {
			effects = append(effects, "risk_min")
		}
		if len(r.rule.Then.HideFields) > 0 {
			effects = append(effects, "hide_fields")
		}
		out = append(out, RuleApplied{RuleID: r.rule.ID, Effects: effects})
	}
	return out
}

// computeRisk is the one place a risk level is calculated. Everything goes
// through contracts.RiskLevel.Escalate, which only raises and resolves an
// invalid level to the highest one, so no input can produce a level below the
// platform floor.
func computeRisk(decl CapabilityDecl, pol *Policy, applied []*compiledRule) RiskRecord {
	floor, known := decl.EffectClass.Floor()
	rr := RiskRecord{
		Declared:      decl.RiskLevel,
		PlatformFloor: floor,
		HostMinimum:   contracts.RiskRead,
		EffectClass:   string(decl.EffectClass),
		RaisedBy:      []RiskRaise{},
	}
	if !known {
		rr.EffectClass = "unknown"
	}
	level := decl.RiskLevel.Escalate(floor)
	raise := func(source, id string, lvl contracts.RiskLevel) {
		level = level.Escalate(lvl)
		if lvl > decl.RiskLevel {
			rr.RaisedBy = append(rr.RaisedBy, RiskRaise{Source: source, ID: id, Level: lvl})
		}
	}
	raise(RaiseFloor, "", floor)
	if o, ok := pol.overrideFor(decl.ID); ok {
		rr.HostMinimum = rr.HostMinimum.Escalate(o.level)
		raise(RaiseOverride, o.policyID, o.level)
	}
	for _, r := range applied {
		if r.rule.Then.RiskMin != nil {
			rr.HostMinimum = rr.HostMinimum.Escalate(*r.rule.Then.RiskMin)
			raise(RaiseRule, r.rule.ID, *r.rule.Then.RiskMin)
		}
	}
	rr.Effective = level
	rank := map[string]int{RaiseFloor: 0, RaiseOverride: 1, RaiseRule: 2}
	sort.SliceStable(rr.RaisedBy, func(i, j int) bool {
		a, b := rr.RaisedBy[i], rr.RaisedBy[j]
		if a.Level != b.Level {
			return a.Level > b.Level
		}
		if rank[a.Source] != rank[b.Source] {
			return rank[a.Source] < rank[b.Source]
		}
		return a.ID < b.ID
	})
	return rr
}

// requirements derives what a commit needs from the effective level and the
// capability's declared confirmation mode, whichever is stricter.
func requirements(level contracts.RiskLevel, decl CapabilityDecl) Requirements {
	return Requirements{
		Confirmation: level.RequiresConfirmation() || decl.Confirmation != ConfirmNone,
		StepUp:       level.RequiresStepUp() || decl.Confirmation == ConfirmStepUp,
	}
}

// visibleFields computes what the model may see of a capability.
//
// Start from the declared fields. The projection of every role that grants the
// capability narrows them; with several granting roles the result is the
// intersection of their projections (a role without a projection does not
// widen it), the least-capability choice. A projection never exposes a field
// the capability does not declare. Rules that apply hide further fields. A
// sensitive field stays withheld unless the projection names it or the current
// task needs it. hiddenRequired reports that a required field is withheld by a
// projection or a rule, which makes the capability unusable for the model.
func visibleFields(decl CapabilityDecl, pol *Policy, granting []string, applied []*compiledRule, taskFields []string) (visible []string, withheld []WithheldField, hiddenRequired bool, ruleID string) {
	var allowed map[string]bool // nil: no projection applies
	for _, role := range granting {
		pr := pol.projectionFor(decl.ID, role)
		if pr == nil {
			continue
		}
		if allowed == nil {
			allowed = make(map[string]bool, len(pr.fields))
			for f := range pr.fields {
				allowed[f] = true
			}
			continue
		}
		for f := range allowed {
			if !pr.fields[f] {
				delete(allowed, f)
			}
		}
	}
	hiddenBy := map[string]string{}
	for _, r := range applied {
		for _, f := range r.rule.Then.HideFields {
			if _, ok := hiddenBy[f]; !ok {
				hiddenBy[f] = r.rule.ID
			}
		}
	}
	needed := toSet(taskFields)
	visible = []string{}
	withheld = []WithheldField{}
	for _, f := range decl.Fields {
		switch {
		case allowed != nil && !allowed[f.Name]:
			withheld = append(withheld, WithheldField{Field: f.Name, Reason: WithheldNotProjected})
			if f.Required {
				hiddenRequired = true
			}
		case hiddenBy[f.Name] != "":
			withheld = append(withheld, WithheldField{Field: f.Name, Reason: WithheldRule, RuleID: hiddenBy[f.Name]})
			if f.Required {
				hiddenRequired = true
				if ruleID == "" {
					ruleID = hiddenBy[f.Name]
				}
			}
		case f.Sensitive && allowed == nil && !needed[f.Name]:
			withheld = append(withheld, WithheldField{Field: f.Name, Reason: WithheldSensitive})
		default:
			visible = append(visible, f.Name)
		}
	}
	return visible, withheld, hiddenRequired, ruleID
}

// decision turns the finished evaluation into the value returned to callers.
func (e *evaluation) decision() Decision {
	rec := e.rec
	d := Decision{
		Allowed:       e.allowed,
		EffectiveRisk: e.risk,
		Requires:      rec.Requires,
		Record:        rec,
	}
	if e.allowed {
		d.VisibleFields = append([]string{}, e.visible...)
		d.Record.Outcome = OutcomeAllow
		d.Record.Code = ""
		d.Code = ""
	} else {
		d.Record.Outcome = OutcomeDeny
		d.Record.Code = e.code
		d.Code = e.code
		d.Record.VisibleFields = []string{}
		d.Record.WithheldFields = []WithheldField{}
		d.VisibleFields = []string{}
	}
	for _, c := range e.rec.Checks {
		if !c.Passed || c.RuleID != "" {
			d.Reasons = append(d.Reasons, c)
		}
	}
	return d
}

// finish hands the record to the sink and returns the decision. A sink that
// fails turns the decision into a denial: a decision that cannot be recorded is
// not made.
func (g *Gateway) finish(ctx context.Context, ev *evaluation) Decision {
	d := ev.decision()
	if g.sink == nil {
		return d
	}
	if err := g.emit(ctx, d.Record); err != nil {
		ev.deny(CheckSink, contracts.ErrUnavailable, "audit_unavailable", "")
		ev.dropVisible()
		d = ev.decision()
	}
	return d
}

// emit calls the sink and converts a panic into an error.
func (g *Gateway) emit(ctx context.Context, r Record) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("audit sink panicked: %v", p)
		}
	}()
	return g.sink.Emit(ctx, r)
}
