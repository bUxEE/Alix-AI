package degradation

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

// The world is the application side of the service as the composition root wires it, with the model scripted:
// the real session store, dialogue engine, policy gateway, audit log and action coordinator, and the in-memory
// host application (internal/adapters/fake) behind adapters.Wrap. Failure is injected where the master plan
// section 18 says it happens: in the model provider (pfake.FailNext, rules) and in the host (fake.App.SetAvailable
// and Inject). Nothing is mocked inside the code under test.

const (
	appID     = "demo-app"
	tenantA   = "tnt-acme"
	workspace = "wsp-north"
	capGet    = fake.ReadCapability   // items.get: a read
	capCreate = fake.ActionCapability // items.create: consequential, explicit confirmation
)

var t0 = time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC)

func ctxb() context.Context { return context.Background() }

func memberScope() contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep-prod", ApplicationID: appID, TenantID: tenantA, WorkspaceID: workspace,
		PrincipalID: "usr-jane", Roles: []string{"member"}, CapabilityScopes: []string{capGet, capCreate}, SessionID: "ses-one",
	}
}

// ---- the typed package ----

// typedPkg is an application package built in memory: the policy view beside the typed view. It satisfies
// capabilities.Package without a signed archive.
type typedPkg struct {
	*policy.Package
	specs     map[string]ap.Capability
	contracts map[[2]string]ap.ConversationContract
	terms     []ap.TerminologyTerm
}

func (p *typedPkg) ContentHash() string          { return strings.Repeat("0", 61) + "abc" }
func (p *typedPkg) SupportedLanguages() []string { return []string{"en"} }
func (p *typedPkg) Terminology() []ap.TerminologyTerm {
	return p.terms
}
func (p *typedPkg) Spec(id string) (ap.Capability, bool) { s, ok := p.specs[id]; return s, ok }
func (p *typedPkg) ContractFor(c, l string) (ap.ConversationContract, bool) {
	x, ok := p.contracts[[2]string{c, l}]
	return x, ok
}

var _ capabilities.Package = (*typedPkg)(nil)

func i64(n int64) *int64     { return &n }
func f64(n float64) *float64 { return &n }

func buildPackage(t testing.TB) *typedPkg {
	t.Helper()
	slug := func(s string) ap.CapabilityID { return ap.CapabilityID(s) }
	specs := map[string]ap.Capability{
		capGet: {
			ID: slug(capGet), Audience: ap.CapabilityAudienceUser, Title: "Get an item",
			Purpose:     "Look up one item by its ID and report its name. Use it when the user asks about an item.",
			Fields:      []ap.CapabilityField{{Name: "item_id", Type: ap.CapabilityFieldTypeString, Required: true, Description: "The item to look up."}},
			EntityTypes: map[string]ap.CapabilityEntityType{},
		},
		capCreate: {
			ID: slug(capCreate), Audience: ap.CapabilityAudienceUser, Title: "Create an item",
			Purpose: "Create a new item with a name and a quantity. The item is prepared first and created only after the user confirms.",
			Fields: []ap.CapabilityField{
				{Name: "name", Type: ap.CapabilityFieldTypeString, Required: true, Description: "The item name.", MinLength: i64(1), MaxLength: i64(64)},
				{Name: "quantity", Type: ap.CapabilityFieldTypeInteger, Required: true, Description: "How many.", Minimum: f64(1), Maximum: f64(99)},
			},
			EntityTypes: map[string]ap.CapabilityEntityType{},
			EffectClass: ap.CapabilityEffectClass(policy.EffectExternal), RiskLevel: ap.RiskLevel(contracts.RiskConsequential),
			Confirmation: ap.CapabilityConfirmation{Mode: ap.CapabilityConfirmationModeExplicit},
		},
	}
	decls := []policy.CapabilityDecl{
		{ID: capGet, Audience: policy.AudienceUser, EffectClass: policy.EffectNone, RiskLevel: contracts.RiskRead,
			Confirmation: policy.ConfirmNone, Fields: []policy.FieldDecl{{Name: "item_id", Required: true}}},
		{ID: capCreate, Audience: policy.AudienceUser, EffectClass: policy.EffectExternal, RiskLevel: contracts.RiskConsequential,
			Confirmation: policy.ConfirmExplicit, Fields: []policy.FieldDecl{{Name: "name", Required: true}, {Name: "quantity", Required: true}}},
	}
	p := &typedPkg{specs: specs, contracts: map[[2]string]ap.ConversationContract{}}
	label := func(en string, syn ...ap.Label) map[string]ap.TerminologyLabel {
		return map[string]ap.TerminologyLabel{"en": {Preferred: ap.Label(en), Synonyms: syn}}
	}
	p.terms = []ap.TerminologyTerm{
		{ID: "create", Labels: label("create", "add", "new")},
		{ID: "item", Labels: label("item", "thing", "article")},
		{ID: "lookup", Labels: label("get", "find", "show", "lookup")},
		{ID: "name", Labels: label("name")},
		{ID: "quantity", Labels: label("quantity")},
	}
	sort.Slice(p.terms, func(i, j int) bool { return p.terms[i].ID < p.terms[j].ID })
	prompts := func(items ...[2]string) []ap.ConversationContractFieldPrompt {
		var out []ap.ConversationContractFieldPrompt
		for _, it := range items {
			out = append(out, ap.ConversationContractFieldPrompt{Field: common.FieldName(it[0]), Label: ap.Label(it[1]), Prompt: ap.Template("What is the " + it[1] + "?")})
		}
		return out
	}
	p.contracts[[2]string{capGet, "en"}] = ap.ConversationContract{CapabilityID: slug(capGet), Language: "en",
		Terminology: []ap.Slug{"lookup", "item"}, FieldPrompts: prompts([2]string{"item_id", "Item code"})}
	p.contracts[[2]string{capCreate, "en"}] = ap.ConversationContract{CapabilityID: slug(capCreate), Language: "en",
		Terminology: []ap.Slug{"create", "item", "quantity", "name"}, FieldPrompts: prompts([2]string{"name", "Item name"}, [2]string{"quantity", "Quantity"})}

	hp, err := policy.ParseHostPolicy([]byte(`{
  "policy_id": "default-access",
  "role_grants": [ { "role": "member", "capabilities": ["items.get", "items.create"] } ],
  "risk_overrides": [],
  "field_projection": []
}`))
	if err != nil {
		t.Fatal(err)
	}
	pp, err := policy.NewPackage(policy.PackageSpec{
		ApplicationID: appID, Version: "1.0.0", Status: policy.StatusActive,
		Roles: []string{"member"}, Capabilities: decls, Policies: []*policy.HostPolicy{hp},
	})
	if err != nil {
		t.Fatalf("NewPackage: %v", err)
	}
	p.Package = pp
	return p
}

// ---- the world ----

type policyRecorder struct {
	mu   sync.Mutex
	recs []policy.Record
}

func (r *policyRecorder) Emit(_ context.Context, rec policy.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, rec)
	return nil
}

type tee []policy.Sink

func (t tee) Emit(ctx context.Context, r policy.Record) error {
	for _, s := range t {
		if err := s.Emit(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

type world struct {
	t     *testing.T
	db    *store.DB
	clk   *clock.Fake
	alog  *audit.Log
	app   *fake.App
	guard adapters.AppAdapter
	co    *actions.Coordinator
	store *sessions.Store
	model *pfake.Provider
	eng   *dialogue.Engine
}

func newWorld(t *testing.T) *world { return newWorldWith(t, nil) }

// newWorldWith builds the world with the model provider wrapped by wrap (nil leaves it as it is): the wrapper stands
// for whatever sits between the engine and the runtime, a supervisor gate for example.
func newWorldWith(t *testing.T, wrap func(providers.TextGenerator) providers.TextGenerator) *world {
	t.Helper()
	w := &world{t: t, clk: clock.NewFake(t0)}
	w.db = storetest.Open(t, nil)
	var err error
	if w.alog, err = audit.New(w.db, audit.Options{Clock: w.clk}); err != nil {
		t.Fatal(err)
	}
	sink := audit.NewRecordSink[policy.Record](w.alog, audit.EventPolicyDecision, func(r policy.Record) (audit.Locator, error) {
		s := r.Subject
		return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
	})
	gw := policy.NewGateway(w.clk, policy.WithSink(tee{sink, &policyRecorder{}}))
	w.app = fake.NewApp(fake.WithClock(w.clk))
	if w.guard, err = adapters.Wrap(fake.NewAdapter(w.app), adapters.Options{ApplicationID: appID}); err != nil {
		t.Fatal(err)
	}
	pkg := buildPackage(t)
	reg := capabilities.NewRegistry(nil, capabilities.WithClock(w.clk))
	if err := reg.Activate(pkg); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	adapterSource := actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return w.guard, nil })
	if w.co, err = actions.New(actions.Deps{
		DB: w.db, Policy: gw, Audit: w.alog,
		Packages: actions.PackageSourceFunc(func(context.Context, string) (policy.ActivePackage, error) { return pkg, nil }),
		Adapters: adapterSource, Clock: w.clk,
	}); err != nil {
		t.Fatal(err)
	}
	if w.store, err = sessions.New(w.db, w.clk, sessions.Config{}); err != nil {
		t.Fatal(err)
	}
	w.model = pfake.New(pfake.WithClock(w.clk))
	var gen providers.TextGenerator = w.model
	if wrap != nil {
		gen = wrap(gen)
	}
	sel, err := dialogue.LocalSelector(gen, providers.Budget{MaxPromptTokens: 4096, MaxOutputTokens: 1024, MaxBatchItems: 4, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if w.eng, err = dialogue.New(dialogue.Deps{
		Sessions: w.store, Registry: reg,
		Packages: dialogue.PackageSourceFunc(func(context.Context, string) (capabilities.Package, error) { return pkg, nil }),
		Policy:   gw, Actions: w.co, Adapters: adapterSource,
		Providers: sel, Audit: w.alog, Clock: w.clk,
	}); err != nil {
		t.Fatal(err)
	}
	return w
}

const sayCreate = "create an item named widget with quantity 3"

func (w *world) say(text string) dialogue.Result {
	w.t.Helper()
	res, err := w.eng.Turn(ctxb(), memberScope(), dialogue.TurnInput{Text: text, Language: "en"})
	if err != nil {
		w.t.Fatalf("Turn(%q): %v", text, err)
	}
	return res
}

const createProposal = `{"capability_id":"items.create","fields":{"name":"widget","quantity":3},"entity_refs":[],"intent":"task","uncertainty":{"overall":0.9}}`

// pfakeProposal matches the first model call of a turn: the JSON-schema constrained proposal.
func pfakeProposal() pfake.Matcher { return pfake.Constrained(providers.ConstraintJSONSchema) }

// pfakeAct matches the second model call: the response for one dialogue act.
func pfakeAct(act string) pfake.Matcher {
	return pfake.All(pfake.Constrained(providers.ConstraintNone), pfake.Contains("Act: "+act+"\n"))
}

// prepareCreate runs a healthy turn that prepares an items.create action and waits for the confirmation.
func (w *world) prepareCreate() dialogue.Result {
	w.t.Helper()
	w.model.On(pfakeProposal()).Reply(createProposal).Times(1)
	w.model.On(pfakeAct("request_confirmation")).Reply("Please confirm: create an item named widget.")
	res := w.say(sayCreate)
	if string(res.Directive.Act) != "request_confirmation" || res.Prepared == nil {
		w.t.Fatalf("setup: act %s code %q prepared %v", res.Directive.Act, res.Code, res.Prepared)
	}
	return res
}

func confirmInput(res dialogue.Result) dialogue.TurnInput {
	return dialogue.TurnInput{Confirm: &dialogue.Confirmation{PreparationID: string(res.Prepared.PreparationID), Token: res.Confirmation}}
}

func (w *world) hostCalls(op adapters.Operation) int {
	n := 0
	for _, c := range w.app.Calls() {
		if c.Operation == op {
			n++
		}
	}
	return n
}

func (w *world) hostOps() []adapters.Operation {
	var ops []adapters.Operation
	for _, c := range w.app.Calls() {
		ops = append(ops, c.Operation)
	}
	return ops
}

// prepState reads a preparation's stored state around the coordinator.
func (w *world) prepState(id string) string {
	w.t.Helper()
	var s string
	if err := w.db.QueryRowContext(ctxb(), `SELECT state FROM action_preparations WHERE id = ?`, id).Scan(&s); err != nil {
		w.t.Fatalf("state of %s: %v", id, err)
	}
	return s
}

func wantCode(t testing.TB, err error, code contracts.ErrCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want %s", code)
	}
	if got := contracts.CodeOf(err); got != code {
		t.Fatalf("error code = %s (%v), want %s", got, err, code)
	}
}
