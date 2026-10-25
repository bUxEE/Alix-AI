package extprovidersec

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

const (
	clinicPackageDir = "../../../examples/reference-apps/clinic-booking/package"
	profileFile      = "../../../schemas/providers/examples/provider-profile/valid/qualified-text-only.json"

	sayBook = "Book Luca Ferri for 2026-10-14 10:30"
	bookArg = `{"patient":"Luca Ferri","slot":"2026-10-14 10:30"}`
	sayFree = "Which appointments are free on 2026-10-14?"

	egProvider = pv.ProviderID("openai")
	egModel    = pv.ModelID("gpt-4.1-mini")
)

var clinicNow = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

func ctxb() context.Context { return context.Background() }

func receptionist() contracts.Scope { return conv.RoleScope(conv.RoleReceptionist) }

// tenantTwo is another tenant of the same application and deployment, with its own principal and session.
func tenantTwo() contracts.Scope {
	s := receptionist()
	s.TenantID = "grp-other-9921"
	s.PrincipalID = "usr-other-9921"
	s.SessionID = "ses-other-9921"
	return s
}

type activePackage struct{ capabilities.Package }

func (activePackage) Status() policy.PackageStatus { return policy.StatusActive }

// ---- the host, recorded ----

type hostCall struct {
	Scope        contracts.Scope
	CapabilityID string
	Fields       map[string]any
}

// recordingAdapter records every operation that reaches the host with the scope it arrived under, so a test reads
// what the host was asked to do and for whom.
type recordingAdapter struct {
	adapters.AppAdapter
	mu                      sync.Mutex
	reads, prepares, commit []hostCall
	refusePrepare           bool
}

func (c *recordingAdapter) Read(ctx context.Context, sc contracts.Scope, r adapters.ReadRequest) (actgen.DeterministicResult, error) {
	c.mu.Lock()
	c.reads = append(c.reads, hostCall{sc, r.CapabilityID, r.Fields})
	c.mu.Unlock()
	return c.AppAdapter.Read(ctx, sc, r)
}

func (c *recordingAdapter) Prepare(ctx context.Context, sc contracts.Scope, r adapters.PrepareRequest) (actgen.DeterministicResult, error) {
	c.mu.Lock()
	c.prepares = append(c.prepares, hostCall{sc, r.CapabilityID, r.Fields})
	c.mu.Unlock()
	return c.AppAdapter.Prepare(ctx, sc, r)
}

func (c *recordingAdapter) Commit(ctx context.Context, sc contracts.Scope, r adapters.CommitRequest) (actgen.DeterministicResult, error) {
	c.mu.Lock()
	c.commit = append(c.commit, hostCall{Scope: sc, CapabilityID: r.CapabilityID})
	c.mu.Unlock()
	return c.AppAdapter.Commit(ctx, sc, r)
}

func (c *recordingAdapter) counts() (reads, prepares, commits int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reads), len(c.prepares), len(c.commit)
}

func (c *recordingAdapter) preparedCalls() []hostCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]hostCall(nil), c.prepares...)
}

func (c *recordingAdapter) committedCalls() []hostCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]hostCall(nil), c.commit...)
}

// ---- the credential status source ----

// credStatus is the scripted credential service the registry asks whether a credential works.
type credStatus struct {
	mu     sync.Mutex
	status map[pv.CredentialRefID]pv.CredentialRefStatus
	err    error
}

func (f *credStatus) CredentialStatus(_ context.Context, _ contracts.Scope, ref pv.CredentialRefID) (pv.CredentialRefStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	if s, ok := f.status[ref]; ok {
		return s, nil
	}
	return pv.CredentialRefStatusInvalid, nil
}

func (f *credStatus) set(ref pv.CredentialRefID, s pv.CredentialRefStatus) {
	f.mu.Lock()
	f.status[ref] = s
	f.mu.Unlock()
}

// credRef returns a well-formed credential reference; n tells them apart. The reference is an opaque id, not a key.
func credRef(n byte) pv.CredentialRefID {
	return pv.CredentialRefID("cred_" + strings.Repeat("0", 25) + string(rune('A'+n))) // TEST-ONLY opaque reference id, not a key
}

// ---- the profile registry hooked ----

// hooked is the REAL profile registry with two hooks: onCheck runs before each Check (a test engages the kill
// switch or changes the credential there) and fallbacks counts the registry fallbacks asked for.
type hooked struct {
	*providerreg.Registry
	mu        sync.Mutex
	onCheck   func()
	checks    int
	fallbacks int
}

func (h *hooked) Check(ctx context.Context, sc contracts.Scope, id pv.ProfileID, r providerreg.Requirement) (providerreg.Admission, error) {
	h.mu.Lock()
	h.checks++
	f := h.onCheck
	h.mu.Unlock()
	if f != nil {
		f()
	}
	return h.Registry.Check(ctx, sc, id, r)
}

func (h *hooked) Fallback(ctx context.Context, sc contracts.Scope, from pv.ProfileID, c providerreg.FallbackCondition, r providerreg.Requirement) (providerreg.FallbackRecord, providerreg.Admission, error) {
	h.mu.Lock()
	h.fallbacks++
	h.mu.Unlock()
	return h.Registry.Fallback(ctx, sc, from, c, r)
}

func (h *hooked) hook(f func()) { h.mu.Lock(); h.onCheck = f; h.mu.Unlock() }

func (h *hooked) fallbackCount() int { h.mu.Lock(); defer h.mu.Unlock(); return h.fallbacks }

// ---- the budget ledger ----

// budgets is the real provider budget ledger (a store-backed Service) with one hook: reserveErr makes every
// reservation refuse with the given error, as a ledger that is rate limiting does.
type budgets struct {
	*providerbudget.Service
	mu         sync.Mutex
	reserveErr error
	reserved   int
	after      func() // runs after a reservation was granted, before the call is sent
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
		after := b.after
		b.mu.Unlock()
		if after != nil {
			after()
		}
	}
	return r, err
}

func (b *budgets) Settle(ctx context.Context, sc contracts.Scope, id pv.ReservationID, a providerbudget.Actual) (providerbudget.Settlement, error) {
	b.mu.Lock()
	b.settled = append(b.settled, a)
	b.mu.Unlock()
	return b.Service.Settle(ctx, sc, id, a)
}

func (b *budgets) reservations() int { b.mu.Lock(); defer b.mu.Unlock(); return b.reserved }

func (b *budgets) lastSettled() (providerbudget.Actual, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.settled) == 0 {
		return providerbudget.Actual{}, false
	}
	return b.settled[len(b.settled)-1], true
}

// ---- the kill switch as the router reads it ----

// killProbe is the switch the mode resolver and the router read. It passes to the real data-egress service and can be
// made unreadable, the way a failing store would be, and counts the reads.
type killProbe struct {
	*dataegress.Service
	mu    sync.Mutex
	err   error
	reads int
}

func (k *killProbe) KillSwitch(ctx context.Context, deploymentID string) (dataegress.KillSwitchState, error) {
	k.mu.Lock()
	err := k.err
	k.reads++
	k.mu.Unlock()
	if err != nil {
		return dataegress.KillSwitchState{}, err
	}
	return k.Service.KillSwitch(ctx, deploymentID)
}

func (k *killProbe) unreadable(err error) { k.mu.Lock(); k.err = err; k.mu.Unlock() }

func (k *killProbe) readCount() int { k.mu.Lock(); defer k.mu.Unlock(); return k.reads }

// ---- the local runtime's availability ----

type localUp struct {
	mu sync.Mutex
	up bool
}

func (l *localUp) Usable(modes.Role) bool { l.mu.Lock(); defer l.mu.Unlock(); return l.up }
func (l *localUp) set(up bool)            { l.mu.Lock(); l.up = up; l.mu.Unlock() }

// ---- the sender ----

// sent is one request that reached the provider's wire: the whole SendRequest and the scope it was sent for.
type sent struct {
	Scope contracts.Scope
	Req   extmode.SendRequest
}

// sender is the wire to the external provider. It records everything it is handed (the profile, the validated
// request, the native features that were allowed), forwards it to the scripted provider, and can lose the
// connection after the provider has processed the request.
type sender struct {
	gen       providers.MultimodalGenerator
	mu        sync.Mutex
	log       []sent
	lostAfter error
	// reply, when set, rewrites the provider's result before the runtime sees it: the way a hostile or
	// misconfigured provider answers.
	reply func(providers.RichResult) providers.RichResult
}

func (s *sender) Send(ctx context.Context, scope contracts.Scope, req extmode.SendRequest) (providers.RichResult, error) {
	s.mu.Lock()
	s.log = append(s.log, sent{Scope: scope, Req: req})
	lost, rewrite := s.lostAfter, s.reply
	s.mu.Unlock()
	res, err := s.gen.Generate(ctx, scope, req.Rich)
	if err != nil {
		return providers.RichResult{}, err
	}
	if lost != nil {
		return providers.RichResult{}, lost
	}
	if rewrite != nil {
		res = rewrite(res)
	}
	return res, nil
}

func (s *sender) sends() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.log) }

func (s *sender) all() []sent { s.mu.Lock(); defer s.mu.Unlock(); return append([]sent(nil), s.log...) }

func (s *sender) sentTo() []pv.ProfileID {
	var out []pv.ProfileID
	for _, e := range s.all() {
		out = append(out, e.Req.Profile.ProfileID)
	}
	return out
}

func (s *sender) setReply(f func(providers.RichResult) providers.RichResult) {
	s.mu.Lock()
	s.reply = f
	s.mu.Unlock()
}

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
	counter  *recordingAdapter
	co       *actions.Coordinator
	pkg      capabilities.Package
	store    *sessions.Store
	eg       *dataegress.Service
	ks       *killProbe
	modes    *modes.MemoryModes
	policy   modes.StaticPolicy
	local    *pfake.Provider
	ext      *pfake.Provider
	snd      *sender
	reg      *hooked
	creds    *credStatus
	budget   *budgets
	localUp  *localUp
	eng      *dialogue.Engine
	rt       *extmode.Runtime
	retr     *fakeRetriever
	catalog  *fakeCatalog
	know     *extmode.Knowledge
	profA    pv.ProfileID
	profB    pv.ProfileID
	outcomes []modes.Outcome
	mu       sync.Mutex
}

type opt func(*worldOpts)

type worldOpts struct {
	noEgressPolicy bool
	localFallback  []modes.Condition
	second         bool
	namedFallback  []pv.ProviderProfileFallbackOnItem
	dailyMicro     int64
	modelReplies   bool
	onlyTenant     bool
	tenantTwo      bool
	features       extmode.Features
	convenience    []extmode.Feature
	edit           func(*pv.ProviderProfile)
}

func withoutEgressPolicy() opt { return func(o *worldOpts) { o.noEgressPolicy = true } }
func withLocalFallback(c ...modes.Condition) opt {
	return func(o *worldOpts) { o.localFallback = c }
}

// withSecondProfile registers a second qualified profile and routes the controller role to both (the first that
// can serve wins).
func withSecondProfile() opt { return func(o *worldOpts) { o.second = true } }

// withNamedFallback makes profile A name profile B as its own fallback on the conditions (B is registered and the
// controller role is routed to A only, so B is reached only through the registry's fallback).
func withNamedFallback(on ...pv.ProviderProfileFallbackOnItem) opt {
	return func(o *worldOpts) { o.second, o.namedFallback = true, on }
}

// withModelReplies lets the external provider realise the response too (the directive leaves at class internal).
func withModelReplies() opt { return func(o *worldOpts) { o.modelReplies = true } }

// withBudget sets the provider budget of the account in micro-units per day; the default is far above any test.
func withBudget(dailyMicro int64) opt { return func(o *worldOpts) { o.dailyMicro = dailyMicro } }

// withProfileEdit changes the operator draft of every profile before it is registered.
func withProfileEdit(f func(*pv.ProviderProfile)) opt { return func(o *worldOpts) { o.edit = f } }

// withoutBudget configures no budget at all for the provider.
func withoutBudget() opt { return func(o *worldOpts) { o.dailyMicro = -1 } }

// withProfilePermittedForTenantOnly narrows the profile's permitted scopes to the receptionist's tenant.
func withProfilePermittedForTenantOnly() opt { return func(o *worldOpts) { o.onlyTenant = true } }

// withTenantTwo gives the second tenant the same mode and egress policy as the first.
func withTenantTwo() opt { return func(o *worldOpts) { o.tenantTwo = true } }

// withFeatures lets the operator ask for native features and classify them as conveniences.
func withFeatures(f extmode.Features, conv ...extmode.Feature) opt {
	return func(o *worldOpts) { o.features, o.convenience = f, conv }
}

// profileDraft is the operator's draft of a text profile: the qualified example with everything the registry
// records itself removed.
func profileDraft(t testing.TB) pv.ProviderProfile {
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
	p.ProfileID, p.CredRef, p.BillingNoticeAck, p.LastConformance = "", nil, nil, nil
	p.Lifecycle, p.QuarantineReason, p.CreatedAt, p.UpdatedAt = "", nil, "", ""
	p.PermittedScopes = []pv.ProviderProfileScopeGrant{{ApplicationID: common.Identifier(receptionist().ApplicationID)}}
	return p
}

// passingReport is a fully passing conformance report for the profile's exact model (text only: no media declared).
func passingReport(t testing.TB, p pv.ProviderProfile, done time.Time) pv.ConformanceReport {
	t.Helper()
	checks := map[string][]string{
		"inference":  {"text_generation_and_streaming", "bounded_context", "structured_output", "cancellation_and_timeout", "usage_reporting", "malformed_and_error_mapping"},
		"control":    {"typed_task_proposal", "unknown_capability_refusal", "argument_schema_compliance", "no_direct_adapter_invocation", "directive_and_receipt_handling", "confirmation_and_cancellation"},
		"rag":        {"authorized_excerpt_handling", "source_reference_preservation", "no_unsupported_citations", "prompt_injection_resistance", "local_only_source_enforcement", "no_path_or_cred_leakage"},
		"multimodal": {"image_input_output", "file_input_output", "audio_input_output", "cancellation_and_partial_stream", "sanitized_rich_blocks"},
		"operations": {"creds_never_in_responses_or_logs", "quota_and_budget_enforcement", "provider_disablement", "fallback_and_outage_behavior", "tenant_isolation", "audit_completeness", "terms_and_capability_profile_validation"},
	}
	media := map[string]bool{"image_input_output": true, "file_input_output": true, "audio_input_output": true}
	groups := map[string]map[string]any{}
	for grp, names := range checks {
		groups[grp] = map[string]any{}
		for _, n := range names {
			res := "passed"
			if media[n] {
				res = "not_applicable"
			}
			groups[grp][n] = map[string]any{"result": res, "reason": nil}
		}
	}
	doc := map[string]any{
		"report_id":  "conf_" + strings.Repeat("0", 25) + "R", // TEST-ONLY opaque report id
		"provider":   map[string]any{"profile_id": p.ProfileID, "provider_id": p.ProviderID, "model_id": p.ModelID, "model_version": p.ModelVersion},
		"suite":      map[string]any{"version": "1.0.0", "mode": "fixtures"},
		"declared":   map[string]any{"image": false, "file": false, "audio": false},
		"started_at": done.Add(-4 * time.Minute).UTC().Format(time.RFC3339), "completed_at": done.UTC().Format(time.RFC3339),
		"outcome": "passed", "groups": groups,
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var rep pv.ConformanceReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("building the report: %v", err)
	}
	return rep
}

// qualify registers a draft (after mutate) in the REAL registry and takes it through every step to production
// qualification: a bound and active credential, an acknowledged billing notice, a passing conformance report.
func (w *world) qualify(sc contracts.Scope, ref pv.CredentialRefID, mutate ...func(*pv.ProviderProfile)) pv.ProviderProfile {
	w.t.Helper()
	d := profileDraft(w.t)
	for _, m := range mutate {
		m(&d)
	}
	r := w.reg.Registry
	p, err := r.Register(ctxb(), sc, d)
	if err != nil {
		w.t.Fatalf("Register: %v", err)
	}
	w.creds.set(ref, pv.CredentialRefStatusActive)
	steps := []func() (pv.ProviderProfile, error){
		func() (pv.ProviderProfile, error) { return r.BindCredential(ctxb(), sc, p.ProfileID, ref) },
		func() (pv.ProviderProfile, error) {
			return r.AcknowledgeBillingNotice(ctxb(), sc, p.ProfileID, providerreg.Acknowledgement{
				NoticeVersion: p.BillingNotice.Version, ConsumerSubscriptionIsNotAPIAccess: true, SeparateAPIBillingUnderstood: true,
			})
		},
		func() (pv.ProviderProfile, error) {
			return r.RecordConformance(ctxb(), sc, p.ProfileID, passingReport(w.t, p, w.clk.Now().Add(-time.Hour)))
		},
		func() (pv.ProviderProfile, error) { return r.Qualify(ctxb(), sc, p.ProfileID) },
	}
	var out pv.ProviderProfile
	for i, step := range steps {
		var err error
		if out, err = step(); err != nil {
			w.t.Fatalf("qualification step %d: %v", i, err)
		}
	}
	return out
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
	w.counter = &recordingAdapter{AppAdapter: w.host.Adapter()}
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

	// The REAL profile registry, with a scripted credential-status source.
	w.creds = &credStatus{status: map[pv.CredentialRefID]pv.CredentialRefStatus{}}
	rr, err := providerreg.New(w.db, providerreg.Config{Clock: w.clk, Credentials: w.creds})
	if err != nil {
		t.Fatal(err)
	}
	w.reg = &hooked{Registry: rr}
	permit := func(p *pv.ProviderProfile) {
		if o.onlyTenant {
			tn := common.Identifier(sc.TenantID)
			p.PermittedScopes = []pv.ProviderProfileScopeGrant{{ApplicationID: common.Identifier(sc.ApplicationID), TenantID: &tn}}
		}
	}
	edit := func(p *pv.ProviderProfile) {
		if o.edit != nil {
			o.edit(p)
		}
	}
	a := w.qualify(sc, credRef(0), edit, permit)
	w.profA = a.ProfileID
	routes := []pv.ProfileID{w.profA}
	if o.second {
		b := w.qualify(sc, credRef(1), edit, permit)
		w.profB = b.ProfileID
		if len(o.namedFallback) == 0 {
			routes = append(routes, w.profB)
		} else if _, err := rr.SetFallback(ctxb(), sc, w.profA, pv.ProviderProfileFallback{ProfileID: &w.profB, On: o.namedFallback}); err != nil {
			t.Fatalf("SetFallback: %v", err)
		}
	}
	w.policy = modes.StaticPolicy{modes.RoleController: {Profiles: routes, LocalFallback: o.localFallback}}
	w.local = pfake.New(pfake.WithClock(w.clk), pfake.WithName("local"))
	w.ext = pfake.New(pfake.WithClock(w.clk), pfake.WithName("external"), pfake.WithModel(string(egModel)))
	w.snd = &sender{gen: w.ext}
	w.localUp = &localUp{up: true}
	bsvc, err := providerbudget.New(w.db, providerbudget.Config{Clock: w.clk, LocalFallback: func(context.Context, contracts.Scope, pv.ProviderID) bool {
		return w.localUp.Usable(modes.RoleController) && w.policy[modes.RoleController].FallsBackOn(modes.CondBudgetExhausted)
	}})
	if err != nil {
		t.Fatal(err)
	}
	w.budget = &budgets{Service: bsvc}
	w.ks = &killProbe{Service: w.eg}
	if o.dailyMicro >= 0 {
		w.putBudget(sc, o.dailyMicro)
	}
	w.modes = modes.NewMemoryModes()
	scopes := []contracts.Scope{sc}
	if o.tenantTwo {
		scopes = append(scopes, tenantTwo())
	}
	for _, s := range scopes {
		for _, lv := range []modes.Level{modes.LevelDeployment, modes.LevelApplication, modes.LevelTenant} {
			if err := w.modes.Set(lv, s, modes.ModeExternal); err != nil {
				t.Fatal(err)
			}
		}
		if !o.noEgressPolicy {
			for _, lv := range []pv.EgressLevel{pv.EgressLevelDeployment, pv.EgressLevelApplication, pv.EgressLevelTenant, pv.EgressLevelWorkspace} {
				w.putPolicy(s, lv)
			}
		}
	}
	localSel, err := dialogue.LocalSelector(w.local, providers.Budget{MaxPromptTokens: 4096, MaxOutputTokens: 1024, MaxBatchItems: 4, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	pkgSource := dialogue.PackageSourceFunc(func(context.Context, string) (capabilities.Package, error) { return w.pkg, nil })
	rt, err := extmode.New(extmode.Config{
		Local: localSel, Modes: w.modes, KillSwitch: w.ks, Policy: w.policy, LocalProviders: w.localUp,
		Profiles: w.reg, Egress: w.eg, Budgets: w.budget, Packages: pkgSource, Sender: w.snd,
		Budget: providers.Budget{MaxPromptTokens: 16384, MaxOutputTokens: 2048, MaxBatchItems: 4, Timeout: time.Minute}, Clock: w.clk,
		Features: o.features, Convenience: o.convenience,
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
	// Responses are the core's deterministic templates unless the test lets the provider realise them: the
	// failures under test are the controller's.
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

func baseInput(level pv.EgressLevel) dataegress.PolicyInput {
	return dataegress.PolicyInput{
		Level: level, Enabled: true, SourceMode: pv.EgressModeAllowExcerpt,
		AllowedProviders: []pv.ProviderID{egProvider},
		AllowedModels:    []pv.ModelRef{{ProviderID: egProvider, ModelID: egModel}},
		AllowedClasses:   []pv.EgressEligibleClass{pv.EgressEligibleClassPublic, pv.EgressEligibleClassInternal},
		AllowedRegions:   []pv.RegionCode{"us", "eu"},
	}
}

// putPolicyWith writes the baseline policy of one level (the deployment level as the operator) after mut.
func (w *world) putPolicyWith(sc contracts.Scope, level pv.EgressLevel, source pv.SourceID, mut func(*dataegress.PolicyInput)) {
	w.t.Helper()
	in := baseInput(level)
	in.SourceID = source
	if mut != nil {
		mut(&in)
	}
	var (
		cur   pv.EgressPolicy
		found bool
		err   error
	)
	if level == pv.EgressLevelDeployment {
		cur, found, err = w.eg.GetPolicy(ctxb(), adminScope(sc), level, "")
	} else {
		cur, found, err = w.eg.GetPolicy(ctxb(), adminScope(sc), level, source)
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
		_, err = w.eg.PutPolicy(ctxb(), adminScope(sc), in)
	}
	if err != nil {
		w.t.Fatalf("PutPolicy(%s): %v", level, err)
	}
}

func (w *world) putPolicy(sc contracts.Scope, level pv.EgressLevel) {
	w.putPolicyWith(sc, level, "", nil)
}

func (w *world) operator() dataegress.Operator {
	return dataegress.Operator{DeploymentID: receptionist().DeploymentID, PrincipalID: "opr_root"}
}

// putBudget sets the daily and monthly provider budget of the external provider's account.
func (w *world) putBudget(sc contracts.Scope, dailyMicro int64) {
	w.t.Helper()
	op := sc.Clone()
	op.PrincipalID, op.SessionID = "usr_budget_admin", "ses_budget_admin"
	prov := egProvider
	_, err := w.budget.PutBudget(ctxb(), op, pv.Budget{
		Subject: pv.BudgetSubject{Kind: pv.BudgetSubjectKindProvider, ProviderID: &prov},
		Channel: pv.ChannelInteractive, Currency: "USD", WarnAtPercent: 80,
		Limits: pv.BudgetLimits{DailyMicroUnits: pv.MicroUnits(dailyMicro), MonthlyMicroUnits: pv.MicroUnits(dailyMicro)},
	})
	if err != nil {
		w.t.Fatalf("PutBudget: %v", err)
	}
}

// putLimits replaces the provider budget with the cost limits and the optional rate and concurrency limits.
func (w *world) putLimits(dailyMicro int64, perMin, concurrent *int64) {
	w.t.Helper()
	op := receptionist()
	op.PrincipalID, op.SessionID = "usr_budget_admin", "ses_budget_admin"
	prov := egProvider
	b := pv.Budget{
		Subject: pv.BudgetSubject{Kind: pv.BudgetSubjectKindProvider, ProviderID: &prov},
		Channel: pv.ChannelInteractive, Currency: "USD", WarnAtPercent: 80,
		Limits: pv.BudgetLimits{DailyMicroUnits: pv.MicroUnits(dailyMicro), MonthlyMicroUnits: pv.MicroUnits(dailyMicro)},
	}
	if perMin != nil || concurrent != nil {
		b.RateLimits = &pv.BudgetRateLimits{RequestsPerMin: perMin, ConcurrentRequests: concurrent}
	}
	if _, err := w.budget.PutBudget(ctxb(), op, b); err != nil {
		w.t.Fatalf("PutBudget: %v", err)
	}
}

// ---- scripting and driving ----

func offersAnyTool() pfake.Matcher {
	return func(c pfake.Call) bool { return c.Rich != nil && len(c.Rich.Tools) > 0 }
}

// proposeBook scripts the external provider's next proposal prompt: a tool call booking Luca Ferri.
func (w *world) proposeBook() {
	w.t.Helper()
	w.ext.On(offersAnyTool()).ProposeToolCall("call_1", "appointments_book", bookArg).Times(1)
}

// propose scripts the external provider's next proposal prompt: a tool call of the given tool and arguments.
func (w *world) propose(tool, args string) {
	w.t.Helper()
	w.ext.On(offersAnyTool()).ProposeToolCall("call_1", tool, args).Times(1)
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
// nothing prepared, no preparation stored.
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
		for _, tool := range p.Rich.Tools {
			if strings.Contains(tool.Name, sub) || strings.Contains(tool.Description, sub) || strings.Contains(string(tool.Parameters), sub) {
				return true
			}
		}
	}
	return false
}

// routeDirect sends one controller request through the world's router, below the dialogue engine, so the typed
// outcome can be read as the router decided it. It reports what the executors saw.
func (w *world) routeDirect(sc contracts.Scope, commitCapable bool) (out modes.Outcome, err error, sent, localRuns int) {
	w.t.Helper()
	req := modes.Request{
		Scope: sc, Role: modes.RoleController, CommitCapable: commitCapable,
		Requirement: providerreg.Requirement{DataClass: pv.DataClassificationInternal}, Candidate: w.baseCandidate(),
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

// healthy runs the control: with nothing failing the booking is proposed by the external provider, validated and
// prepared for the user's confirmation, never committed.
func healthy(t *testing.T, w *world) {
	t.Helper()
	w.proposeBook()
	res := w.say(receptionist(), sayBook)
	wantAct(t, res, "request_confirmation")
	if res.Prepared == nil || w.snd.sends() != 1 {
		t.Fatalf("control: prepared %v sends %d: the healthy flow must reach the provider and prepare, or the failures below prove nothing", res.Prepared, w.snd.sends())
	}
	if o := w.lastOutcome(); !o.Served() || o.Target != modes.TargetExternal {
		t.Fatalf("control: outcome %+v", o)
	}
	if _, _, commits := w.counter.counts(); commits != 0 {
		t.Fatalf("control: %d commits before any confirmation", commits)
	}
}

// ---- candidates ----

func userPart(text string) dataegress.Part {
	return dataegress.Part{Kind: dataegress.PartText, Text: text, Category: string(pv.SendCategoryUserMessage), Class: string(pv.DataClassificationInternal)}
}

func excerptPart(src pv.SourceID, text, label string) dataegress.Part {
	return dataegress.Part{Kind: dataegress.PartText, Text: text, Label: label, Category: string(pv.SendCategoryRAGExcerpt), Class: string(pv.DataClassificationInternal), SourceID: string(src)}
}

// baseCandidate is a request the pipeline would hand the guard: one internal user message. The router fills in the
// scope, the profile, the role and the channel.
func (w *world) baseCandidate(extra ...dataegress.Part) dataegress.Candidate {
	c := dataegress.Candidate{
		Instructions:   "You are the application controller. Follow the directive and answer briefly.",
		Messages:       []dataegress.Message{{Role: pv.RequestEnvelopeMessageRoleUser, Parts: []dataegress.Part{userPart("Show the free slots")}}},
		ResponseFormat: pv.RequestEnvelopeResponseFormat{Kind: pv.RequestEnvelopeResponseFormatKindText},
		MaxOutputToks:  256, TimeoutMS: 30000, TraceID: "trc_sec",
	}
	if len(extra) > 0 {
		c.Messages = append(c.Messages, dataegress.Message{Role: pv.RequestEnvelopeMessageRoleToolResult, Parts: extra})
	}
	return c
}

// errBoom is a failure that is not a typed provider error.
var errBoom = errors.New("scripted failure")

// anyPrompt matches every call.
func anyPrompt() pfake.Matcher { return pfake.Any() }

// requirementFor is the plain controller requirement the registry gate is asked about.
func requirementFor() providerreg.Requirement {
	return providerreg.Requirement{Role: pv.RoleConversationalController, DataClass: pv.DataClassificationInternal}
}

// providerActual is a valid settlement of a succeeded call.
func providerActual() providerbudget.Actual {
	return providerbudget.Actual{Outcome: pv.UsageRecordOutcomeSucceeded, Metering: providerbudget.Units{ToksIn: 1, ToksOut: 1}}
}
