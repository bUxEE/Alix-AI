package extmode

import (
	"context"
	"encoding/json"
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
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/rag/answer"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	actgen "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
	conv "github.com/buxee/alix-ai/tests/conversation"
)

// The world of these tests: the clinic reference application's signed package behind its stand-in
// host, the real policy gateway, action coordinator, session store and dialogue engine, the real
// data-egress service over a real store and audit log, and two scripted fake providers: the local
// runtime (which a test asserts is never called in external mode) and the external provider.

const clinicPackageDir = "../../../examples/reference-apps/clinic-booking/package"

var clinicNow = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

func ctxb() context.Context { return context.Background() }

type activePackage struct{ capabilities.Package }

func (activePackage) Status() policy.PackageStatus { return policy.StatusActive }

func loadPackage(t testing.TB) capabilities.Package {
	t.Helper()
	res, err := apppkg.Load(ctxb(), apppkg.FromDir(clinicPackageDir), apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
	if err != nil || res.Package == nil {
		t.Fatalf("load package: %v %v", err, res.Findings)
	}
	return activePackage{capabilities.FromApppkg(res.Package)}
}

// countingAdapter counts what reaches the host's operations.
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

// ---- fakes of the router's collaborators ----

const testProfile pv.ProfileID = "pprf_01J0000000000000000000000P"

type fakeProfiles struct{ prof pv.ProviderProfile }

func (f fakeProfiles) Get(context.Context, contracts.Scope, pv.ProfileID) (pv.ProviderProfile, error) {
	return f.prof, nil
}

func (f fakeProfiles) Check(_ context.Context, _ contracts.Scope, id pv.ProfileID, _ providerreg.Requirement) (providerreg.Admission, error) {
	return providerreg.Admission{Profile: f.prof}, nil
}

func (f fakeProfiles) Fallback(context.Context, contracts.Scope, pv.ProfileID, providerreg.FallbackCondition, providerreg.Requirement) (providerreg.FallbackRecord, providerreg.Admission, error) {
	return providerreg.FallbackRecord{}, providerreg.Admission{}, contracts.New(contracts.ErrPermissionDenied, "no fallback named")
}

type fakeBudgets struct {
	mu       sync.Mutex
	reserved []providerbudget.Call
	settled  int
}

func (b *fakeBudgets) Reserve(_ context.Context, _ contracts.Scope, c providerbudget.Call) (providerbudget.Reservation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reserved = append(b.reserved, c)
	return providerbudget.Reservation{ID: "rsv_01J0000000000000000000000R"}, nil
}

func (b *fakeBudgets) Settle(context.Context, contracts.Scope, pv.ReservationID, providerbudget.Actual) (providerbudget.Settlement, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.settled++
	return providerbudget.Settlement{}, nil
}

func (b *fakeBudgets) States(context.Context, contracts.Scope, pv.ProviderID, pv.Channel) ([]pv.BudgetState, error) {
	return nil, nil
}

type fakeLocal struct{}

func (fakeLocal) Usable(modes.Role) bool { return true }

// recordingSender wraps the scripted external provider and records what the sender was given.
type recordingSender struct {
	gen  providers.MultimodalGenerator
	mu   sync.Mutex
	reqs []SendRequest
}

func (s *recordingSender) Send(ctx context.Context, scope contracts.Scope, req SendRequest) (providers.RichResult, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	return s.gen.Generate(ctx, scope, req.Rich)
}

func (s *recordingSender) sent() []SendRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SendRequest(nil), s.reqs...)
}

// ---- the world ----

type world struct {
	t       testing.TB
	clk     *clock.Fake
	db      *store.DB
	alog    *audit.Log
	gw      *policy.Gateway
	host    *conv.ClinicHost
	counter *countingAdapter
	pkg     capabilities.Package
	store   *sessions.Store
	eg      *dataegress.Service
	modes   *modes.MemoryModes
	local   *pfake.Provider
	ext     *pfake.Provider
	sender  *recordingSender
	budgets *fakeBudgets
	rt      *Runtime
	eng     *dialogue.Engine
	prof    pv.ProviderProfile
	// the RAG side
	retr      *fakeRetriever
	catalog   *fakeCatalog
	know      *Knowledge
	localAns  *answer.Answerer
	outcomes  []modes.Outcome
	outcomeMu sync.Mutex
}

type worldOpts struct {
	mode            modes.Mode
	directiveClass  string
	modelResponses  bool
	features        Features
	convenience     []Feature
	noEgressPolicy  bool
	withKnowledge   bool
	externalProfile func(*pv.ProviderProfile)
}

type worldOpt func(*worldOpts)

func withMode(m modes.Mode) worldOpt       { return func(o *worldOpts) { o.mode = m } }
func withDirectiveClass(c string) worldOpt { return func(o *worldOpts) { o.directiveClass = c } }
func withModelResponses() worldOpt         { return func(o *worldOpts) { o.modelResponses = true } }
func withoutEgressPolicy() worldOpt        { return func(o *worldOpts) { o.noEgressPolicy = true } }
func withKnowledge() worldOpt              { return func(o *worldOpts) { o.withKnowledge = true } }
func withFeatures(f Features, c ...Feature) worldOpt {
	return func(o *worldOpts) { o.features, o.convenience = f, c }
}

func allActs() []conversation.DialogueDirectiveAct { return []conversation.DialogueDirectiveAct{} }

func loadProfile(t testing.TB) pv.ProviderProfile {
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
	p.PermittedScopes = []pv.ProviderProfileScopeGrant{{ApplicationID: common.Identifier(conv.RoleScope(conv.RoleReceptionist).ApplicationID)}}
	return p
}

func newWorld(t testing.TB, opts ...worldOpt) *world {
	t.Helper()
	o := worldOpts{mode: modes.ModeExternal}
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
		DB: w.db, Policy: w.gw, Audit: w.alog,
		Packages: actions.PackageSourceFunc(func(context.Context, string) (policy.ActivePackage, error) { return w.pkg, nil }),
		Adapters: adapterSrc, Clock: w.clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	if w.store, err = sessions.New(w.db, w.clk, sessions.Config{}); err != nil {
		t.Fatal(err)
	}

	// External mode pieces.
	if w.eg, err = dataegress.New(dataegress.Deps{DB: w.db, Clock: w.clk, Audit: w.alog, IDs: ids.NewGenerator(w.clk)}); err != nil {
		t.Fatal(err)
	}
	w.prof = loadProfile(t)
	if o.externalProfile != nil {
		o.externalProfile(&w.prof)
	}
	w.local = pfake.New(pfake.WithClock(w.clk), pfake.WithName("local"))
	w.ext = pfake.New(pfake.WithClock(w.clk), pfake.WithName("external"), pfake.WithModel("gpt-4.1-mini"))
	w.sender = &recordingSender{gen: w.ext}
	w.budgets = &fakeBudgets{}
	w.modes = modes.NewMemoryModes()
	sc := conv.RoleScope(conv.RoleReceptionist)
	if o.mode != modes.ModeLocal {
		for _, lv := range []modes.Level{modes.LevelDeployment, modes.LevelApplication, modes.LevelTenant} {
			if err := w.modes.Set(lv, sc, o.mode); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !o.noEgressPolicy {
		w.enableEgress(sc)
	}
	budget := providers.Budget{MaxPromptTokens: 16384, MaxOutputTokens: 2048, MaxBatchItems: 4, Timeout: time.Minute}
	localSel, err := dialogue.LocalSelector(w.local, providers.Budget{MaxPromptTokens: 4096, MaxOutputTokens: 1024, MaxBatchItems: 4, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	pkgSource := dialogue.PackageSourceFunc(func(context.Context, string) (capabilities.Package, error) { return w.pkg, nil })
	w.rt, err = New(Config{
		Local: localSel, Modes: w.modes, KillSwitch: w.eg,
		Policy:         modes.StaticPolicy{modes.RoleController: {Profiles: []pv.ProfileID{testProfile}}},
		LocalProviders: fakeLocal{}, Profiles: fakeProfiles{prof: w.prof}, Egress: w.eg, Budgets: w.budgets,
		Packages: pkgSource, Sender: w.sender, Budget: budget, Clock: w.clk,
		Features: o.features, Convenience: o.convenience, DirectiveClass: o.directiveClass,
		OnOutcome: func(_ contracts.Scope, out modes.Outcome) {
			w.outcomeMu.Lock()
			w.outcomes = append(w.outcomes, out)
			w.outcomeMu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	deps := dialogue.Deps{
		Sessions: w.store, Registry: reg, Packages: pkgSource, Policy: w.gw, Actions: co,
		Adapters: adapterSrc, Providers: w.rt, Guard: w.rt, Audit: w.alog, Clock: w.clk,
	}
	if !o.modelResponses {
		deps.Config.TemplateOnly = []conversation.DialogueDirectiveAct{
			conversation.DialogueDirectiveActAskMissing, conversation.DialogueDirectiveActAskInvalid,
			conversation.DialogueDirectiveActDisambiguate, conversation.DialogueDirectiveActRequestConfirmation,
			conversation.DialogueDirectiveActRequestStepUp, conversation.DialogueDirectiveActPresentResult,
			conversation.DialogueDirectiveActReportOutcome, conversation.DialogueDirectiveActReportFailure,
			conversation.DialogueDirectiveActRefuse, conversation.DialogueDirectiveActOfferHelp,
			conversation.DialogueDirectiveActAcknowledgeCancel, conversation.DialogueDirectiveActReportNoEvidence,
		}
	} else {
		deps.Config.TemplateOnly = []conversation.DialogueDirectiveAct{conversation.DialogueDirectiveActRefuse}
	}
	if w.eng, err = dialogue.New(deps); err != nil {
		t.Fatal(err)
	}
	if o.withKnowledge {
		w.setupKnowledge()
	}
	return w
}

// ---- data-egress policy ----

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

func (w *world) putPolicy(sc contracts.Scope, level pv.EgressLevel, source pv.SourceID, mut func(*dataegress.PolicyInput)) {
	w.t.Helper()
	in := basePolicy(level)
	in.SourceID = source
	if mut != nil {
		mut(&in)
	}
	ad := adminScope(sc)
	var cur pv.EgressPolicy
	var found bool
	var err error
	if level == pv.EgressLevelDeployment {
		cur, found, err = w.eg.GetPolicy(ctxb(), ad, level, "")
	} else {
		cur, found, err = w.eg.GetPolicy(ctxb(), ad, level, source)
	}
	if err != nil {
		w.t.Fatalf("GetPolicy(%s): %v", level, err)
	}
	if found {
		in.ExpectedRevision = cur.Revision
	}
	if level == pv.EgressLevelDeployment {
		_, err = w.eg.PutDeploymentPolicy(ctxb(), dataegress.Operator{DeploymentID: sc.DeploymentID, PrincipalID: "opr_root"}, in)
	} else {
		_, err = w.eg.PutPolicy(ctxb(), ad, in)
	}
	if err != nil {
		w.t.Fatalf("PutPolicy(%s): %v", level, err)
	}
}

// enableEgress writes the baseline policy at the four request levels of the scope.
func (w *world) enableEgress(sc contracts.Scope) {
	w.t.Helper()
	for _, lv := range []pv.EgressLevel{pv.EgressLevelDeployment, pv.EgressLevelApplication, pv.EgressLevelTenant, pv.EgressLevelWorkspace} {
		w.putPolicy(sc, lv, "", nil)
	}
}

// newSource registers a source policy and returns its id.
func (w *world) newSource(sc contracts.Scope, mode pv.EgressMode, class pv.DataClassification) pv.SourceID {
	w.t.Helper()
	id := pv.SourceID(ids.New("src"))
	cls := class
	w.putPolicy(sc, pv.EgressLevelSource, id, func(in *dataegress.PolicyInput) {
		in.SourceMode, in.SourceClass = mode, &cls
		in.AllowedClasses = []pv.EgressEligibleClass{pv.EgressEligibleClass(class)}
	})
	return id
}

// ---- scripting and driving ----

func receptionist() contracts.Scope { return conv.RoleScope(conv.RoleReceptionist) }

// tenantTwo is another tenant of the same application and deployment.
func tenantTwo() contracts.Scope {
	s := receptionist()
	s.TenantID = "grp-other-9921"
	s.PrincipalID = "usr-other-9921"
	s.SessionID = "ses-other-9921"
	return s
}

// toolCall scripts the external provider's reply to a proposal prompt: a tool call proposal.
func (w *world) toolCall(tool, args string) {
	w.t.Helper()
	w.ext.On(offersAnyTool()).ProposeToolCall("call_1", tool, args).Times(1)
}

// offersAnyTool matches a request that offers at least one tool.
func offersAnyTool() pfake.Matcher {
	return func(c pfake.Call) bool { return c.Rich != nil && len(c.Rich.Tools) > 0 }
}

// respond scripts the external provider's reply to a response prompt (one with no tools).
func (w *world) respond(text string) {
	w.t.Helper()
	w.ext.On(pfake.Not(offersAnyTool())).Reply(text).Times(1)
}

func (w *world) say(sc contracts.Scope, text string, atts ...dialogue.Attachment) dialogue.Result {
	w.t.Helper()
	res, err := w.eng.Turn(ctxb(), sc, dialogue.TurnInput{Text: text, Language: "en", Attachments: atts})
	if err != nil {
		w.t.Fatalf("Turn(%q): %v", text, err)
	}
	return res
}

func wantAct(t testing.TB, res dialogue.Result, a string) {
	t.Helper()
	if string(res.Directive.Act) != a {
		t.Fatalf("act = %s (code %q, response %q), want %s", res.Directive.Act, res.Code, res.Response.Text, a)
	}
}

// promptsContain reports whether any call the external provider received carries sub, anywhere.
func (w *world) promptsContain(sub string) bool {
	for _, p := range w.ext.Prompts() {
		if p.Contains(sub) {
			return true
		}
		for _, m := range p.Rich.Messages {
			for _, part := range m.Parts {
				if strings.Contains(part.Text, sub) {
					return true
				}
			}
		}
	}
	return false
}

func (w *world) lastOutcome() modes.Outcome {
	w.outcomeMu.Lock()
	defer w.outcomeMu.Unlock()
	if len(w.outcomes) == 0 {
		w.t.Fatal("no routed outcome was recorded")
	}
	return w.outcomes[len(w.outcomes)-1]
}

// ---- the RAG side ----

type fakeRetriever struct {
	mu  sync.Mutex
	pkg rag.EvidencePackage
	err error
}

func (f *fakeRetriever) Query(context.Context, contracts.Scope, query.Request) (query.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return query.Result{Package: f.pkg}, f.err
}

func (f *fakeRetriever) set(p rag.EvidencePackage) { f.mu.Lock(); f.pkg = p; f.mu.Unlock() }

type fakeCatalog struct {
	mu     sync.Mutex
	states map[string]query.SourceState
}

func (c *fakeCatalog) Sources(_ context.Context, sc contracts.Scope, ids []string) (map[string]query.SourceState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]query.SourceState{}
	for _, id := range ids {
		if st, ok := c.states[sc.TenantID+"/"+id]; ok {
			out[id] = st
		}
	}
	return out, nil
}

func (c *fakeCatalog) grant(sc contracts.Scope, pkg rag.EvidencePackage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, it := range pkg.Items {
		c.states[sc.TenantID+"/"+string(it.SourceID)] = query.SourceState{
			DisplayName: it.DisplayName, Authorised: true, Availability: it.Availability, Staleness: it.Staleness,
		}
	}
}

func (w *world) setupKnowledge() {
	w.t.Helper()
	w.retr = &fakeRetriever{}
	w.catalog = &fakeCatalog{states: map[string]query.SourceState{}}
	acfg := answer.DefaultConfig()
	var err error
	w.localAns, err = answer.New(answer.Deps{Generator: w.local, Catalog: w.catalog, Clock: w.clk}, acfg)
	if err != nil {
		w.t.Fatal(err)
	}
	extAns, err := answer.New(answer.Deps{Generator: w.rt.ExternalGenerator(), Catalog: w.catalog, Clock: w.clk, Resolver: w.localAns.Resolver()}, acfg)
	if err != nil {
		w.t.Fatal(err)
	}
	if w.know, err = w.rt.NewKnowledge(w.retr, w.localAns, extAns); err != nil {
		w.t.Fatal(err)
	}
	w.eng.SetKnowledge(w.know)
}

// evItem describes one evidence item of a test package.
type evItem struct {
	source string
	text   string
	id     pv.SourceID
	egress rag.SourceEgress
	class  rag.SourceClassification
}

// pkgFor builds the package the query pipeline would build for the scope.
func pkgFor(sc contracts.Scope, question string, items ...evItem) rag.EvidencePackage {
	out := make([]rag.EvidenceItem, len(items))
	for i, s := range items {
		eg, cl := s.egress, s.class
		loc := conversation.CitationLocator{Section: ptr("Section " + string(rune('A'+i)))}
		out[i] = rag.EvidenceItem{
			EvidenceID:   rag.EvidenceID("e" + string(rune('1'+i))),
			Rank:         int64(i + 1),
			ChunkID:      common.Identifier("chk_" + string(rune('1'+i))),
			GenerationID: rag.GenerationID(ids.New("gen")),
			SourceID:     actgen.SourceID(s.id),
			DisplayName:  s.source,
			Version:      actgen.OpaqueVersion("v1"),
			Locator:      &loc,
			Availability: conversation.CitationAvailabilityAvailable,
			Staleness:    conversation.CitationStalenessCurrent,
			OpenRef:      conversation.CitationOpenRef{Value: conversation.CitationOpenRefSource{Kind: "source"}},
			Text:         s.text, EstToks: rag.EstToks((len(s.text) + 2) / 3),
			SourceEgress: &eg, SourceClassification: &cl,
		}
	}
	return rag.EvidencePackage{
		PackageID: rag.EvidencePackageID(ids.New("evp")),
		Scope: rag.EvidencePackageScope{
			ApplicationID: common.Identifier(sc.ApplicationID), TenantID: common.Identifier(sc.TenantID),
			WorkspaceID: common.Identifier(sc.WorkspaceID), PrincipalID: common.Identifier(sc.PrincipalID),
		},
		Query:     rag.SearchRequestQuery(question),
		CreatedAt: common.Timestamp("2026-10-12T09:00:00.000Z"),
		Items:     out,
		Budget:    rag.EvidencePackageBudget{MaxItems: 8, MaxToks: 3000, EstToks: 100},
		Retrieval: rag.EvidencePackageRetrieval{Methods: []rag.RetrievalMethod{rag.RetrievalMethodFts}, Notices: rag.RetrievalNotices{}},
	}
}

func ptr[T any](v T) *T { return &v }
