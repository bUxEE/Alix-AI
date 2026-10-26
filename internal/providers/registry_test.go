package providers_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func roleStatuses(ss []providers.RoleStatus) map[providers.Role]providers.RoleStatus {
	m := make(map[providers.Role]providers.RoleStatus, len(ss))
	for _, s := range ss {
		m[s.Role] = s
	}
	return m
}

func TestRegistryExternallyCoveredControllerIsReadyWithoutLocalProvider(t *testing.T) {
	reg := providers.NewRegistry(clock.NewFake(time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)))
	if reg.Overall() != providers.HealthUnavailable {
		t.Fatalf("empty Overall = %s", reg.Overall())
	}
	reg.MarkExternallyCovered(providers.RoleController)
	if !reg.ExternallyCovered(providers.RoleController) {
		t.Fatal("ExternallyCovered was not recorded")
	}
	if reg.Has(providers.RoleController) || reg.Usable(providers.RoleController) {
		t.Fatal("external cover must not register a local provider")
	}
	if reg.Overall() != providers.HealthOK {
		t.Fatalf("Overall = %s, want ok when the controller is covered externally", reg.Overall())
	}
	s := reg.Status(providers.RoleController)
	if s.Configured || s.Status != providers.HealthOK || s.Reason != providers.ExternalCoverReason || s.Effect != "" {
		t.Fatalf("Status = %+v, want ok with reason %q and no local provider", s, providers.ExternalCoverReason)
	}
	if len(reg.Degraded()) != 0 {
		t.Fatalf("Degraded = %+v, want none", reg.Degraded())
	}
}

func TestRegistryDegradedRoles(t *testing.T) {
	start := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	reg := providers.NewRegistry(clk)

	// Nothing registered: the required controller is missing, so the service
	// is unavailable and the controller is reported; optional roles are not.
	if reg.Overall() != providers.HealthUnavailable {
		t.Fatalf("empty registry Overall = %s", reg.Overall())
	}
	deg := reg.Degraded()
	if len(deg) != 1 || deg[0].Role != providers.RoleController || deg[0].Configured || deg[0].Status != providers.HealthUnavailable {
		t.Fatalf("empty registry Degraded = %+v", deg)
	}
	for _, role := range providers.Roles() {
		if reg.Has(role) || reg.Usable(role) {
			t.Fatalf("role %s should be absent", role)
		}
	}

	llm := fake.New(fake.WithName("llm"))
	emb := fake.New(fake.WithName("emb"))
	if err := reg.Register(providers.RoleController, llm); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(providers.RoleEmbedder, emb); err != nil {
		t.Fatal(err)
	}
	// No reranker, no router, no voice: a deliberate lite configuration.
	if !reg.Has(providers.RoleController) || !reg.Has(providers.RoleEmbedder) || reg.Has(providers.RoleReranker) {
		t.Fatal("Has does not reflect registration")
	}
	if reg.Overall() != providers.HealthOK || len(reg.Degraded()) != 0 {
		t.Fatalf("lite config should be healthy: %s %+v", reg.Overall(), reg.Degraded())
	}

	// A missing role is a handled condition: typed access says unavailable
	// and names the section 18 effect.
	_, err := reg.Reranker()
	wantCode(t, err, contracts.ErrUnavailable)
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce.Details["effect"] != providers.EffectFusedOrder || ce.Details["role"] != "reranker" {
		t.Fatalf("missing-role error should carry role and effect: %+v", ce)
	}
	if tg, err := reg.TextGenerator(providers.RoleController); err != nil || tg == nil {
		t.Fatalf("controller access: %v", err)
	}
	if _, err := reg.TextGenerator(providers.RoleEmbedder); err == nil {
		t.Fatal("TextGenerator must refuse a non-text role")
	}

	// The embedder goes down: semantic recall is reduced, the service is degraded, not down.
	clk.Advance(time.Minute)
	if err := reg.SetHealth(providers.RoleEmbedder, providers.HealthUnavailable, "model unloaded"); err != nil {
		t.Fatal(err)
	}
	deg = reg.Degraded()
	if len(deg) != 1 {
		t.Fatalf("Degraded = %+v", deg)
	}
	d := deg[0]
	if d.Role != providers.RoleEmbedder || d.Status != providers.HealthUnavailable || d.Reason != "model unloaded" ||
		d.Effect != providers.EffectReducedSemanticRecall || d.Provider != "emb" || !d.Configured {
		t.Fatalf("embedder degradation = %+v", d)
	}
	if d.Behavior != "Use authorized FTS and exact search and label reduced semantic recall." {
		t.Fatalf("behavior = %q", d.Behavior)
	}
	if !d.Since.Equal(start.Add(time.Minute)) {
		t.Fatalf("Since = %v, want the time of the transition", d.Since)
	}
	if reg.Overall() != providers.HealthDegraded {
		t.Fatalf("Overall = %s, want degraded", reg.Overall())
	}
	if reg.Has(providers.RoleEmbedder) != true || reg.Usable(providers.RoleEmbedder) {
		t.Fatal("an unavailable embedder is still configured but not usable")
	}

	// Re-reporting the same status does not move Since; degraded stays usable.
	clk.Advance(time.Minute)
	_ = reg.SetHealth(providers.RoleEmbedder, providers.HealthUnavailable, "still unloaded")
	if got := reg.Status(providers.RoleEmbedder).Since; !got.Equal(start.Add(time.Minute)) {
		t.Fatalf("Since moved to %v without a status change", got)
	}
	_ = reg.SetHealth(providers.RoleEmbedder, providers.HealthDegraded, "slow")
	if !reg.Usable(providers.RoleEmbedder) || reg.Status(providers.RoleEmbedder).Effect == "" {
		t.Fatal("degraded is usable and still reported with its effect")
	}

	// The controller going down takes the whole service to unavailable.
	_ = reg.SetHealth(providers.RoleController, providers.HealthUnavailable, "crashed, restarting")
	if reg.Overall() != providers.HealthUnavailable {
		t.Fatalf("Overall = %s, want unavailable", reg.Overall())
	}
	byRole := roleStatuses(reg.Degraded())
	if byRole[providers.RoleController].Effect != providers.EffectLLMUnavailable {
		t.Fatalf("controller effect = %q", byRole[providers.RoleController].Effect)
	}
	// Recovery.
	_ = reg.SetHealth(providers.RoleController, providers.HealthOK, "")
	_ = reg.SetHealth(providers.RoleEmbedder, providers.HealthOK, "")
	if reg.Overall() != providers.HealthOK || len(reg.Degraded()) != 0 {
		t.Fatalf("after recovery: %s %+v", reg.Overall(), reg.Degraded())
	}

	// Snapshot lists every role in canonical order, as a copy.
	snap := reg.Snapshot()
	var order []providers.Role
	for _, s := range snap {
		order = append(order, s.Role)
	}
	if diff := cmp.Diff(providers.Roles(), order); diff != "" {
		t.Fatalf("Snapshot order (-want +got):\n%s", diff)
	}
	snap[0].Status = providers.HealthDegraded
	if reg.Status(providers.RoleController).Status != providers.HealthOK {
		t.Fatal("Snapshot aliases registry state")
	}
	rr := reg.Status(providers.RoleReranker)
	if rr.Configured || rr.Status != providers.HealthUnavailable || rr.Reason != "not configured" {
		t.Fatalf("unconfigured role status = %+v", rr)
	}
}

func TestRegistryRegistrationRules(t *testing.T) {
	reg := providers.NewRegistry(nil) // wall clock
	f := fake.New()

	wantCode(t, reg.Register("nonsense", f), contracts.ErrValidationFailed)
	wantCode(t, reg.Register(providers.RoleController, nil), contracts.ErrValidationFailed)
	wantCode(t, reg.Register(providers.RoleController, struct{}{}), contracts.ErrValidationFailed)
	wantCode(t, reg.Register(providers.RoleEmbedder, badJSONGenerator{}), contracts.ErrValidationFailed)

	// One value may serve several roles; duplicates are refused.
	for _, role := range []providers.Role{providers.RoleController, providers.RoleRouter, providers.RoleEmbedder, providers.RoleReranker} {
		if err := reg.Register(role, f); err != nil {
			t.Fatalf("register %s: %v", role, err)
		}
	}
	wantCode(t, reg.Register(providers.RoleRouter, fake.New()), contracts.ErrConflict)
	if e, err := reg.Embedder(); err != nil || e == nil {
		t.Fatalf("Embedder: %v", err)
	}
	if r, err := reg.Reranker(); err != nil || r == nil {
		t.Fatalf("Reranker: %v", err)
	}

	// Voice roles are reserved: accepted without an interface (M4 owns them),
	// reachable through Lookup, reported like any other role.
	type sttStub struct{ id string }
	if err := reg.Register(providers.RoleSTT, &sttStub{"stt-1"}); err != nil {
		t.Fatalf("reserved role should accept a provider: %v", err)
	}
	stub, err := providers.Lookup[*sttStub](reg, providers.RoleSTT)
	if err != nil || stub.id != "stt-1" {
		t.Fatalf("Lookup = %+v, %v", stub, err)
	}
	_, err = providers.Lookup[providers.Embedder](reg, providers.RoleSTT)
	wantCode(t, err, contracts.ErrInternal)
	_, err = providers.Lookup[*sttStub](reg, providers.RoleTTS)
	wantCode(t, err, contracts.ErrUnavailable)

	wantCode(t, reg.SetHealth(providers.RoleVAD, providers.HealthOK, ""), contracts.ErrUnavailable)
	wantCode(t, reg.SetHealth(providers.RoleSTT, "great", ""), contracts.ErrValidationFailed)

	for _, role := range providers.Roles() {
		if !role.Valid() || role.Effect() == "" || role.Behavior() == "" {
			t.Fatalf("role %q lacks metadata", role)
		}
	}
	if !providers.RoleController.Required() || providers.RoleEmbedder.Required() || !providers.RoleTTS.Reserved() || providers.RoleRouter.Reserved() {
		t.Fatal("role flags wrong")
	}
	if got := reg.String(); got == "" {
		t.Fatal("String empty")
	}
}

type flakyProber struct {
	*fake.Provider
	mu     sync.Mutex
	report providers.HealthReport
	err    error
	hang   bool
}

func (p *flakyProber) Health(ctx context.Context) (providers.HealthReport, error) {
	p.mu.Lock()
	r, err, hang := p.report, p.err, p.hang
	p.mu.Unlock()
	if hang {
		<-ctx.Done()
		return providers.HealthReport{}, ctx.Err()
	}
	return r, err
}

func (p *flakyProber) Name() string { return "flaky" }

func TestRegistryRefreshProbesProviders(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC))
	reg := providers.NewRegistry(clk, providers.WithProbeTimeout(30*time.Millisecond))
	p := &flakyProber{Provider: fake.New(), report: providers.HealthReport{Status: providers.HealthOK, Version: "v1.2.3"}}
	if err := reg.Register(providers.RoleController, p); err != nil {
		t.Fatal(err)
	}
	// A provider without Health keeps whatever was set by hand.
	plain := &plainEmbedder{}
	if err := reg.Register(providers.RoleEmbedder, plain); err != nil {
		t.Fatal(err)
	}
	_ = reg.SetHealth(providers.RoleEmbedder, providers.HealthDegraded, "manual")

	ctx := context.Background()
	reg.Refresh(ctx)
	if s := reg.Status(providers.RoleController); s.Status != providers.HealthOK || s.Version != "v1.2.3" || s.Provider != "flaky" {
		t.Fatalf("after ok probe: %+v", s)
	}
	if reg.Status(providers.RoleEmbedder).Status != providers.HealthDegraded {
		t.Fatal("Refresh must leave providers without a probe alone")
	}

	p.mu.Lock()
	p.report = providers.HealthReport{Status: providers.HealthDegraded, Reason: "model loading"}
	p.mu.Unlock()
	clk.Advance(time.Second)
	reg.Refresh(ctx)
	if s := reg.Status(providers.RoleController); s.Status != providers.HealthDegraded || s.Reason != "model loading" {
		t.Fatalf("after degraded probe: %+v", s)
	}

	p.mu.Lock()
	p.err = errors.New("connection refused")
	p.mu.Unlock()
	reg.Refresh(ctx)
	if s := reg.Status(providers.RoleController); s.Status != providers.HealthUnavailable || s.Reason == "" {
		t.Fatalf("after failing probe: %+v", s)
	}

	p.mu.Lock()
	p.err, p.report = nil, providers.HealthReport{Status: "bogus"}
	p.mu.Unlock()
	reg.Refresh(ctx)
	if reg.Status(providers.RoleController).Status != providers.HealthUnavailable {
		t.Fatal("an invalid status must fail closed to unavailable")
	}

	p.mu.Lock()
	p.hang = true
	p.mu.Unlock()
	start := time.Now()
	reg.Refresh(ctx)
	if time.Since(start) > 2*time.Second || reg.Status(providers.RoleController).Status != providers.HealthUnavailable {
		t.Fatal("a hung probe must time out and count as unavailable")
	}
}

type plainEmbedder struct{}

func (plainEmbedder) Embed(context.Context, contracts.Scope, providers.EmbedRequest) (providers.EmbedResult, error) {
	return providers.EmbedResult{}, errors.New("not used")
}

func TestRegistryConcurrentUse(t *testing.T) {
	reg := providers.NewRegistry(nil)
	f := fake.New()
	_ = reg.Register(providers.RoleController, f)
	_ = reg.Register(providers.RoleEmbedder, f)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				switch (i + j) % 4 {
				case 0:
					_ = reg.SetHealth(providers.RoleEmbedder, providers.HealthDegraded, "x")
				case 1:
					_ = reg.Snapshot()
				case 2:
					reg.Refresh(context.Background())
				default:
					_, _ = reg.Embedder()
					_ = reg.Has(providers.RoleReranker)
					_ = reg.Overall()
				}
			}
		}(i)
	}
	wg.Wait()
}
