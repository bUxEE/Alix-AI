package hybrid

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/dialogue/extmode"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
	conv "github.com/buxee/alix-ai/tests/conversation"
)

// The rig of these tests: the real data-egress service over a real store and audit log, the real
// router, and scripted fakes for the two model runtimes. No test makes a real provider call.

var rigNow = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

const testProfile pv.ProfileID = "pprf_01J0000000000000000000000P"

func ctxb() context.Context { return context.Background() }

func rigScope() contracts.Scope { return conv.RoleScope(conv.RoleReceptionist) }

// ---- fakes of the router's collaborators ----

type fakeProfiles struct{ prof pv.ProviderProfile }

func (f fakeProfiles) Get(context.Context, contracts.Scope, pv.ProfileID) (pv.ProviderProfile, error) {
	return f.prof, nil
}

func (f fakeProfiles) Check(context.Context, contracts.Scope, pv.ProfileID, providerreg.Requirement) (providerreg.Admission, error) {
	return providerreg.Admission{Profile: f.prof}, nil
}

func (f fakeProfiles) Fallback(context.Context, contracts.Scope, pv.ProfileID, providerreg.FallbackCondition, providerreg.Requirement) (providerreg.FallbackRecord, providerreg.Admission, error) {
	return providerreg.FallbackRecord{}, providerreg.Admission{}, contracts.New(contracts.ErrPermissionDenied, "no fallback named")
}

type fakeBudgets struct {
	mu       sync.Mutex
	refuse   error
	reserved []providerbudget.Call
	settled  []providerbudget.Actual
}

func (b *fakeBudgets) Reserve(_ context.Context, _ contracts.Scope, c providerbudget.Call) (providerbudget.Reservation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.refuse != nil {
		return providerbudget.Reservation{}, b.refuse
	}
	b.reserved = append(b.reserved, c)
	return providerbudget.Reservation{ID: "rsv_01J0000000000000000000000R"}, nil
}

func (b *fakeBudgets) Settle(_ context.Context, _ contracts.Scope, _ pv.ReservationID, a providerbudget.Actual) (providerbudget.Settlement, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.settled = append(b.settled, a)
	return providerbudget.Settlement{}, nil
}

func (b *fakeBudgets) States(context.Context, contracts.Scope, pv.ProviderID, pv.Channel) ([]pv.BudgetState, error) {
	return nil, nil
}

type fakeLocal map[modes.Role]bool

func (l fakeLocal) Usable(r modes.Role) bool { return l[r] }

// countingGuard counts the requests that reached the egress guard.
type countingGuard struct {
	dataegress.Guard
	mu sync.Mutex
	n  int
}

func (g *countingGuard) Evaluate(ctx context.Context, c dataegress.Candidate) (dataegress.Result, error) {
	g.mu.Lock()
	g.n++
	g.mu.Unlock()
	return g.Guard.Evaluate(ctx, c)
}

func (g *countingGuard) count() int { g.mu.Lock(); defer g.mu.Unlock(); return g.n }

// recordingSender wraps the scripted external provider and keeps what the sender was given.
type recordingSender struct {
	gen  providers.MultimodalGenerator
	mu   sync.Mutex
	reqs []extmode.SendRequest
}

func (s *recordingSender) Send(ctx context.Context, scope contracts.Scope, req extmode.SendRequest) (providers.RichResult, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	return s.gen.Generate(ctx, scope, req.Rich)
}

func (s *recordingSender) sent() []extmode.SendRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]extmode.SendRequest(nil), s.reqs...)
}

// recorder keeps the transitions and also appends them to the real audit log.
type recorder struct {
	inner Recorder
	mu    sync.Mutex
	got   []Transition
}

func (r *recorder) Record(ctx context.Context, scope contracts.Scope, t Transition) error {
	r.mu.Lock()
	r.got = append(r.got, t)
	r.mu.Unlock()
	return r.inner.Record(ctx, scope, t)
}

func (r *recorder) all() []Transition {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Transition(nil), r.got...)
}

// fakeUsage stands in for the budget service's usage export.
type fakeUsage struct {
	filter providerbudget.UsageFilter
	export providerbudget.UsageExport
}

func (u *fakeUsage) ExportUsage(_ context.Context, _ contracts.Scope, f providerbudget.UsageFilter) (providerbudget.UsageExport, error) {
	u.filter = f
	return u.export, nil
}

// ---- the world ----

type world struct {
	t       testing.TB
	clk     *clock.Fake
	alog    *audit.Log
	eg      *dataegress.Service
	guard   *countingGuard
	modes   *modes.MemoryModes
	local   *pfake.Provider
	ext     *pfake.Provider
	sender  *recordingSender
	budgets *fakeBudgets
	usage   *fakeUsage
	rec     *recorder
	prof    pv.ProviderProfile
	locals  fakeLocal
	policy  modes.StaticPolicy
	h       *Hybrid
	scope   contracts.Scope
}

type worldOpts struct {
	mode      modes.Mode
	factClass pv.DataClassification
	policy    modes.StaticPolicy
	noLocal   bool
}

type worldOpt func(*worldOpts)

func withMode(m modes.Mode) worldOpt                 { return func(o *worldOpts) { o.mode = m } }
func withFactClass(c pv.DataClassification) worldOpt { return func(o *worldOpts) { o.factClass = c } }
func withPolicy(p modes.StaticPolicy) worldOpt       { return func(o *worldOpts) { o.policy = p } }
func withoutLocalModel() worldOpt                    { return func(o *worldOpts) { o.noLocal = true } }

func loadProfile(t testing.TB, scope contracts.Scope) pv.ProviderProfile {
	t.Helper()
	raw, err := os.ReadFile("../../../schemas/providers/examples/provider-profile/valid/qualified-text-only.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var p pv.ProviderProfile
	if err := dec.Decode(&p); err != nil {
		t.Fatal(err)
	}
	p.PermittedScopes = []pv.ProviderProfileScopeGrant{{ApplicationID: common.Identifier(scope.ApplicationID)}}
	return p
}

func newWorld(t testing.TB, opts ...worldOpt) *world {
	t.Helper()
	o := worldOpts{mode: modes.ModeHybrid, factClass: pv.DataClassificationInternal}
	for _, f := range opts {
		f(&o)
	}
	w := &world{t: t, clk: clock.NewFake(rigNow), scope: rigScope()}
	db := storetest.Open(t, nil)
	var err error
	if w.alog, err = audit.New(db, audit.Options{Clock: w.clk}); err != nil {
		t.Fatal(err)
	}
	if w.eg, err = dataegress.New(dataegress.Deps{DB: db, Clock: w.clk, Audit: w.alog, IDs: ids.NewGenerator(w.clk)}); err != nil {
		t.Fatal(err)
	}
	w.guard = &countingGuard{Guard: w.eg}
	w.prof = loadProfile(t, w.scope)
	w.local = pfake.New(pfake.WithClock(w.clk), pfake.WithName("local"))
	w.ext = pfake.New(pfake.WithClock(w.clk), pfake.WithName("external"), pfake.WithModel("gpt-4.1-mini"))
	w.sender = &recordingSender{gen: w.ext}
	w.budgets = &fakeBudgets{}
	w.usage = &fakeUsage{}
	w.rec = &recorder{inner: AuditRecorder{Log: w.alog}}
	w.modes = modes.NewMemoryModes()
	if o.mode != modes.ModeLocal {
		for _, lv := range []modes.Level{modes.LevelDeployment, modes.LevelApplication, modes.LevelTenant} {
			if err := w.modes.Set(lv, w.scope, o.mode); err != nil {
				t.Fatal(err)
			}
		}
	}
	w.enableEgress()
	w.locals = fakeLocal{modes.RoleController: true, modes.RoleRealisation: true, modes.RoleEmbedder: true, modes.RoleSTT: true, modes.RoleTTS: true}
	w.policy = o.policy
	if w.policy == nil {
		// The operator also lists an external profile for the controller: Hybrid must ignore it.
		w.policy = modes.StaticPolicy{
			modes.RoleRealisation: RealisationPolicy(testProfile),
			modes.RoleController:  {Profiles: []pv.ProfileID{testProfile}},
		}
	}
	cfg := Config{
		Modes: w.modes, KillSwitch: w.eg, Policy: w.policy, LocalProviders: w.locals,
		Profiles: fakeProfiles{prof: w.prof}, Egress: w.guard, Budgets: w.budgets, Sender: w.sender,
		Budget:    providers.Budget{MaxPromptTokens: 16384, MaxOutputTokens: 2048, MaxBatchItems: 4, Timeout: time.Minute},
		FactClass: o.factClass, Recorder: w.rec, Usage: w.usage, Clock: w.clk,
	}
	if !o.noLocal {
		cfg.Local = w.local
	}
	if w.h, err = New(cfg); err != nil {
		t.Fatal(err)
	}
	return w
}

// ---- data-egress policy (as the extmode tests write it) ----

const (
	egProvider = pv.ProviderID("openai")
	egModel    = pv.ModelID("gpt-4.1-mini")
)

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

func basePolicy(level pv.EgressLevel) dataegress.PolicyInput {
	return dataegress.PolicyInput{
		Level: level, Enabled: true, SourceMode: pv.EgressModeAllowExcerpt,
		AllowedProviders: []pv.ProviderID{egProvider},
		AllowedModels:    []pv.ModelRef{{ProviderID: egProvider, ModelID: egModel}},
		AllowedClasses:   []pv.EgressEligibleClass{pv.EgressEligibleClassPublic, pv.EgressEligibleClassInternal},
		AllowedRegions:   []pv.RegionCode{"us", "eu"},
	}
}

func (w *world) putPolicy(level pv.EgressLevel, source pv.SourceID, mut func(*dataegress.PolicyInput)) {
	w.t.Helper()
	in := basePolicy(level)
	in.SourceID = source
	if mut != nil {
		mut(&in)
	}
	ad := adminScope(w.scope)
	var err error
	if level == pv.EgressLevelDeployment {
		_, err = w.eg.PutDeploymentPolicy(ctxb(), dataegress.Operator{DeploymentID: w.scope.DeploymentID, PrincipalID: "opr_root"}, in)
	} else {
		_, err = w.eg.PutPolicy(ctxb(), ad, in)
	}
	if err != nil {
		w.t.Fatalf("PutPolicy(%s): %v", level, err)
	}
}

func (w *world) enableEgress() {
	w.t.Helper()
	for _, lv := range []pv.EgressLevel{pv.EgressLevelDeployment, pv.EgressLevelApplication, pv.EgressLevelTenant, pv.EgressLevelWorkspace} {
		w.putPolicy(lv, "", nil)
	}
}

// newSource registers a source policy and returns its id.
func (w *world) newSource(mode pv.EgressMode, class pv.DataClassification) pv.SourceID {
	w.t.Helper()
	id := pv.SourceID(ids.New("src"))
	cls := class
	w.putPolicy(pv.EgressLevelSource, id, func(in *dataegress.PolicyInput) {
		in.SourceMode, in.SourceClass = mode, &cls
		in.AllowedClasses = []pv.EgressEligibleClass{pv.EgressEligibleClass(class)}
	})
	return id
}

// ---- directives ----

const (
	turnID    = "trn_01J0000000000000000000000A"
	receiptID = "rcpt_01J0000000000000000000000A"
	prepID    = "prep_01J0000000000000000000000A"
)

func ptr[T any](v T) *T { return &v }

func citation(id pv.SourceID, name string, claims ...string) conversation.Citation {
	return conversation.Citation{
		SourceID: actions.SourceID(id), DisplayName: name, Version: "rev-12",
		Availability: conversation.CitationAvailabilityAvailable, Staleness: conversation.CitationStalenessCurrent,
		OpenRef: conversation.CitationOpenRef{Value: conversation.CitationOpenRefSource{Kind: "source"}},
		Claims:  claims,
	}
}

func textFact(field, v string) actions.Fact {
	return actions.Fact{Field: common.FieldName(field), Value: actions.CanonicalValue{Value: actions.TextValue{Value: v}}}
}

func dateFact(field, v string) actions.Fact {
	return actions.Fact{Field: common.FieldName(field), Value: actions.CanonicalValue{Value: actions.DateValue{Value: v}}}
}

func moneyFact(field, amount, cur string) actions.Fact {
	return actions.Fact{Field: common.FieldName(field), Value: actions.CanonicalValue{Value: actions.MoneyValue{Value: amount, Currency: cur}}}
}

// resultDirective is a present_result directive: a doctor, a date and a fee that must be stated.
func resultDirective(srcs ...conversation.Citation) *conversation.DialogueDirective {
	capID := common.CapabilityID("appointments.book")
	return &conversation.DialogueDirective{
		TurnID: actions.TurnID(turnID), CapabilityID: &capID, Act: conversation.DialogueDirectiveActPresentResult,
		Facts:        []actions.Fact{textFact("doctor", "Dr. Bianchi"), dateFact("appointment_date", "2026-10-14"), moneyFact("fee", "50.00", "EUR")},
		MustState:    []common.FieldName{"doctor", "appointment_date", "fee"},
		Confirmation: actions.ConfirmationRequirement{Mode: actions.ConfirmationRequirementModeNone},
		Sources:      srcs,
	}
}

// outcomeDirective is a report_outcome directive with its committed receipt.
func outcomeDirective() (*conversation.DialogueDirective, *actions.ActionReceipt) {
	capID := common.CapabilityID("appointments.book")
	rid := actions.ReceiptID(receiptID)
	d := &conversation.DialogueDirective{
		TurnID: actions.TurnID(turnID), CapabilityID: &capID, Act: conversation.DialogueDirectiveActReportOutcome, ReceiptID: &rid,
		Facts:        []actions.Fact{textFact("doctor", "Dr. Bianchi"), dateFact("appointment_date", "2026-10-14")},
		MustState:    []common.FieldName{"doctor", "appointment_date"},
		Confirmation: actions.ConfirmationRequirement{Mode: actions.ConfirmationRequirementModeNone},
	}
	r := &actions.ActionReceipt{
		ReceiptID: rid, PreparationID: actions.PreparationID(prepID), CapabilityID: "appointments.book",
		Status: actions.ActionReceiptStatusCommitted, Facts: d.Facts,
		AuditRef: "aud_01J0000000000000000000000A", IssuedAt: "2026-10-12T08:03:00Z",
	}
	return d, r
}

var enOpts = verify.Options{Language: verify.LangEN}

func (w *world) turn(d *conversation.DialogueDirective, r *actions.ActionReceipt) Turn {
	return Turn{Scope: w.scope, Directive: d, Receipt: r, Options: enOpts}
}

// rendered is the deterministic text of a directive: it is guaranteed to verify.
func rendered(t testing.TB, d *conversation.DialogueDirective, r *actions.ActionReceipt) string {
	t.Helper()
	s, err := verify.Render(d, r, enOpts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// ---- scripting and reading ----

// reply scripts the next external reply.
func (w *world) reply(text string) *pfake.Rule { return w.ext.On(pfake.Any()).Reply(text).Times(1) }

// localReply scripts the next local reply.
func (w *world) localReply(text string) { w.local.On(pfake.Any()).Reply(text).Times(1) }

// sentText is everything the external provider was sent, as one string.
func (w *world) sentText() string {
	var b strings.Builder
	for _, p := range w.ext.Prompts() {
		b.WriteString(strings.Join(p.Texts(), "\n"))
		for _, m := range p.Rich.Messages {
			for _, part := range m.Parts {
				b.WriteString("\n" + part.Text)
			}
		}
	}
	return b.String()
}

// categories lists the egress categories of the parts the sender was given.
func (w *world) categories() []string {
	var out []string
	for _, rq := range w.sender.sent() {
		for _, m := range rq.Call.Envelope.Payload.Messages {
			for _, p := range m.Parts {
				switch v := p.Value.(type) {
				case pv.RequestEnvelopeTextPart:
					out = append(out, string(v.Internal.Category))
				case *pv.RequestEnvelopeTextPart:
					out = append(out, string(v.Internal.Category))
				}
			}
		}
	}
	return out
}

// auditedTransitions reads the hybrid.transition events back out of the real audit log.
func (w *world) auditedTransitions() []Transition {
	w.t.Helper()
	sc := w.scope.Clone()
	sc.CapabilityScopes = append(sc.CapabilityScopes, audit.CapabilityExport)
	var buf bytes.Buffer
	if _, err := w.alog.Export(ctxb(), sc, &buf, audit.ExportOptions{Types: []audit.EventType{EventTransition.Name()}}); err != nil {
		w.t.Fatalf("export audit: %v", err)
	}
	var out []Transition
	dec := json.NewDecoder(&buf)
	for dec.More() {
		var ev audit.Event
		if err := dec.Decode(&ev); err != nil {
			w.t.Fatal(err)
		}
		var tr Transition
		if err := json.Unmarshal(ev.Data, &tr); err != nil {
			w.t.Fatal(err)
		}
		out = append(out, tr)
	}
	return out
}

func (w *world) realise(t Turn) Realised {
	w.t.Helper()
	r, err := w.h.Realise(ctxb(), t)
	if err != nil {
		w.t.Fatalf("Realise: %v", err)
	}
	return r
}
