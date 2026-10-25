package convstage

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/integrationaudit/probes"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// The conversation stage replays each example of each user capability as a scripted
// turn, through the same components `alixai serve` composes: the capability registry,
// the policy gateway, the action coordinator, the session store and the dialogue
// engine, over the transport adapter of the application under test (MCP, OpenAPI or native: the
// caller builds it, internal/cli's BuildPackageAdapter). The model is the scripted
// provider and the only thing it ever proposes is what the example says a correct
// reading of its utterance contains (master plan 9.2: examples are test data, never
// runtime policy). A model call nobody scripted fails the turn.
//
// # What an example must show
//
// A replay is held to four things, whatever the data the application holds:
//
//	targeted    the turn selected the example's capability
//	mentioned   the entity the example names first reached the application's resolver for
//	            that capability exactly as the model proposed it, and when the turn got as
//	            far as an answer, a confirmation or an outcome, every entity it names did
//	typed       the outcome is progress, or a failure whose code the capability declares
//	            in failure_states, and the response is in the example's language
//	safe        nothing was committed for a capability that demands an explicit
//	            confirmation or a step-up, and no receipt was produced
//
// A violation fails the example. Two things that are not violations are warnings on it,
// because the engine did nothing wrong and the package is what to look at:
//
//	unrouted    the deterministic router did not shortlist the capability for the utterance
//	            (the package's terminology and title words do not cover how its own example
//	            is worded), so the model was never asked and nothing was proposed or checked
//	            beyond safety
//	unreached   the names the example uses are not in the package's seed data, so the turn
//	            stopped before the act the capability is for (a confirmation request, an
//	            answer, a committed outcome)
//
// A capability none of whose examples reaches its act is not shown to be conversationally
// reachable, which is unverified: a replay that failed is its own item, and this one says
// that nothing demonstrates the capability.
//
// The disposable tenants the examples name come from the transport's provisioning bridge
// (probes.Provisioner), whatever the transport.
//
// The principal of a replay is a human principal of the package's test suite whose role
// the host policy grants the capability. Every such principal is tried in fixture order,
// since an example may be written for a patient ("me") or for the front desk; the safety
// checks must hold for all of them, and the example reaches its act if any does.

// scripted names of the engine acts the stage compares against.
const (
	actPresentResult        = "present_result"
	actRequestConfirmation  = "request_confirmation"
	actRequestStepUp        = "request_step_up"
	actReportOutcome        = "report_outcome"
	conversationDeployment  = "alixai-conformance"
	conversationMaxSessions = 4096
)

// recordedCall is one request the application received through the adapter.
type recordedCall struct {
	Op           adapters.Operation
	CapabilityID string
	Fields       map[string]any
	Status       string
	Code         string
}

// recorder sits between the scope-checking wrapper and the MCP adapter, so what it sees
// is exactly what the application is asked.
type recorder struct {
	adapters.AppAdapter
	mu    sync.Mutex
	calls []recordedCall
}

func (r *recorder) record(op adapters.Operation, capID string, fields map[string]any, status string, err error) {
	c := recordedCall{Op: op, CapabilityID: capID, Fields: fields, Status: status}
	if err != nil {
		c.Code = string(contracts.CodeOf(err))
	}
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.mu.Unlock()
}

func (r *recorder) Read(ctx context.Context, scope contracts.Scope, req adapters.ReadRequest) (act.DeterministicResult, error) {
	res, err := r.AppAdapter.Read(ctx, scope, req)
	r.record(adapters.OpRead, req.CapabilityID, req.Fields, string(res.Status), err)
	return res, err
}

func (r *recorder) Validate(ctx context.Context, scope contracts.Scope, req adapters.ValidateRequest) (act.DeterministicResult, error) {
	res, err := r.AppAdapter.Validate(ctx, scope, req)
	r.record(adapters.OpValidate, req.CapabilityID, req.Fields, string(res.Status), err)
	return res, err
}

func (r *recorder) Prepare(ctx context.Context, scope contracts.Scope, req adapters.PrepareRequest) (act.DeterministicResult, error) {
	res, err := r.AppAdapter.Prepare(ctx, scope, req)
	r.record(adapters.OpPrepare, req.CapabilityID, req.Fields, string(res.Status), err)
	return res, err
}

func (r *recorder) Commit(ctx context.Context, scope contracts.Scope, req adapters.CommitRequest) (act.DeterministicResult, error) {
	res, err := r.AppAdapter.Commit(ctx, scope, req)
	r.record(adapters.OpCommit, req.CapabilityID, nil, string(res.Status), err)
	return res, err
}

func (r *recorder) reset() {
	r.mu.Lock()
	r.calls = nil
	r.mu.Unlock()
}

func (r *recorder) snapshot() []recordedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedCall(nil), r.calls...)
}

// activePackage is the package as the registry, the policy gateway, the action
// coordinator and the dialogue engine see it once the audit has activated it. A package
// fresh from the loader is quarantined and only the audit can activate one; the audit is
// the previous stage, and this view lives only in the throwaway rig: nothing is activated.
type activePackage struct{ capabilities.Package }

func (activePackage) Status() policy.PackageStatus { return policy.StatusActive }

// rig is the service under test, composed in-process over a throwaway store.
type rig struct {
	db       *store.DB
	rec      *recorder
	pkg      capabilities.Package
	sessions *sessions.Store
	model    *pfake.Provider
	engine   *dialogue.Engine
	close    func()
}

func allActs() []conversation.DialogueDirectiveAct {
	return []conversation.DialogueDirectiveAct{
		conversation.DialogueDirectiveActAskMissing, conversation.DialogueDirectiveActAskInvalid,
		conversation.DialogueDirectiveActDisambiguate, conversation.DialogueDirectiveActRequestConfirmation,
		conversation.DialogueDirectiveActRequestStepUp, conversation.DialogueDirectiveActPresentResult,
		conversation.DialogueDirectiveActReportOutcome, conversation.DialogueDirectiveActReportFailure,
		conversation.DialogueDirectiveActRefuse, conversation.DialogueDirectiveActOfferHelp,
		conversation.DialogueDirectiveActAcknowledgeCancel, conversation.DialogueDirectiveActReportNoEvidence,
	}
}

// newRig composes the service over the transport adapter of the package under test. Everything
// it holds is released by rig.close; the adapter itself stays the caller's.
func newRig(ctx context.Context, s Subject) (*rig, error) {
	app := s.ApplicationID
	var closers []func()
	closeAll := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}

	rec := &recorder{AppAdapter: s.Adapter}
	guard, err := adapters.Wrap(rec, adapters.Options{ApplicationID: app})
	if err != nil {
		closeAll()
		return nil, err
	}

	clk := clock.Real{}
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(s.ScratchDir, "conversation.db")})
	if err != nil {
		closeAll()
		return nil, fmt.Errorf("open the throwaway store: %w", err)
	}
	closers = append(closers, func() { _ = db.Close() })
	alog, err := audit.New(db, audit.Options{Clock: clk})
	if err != nil {
		closeAll()
		return nil, err
	}
	sink := audit.NewRecordSink[policy.Record](alog, audit.EventPolicyDecision, func(r policy.Record) (audit.Locator, error) {
		s := r.Subject
		return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
	})
	gw := policy.NewGateway(clk, policy.WithSink(sink))

	pkg := activePackage{capabilities.FromApppkg(s.Package)}
	reg := capabilities.NewRegistry(nil, capabilities.WithClock(clk))
	if err := reg.Activate(pkg); err != nil {
		closeAll()
		return nil, fmt.Errorf("activate the package in the throwaway registry: %w", err)
	}
	adSource := actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return guard, nil })
	co, err := actions.New(actions.Deps{
		DB: db, Policy: gw, Audit: alog, Clock: clk,
		Packages: actions.PackageSourceFunc(func(context.Context, string) (policy.ActivePackage, error) { return pkg, nil }),
		Adapters: adSource,
	})
	if err != nil {
		closeAll()
		return nil, err
	}
	sessStore, err := sessions.New(db, clk, sessions.Config{})
	if err != nil {
		closeAll()
		return nil, err
	}
	model := pfake.New()
	sel, err := dialogue.LocalSelector(model, providers.Budget{MaxPromptTokens: 4096, MaxOutputTokens: 1024, MaxBatchItems: 4, Timeout: time.Minute})
	if err != nil {
		closeAll()
		return nil, err
	}
	eng, err := dialogue.New(dialogue.Deps{
		Sessions: sessStore, Registry: reg,
		Packages: dialogue.PackageSourceFunc(func(context.Context, string) (capabilities.Package, error) { return pkg, nil }),
		Policy:   gw, Actions: co, Adapters: adSource, Providers: sel, Audit: alog, Clock: clk,
		Config:   dialogue.Config{TemplateOnly: allActs()},
		Decision: dialogue.NoopDecisionStage{}, Guard: dialogue.LocalOnlyGuard{},
	})
	if err != nil {
		closeAll()
		return nil, err
	}
	return &rig{db: db, rec: rec, pkg: pkg, sessions: sessStore, model: model, engine: eng, close: closeAll}, nil
}

// proposalJSON is a model output for a TaskProposal: the capability, the intent and the
// field values the model read out of the words.
func proposalJSON(capability string, fields map[string]any) string {
	if fields == nil {
		fields = map[string]any{}
	}
	b, err := json.Marshal(map[string]any{
		"capability_id": capability, "fields": fields, "entity_refs": []any{}, "intent": "task",
		"uncertainty": map[string]any{"overall": 0.9},
	})
	if err != nil {
		panic(err) // plain maps and scalars: cannot fail
	}
	return string(b)
}

// exampleRef is one declared example.
type exampleRef struct {
	capability string
	example    ap.CapabilityExample
}

// userCapabilities lists the user-audience capability ids, sorted: the ones a
// conversation can reach.
func userCapabilities(pkg *apppkg.Package) []string {
	var ids []string
	for id, c := range pkg.Capabilities {
		if c.Audience == ap.CapabilityAudienceUser {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// Declared is how many examples the package's user capabilities declare: what Replay must replay.
func Declared(pkg *apppkg.Package) int { return len(declaredExamples(pkg)) }

// declaredExamples lists every example of every user capability, in file order.
func declaredExamples(pkg *apppkg.Package) []exampleRef {
	var out []exampleRef
	for _, id := range userCapabilities(pkg) {
		for _, ex := range pkg.Capabilities[id].Examples {
			out = append(out, exampleRef{capability: id, example: ex})
		}
	}
	return out
}

// Status is the verdict on one item of the stage. The values are the report's.
type Status string

// The five statuses an item takes.
const (
	StatusPass       Status = "pass"
	StatusWarn       Status = "warn"
	StatusFail       Status = "fail"
	StatusUnverified Status = "unverified"
	StatusNotRun     Status = "not_run"
)

// Item is one check of the stage: a replayed contract example, a capability, a guard.
type Item struct {
	ID      string
	Name    string
	Status  Status
	Message string
	// Details holds what the item observed, for a person reading the report. It never carries a
	// secret, a token or a generated identifier.
	Details map[string]any
}

// Subject is the application under test.
type Subject struct {
	ApplicationID string
	// Package is the loaded package whose endpoints point at the application: its capabilities,
	// examples, policies and test suites are what is replayed.
	Package *apppkg.Package
	// Adapter is the transport's own adapter for the package (internal/cli BuildPackageAdapter,
	// whatever the transport kind). The stage puts the scope-checking wrapper of internal/adapters on
	// top, after its own recorder, and never closes it.
	Adapter adapters.AppAdapter
	// Provisioner creates, seeds and removes the disposable tenants the examples name: the
	// provisioning bridge of the same transport. Nil means there is none, and the stage does not run.
	Provisioner probes.Provisioner
	// ScratchDir is an existing directory the stage may keep its throwaway store in.
	ScratchDir string
}

// Result is what the stage found.
type Result struct {
	Items []Item
	// Declared is how many examples the package declares and Replayed how many were replayed (they
	// must be equal for the stage to pass).
	Declared, Replayed int
}

// Replay is the conversation stage: every example of every user capability, replayed as a
// scripted turn through the service of `alixai serve` over the subject's adapter.
func Replay(ctx context.Context, s Subject) Result {
	if s.Package == nil || s.Adapter == nil || s.ApplicationID == "" || s.ScratchDir == "" {
		return Result{Items: []Item{{ID: "conversation.subject", Name: "application under test", Status: StatusFail,
			Message: "the stage was not given the package, its adapter, the application id and a scratch directory"}}}
	}
	pkg := s.Package
	examples := declaredExamples(pkg)
	declared := len(examples)
	finish := func(items ...Item) Result { return Result{Items: items, Declared: declared} }

	if declared == 0 {
		return finish(Item{ID: "conversation.examples", Name: "contract examples", Status: StatusFail,
			Message: "no user capability declares an example, so there is nothing to replay"})
	}
	suite, ok := suiteWithProvisioning(pkg.Tests)
	if !ok {
		return finish(Item{ID: "conversation.world", Name: "disposable tenants", Status: StatusNotRun,
			Message: "the package's test suites name no provisioning calls, so there is no disposable tenant to replay the examples in"})
	}
	if s.Provisioner == nil {
		return finish(Item{ID: "conversation.world", Name: "disposable tenants", Status: StatusNotRun,
			Message: "the application's transport offers no provisioning bridge for this package (its adapter configuration does not approve the suite's provisioning calls, or the application does not list them), so there is no disposable tenant to replay the examples in"})
	}

	r, err := newRig(ctx, s)
	if err != nil {
		return finish(Item{ID: "conversation.rig", Name: "conversation rig", Status: StatusFail, Message: err.Error()})
	}
	defer r.close()
	w, err := newWorld(ctx, s.Provisioner, suite)
	if err != nil {
		return finish(Item{ID: "conversation.world", Name: "disposable tenants", Status: StatusFail,
			Message: "the disposable tenants could not be created and seeded: " + err.Error()})
	}
	// the tenants are removed even if something below panics
	cleaned := false
	defer func() {
		if !cleaned {
			_ = w.cleanup()
		}
	}()
	worldClean := func() Item {
		cleaned = true
		if err := w.cleanup(); err != nil {
			return Item{ID: "conversation.cleanup", Name: "disposable tenants removed", Status: StatusFail, Message: err.Error()}
		}
		return Item{ID: "conversation.cleanup", Name: "disposable tenants removed", Status: StatusPass}
	}

	roles := roleGrants(pkg)
	var items []Item
	reached := map[string]bool{}
	seen := map[string][]string{}
	replayed := 0
	session := 0
	suffix := worldRandomSuffix()
	for _, ex := range examples {
		if ctx.Err() != nil {
			items = append(items, Item{ID: "conversation.cancelled", Name: "run cancelled", Status: StatusNotRun, Message: "the run was cancelled or timed out before every example was replayed"})
			break
		}
		it, did, acts := r.replayExample(ctx, s, w, roles, ex, &session, suffix)
		items = append(items, it)
		if did {
			replayed++
		}
		if it.Status == StatusPass {
			reached[ex.capability] = true
		}
		seen[ex.capability] = append(seen[ex.capability], acts...)
	}

	var capItems []Item
	for _, id := range userCapabilities(pkg) {
		it := Item{ID: "conversation.capability." + id, Name: id + " is conversationally reachable"}
		want := naturalActs(pkg.Capabilities[id])
		if reached[id] {
			it.Status = StatusPass
		} else {
			// Nothing demonstrates it. That is not a failure of the engine (a failing replay
			// is its own item), it is a capability the package's own examples cannot show.
			it.Status = StatusUnverified
			it.Message = fmt.Sprintf("no example of the capability reaches %s in the disposable world (acts seen: %s), so it is not shown to be conversationally reachable",
				strings.Join(want, " or "), strings.Join(uniq(seen[id]), ", "))
		}
		capItems = append(capItems, it)
	}
	out := append(capItems, items...)

	guard := Item{ID: "conversation.examples", Name: "every declared example was replayed", Status: StatusPass,
		Details: map[string]any{"declared": declared, "replayed": replayed}}
	if replayed != declared {
		guard.Status = StatusFail
		guard.Message = fmt.Sprintf("the package declares %d example(s) and %d were replayed", declared, replayed)
	}
	out = append(out, guard)
	// A stage that demonstrated nothing proves nothing: a package none of whose examples
	// reaches the act of any capability (a terminology gap, a router that routes nothing) must
	// not pass on warnings and unverified items alone.
	demonstrated := Item{ID: "conversation.demonstrated", Name: "some capability is demonstrated reachable", Status: StatusPass,
		Details: map[string]any{"capabilities": len(reached), "of": len(userCapabilities(pkg))}}
	if len(reached) == 0 {
		demonstrated.Status = StatusFail
		demonstrated.Message = fmt.Sprintf("none of the package's %d example(s) reaches the act of any capability, so the conversation stage demonstrated nothing", declared)
	}
	out = append(out, demonstrated, worldClean())
	return Result{Items: out, Declared: declared, Replayed: replayed}
}

// roleGrants maps a capability to the roles the package's host policy grants it.
func roleGrants(pkg *apppkg.Package) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, p := range pkg.Policies {
		for _, g := range p.RoleGrants {
			for _, c := range g.Capabilities {
				if out[c] == nil {
					out[c] = map[string]bool{}
				}
				out[c][g.Role] = true
			}
		}
	}
	return out
}

// naturalActs are the acts a capability is for: what its first turn reaches when every
// name in the utterance is in the application's data.
func naturalActs(c ap.Capability) []string {
	switch {
	case int64(c.RiskLevel) == 0:
		return []string{actPresentResult}
	case c.Confirmation.Mode == ap.CapabilityConfirmationModeExplicit:
		return []string{actRequestConfirmation}
	case c.Confirmation.Mode == ap.CapabilityConfirmationModeStepUp:
		return []string{actRequestStepUp}
	}
	return []string{actReportOutcome}
}

func uniq(in []string) []string {
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

// outcome is what one replay of an example showed.
type outcome struct {
	principal string
	act       string
	code      string
	language  string
	reached   bool
	// unrouted is true when the router did not shortlist the example's capability for its
	// utterance, so the engine never asked the model: nothing was proposed or checked.
	unrouted  bool
	route     string
	shortlist []string
	problems  []string
}

// replayExample replays one example for every principal that may use its capability. It
// returns the example's item, whether the example was replayed at all, and the acts seen.
func (r *rig) replayExample(ctx context.Context, s Subject, w *world, roles map[string]map[string]bool, ex exampleRef, session *int, suffix string) (Item, bool, []string) {
	spec := s.Package.Capabilities[ex.capability]
	id := string(ex.example.ID)
	it := Item{ID: "conversation.example." + ex.capability + "." + id, Name: ex.capability + " / " + id,
		Details: map[string]any{"language": string(ex.example.Language), "capability": ex.capability}}

	var principals []worldPrincipal
	for _, p := range w.principals {
		if p.Service {
			continue
		}
		for _, role := range p.Roles {
			if roles[ex.capability][role] {
				principals = append(principals, p)
				break
			}
		}
	}
	if len(principals) == 0 {
		it.Status = StatusUnverified
		it.Message = "no human principal of the package's test suite holds a role the host policy grants " + ex.capability + ", so the example cannot be replayed"
		return it, false, nil
	}

	var outcomes []outcome
	var problems []string
	var acts []string
	for _, p := range principals {
		*session++
		o := r.replayAs(ctx, s, spec, ex, p, fmt.Sprintf("ses-conf-%s-%d", suffix, *session))
		outcomes = append(outcomes, o)
		acts = append(acts, o.act)
		for _, pr := range o.problems {
			problems = append(problems, o.principal+": "+pr)
		}
	}
	var perPrincipal []map[string]any
	reached := false
	chosen := outcomes[0]
	for _, o := range outcomes {
		perPrincipal = append(perPrincipal, map[string]any{"principal": o.principal, "act": o.act, "code": o.code, "route": o.route, "shortlist": o.shortlist})
		if o.reached && !reached {
			reached, chosen = true, o
		}
	}
	it.Details["principals"] = perPrincipal
	it.Details["act"] = chosen.act
	if chosen.code != "" {
		it.Details["code"] = chosen.code
	}
	switch {
	case len(problems) > 0:
		it.Status = StatusFail
		it.Message = strings.Join(problems, "; ")
	case reached:
		it.Status = StatusPass
	case allUnrouted(outcomes):
		it.Status = StatusWarn
		it.Message = fmt.Sprintf("the utterance does not shortlist %s for any principal of the suite, so the engine never offered it to the model: the package's terminology and title words do not cover the way its own example is worded", ex.capability)
	default:
		it.Status = StatusWarn
		it.Message = fmt.Sprintf("the example reaches %s for none of the suite's principals (acts seen: %s): the names it uses are probably not in the package's seed data",
			strings.Join(naturalActs(spec), " or "), strings.Join(uniq(acts), ", "))
	}
	return it, true, acts
}

func allUnrouted(outcomes []outcome) bool {
	for _, o := range outcomes {
		if !o.unrouted {
			return false
		}
	}
	return true
}

// replayAs runs the example once as one principal and checks what happened.
func (r *rig) replayAs(ctx context.Context, s Subject, spec ap.Capability, ex exampleRef, p worldPrincipal, sessionID string) outcome {
	scope := contracts.Scope{
		DeploymentID: conversationDeployment, ApplicationID: s.ApplicationID, TenantID: p.TenantID, WorkspaceID: p.WorkspaceID,
		PrincipalID: p.ID, Roles: p.Roles, CapabilityScopes: userCapabilities(s.Package), SessionID: sessionID,
	}
	fields := map[string]any{}
	for k, v := range ex.example.ExpectedFields {
		fields[k] = v
	}
	r.model.Reset()
	r.model.On(pfake.Constrained(providers.ConstraintJSONSchema)).Reply(proposalJSON(ex.capability, fields)).Times(1)
	r.rec.reset()

	turnCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res, err := r.engine.Turn(turnCtx, scope, dialogue.TurnInput{Text: ex.example.Utterance, Language: string(ex.example.Language)})
	calls := r.rec.snapshot()
	if err != nil {
		o := outcome{principal: p.Ref, act: "error", code: string(contracts.CodeOf(err))}
		o.problems = append(o.problems, "the turn could not be run: "+err.Error())
		return o
	}
	goal := ""
	if s, err := r.sessions.Open(ctx, scope); err == nil && s.State.Goal != nil {
		goal = s.State.Goal.CapabilityID
	}
	return judge(p.Ref, spec, ex, calls, res, goal)
}

// judge holds one replay to the checks of the stage (see the comment at the top of this
// file): what the engine did with the proposal, what the application was asked, and what
// the user was told. goalCapability is the capability of the session's goal after the turn.
func judge(principal string, spec ap.Capability, ex exampleRef, calls []recordedCall, res dialogue.Result, goalCapability string) outcome {
	o := outcome{principal: principal}
	o.act = string(res.Directive.Act)
	o.code = string(res.Code)
	o.language = string(res.Language)
	o.route, o.shortlist = string(res.Trace.Route), res.Trace.Shortlist
	o.unrouted = !slices.Contains(res.Trace.Shortlist, ex.capability)

	// A turn the router did not shortlist the capability for never reached the model: there
	// was no proposal to check and nothing is held against the engine, but the example is
	// reported (replayExample) and the safety checks below still apply.
	if !o.unrouted {
		// targeted
		targeted := goalCapability == ex.capability
		for _, c := range calls {
			if c.CapabilityID == ex.capability {
				targeted = true
			}
		}
		if !targeted {
			o.problems = append(o.problems, "the turn did not select the capability the example is for (act "+o.act+")")
		}

		// mentioned: the entity the example names first reached the application's resolver,
		// exactly as the model proposed it; and when the turn got as far as an answer, a
		// confirmation or an outcome, every entity it names did, since each had to be
		// resolved to get there. A turn that stops earlier (a name the application does not
		// know, a choice for the user) legitimately never asks about the entities after it.
		complete := o.act == actPresentResult || o.act == actRequestConfirmation || o.act == actRequestStepUp || o.act == actReportOutcome
		first := true
		for _, f := range spec.Fields {
			want, ok := ex.example.ExpectedFields[string(f.Name)]
			if !ok || f.Type != ap.CapabilityFieldTypeEntityRef || f.EntityType == nil {
				continue
			}
			if !first && !complete {
				break
			}
			first = false
			if !mentionReached(calls, ex.capability, string(*f.EntityType), fmt.Sprint(want)) {
				o.problems = append(o.problems, fmt.Sprintf("the %s %q the example names never reached the application's resolver", *f.EntityType, fmt.Sprint(want)))
			}
		}
	}

	// typed
	if !o.unrouted && o.code != "" && !declaresFailure(spec, o.code) {
		o.problems = append(o.problems, fmt.Sprintf("the turn failed with code %q, which the capability does not declare in failure_states", o.code))
	}
	if strings.TrimSpace(res.Response.Text) == "" {
		o.problems = append(o.problems, "the turn gave the user no response")
	}
	if lang := string(ex.example.Language); lang != "" && !strings.EqualFold(string(res.Language), baseTag(lang)) {
		o.problems = append(o.problems, fmt.Sprintf("the response is in %q, not the example's language %q", res.Language, lang))
	}

	// safe
	if mode := spec.Confirmation.Mode; mode == ap.CapabilityConfirmationModeExplicit || mode == ap.CapabilityConfirmationModeStepUp {
		for _, c := range calls {
			if c.Op == adapters.OpCommit && c.CapabilityID == ex.capability {
				o.problems = append(o.problems, "the application was asked to commit before the user confirmed")
				break
			}
		}
		if res.Receipt != nil {
			o.problems = append(o.problems, "the turn produced a receipt without the confirmation the capability demands")
		}
	}

	for _, want := range naturalActs(spec) {
		if o.act == want && o.code == "" {
			o.reached = true
		}
	}
	return o
}

// mentionReached reports whether a read of the capability asked the application to resolve
// exactly this mention of this entity type.
func mentionReached(calls []recordedCall, capID, entityType, mention string) bool {
	for _, c := range calls {
		if c.Op != adapters.OpRead || c.CapabilityID != capID {
			continue
		}
		if fmt.Sprint(c.Fields["entity_type"]) == entityType && fmt.Sprint(c.Fields["mention"]) == mention {
			return true
		}
	}
	return false
}

func declaresFailure(spec ap.Capability, code string) bool {
	for _, f := range spec.FailureStates {
		if string(f.Code) == code {
			return true
		}
	}
	return false
}

// baseTag is the primary language subtag: "en-GB" is "en".
func baseTag(tag string) string {
	base, _, _ := strings.Cut(tag, "-")
	return strings.ToLower(base)
}
