package settings

import (
	"context"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

type fixedSources struct {
	dep     an.Config
	app     *applicationpackage.ManifestAssistant
	capable bool
	web     bool
}

func (f fixedSources) DeploymentConfig(context.Context, contracts.Scope) (an.Config, error) {
	return f.dep, nil
}
func (f fixedSources) ApplicationAssistant(context.Context, contracts.Scope) (*applicationpackage.ManifestAssistant, error) {
	return f.app, nil
}
func (f fixedSources) ControllerAnalystCapable(context.Context, contracts.Scope) (bool, error) {
	return f.capable, nil
}
func (f fixedSources) WebConfigured(context.Context, contracts.Scope) (bool, error) {
	return f.web, nil
}

func testScope(tenant, principal string, caps ...string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep_test", ApplicationID: "app_demo", TenantID: tenant, WorkspaceID: "wsp_main",
		PrincipalID: principal, SessionID: "ses_01J00000000000000000000001",
		Roles: []string{"member"}, CapabilityScopes: append([]string(nil), caps...),
	}
}

func TestServicePutNarrowEffectiveAndAuthz(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC))
	db := storetest.Open(t, nil, func(o *store.Options) { o.Clock = clk })
	alog, err := audit.New(db, audit.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	src := fixedSources{dep: allOnConfig(), app: allOnApp(), capable: true, web: true}
	svc := &Service{
		Store:  NewStore(db, clk, alog),
		Auth:   DefaultAuthorizer{},
		Source: src,
	}
	ctx := context.Background()
	sc := testScope("ten_acme", "usr_admin", "ops.analyst.read", "ops.analyst.write")

	if err := svc.AuthorizeTenantWrite(ctx, sc, "ten_other"); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Fatalf("cross-tenant write: %v", err)
	}
	if err := svc.AuthorizeTenantWrite(ctx, sc, "ten_acme"); err != nil {
		t.Fatal(err)
	}

	widen := an.TenantSettings{Abilities: &an.TenantSettingsAbilities{WebSearch: ptr(true)}}
	src.dep.Abilities.WebSearch = ptr(false)
	svc.Source = src
	_, err = svc.PutTenant(ctx, sc, "ten_acme", widen)
	if contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Fatalf("widen: %v", err)
	}

	src.dep = allOnConfig()
	svc.Source = src
	narrow := an.TenantSettings{Abilities: &an.TenantSettingsAbilities{
		Loop: ptr(true), Compute: ptr(true), Forecasts: ptr(false), Presentation: ptr(true),
		Drafts: ptr(true), Knowledge: ptr(true), WebSearch: ptr(false), WebFetch: ptr(true),
	}}
	got, err := svc.PutTenant(ctx, sc, "ten_acme", narrow)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpdatedAt == nil {
		t.Fatal("updated_at")
	}
	read, err := svc.GetTenant(ctx, sc, "ten_acme")
	if err != nil || read.Abilities == nil || boolOr(read.Abilities.WebSearch, true) {
		t.Fatalf("read back: %+v %v", read, err)
	}

	eff, err := svc.Effective(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Abilities.Loop || eff.Abilities.WebSearch || eff.Abilities.Forecasts {
		t.Fatalf("effective %+v", eff.Abilities)
	}
}
