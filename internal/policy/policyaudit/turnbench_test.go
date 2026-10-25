package policyaudit_test

// The dialogue read benchmark at several catalogue sizes (docs/efficiency/M1-21.md).
//
// internal/dialogue has a BenchmarkTurnRead of a package with three declared
// capabilities. Its test kit cannot be imported, and the cost of the policy
// gateway's audit appends scales with the number of declared capabilities, so
// this file rebuilds the same engine from exported APIs, wired as a composition
// root wires it (the real audit log behind audit.NewRecordSink[policy.Record],
// the real policy gateway, the capability router, the action coordinator, the
// in-memory host and the scripted model), on a package of n capabilities: the
// three of the dialogue kit (items.get, items.create, admin.purge) plus n-3
// ordinary user capabilities nothing in the benchmark asks for.
//
//	go test -run xxx -bench BenchmarkDialogueReadTurn -benchmem -benchtime 200x -count 3 ./internal/policy/policyaudit/
//
// Each run reports, next to the usual figures, what one read turn wrote to the
// audit log (policy events, bytes of policy event data, total audit bytes) and
// the time it spent inside the log's AppendAt, for policy events and for the
// rest (the dialogue's own turn event).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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

const (
	benchApp       = "demo-app"
	benchTenant    = "tnt-acme-7781"
	benchWorkspace = "wsp-north-2290"
	benchCapGet    = fake.ReadCapability   // items.get
	benchCapCreate = fake.ActionCapability // items.create
	benchCapAdmin  = "admin.purge"
)

var benchT0 = time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC)

// benchPackage is an application package of n capabilities in memory: the policy
// view from policy.NewPackage and the typed view beside it, which is what
// capabilities.Package asks for.
type benchPackage struct {
	*policy.Package
	specs     map[string]ap.Capability
	contracts map[[2]string]ap.ConversationContract
	terms     []ap.TerminologyTerm
	ids       []string
}

func (p *benchPackage) ContentHash() string {
	return "0000000000000000000000000000000000000000000000000000000000000abc"
}
func (p *benchPackage) SupportedLanguages() []string         { return []string{"en", "it"} }
func (p *benchPackage) Terminology() []ap.TerminologyTerm    { return p.terms }
func (p *benchPackage) Spec(id string) (ap.Capability, bool) { s, ok := p.specs[id]; return s, ok }
func (p *benchPackage) ContractFor(c, l string) (ap.ConversationContract, bool) {
	x, ok := p.contracts[[2]string{c, l}]
	return x, ok
}

var _ capabilities.Package = (*benchPackage)(nil)

func i64p(n int64) *int64     { return &n }
func f64p(n float64) *float64 { return &n }
func boolPtr(b bool) *bool    { return &b }

// newBenchPackage builds the package: n is the number of declared capabilities
// (at least 3).
func newBenchPackage(tb testing.TB, n int) *benchPackage {
	tb.Helper()
	if n < 3 {
		tb.Fatalf("a bench package declares at least 3 capabilities, got %d", n)
	}
	specs := map[string]ap.Capability{
		benchCapGet: {
			ID: ap.CapabilityID(benchCapGet), Audience: ap.CapabilityAudienceUser, Title: "Get an item",
			Purpose: "Look up one item by its ID and report its name. Use it when the user asks about an item.",
			Fields: []ap.CapabilityField{
				{Name: "item_id", Type: ap.CapabilityFieldTypeString, Required: true, Description: "The item to look up."},
			},
			EntityTypes: map[string]ap.CapabilityEntityType{},
			NextActions: []ap.CapabilityNextAction{{ID: "make_another", After: ap.CapabilityNextActionAfterSuccess, CapabilityID: ap.CapabilityID(benchCapCreate)}},
		},
		benchCapCreate: {
			ID: ap.CapabilityID(benchCapCreate), Audience: ap.CapabilityAudienceUser, Title: "Create an item",
			Purpose: "Create a new item with a name and a quantity. The item is prepared first and created only after the user confirms.",
			Fields: []ap.CapabilityField{
				{Name: "name", Type: ap.CapabilityFieldTypeString, Required: true, Description: "The item name.", MinLength: i64p(1), MaxLength: i64p(64)},
				{Name: "quantity", Type: ap.CapabilityFieldTypeInteger, Required: true, Description: "How many.", Minimum: f64p(1), Maximum: f64p(99)},
				{Name: "notes", Type: ap.CapabilityFieldTypeString, Description: "A private note.", Sensitive: boolPtr(true), MaxLength: i64p(300)},
			},
			EntityTypes:  map[string]ap.CapabilityEntityType{},
			EffectClass:  ap.CapabilityEffectClass(policy.EffectExternal),
			RiskLevel:    ap.RiskLevel(contracts.RiskConsequential),
			Confirmation: ap.CapabilityConfirmation{Mode: ap.CapabilityConfirmationModeExplicit},
		},
		benchCapAdmin: {
			ID: ap.CapabilityID(benchCapAdmin), Audience: ap.CapabilityAudienceAdmin, Title: "Purge everything",
			Purpose: "Operators only: delete every item.", EntityTypes: map[string]ap.CapabilityEntityType{},
		},
	}
	decls := []policy.CapabilityDecl{
		{ID: benchCapGet, Audience: policy.AudienceUser, EffectClass: policy.EffectNone, RiskLevel: contracts.RiskRead,
			Confirmation: policy.ConfirmNone, Fields: []policy.FieldDecl{{Name: "item_id", Required: true}}},
		{ID: benchCapCreate, Audience: policy.AudienceUser, EffectClass: policy.EffectExternal, RiskLevel: contracts.RiskConsequential,
			Confirmation: policy.ConfirmExplicit, Fields: []policy.FieldDecl{{Name: "name", Required: true}, {Name: "quantity", Required: true}, {Name: "notes", Sensitive: true}}},
		{ID: benchCapAdmin, Audience: policy.AudienceAdmin, EffectClass: policy.EffectPrivileged, RiskLevel: contracts.RiskPrivileged, Confirmation: policy.ConfirmStepUp},
	}
	granted := []string{benchCapGet, benchCapCreate}
	// Ordinary user capabilities the benchmark never asks for: they only make the
	// catalogue as large as a real application's. Their words (ledger, digest,
	// period) share nothing with the request.
	for i := 0; i < n-3; i++ {
		id := fmt.Sprintf("ledger.digest_%03d", i)
		granted = append(granted, id)
		specs[id] = ap.Capability{
			ID: ap.CapabilityID(id), Audience: ap.CapabilityAudienceUser, Title: ap.Label(fmt.Sprintf("Ledger digest %d", i)),
			Purpose:     ap.Text(fmt.Sprintf("Summarise the ledger for accounting period %d for the finance team.", i)),
			EntityTypes: map[string]ap.CapabilityEntityType{},
		}
		decls = append(decls, policy.CapabilityDecl{ID: id, Audience: policy.AudienceUser, EffectClass: policy.EffectNone,
			RiskLevel: contracts.RiskRead, Confirmation: policy.ConfirmNone})
	}
	quoted := make([]string, len(granted))
	for i, id := range granted {
		quoted[i] = `"` + id + `"`
	}
	list := ""
	for i, q := range quoted {
		if i > 0 {
			list += ", "
		}
		list += q
	}
	hp, err := policy.ParseHostPolicy([]byte(`{
  "policy_id": "default-access",
  "role_grants": [
    { "role": "member", "capabilities": [` + list + `] },
    { "role": "visitor", "capabilities": ["items.get"] }
  ],
  "risk_overrides": [],
  "field_projection": []
}`))
	if err != nil {
		tb.Fatal(err)
	}
	pp, err := policy.NewPackage(policy.PackageSpec{
		ApplicationID: benchApp, Version: "1.0.0", Status: policy.StatusActive,
		Roles: []string{"member", "visitor", "staff"}, Capabilities: decls, Policies: []*policy.HostPolicy{hp},
	})
	if err != nil {
		tb.Fatalf("NewPackage: %v", err)
	}
	p := &benchPackage{Package: pp, specs: specs, contracts: map[[2]string]ap.ConversationContract{}, ids: pp.CapabilityIDs()}
	label := func(en, it string) map[string]ap.TerminologyLabel {
		return map[string]ap.TerminologyLabel{"en": {Preferred: ap.Label(en)}, "it": {Preferred: ap.Label(it)}}
	}
	p.terms = []ap.TerminologyTerm{
		{ID: "create", Labels: map[string]ap.TerminologyLabel{
			"en": {Preferred: "create", Synonyms: []ap.Label{"add", "new"}}, "it": {Preferred: "crea", Synonyms: []ap.Label{"aggiungi", "nuovo"}}}},
		{ID: "item", Labels: map[string]ap.TerminologyLabel{
			"en": {Preferred: "item", Synonyms: []ap.Label{"thing", "article"}}, "it": {Preferred: "articolo", Synonyms: []ap.Label{"oggetto"}}}},
		{ID: "lookup", Labels: map[string]ap.TerminologyLabel{
			"en": {Preferred: "get", Synonyms: []ap.Label{"find", "show", "lookup"}}, "it": {Preferred: "trova", Synonyms: []ap.Label{"mostra", "cerca"}}}},
		{ID: "name", Labels: label("name", "nome")},
		{ID: "quantity", Labels: label("quantity", "quantità")},
	}
	sort.Slice(p.terms, func(i, j int) bool { return p.terms[i].ID < p.terms[j].ID })
	prompts := func(items ...[2]string) []ap.ConversationContractFieldPrompt {
		var out []ap.ConversationContractFieldPrompt
		for _, it := range items {
			out = append(out, ap.ConversationContractFieldPrompt{Field: common.FieldName(it[0]), Label: ap.Label(it[1]), Prompt: ap.Template("What is the " + it[1] + "?")})
		}
		return out
	}
	for _, lang := range []string{"en", "it"} {
		p.contracts[[2]string{benchCapGet, lang}] = ap.ConversationContract{CapabilityID: ap.CapabilityID(benchCapGet), Language: ap.LanguageTag(lang),
			Terminology: []ap.Slug{"lookup", "item"}, FieldPrompts: prompts([2]string{"item_id", "Item code"})}
		p.contracts[[2]string{benchCapCreate, lang}] = ap.ConversationContract{CapabilityID: ap.CapabilityID(benchCapCreate), Language: ap.LanguageTag(lang),
			Terminology:  []ap.Slug{"create", "item", "quantity", "name"},
			FieldPrompts: prompts([2]string{"name", "Item name"}, [2]string{"quantity", "Quantity"})}
	}
	return p
}

func benchScope() contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep-prod-3301", ApplicationID: benchApp, TenantID: benchTenant, WorkspaceID: benchWorkspace,
		PrincipalID: "usr-jane-4410", Roles: []string{"member"}, SessionID: "ses-xyz-5521",
		// the token of a member permits every user capability of the application
		CapabilityScopes: nil,
	}
}

// byteCounter is a policy.Sink that tallies what the gateway hands to the audit
// sink, so a run can say how many records and bytes a turn produced even where the
// audit log is not the consumer.
type byteCounter struct {
	mu      sync.Mutex
	records int
	bytes   int
}

func (c *byteCounter) Emit(_ context.Context, r policy.Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.records++
	c.bytes += len(b)
	c.mu.Unlock()
	return nil
}

// timedAppender is an audit.Appender that adds up the time spent inside AppendAt,
// by whether the event is a policy event, so a run can say how long a turn spent
// in the audit log on the policy gateway's account. The turns of a benchmark run
// one after the other, so the time is that of the one goroutine that appends.
type timedAppender struct {
	audit.Appender
	mu      sync.Mutex
	policy  time.Duration
	other   time.Duration
	policyN int
}

func (a *timedAppender) AppendAt(ctx context.Context, loc audit.Locator, ev audit.NewEvent) (audit.Event, error) {
	start := time.Now()
	out, err := a.Appender.AppendAt(ctx, loc, ev)
	d := time.Since(start)
	a.mu.Lock()
	if strings.HasPrefix(string(ev.Type), "policy.") {
		a.policy += d
		a.policyN++
	} else {
		a.other += d
	}
	a.mu.Unlock()
	return out, err
}

func (a *timedAppender) totals() (policy, other time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.policy, a.other
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

type benchEnv struct {
	db      *store.DB
	eng     *dialogue.Engine
	counter *byteCounter
	timed   *timedAppender
	scope   contracts.Scope
}

// newBenchEnv wires the engine as dialogue's own kit does, on a package of n
// capabilities, and scripts the model so that "show me item_1" is a read of
// items.get.
func newBenchEnv(tb testing.TB, n int) *benchEnv {
	tb.Helper()
	clk := clock.NewFake(benchT0)
	db := storetest.Open(tb, nil)
	alog, err := audit.New(db, audit.Options{Clock: clk})
	if err != nil {
		tb.Fatal(err)
	}
	timed := &timedAppender{Appender: alog}
	// The wiring of internal/cli/cmd_serve.go: one RecordSink for policy records.
	sink := audit.NewRecordSink[policy.Record](timed, audit.EventPolicyDecision, func(r policy.Record) (audit.Locator, error) {
		s := r.Subject
		return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
	})
	counter := &byteCounter{}
	gw := policy.NewGateway(clk, policy.WithSink(tee{sink, counter}))

	app := fake.NewApp(fake.WithClock(clk))
	app.Seed(benchTenant, benchWorkspace, "item_1", "Alpha widget")
	guard, err := adapters.Wrap(fake.NewAdapter(app), adapters.Options{ApplicationID: benchApp})
	if err != nil {
		tb.Fatal(err)
	}
	pkg := newBenchPackage(tb, n)
	reg := capabilities.NewRegistry(nil, capabilities.WithClock(clk))
	if err := reg.Activate(pkg); err != nil {
		tb.Fatalf("Activate: %v", err)
	}
	co, err := actions.New(actions.Deps{
		DB: db, Policy: gw, Audit: alog,
		Packages: actions.PackageSourceFunc(func(context.Context, string) (policy.ActivePackage, error) { return pkg, nil }),
		Adapters: actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return guard, nil }),
		Clock:    clk,
	})
	if err != nil {
		tb.Fatal(err)
	}
	st, err := sessions.New(db, clk, sessions.Config{})
	if err != nil {
		tb.Fatal(err)
	}
	model := pfake.New(pfake.WithClock(clk))
	proposal, err := json.Marshal(map[string]any{
		"capability_id": benchCapGet, "fields": map[string]any{"item_id": "item_1"}, "entity_refs": []any{}, "intent": "task",
		"uncertainty": map[string]any{"overall": 0.9},
	})
	if err != nil {
		tb.Fatal(err)
	}
	model.On(pfake.Contains("show me")).Reply(string(proposal))
	model.On(pfake.All(pfake.Constrained(providers.ConstraintNone), pfake.Contains("Act: present_result\n"))).Reply("Here is what I found. Name: Alpha widget.")
	sel, err := dialogue.LocalSelector(model, providers.Budget{MaxPromptTokens: 4096, MaxOutputTokens: 1024, MaxBatchItems: 4, Timeout: time.Minute})
	if err != nil {
		tb.Fatal(err)
	}
	eng, err := dialogue.New(dialogue.Deps{
		Sessions: st, Registry: reg,
		Packages: dialogue.PackageSourceFunc(func(context.Context, string) (capabilities.Package, error) { return pkg, nil }),
		Policy:   gw, Actions: co,
		Adapters:  actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return guard, nil }),
		Providers: sel, Audit: timed, Clock: clk,
		Limiter: dialogue.NewWindowLimiter(clk, dialogue.RateConfig{MaxTurns: 1 << 30, Window: time.Hour, MaxKeys: 16}),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		tb.Fatal(err)
	}
	sc := benchScope()
	sc.CapabilityScopes = pkg.ids
	return &benchEnv{db: db, eng: eng, counter: counter, timed: timed, scope: sc}
}

func (e *benchEnv) readTurn(ctx context.Context) (dialogue.Result, error) {
	return e.eng.Turn(ctx, e.scope, dialogue.TurnInput{Text: "show me item_1", Language: "en"})
}

// auditTotals reads what the audit log holds: events and bytes of data, for the
// events whose type starts with prefix (all events when empty).
func (e *benchEnv) auditTotals(tb testing.TB, prefix string) (events, bytes int) {
	tb.Helper()
	err := e.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*), COALESCE(SUM(LENGTH(data)), 0) FROM audit_events WHERE event_type LIKE ?`, prefix+"%").Scan(&events, &bytes)
	if err != nil {
		tb.Fatal(err)
	}
	return events, bytes
}

// BenchmarkDialogueReadTurn is dialogue's BenchmarkTurnRead on a package of 3, 20
// and 100 capabilities. It reports what one turn wrote to the audit log as
// policy_events/op, policy_B/op and audit_B/op, and the time spent appending as
// policy_append_ms/op and other_append_ms/op.
func BenchmarkDialogueReadTurn(b *testing.B) {
	for _, n := range []int{3, 20, 100} {
		b.Run(fmt.Sprintf("caps=%d", n), func(b *testing.B) {
			e := newBenchEnv(b, n)
			ctx := context.Background()
			// One turn outside the timer: the first turn opens the session and warms
			// the router's caches, which is not what a read turn costs.
			res, err := e.readTurn(ctx)
			if err != nil || string(res.Directive.Act) != "present_result" {
				b.Fatalf("warm-up turn: act %s, %v", res.Directive.Act, err)
			}
			pe0, pb0 := e.auditTotals(b, "policy.")
			_, ab0 := e.auditTotals(b, "")
			pt0, ot0 := e.timed.totals()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := e.readTurn(ctx); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			pe1, pb1 := e.auditTotals(b, "policy.")
			_, ab1 := e.auditTotals(b, "")
			pt1, ot1 := e.timed.totals()
			b.ReportMetric(float64(pe1-pe0)/float64(b.N), "policy_events/op")
			b.ReportMetric(float64(pb1-pb0)/float64(b.N), "policy_B/op")
			b.ReportMetric(float64(ab1-ab0)/float64(b.N), "audit_B/op")
			b.ReportMetric(float64(pt1-pt0)/float64(b.N)/1e6, "policy_append_ms/op")
			b.ReportMetric(float64(ot1-ot0)/float64(b.N)/1e6, "other_append_ms/op")
		})
	}
}

// TestDialogueReadTurnOnALargeCatalogue guards the harness itself: the read turn
// must reach the host on every catalogue size, or the numbers measure a refusal.
func TestDialogueReadTurnOnALargeCatalogue(t *testing.T) {
	for _, n := range []int{3, 20, 100} {
		e := newBenchEnv(t, n)
		res, err := e.readTurn(context.Background())
		if err != nil {
			t.Fatalf("%d capabilities: %v", n, err)
		}
		if string(res.Directive.Act) != "present_result" {
			t.Fatalf("%d capabilities: act = %s (code %q), want present_result", n, res.Directive.Act, res.Code)
		}
		if events, _ := e.auditTotals(t, "policy."); events == 0 {
			t.Fatalf("%d capabilities: no policy event was written", n)
		}
	}
}
