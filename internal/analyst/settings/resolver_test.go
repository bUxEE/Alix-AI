package settings

import (
	"testing"
	"time"

	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

func ptr[T any](v T) *T { return &v }

func allOnConfig() an.Config {
	return an.Config{
		Enabled: ptr(true),
		Abilities: &an.ConfigAbilityFlags{
			Loop: ptr(true), Compute: ptr(true), Forecasts: ptr(true), Presentation: ptr(true),
			Drafts: ptr(true), Knowledge: ptr(true), WebSearch: ptr(true), WebFetch: ptr(true),
		},
	}
}

func allOnApp() *applicationpackage.ManifestAssistant {
	return &applicationpackage.ManifestAssistant{
		Domain: &an.Domain{
			Description: "d", InScope: []string{"a"}, OutOfScope: []string{"b"},
			KnowledgeAllowed: true, WebSearchAllowed: true,
		},
		Abilities: &applicationpackage.ManifestAssistantAbilities{
			Loop: ptr(true), Compute: ptr(true), Forecasts: ptr(true), Presentation: ptr(true),
			Drafts: ptr(true), Knowledge: ptr(true), WebSearch: ptr(true), WebFetch: ptr(true),
		},
	}
}

func TestResolverMostRestrictiveWins(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	base := ResolveInput{
		Deployment: allOnConfig(), Application: allOnApp(), AnalystCapable: true,
		WebConfigured: true, DomainDeclared: true, Now: now,
	}
	got := AbilityResolver{}.Resolve(base)
	if !got.Enabled || !got.Abilities.Loop || !got.Abilities.WebSearch {
		t.Fatalf("all on: %+v", got)
	}

	t.Run("deployment_off", func(t *testing.T) {
		in := base
		in.Deployment.Abilities.WebSearch = ptr(false)
		g := AbilityResolver{}.Resolve(in)
		if g.Abilities.WebSearch {
			t.Fatal("deployment off must win")
		}
		if len(g.DisabledReasons.WebSearch) == 0 || g.DisabledReasons.WebSearch[0] != an.AbilitiesReasonListItemDeploymentOff {
			t.Fatalf("reasons %+v", g.DisabledReasons.WebSearch)
		}
	})
	t.Run("application_off", func(t *testing.T) {
		in := base
		in.Application.Abilities.Compute = ptr(false)
		g := AbilityResolver{}.Resolve(in)
		if g.Abilities.Compute {
			t.Fatal("app off")
		}
	})
	t.Run("tenant_off", func(t *testing.T) {
		in := base
		in.Tenant = an.TenantSettings{Abilities: &an.TenantSettingsAbilities{Loop: ptr(false)}}
		g := AbilityResolver{}.Resolve(in)
		if g.Abilities.Loop || g.Enabled {
			t.Fatalf("%+v", g)
		}
	})
	t.Run("model_not_capable", func(t *testing.T) {
		in := base
		in.AnalystCapable = false
		g := AbilityResolver{}.Resolve(in)
		if g.Abilities.Loop || g.Enabled {
			t.Fatal("non-capable model")
		}
		found := false
		for _, r := range g.DisabledReasons.Loop {
			if r == an.AbilitiesReasonListItemModelNotAnalystCapable {
				found = true
			}
		}
		if !found {
			t.Fatalf("reasons %+v", g.DisabledReasons.Loop)
		}
	})
	t.Run("domain_undeclared", func(t *testing.T) {
		in := base
		in.DomainDeclared = false
		g := AbilityResolver{}.Resolve(in)
		if g.Abilities.Knowledge || g.Abilities.WebSearch {
			t.Fatal("domain gate")
		}
	})
	t.Run("provider_unconfigured", func(t *testing.T) {
		in := base
		in.WebConfigured = false
		g := AbilityResolver{}.Resolve(in)
		if g.Abilities.WebFetch {
			t.Fatal("web gate")
		}
	})
}

func TestTenantCannotWiden(t *testing.T) {
	dep := allOnConfig()
	dep.Abilities.WebSearch = ptr(false)
	app := allOnApp()
	ten := an.TenantSettings{Abilities: &an.TenantSettingsAbilities{WebSearch: ptr(true)}}
	if bad := TenantWidens(ten, dep, app); len(bad) == 0 {
		t.Fatal("expected widen refusal")
	}
	ten2 := an.TenantSettings{Abilities: &an.TenantSettingsAbilities{WebSearch: ptr(false), Loop: ptr(true)}}
	if bad := TenantWidens(ten2, allOnConfig(), app); len(bad) != 0 {
		t.Fatalf("narrowing loop+off web should be ok: %v", bad)
	}
}
