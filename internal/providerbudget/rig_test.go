package providerbudget

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

const (
	tApp    = "app_crm"
	tTenant = "ten_1"
	tWsp    = "wsp_1"
	tUser   = "usr_1"
)

var tStart = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

type rig struct {
	t     *testing.T
	db    *store.DB
	svc   *Service
	clk   *clock.Fake
	scope contracts.Scope
	// fallback is what the LocalFallback hook answers.
	mu       sync.Mutex
	fallback map[providers.ProviderID]bool
}

func newRig(t *testing.T, mutate ...func(*Config)) *rig {
	t.Helper()
	reg := store.NewRegistry()
	if err := reg.Register(Module, migrations); err != nil {
		t.Fatalf("registering migrations: %v", err)
	}
	return newRigOn(t, storetest.Open(t, reg), mutate...)
}

func newRigOn(t *testing.T, db *store.DB, mutate ...func(*Config)) *rig {
	t.Helper()
	g := &rig{t: t, db: db, clk: clock.NewFake(tStart), fallback: map[providers.ProviderID]bool{}}
	cfg := Config{Clock: g.clk, LocalFallback: func(_ context.Context, _ contracts.Scope, p providers.ProviderID) bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.fallback[p]
	}}
	for _, m := range mutate {
		m(&cfg)
	}
	svc, err := New(db, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g.svc = svc
	g.scope = g.scopeOf(tApp, tTenant, tWsp, tUser)
	return g
}

func (g *rig) scopeOf(app, tenant, wsp, user string) contracts.Scope {
	s := storetest.Scope(app, tenant, wsp)
	s.PrincipalID = user
	return s
}

// prices: 1 micro-unit per input tok, 2 per output tok, 0.5 per cached input tok, 5000 per image in,
// 40000 per image out, 100 per audio second in, 10 per 1000 file bytes, requests free.
func prices() providers.PriceTable {
	p := func(per, micro int64) *providers.PriceTablePrice {
		return &providers.PriceTablePrice{PerUnits: per, MicroUnits: providers.MicroUnits(micro)}
	}
	return providers.PriceTable{
		Currency: "USD", EffectiveOn: "2026-10-01",
		Prices: providers.PriceTablePrices{
			ToksIn: p(1000, 1000), ToksOut: p(1000, 2000), ToksInCached: p(1000, 500),
			ImagesIn: p(1, 5000), ImagesOut: p(1, 40000), AudioSecondsIn: p(60, 6000),
			FileBytesIn: p(1000, 10), Requests: p(1, 0),
		},
	}
}

func ptr[T any](v T) *T { return &v }

func providerBudget(provider string, ch providers.Channel, daily, monthly int64) providers.Budget {
	return providers.Budget{
		Subject: providers.BudgetSubject{Kind: providers.BudgetSubjectKindProvider, ProviderID: ptr(providers.ProviderID(provider))},
		Channel: ch, Currency: "USD", WarnAtPercent: 80,
		Limits: providers.BudgetLimits{DailyMicroUnits: providers.MicroUnits(daily), MonthlyMicroUnits: providers.MicroUnits(monthly)},
	}
}

func idp(s string) *common.Identifier { return ptr(common.Identifier(s)) }

func tenantBudget(app, tenant string, daily, monthly int64) providers.Budget {
	b := providerBudget("x", providers.ChannelInteractive, daily, monthly)
	b.Subject = providers.BudgetSubject{Kind: providers.BudgetSubjectKindTenant, ApplicationID: idp(app), TenantID: idp(tenant)}
	return b
}

func workspaceBudget(app, tenant, wsp string, daily, monthly int64) providers.Budget {
	b := tenantBudget(app, tenant, daily, monthly)
	b.Subject = providers.BudgetSubject{Kind: providers.BudgetSubjectKindWorkspace, ApplicationID: idp(app), TenantID: idp(tenant), WorkspaceID: idp(wsp)}
	return b
}

func principalBudget(app, tenant, wsp, user string, daily, monthly int64) providers.Budget {
	b := tenantBudget(app, tenant, daily, monthly)
	b.Subject = providers.BudgetSubject{Kind: providers.BudgetSubjectKindPrincipal, ApplicationID: idp(app), TenantID: idp(tenant),
		WorkspaceID: idp(wsp), PrincipalID: idp(user)}
	return b
}

func (g *rig) put(b providers.Budget) providers.Budget {
	g.t.Helper()
	out, err := g.svc.PutBudget(context.Background(), g.operator(), b)
	if err != nil {
		g.t.Fatalf("PutBudget: %v", err)
	}
	return out
}

// operator is the scope budgets are set under: the same application.
func (g *rig) operator() contracts.Scope {
	return g.scopeOf(tApp, "ten_admin", "wsp_admin", "usr_admin")
}

// callOf builds a call to provider on the interactive channel with the given units.
func callOf(provider string, u Units) Call {
	return Call{
		Provider: providers.ProviderRef{
			ProfileID: providers.ProfileID(ids.New("pprf")), ProviderID: providers.ProviderID(provider),
			ModelID: "model-a", ModelVersion: "2026-10-01",
		},
		Role: providers.RoleConversationalController, Channel: providers.ChannelInteractive,
		EnvelopeID: providers.EnvelopeID(ids.New("envl")), DecisionID: providers.EgressDecisionID(ids.New("edec")),
		Prices: prices(), Units: u,
	}
}

// out is a call whose upper bound is n output toks, costing 2n micro-units.
func out(n int) Units { return Units{ToksOut: providers.Counter(n)} }

func (g *rig) reserve(scope contracts.Scope, c Call) (Reservation, error) {
	return g.svc.Reserve(context.Background(), scope, c)
}

func (g *rig) mustReserve(c Call) Reservation {
	g.t.Helper()
	r, err := g.reserve(g.scope, c)
	if err != nil {
		g.t.Fatalf("Reserve: %v", err)
	}
	return r
}

func (g *rig) mustSettle(r Reservation, a Actual) Settlement {
	g.t.Helper()
	if a.Outcome == "" {
		a.Outcome = providers.UsageRecordOutcomeSucceeded
	}
	s, err := g.svc.Settle(context.Background(), g.scope, r.ID, a)
	if err != nil {
		g.t.Fatalf("Settle: %v", err)
	}
	return s
}

func (g *rig) state(b providers.Budget) providers.BudgetState {
	g.t.Helper()
	st, err := g.svc.State(context.Background(), g.operator(), b.BudgetID)
	if err != nil {
		g.t.Fatalf("State: %v", err)
	}
	assertValid(g.t, urnBudgetState, st)
	return st
}

func win(st providers.BudgetState, w providers.BudgetStateWindowWindow) providers.BudgetStateWindow {
	for _, x := range st.Windows {
		if x.Window == w {
			return x
		}
	}
	panic("no such window")
}

func wantCode(t *testing.T, err error, code contracts.ErrCode, reason string) *contracts.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s (%s), got no error", code, reason)
	}
	if got := contracts.CodeOf(err); got != code {
		t.Fatalf("code = %s, want %s: %v", got, code, err)
	}
	if got := ReasonOf(err); got != reason {
		t.Fatalf("reason = %q, want %q: %v", got, reason, err)
	}
	var ce *contracts.Error
	_ = errors.As(err, &ce)
	return ce
}

func count(t *testing.T, db *store.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// ---- schema validation ----------------------------------------------------------------------

const (
	urnBudgetState = "urn:alixai:schema:providers:budget-state:v1"
	urnUsageRecord = "urn:alixai:schema:providers:usage-record:v1"
	urnBudget      = "urn:alixai:schema:providers:budget:v1"
)

type refuseLoader struct{}

func (refuseLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("schema reference %q is not one of the loaded schemas", url)
}

var (
	schemaOnce sync.Once
	schemaComp *jsonschema.Compiler
	schemaErr  error
	schemaMu   sync.Mutex
)

func schemaCompiler() (*jsonschema.Compiler, error) {
	schemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		c.UseLoader(refuseLoader{})
		for _, dir := range []string{"../../schemas/common", "../../schemas/providers"} {
			files, err := filepath.Glob(filepath.Join(dir, "*.schema.json"))
			if err != nil || len(files) == 0 {
				schemaErr = fmt.Errorf("no schemas under %s", dir)
				return
			}
			for _, f := range files {
				raw, err := os.ReadFile(f)
				if err != nil {
					schemaErr = err
					return
				}
				doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
				if err != nil {
					schemaErr = fmt.Errorf("%s: %w", f, err)
					return
				}
				obj, _ := doc.(map[string]any)
				id, _ := obj["$id"].(string)
				if err := c.AddResource(id, doc); err != nil {
					schemaErr = fmt.Errorf("%s: %w", f, err)
					return
				}
			}
		}
		schemaComp = c
	})
	return schemaComp, schemaErr
}

func assertValid(t testing.TB, urn string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal for %s: %v", urn, err)
	}
	c, err := schemaCompiler()
	if err != nil {
		t.Fatalf("schemas: %v", err)
	}
	schemaMu.Lock()
	sch, err := c.Compile(urn)
	schemaMu.Unlock()
	if err != nil {
		t.Fatalf("compile %s: %v", urn, err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("%s: not JSON: %v", urn, err)
	}
	if err := sch.Validate(inst); err != nil {
		t.Errorf("does not validate against %s:\n%s\n%s", urn, strings.TrimSpace(err.Error()), raw)
	}
}
