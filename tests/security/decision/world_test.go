package decisionsec

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	core "github.com/buxee/alix-ai/internal/decision"
	"github.com/buxee/alix-ai/internal/decision/fakescorer"
	"github.com/buxee/alix-ai/internal/dialogue"
	dlg "github.com/buxee/alix-ai/internal/dialogue/decision"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	actgen "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
	conv "github.com/buxee/alix-ai/tests/conversation"
)

// The dialogue world is the one of the conversation tests: the clinic reference application's real signed
// package behind its stand-in host, the real policy gateway, action coordinator, session store and dialogue
// engine, a scripted controller (the fake text provider) and, in front of the controller, the decision layer
// with the scripted fake scorer behind it. The host counts every Prepare and Commit that reaches it.

const clinicPackageDir = "../../../examples/reference-apps/clinic-booking/package"

var clinicNow = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

// The utterances of the clinic's conversation corpus (tests/conversation).
const (
	sayBook     = "Book Luca Ferri for 2026-10-14 10:30" // shortlist: book, cancel, list
	sayParaCanc = "scrap the whole thing"                // a paraphrase of cancel no exact rule recognises
	sayParaYes  = "yes that looks right to me"           // a paraphrase of yes no exact rule recognises
)

func recep() contracts.Scope  { return conv.RoleScope(conv.RoleReceptionist) }
func doctor() contracts.Scope { return conv.RoleScope(conv.RoleDoctor) }

type activePackage struct{ capabilities.Package }

func (activePackage) Status() policy.PackageStatus { return policy.StatusActive }

// quarantined is the package as the gateway sees it after an integration re-audit pulled it.
type quarantined struct{ capabilities.Package }

func (quarantined) Status() policy.PackageStatus { return policy.StatusQuarantined }

func loadPackage(t testing.TB) capabilities.Package {
	t.Helper()
	res, err := apppkg.Load(bg, apppkg.FromDir(clinicPackageDir), apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if res.Package == nil {
		t.Fatalf("package did not load: %v", res.Findings)
	}
	return activePackage{capabilities.FromApppkg(res.Package)}
}

// countingAdapter counts what reaches the host's consequential operations.
type countingAdapter struct {
	adapters.AppAdapter
	mu                sync.Mutex
	prepares, commits int
}

func (c *countingAdapter) Prepare(ctx context.Context, sc contracts.Scope, r adapters.PrepareRequest) (actgen.DeterministicResult, error) {
	c.mu.Lock()
	c.prepares++
	c.mu.Unlock()
	return c.AppAdapter.Prepare(ctx, sc, r)
}

func (c *countingAdapter) Commit(ctx context.Context, sc contracts.Scope, r adapters.CommitRequest) (actgen.DeterministicResult, error) {
	c.mu.Lock()
	c.commits++
	c.mu.Unlock()
	return c.AppAdapter.Commit(ctx, sc, r)
}

func (c *countingAdapter) counts() (prepares, commits int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.prepares, c.commits
}

type world struct {
	t       testing.TB
	clk     *clock.Fake
	alog    *audit.Log
	gw      *policy.Gateway
	host    *conv.ClinicHost
	counter *countingAdapter
	pkg     capabilities.Package
	store   *sessions.Store
	model   *pfake.Provider
	eng     *dialogue.Engine
	turner  dlg.Turner

	layer  *dlg.Layer
	aud    *auditTee
	scorer *captureScorer
	// quarantineNext makes the next package lookup of the layer report a quarantined package.
	quarantineNext bool
	providers      *providers.Registry
}

type worldOpts struct {
	noLayer    bool
	disabled   bool
	script     []fakescorer.Step
	noScorer   bool
	textRouter bool
	knowledge  dialogue.Knowledge
	mutateCore func(*core.Config)
}

type worldOpt func(*worldOpts)

func withoutLayer() worldOpt                      { return func(o *worldOpts) { o.noLayer = true } }
func withScript(s ...fakescorer.Step) worldOpt    { return func(o *worldOpts) { o.script = s } }
func withNoScorer() worldOpt                      { return func(o *worldOpts) { o.noScorer = true } }
func withTextRouter() worldOpt                    { return func(o *worldOpts) { o.textRouter = true } }
func withKnowledge(k dialogue.Knowledge) worldOpt { return func(o *worldOpts) { o.knowledge = k } }
func withCore(f func(*core.Config)) worldOpt      { return func(o *worldOpts) { o.mutateCore = f } }

// testCore is the engine policy the layer runs under: on, calibrated for the two contract versions of the fake
// scorer, cache off so a repeated utterance calls the scorer again.
func testCore() core.Config {
	c := core.DefaultConfig()
	c.Enabled = true
	c.CacheEntries = 0
	c.SupportedTypes = []gen.DecisionType{gen.DecisionTypeCapabilityRouting, gen.DecisionTypeDialogueAct}
	for _, v := range []string{dlg.ContractRouting, dlg.ContractAct} {
		c.Calibration = append(c.Calibration, core.Calibration{ModelID: "fake-model", ModelRevision: "rev1", ContractVersion: v, ECE: 0.02, MaxECE: 0.05})
	}
	return c
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

func newWorld(t testing.TB, opts ...worldOpt) *world {
	t.Helper()
	var o worldOpts
	for _, f := range opts {
		f(&o)
	}
	w := &world{t: t, clk: clock.NewFake(clinicNow)}
	db := storetest.Open(t, nil)
	var err error
	if w.alog, err = audit.New(db, audit.Options{Clock: w.clk}); err != nil {
		t.Fatal(err)
	}
	sink := audit.NewRecordSink[policy.Record](w.alog, audit.EventPolicyDecision, func(r policy.Record) (audit.Locator, error) {
		s := r.Subject
		return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
	})
	w.gw = policy.NewGateway(w.clk, policy.WithSink(sink))
	w.host = conv.NewClinicHost(w.clk)
	w.counter = &countingAdapter{AppAdapter: w.host.Adapter()}
	appID := conv.RoleScope(conv.RoleReceptionist).ApplicationID
	guard, err := adapters.Wrap(w.counter, adapters.Options{ApplicationID: appID})
	if err != nil {
		t.Fatal(err)
	}
	w.pkg = loadPackage(t)
	reg := capabilities.NewRegistry(nil, capabilities.WithClock(w.clk))
	if err := reg.Activate(w.pkg); err != nil {
		t.Fatal(err)
	}
	adapterSrc := actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return guard, nil })
	co, err := actions.New(actions.Deps{
		DB: db, Policy: w.gw, Audit: w.alog,
		Packages: actions.PackageSourceFunc(func(context.Context, string) (policy.ActivePackage, error) { return w.pkg, nil }),
		Adapters: adapterSrc, Clock: w.clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	if w.store, err = sessions.New(db, w.clk, sessions.Config{}); err != nil {
		t.Fatal(err)
	}
	w.model = pfake.New(pfake.WithClock(w.clk))
	budget := providers.Budget{MaxPromptTokens: 4096, MaxOutputTokens: 1024, MaxBatchItems: 4, Timeout: time.Minute}
	sel, err := dialogue.LocalSelector(w.model, budget)
	if err != nil {
		t.Fatal(err)
	}
	pkgSource := dialogue.PackageSourceFunc(func(context.Context, string) (capabilities.Package, error) { return w.pkg, nil })
	layerPackages := dialogue.PackageSourceFunc(func(ctx context.Context, app string) (capabilities.Package, error) {
		if w.quarantineNext {
			w.quarantineNext = false
			return quarantined{w.pkg}, nil
		}
		return pkgSource.Package(ctx, app)
	})
	deps := dialogue.Deps{
		Sessions: w.store, Registry: reg, Packages: pkgSource, Policy: w.gw, Actions: co,
		Adapters: adapterSrc, Providers: sel, Audit: w.alog, Clock: w.clk, Knowledge: o.knowledge,
	}
	deps.Config.TemplateOnly = allActs()
	if !o.noLayer {
		cc := testCore()
		if o.mutateCore != nil {
			o.mutateCore(&cc)
		}
		w.scorer = newCapture(w.clk)
		w.scorer.Script(o.script...)
		w.aud = &auditTee{log: w.alog}
		ld := dlg.Deps{Clock: w.clk, Core: cc, Audit: w.aud, Policy: w.gw, Packages: layerPackages}
		w.providers = providers.NewRegistry(w.clk)
		ld.Registry = w.providers
		switch {
		case o.noScorer:
		case o.textRouter:
			if err := w.providers.Register(providers.RoleRouter, pfake.New()); err != nil {
				t.Fatal(err)
			}
		default:
			if err := core.RegisterRouter(w.providers, w.scorer); err != nil {
				t.Fatal(err)
			}
		}
		if w.layer, err = dlg.New(ld); err != nil {
			t.Fatal(err)
		}
		deps.Decision = w.layer.Stage()
		deps.Detectors = w.layer.Detectors()
	}
	if w.eng, err = dialogue.New(deps); err != nil {
		t.Fatal(err)
	}
	w.turner = w.eng
	if w.layer != nil {
		w.turner = w.layer.Wrap(w.eng)
	}
	return w
}

// ---- scripting and driving ----

func proposalJSON(capability, intent string, fields map[string]any) string {
	m := map[string]any{
		"capability_id": nil, "fields": map[string]any{}, "entity_refs": []any{}, "intent": intent,
		"uncertainty": map[string]any{"overall": 0.9},
	}
	if capability != "" {
		m["capability_id"] = capability
	}
	if fields != nil {
		m["fields"] = fields
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// propose scripts the controller's next TaskProposal and steps aside after one use.
func (w *world) propose(capability, intent string, fields map[string]any) {
	w.t.Helper()
	w.model.On(pfake.Constrained(providers.ConstraintJSONSchema)).Reply(proposalJSON(capability, intent, fields)).Times(1)
}

func (w *world) say(sc contracts.Scope, text string) dialogue.Result {
	w.t.Helper()
	res, err := w.turner.Turn(bg, sc, dialogue.TurnInput{Text: text, Language: "en"})
	if err != nil {
		w.t.Fatalf("Turn(%q): %v", text, err)
	}
	return res
}

func (w *world) goal(sc contracts.Scope) *sessions.Goal {
	w.t.Helper()
	s, err := w.store.Open(bg, sc)
	if err != nil {
		w.t.Fatal(err)
	}
	return s.State.Goal
}

func (w *world) commits() int {
	_, c := w.counter.counts()
	return c
}

func (w *world) noCommits() {
	w.t.Helper()
	if c := w.commits(); c != 0 {
		w.t.Fatalf("the host saw %d Commit calls, want 0", c)
	}
}

// prompts returns the text of every prompt the controller was given.
func (w *world) prompts() string {
	var b strings.Builder
	for _, p := range w.model.Prompts() {
		raw, _ := json.Marshal(p.Chat)
		b.Write(raw)
		b.WriteByte('\n')
	}
	return b.String()
}

// proposalCaps is how many capabilities the (first) proposal prompt listed.
func proposalCaps(t testing.TB, res dialogue.Result) int {
	t.Helper()
	for _, c := range res.Trace.ModelCalls {
		if c.Purpose == "proposal" {
			return c.Capabilities
		}
	}
	t.Fatalf("no proposal call in %+v", res.Trace.ModelCalls)
	return 0
}

// shape is what a turn did, for comparing two runs: the deterministic outcome, not the words.
type shape struct {
	Act        string
	Code       contracts.ErrCode
	Capability string
	Missing    []string
	Prepared   bool
	Receipt    bool
	Goal       string
	Risk       actgen.RiskLevel
}

func (w *world) shapeOf(sc contracts.Scope, res dialogue.Result) shape {
	s := shape{Act: string(res.Directive.Act), Code: res.Code, Prepared: res.Prepared != nil, Receipt: res.Receipt != nil}
	if res.Prepared != nil {
		s.Risk = res.Prepared.RiskLevel
	}
	if res.Directive.CapabilityID != nil {
		s.Capability = string(*res.Directive.CapabilityID)
	}
	for _, m := range res.Directive.MissingFields {
		s.Missing = append(s.Missing, string(m))
	}
	if g := w.goal(sc); g != nil {
		s.Goal = g.CapabilityID
	}
	return s
}

func wantAct(t testing.TB, res dialogue.Result, a string) {
	t.Helper()
	if string(res.Directive.Act) != a {
		t.Fatalf("act = %s (code %q, response %q), want %s", res.Directive.Act, res.Code, res.Response.Text, a)
	}
}
