package ext

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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
	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/dialogue/extmode"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	actgen "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
	conv "github.com/buxee/alix-ai/tests/conversation"
)

// The world: the clinic reference application's signed package behind its stand-in host, the real policy gateway,
// action coordinator, session store and dialogue engine, the real data-egress service over a real store, and the
// external-mode runtime routing between two scripted fake providers (the external one and the local one). Only
// the profile registry and the budget ledger are scripted here (they have their own suites); they fail the way
// the real ones do, with the same typed errors and reasons.

const (
	clinicPackageDir = "../../../examples/reference-apps/clinic-booking/package"
	profileFile      = "../../../schemas/providers/examples/provider-profile/valid/qualified-text-only.json"

	sayBook = "Book Luca Ferri for 2026-10-14 10:30"
	bookArg = `{"patient":"Luca Ferri","slot":"2026-10-14 10:30"}`

	profA pv.ProfileID = "pprf_01J0000000000000000000000P"
	profB pv.ProfileID = "pprf_01J0000000000000000000000Q"

	egProvider = pv.ProviderID("openai")
	egModel    = pv.ModelID("gpt-4.1-mini")
)

var clinicNow = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

func ctxb() context.Context { return context.Background() }

func receptionist() contracts.Scope { return conv.RoleScope(conv.RoleReceptionist) }

type activePackage struct{ capabilities.Package }

func (activePackage) Status() policy.PackageStatus { return policy.StatusActive }

// ---- the host, counted ----

// countingAdapter counts what reaches the host's operations.
type countingAdapter struct {
	adapters.AppAdapter
	mu                       sync.Mutex
	reads, prepares, commits int
}

func (c *countingAdapter) Read(ctx context.Context, sc contracts.Scope, r adapters.ReadRequest) (actgen.DeterministicResult, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	return c.AppAdapter.Read(ctx, sc, r)
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

func (c *countingAdapter) counts() (reads, prepares, commits int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads, c.prepares, c.commits
}

// ---- the scripted profile registry ----

// profiles is the registry the router sees. It refuses the way the real one does: a capability the profile lacks,
// a terms change that quarantines the profile (and keeps it quarantined until a review, which no test performs).
type profiles struct {
	mu          sync.Mutex
	byID        map[pv.ProfileID]pv.ProviderProfile
	checkErr    map[pv.ProfileID]error
	quarantined map[pv.ProfileID]providerreg.Reason
	fallbacks   int
}

func reasonErr(code contracts.ErrCode, reason providerreg.Reason) error {
	return contracts.New(code, "providerreg: refused").WithDetails("reason", string(reason))
}

func (p *profiles) Get(_ context.Context, _ contracts.Scope, id pv.ProfileID) (pv.ProviderProfile, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	prof, ok := p.byID[id]
	if !ok {
		return pv.ProviderProfile{}, reasonErr(contracts.ErrValidationFailed, providerreg.ReasonProfileNotFound)
	}
	if _, q := p.quarantined[id]; q {
		prof.Lifecycle = pv.ProfileLifecycleQuarantined
	}
	return prof, nil
}

func (p *profiles) Check(_ context.Context, _ contracts.Scope, id pv.ProfileID, _ providerreg.Requirement) (providerreg.Admission, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	prof, ok := p.byID[id]
	if !ok {
		return providerreg.Admission{}, reasonErr(contracts.ErrValidationFailed, providerreg.ReasonProfileNotFound)
	}
	if why, q := p.quarantined[id]; q {
		return providerreg.Admission{}, reasonErr(contracts.ErrQuarantined, why)
	}
	if err := p.checkErr[id]; err != nil {
		if providerreg.ReasonOf(err) == providerreg.ReasonTermsChanged {
			p.quarantined[id] = providerreg.ReasonTermsChanged // the registry quarantines on the change and stays there
		}
		return providerreg.Admission{}, err
	}
	return providerreg.Admission{Profile: prof}, nil
}

// Fallback names no alternate provider: a profile without a fallback of its own never gets one.
func (p *profiles) Fallback(context.Context, contracts.Scope, pv.ProfileID, providerreg.FallbackCondition, providerreg.Requirement) (providerreg.FallbackRecord, providerreg.Admission, error) {
	p.mu.Lock()
	p.fallbacks++
	p.mu.Unlock()
	return providerreg.FallbackRecord{}, providerreg.Admission{}, reasonErr(contracts.ErrPermissionDenied, providerreg.ReasonFallbackNotNamed)
}

func (p *profiles) setCheckErr(id pv.ProfileID, err error) {
	p.mu.Lock()
	p.checkErr[id] = err
	p.mu.Unlock()
}

func (p *profiles) isQuarantined(id pv.ProfileID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, q := p.quarantined[id]
	return q
}

// ---- the budget ledger ----

// budgets is the real provider budget ledger (a store-backed Service) with one hook: reserveErr makes every
// reservation refuse with the given error, as a ledger that is rate limiting does.
type budgets struct {
	*providerbudget.Service
	mu         sync.Mutex
	reserveErr error
	reserved   int
	settled    []providerbudget.Actual
}

func (b *budgets) Reserve(ctx context.Context, sc contracts.Scope, c providerbudget.Call) (providerbudget.Reservation, error) {
	b.mu.Lock()
	err := b.reserveErr
	b.mu.Unlock()
	if err != nil {
		return providerbudget.Reservation{}, err
	}
	r, err := b.Service.Reserve(ctx, sc, c)
	if err == nil {
		b.mu.Lock()
		b.reserved++
		b.mu.Unlock()
	}
	return r, err
}

func (b *budgets) Settle(ctx context.Context, sc contracts.Scope, id pv.ReservationID, a providerbudget.Actual) (providerbudget.Settlement, error) {
	b.mu.Lock()
	b.settled = append(b.settled, a)
	b.mu.Unlock()
	return b.Service.Settle(ctx, sc, id, a)
}

func (b *budgets) setReserveErr(err error) { b.mu.Lock(); b.reserveErr = err; b.mu.Unlock() }

func (b *budgets) reservations() int { b.mu.Lock(); defer b.mu.Unlock(); return b.reserved }

func (b *budgets) lastSettled() (providerbudget.Actual, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.settled) == 0 {
		return providerbudget.Actual{}, false
	}
	return b.settled[len(b.settled)-1], true
}

// ---- the local runtime's availability ----

type localUp struct {
	mu sync.Mutex
	up bool
}

func (l *localUp) Usable(modes.Role) bool { l.mu.Lock(); defer l.mu.Unlock(); return l.up }
func (l *localUp) set(up bool)            { l.mu.Lock(); l.up = up; l.mu.Unlock() }

// ---- the sender ----

// sender is the wire to the external provider: it counts what it was given, hands it to the scripted provider,
// and can lose the connection after the provider has processed the request (the reply never arrives).
type sender struct {
	gen       providers.MultimodalGenerator
	mu        sync.Mutex
	n         int
	lostAfter error
	profiles  []pv.ProfileID
}

func (s *sender) Send(ctx context.Context, scope contracts.Scope, req extmode.SendRequest) (providers.RichResult, error) {
	s.mu.Lock()
	s.n++
	s.profiles = append(s.profiles, req.Profile.ProfileID)
	lost := s.lostAfter
	s.mu.Unlock()
	res, err := s.gen.Generate(ctx, scope, req.Rich)
	if err != nil {
		return providers.RichResult{}, err
	}
	if lost != nil {
		return providers.RichResult{}, lost
	}
	return res, nil
}

func (s *sender) sentTo() []pv.ProfileID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]pv.ProfileID(nil), s.profiles...)
}

func (s *sender) sends() int { s.mu.Lock(); defer s.mu.Unlock(); return s.n }

func (s *sender) loseConnection(err error) { s.mu.Lock(); s.lostAfter = err; s.mu.Unlock() }

// connectionReset is what a dropped TCP connection looks like to the sender: not a typed provider error.
var connectionReset = errors.New("read tcp 127.0.0.1:50000->127.0.0.1:8099: connection reset by peer")

// ---- the world ----

type world struct {
	t        testing.TB
	clk      *clock.Fake
	db       *store.DB
	alog     *audit.Log
	gw       *policy.Gateway
	host     *conv.ClinicHost
	counter  *countingAdapter
	co       *actions.Coordinator
	pkg      capabilities.Package
	store    *sessions.Store
	eg       *dataegress.Service
	modes    *modes.MemoryModes
	policy   modes.StaticPolicy
	local    *pfake.Provider
	ext      *pfake.Provider
	snd      *sender
	prof     *profiles
	budget   *budgets
	localUp  *localUp
	eng      *dialogue.Engine
	rt       *extmode.Runtime
	outcomes []modes.Outcome
	mu       sync.Mutex
}

type opt func(*worldOpts)

type worldOpts struct {
	noEgressPolicy bool
	localFallback  []modes.Condition
	second         bool
	dailyMicro     int64
	modelReplies   bool
}

func withoutEgressPolicy() opt { return func(o *worldOpts) { o.noEgressPolicy = true } }
func withLocalFallback(c ...modes.Condition) opt {
	return func(o *worldOpts) { o.localFallback = c }
}
func withSecondProfile() opt { return func(o *worldOpts) { o.second = true } }

// withModelReplies lets the external provider realise the response too (the directive leaves at class internal):
// the second, non-committing kind of request a turn can send.
func withModelReplies() opt { return func(o *worldOpts) { o.modelReplies = true } }

// withBudget sets the provider budget of the account in micro-units per day; the default is far above any test.
func withBudget(dailyMicro int64) opt { return func(o *worldOpts) { o.dailyMicro = dailyMicro } }

func loadProfile(t testing.TB) pv.ProviderProfile {
	t.Helper()
	raw, err := os.ReadFile(profileFile)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var p pv.ProviderProfile
	if err := dec.Decode(&p); err != nil {
		t.Fatal(err)
	}
	p.ProfileID = profA
	p.PermittedScopes = []pv.ProviderProfileScopeGrant{{ApplicationID: common.Identifier(receptionist().ApplicationID)}}
	return p
}

func newWorld(t testing.TB, opts ...opt) *world {
	t.Helper()
	o := worldOpts{dailyMicro: 1_000_000_000_000}
	for _, f := range opts {
		f(&o)
	}
	w := &world{t: t, clk: clock.NewFake(clinicNow)}
	w.db = storetest.Open(t, nil)
	var err error
	if w.alog, err = audit.New(w.db, audit.Options{Clock: w.clk}); err != nil {
		t.Fatal(err)
	}
	sink := audit.NewRecordSink[policy.Record](w.alog, audit.EventPolicyDecision, func(r policy.Record) (audit.Locator, error) {
		s := r.Subject
		return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
	})
	w.gw = policy.NewGateway(w.clk, policy.WithSink(sink))
	w.host = conv.NewClinicHost(w.clk)
	w.counter = &countingAdapter{AppAdapter: w.host.Adapter()}
	sc := receptionist()
	guard, err := adapters.Wrap(w.counter, adapters.Options{ApplicationID: sc.ApplicationID})
	if err != nil {
		t.Fatal(err)
	}
	res, err := apppkg.Load(ctxb(), apppkg.FromDir(clinicPackageDir), apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
	if err != nil || res.Package == nil {
		t.Fatalf("load package: %v %v", err, res.Findings)
	}
	w.pkg = activePackage{capabilities.FromApppkg(res.Package)}
	reg := capabilities.NewRegistry(nil, capabilities.WithClock(w.clk))
	if err := reg.Activate(w.pkg); err != nil {
		t.Fatal(err)
	}
	adapterSrc := actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return guard, nil })
	if w.co, err = actions.New(actions.Deps{
		DB: w.db, Policy: w.gw, Audit: w.alog,
		Packages: actions.PackageSourceFunc(func(context.Context, string) (policy.ActivePackage, error) { return w.pkg, nil }),
		Adapters: adapterSrc, Clock: w.clk,
	}); err != nil {
		t.Fatal(err)
	}
	if w.store, err = sessions.New(w.db, w.clk, sessions.Config{}); err != nil {
		t.Fatal(err)
	}
	if w.eg, err = dataegress.New(dataegress.Deps{DB: w.db, Clock: w.clk, Audit: w.alog, IDs: ids.NewGenerator(w.clk)}); err != nil {
		t.Fatal(err)
	}

	a := loadProfile(t)
	w.prof = &profiles{byID: map[pv.ProfileID]pv.ProviderProfile{profA: a}, checkErr: map[pv.ProfileID]error{}, quarantined: map[pv.ProfileID]providerreg.Reason{}}
	routes := []pv.ProfileID{profA}
	if o.second {
		b := a
		b.ProfileID = profB
		w.prof.byID[profB] = b
		routes = append(routes, profB)
	}
	w.policy = modes.StaticPolicy{modes.RoleController: {Profiles: routes, LocalFallback: o.localFallback}}
	w.local = pfake.New(pfake.WithClock(w.clk), pfake.WithName("local"))
	w.ext = pfake.New(pfake.WithClock(w.clk), pfake.WithName("external"), pfake.WithModel(string(egModel)))
	w.snd = &sender{gen: w.ext}
	bsvc, err := providerbudget.New(w.db, providerbudget.Config{Clock: w.clk, LocalFallback: func(context.Context, contracts.Scope, pv.ProviderID) bool {
		return w.localUp.Usable(modes.RoleController) && w.policy[modes.RoleController].FallsBackOn(modes.CondBudgetExhausted)
	}})
	if err != nil {
		t.Fatal(err)
	}
	w.budget = &budgets{Service: bsvc}
	w.putBudget(sc, o.dailyMicro)
	w.localUp = &localUp{up: true}
	w.modes = modes.NewMemoryModes()
	for _, lv := range []modes.Level{modes.LevelDeployment, modes.LevelApplication, modes.LevelTenant} {
		if err := w.modes.Set(lv, sc, modes.ModeExternal); err != nil {
			t.Fatal(err)
		}
	}
	if !o.noEgressPolicy {
		for _, lv := range []pv.EgressLevel{pv.EgressLevelDeployment, pv.EgressLevelApplication, pv.EgressLevelTenant, pv.EgressLevelWorkspace} {
			w.putPolicy(sc, lv)
		}
	}
	localSel, err := dialogue.LocalSelector(w.local, providers.Budget{MaxPromptTokens: 4096, MaxOutputTokens: 1024, MaxBatchItems: 4, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	pkgSource := dialogue.PackageSourceFunc(func(context.Context, string) (capabilities.Package, error) { return w.pkg, nil })
	rt, err := extmode.New(extmode.Config{
		Local: localSel, Modes: w.modes, KillSwitch: w.eg, Policy: w.policy, LocalProviders: w.localUp,
		Profiles: w.prof, Egress: w.eg, Budgets: w.budget, Packages: pkgSource, Sender: w.snd,
		Budget: providers.Budget{MaxPromptTokens: 16384, MaxOutputTokens: 2048, MaxBatchItems: 4, Timeout: time.Minute}, Clock: w.clk,
		DirectiveClass: map[bool]string{true: "internal", false: ""}[o.modelReplies],
		OnOutcome: func(_ contracts.Scope, out modes.Outcome) {
			w.mu.Lock()
			w.outcomes = append(w.outcomes, out)
			w.mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	w.rt = rt
	deps := dialogue.Deps{
		Sessions: w.store, Registry: reg, Packages: pkgSource, Policy: w.gw, Actions: w.co,
		Adapters: adapterSrc, Providers: rt, Guard: rt, Audit: w.alog, Clock: w.clk,
	}
	// Responses are the core's deterministic templates: the failures under test are the controller's.
	deps.Config.TemplateOnly = []conversation.DialogueDirectiveAct{
		conversation.DialogueDirectiveActAskMissing, conversation.DialogueDirectiveActAskInvalid,
		conversation.DialogueDirectiveActDisambiguate, conversation.DialogueDirectiveActRequestConfirmation,
		conversation.DialogueDirectiveActRequestStepUp, conversation.DialogueDirectiveActPresentResult,
		conversation.DialogueDirectiveActReportOutcome, conversation.DialogueDirectiveActReportFailure,
		conversation.DialogueDirectiveActRefuse, conversation.DialogueDirectiveActOfferHelp,
		conversation.DialogueDirectiveActAcknowledgeCancel, conversation.DialogueDirectiveActReportNoEvidence,
	}
	if o.modelReplies {
		deps.Config.TemplateOnly = []conversation.DialogueDirectiveAct{conversation.DialogueDirectiveActRefuse}
	}
	if w.eng, err = dialogue.New(deps); err != nil {
		t.Fatal(err)
	}
	return w
}

// ---- data-egress policy ----

func adminScope(sc contracts.Scope) contracts.Scope {
	a := sc.Clone()
	a.PrincipalID = "usr_egress_admin"
	a.SessionID = "ses_egress_admin"
	a.CapabilityScopes = []string{dataegress.CapabilityRead, dataegress.CapabilityApprove}
	for _, lv := range []pv.EgressLevel{pv.EgressLevelApplication, pv.EgressLevelTenant, pv.EgressLevelWorkspace, pv.EgressLevelSource} {
		c, _ := dataegress.WriteCapability(lv)
		a.CapabilityScopes = append(a.CapabilityScopes, c)
	}
	return a
}

func (w *world) putPolicy(sc contracts.Scope, level pv.EgressLevel) {
	w.t.Helper()
	in := dataegress.PolicyInput{
		Level: level, Enabled: true, SourceMode: pv.EgressModeAllowExcerpt,
		AllowedProviders: []pv.ProviderID{egProvider},
		AllowedModels:    []pv.ModelRef{{ProviderID: egProvider, ModelID: egModel}},
		AllowedClasses:   []pv.EgressEligibleClass{pv.EgressEligibleClassPublic, pv.EgressEligibleClassInternal},
		AllowedRegions:   []pv.RegionCode{"us", "eu"},
	}
	var err error
	if level == pv.EgressLevelDeployment {
		_, err = w.eg.PutDeploymentPolicy(ctxb(), dataegress.Operator{DeploymentID: sc.DeploymentID, PrincipalID: "opr_root"}, in)
	} else {
		_, err = w.eg.PutPolicy(ctxb(), adminScope(sc), in)
	}
	if err != nil {
		w.t.Fatalf("PutPolicy(%s): %v", level, err)
	}
}

// putBudget sets the daily and monthly provider budget of the external provider's account.
func (w *world) putBudget(sc contracts.Scope, dailyMicro int64) {
	w.t.Helper()
	op := sc.Clone()
	op.PrincipalID, op.SessionID = "usr_budget_admin", "ses_budget_admin"
	prov := egProvider
	a := w.prof.byID[profA]
	_, err := w.budget.PutBudget(ctxb(), op, pv.Budget{
		Subject: pv.BudgetSubject{Kind: pv.BudgetSubjectKindProvider, ProviderID: &prov},
		Channel: pv.ChannelInteractive, Currency: a.Pricing.Currency, WarnAtPercent: 80,
		Limits: pv.BudgetLimits{DailyMicroUnits: pv.MicroUnits(dailyMicro), MonthlyMicroUnits: pv.MicroUnits(dailyMicro)},
	})
	if err != nil {
		w.t.Fatalf("PutBudget: %v", err)
	}
}

// ---- scripting and driving ----

// proposeBook scripts the external provider's next proposal prompt: a tool call booking Luca Ferri.
func (w *world) proposeBook() {
	w.t.Helper()
	w.ext.On(offersAnyTool()).ProposeToolCall("call_1", "appointments_book", bookArg).Times(1)
}

// localProposeBook scripts the LOCAL runtime's proposal: the same booking as a TaskProposal.
func (w *world) localProposeBook() {
	w.t.Helper()
	w.local.On(pfake.Any()).ReplyJSON(map[string]any{
		"capability_id": "appointments.book",
		"fields":        map[string]any{"patient": "Luca Ferri", "slot": "2026-10-14 10:30"},
		"entity_refs":   []any{}, "intent": "task", "uncertainty": map[string]any{"overall": 0.6},
	})
}

func offersAnyTool() pfake.Matcher {
	return func(c pfake.Call) bool { return c.Rich != nil && len(c.Rich.Tools) > 0 }
}

func (w *world) say(sc contracts.Scope, text string) dialogue.Result {
	w.t.Helper()
	res, err := w.eng.Turn(ctxb(), sc, dialogue.TurnInput{Text: text, Language: "en"})
	if err != nil {
		w.t.Fatalf("Turn(%q): %v", text, err)
	}
	return res
}

func (w *world) confirm(sc contracts.Scope, prep dialogue.Result) dialogue.Result {
	w.t.Helper()
	res, err := w.eng.Turn(ctxb(), sc, dialogue.TurnInput{Confirm: &dialogue.Confirmation{
		PreparationID: string(prep.Prepared.PreparationID), Token: prep.Confirmation,
	}})
	if err != nil {
		w.t.Fatalf("confirm: %v", err)
	}
	return res
}

func wantAct(t testing.TB, res dialogue.Result, a string) {
	t.Helper()
	if string(res.Directive.Act) != a {
		t.Fatalf("act = %s (code %q, response %q), want %s", res.Directive.Act, res.Code, res.Response.Text, a)
	}
}

func (w *world) lastOutcome() modes.Outcome {
	w.t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.outcomes) == 0 {
		w.t.Fatal("no routed outcome was recorded: the request never reached the router")
	}
	return w.outcomes[len(w.outcomes)-1]
}

func (w *world) routed() int { w.mu.Lock(); defer w.mu.Unlock(); return len(w.outcomes) }

// nothingExecuted fails unless the failed turn left the host untouched: no Prepare, no Commit, no receipt,
// nothing prepared, no preparation stored. A failed model call must never be mistaken for progress.
func (w *world) nothingExecuted(res dialogue.Result) {
	w.t.Helper()
	_, prepares, commits := w.counter.counts()
	if prepares != 0 || commits != 0 {
		w.t.Fatalf("the host saw %d Prepare and %d Commit calls from a failed request", prepares, commits)
	}
	if res.Prepared != nil || res.Receipt != nil {
		w.t.Fatalf("prepared %+v receipt %+v: a failed request must not look like progress", res.Prepared, res.Receipt)
	}
	var n int
	if err := w.db.QueryRowContext(ctxb(), `SELECT count(*) FROM action_preparations`).Scan(&n); err != nil || n != 0 {
		w.t.Fatalf("%d preparations stored (err %v)", n, err)
	}
}

func (w *world) failedWith(res dialogue.Result, code contracts.ErrCode) {
	w.t.Helper()
	if !res.Failed() || res.Code != code {
		w.t.Fatalf("code = %q (act %s), want %s", res.Code, res.Directive.Act, code)
	}
}

// goal returns the open goal of the receptionist's session, or nil.
func (w *world) goal() *sessions.Goal {
	w.t.Helper()
	g, err := w.store.Open(ctxb(), receptionist())
	if err != nil {
		w.t.Fatal(err)
	}
	return g.State.Goal
}

func confirmTurn(prep dialogue.Result) dialogue.TurnInput {
	return dialogue.TurnInput{Confirm: &dialogue.Confirmation{PreparationID: string(prep.Prepared.PreparationID), Token: prep.Confirmation}}
}

// routeDirect sends one controller request through the world's router, below the dialogue engine, so the typed
// outcome of a failure that the engine's guard turns into a refusal can be read as the router decided it. It
// reports what the executors saw.
func (w *world) routeDirect(commitCapable bool) (out modes.Outcome, err error, sent, localRuns int) {
	w.t.Helper()
	req := modes.Request{
		Scope: receptionist(), Role: modes.RoleController, CommitCapable: commitCapable,
		Requirement: providerreg.Requirement{DataClass: pv.DataClassificationInternal},
		Estimate: func(pv.ProviderProfile) (providerbudget.Units, pv.MicroUnits, error) {
			return providerbudget.Units{ToksIn: 10, ToksOut: 5}, 100, nil
		},
	}
	out, err = w.rt.Router().Execute(ctxb(), req, modes.Executors{
		Local: func(context.Context) error { localRuns++; return nil },
		External: func(context.Context, modes.ExternalCall) (modes.ExternalResult, error) {
			sent++
			return modes.ExternalResult{Metering: providerbudget.Units{ToksIn: 8, ToksOut: 4}}, nil
		},
	})
	return out, err, sent, localRuns
}
