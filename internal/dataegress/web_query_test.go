package dataegress

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func (f *fixture) enableWebQuery(provs []providers.EgressPolicyWebQueryProvider) {
	f.t.Helper()
	for _, lv := range []providers.EgressLevel{
		providers.EgressLevelDeployment, providers.EgressLevelApplication,
		providers.EgressLevelTenant, providers.EgressLevelWorkspace,
	} {
		f.put(lv, func(in *PolicyInput) {
			in.WebQuery = &providers.EgressPolicyWebQueryEgress{Enabled: true, AllowedProviders: append([]providers.EgressPolicyWebQueryProvider{}, provs...)}
		})
	}
}

func TestWebQueryAbsentRefused(t *testing.T) {
	f := newFixture(t)
	f.enableAll()
	d, err := f.svc.WebQuery(context.Background(), f.scope, providers.EgressPolicyWebQueryProviderSearxng)
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed || d.Reason != "web_query_absent_deployment" {
		t.Fatalf("got %+v, want absent_deployment", d)
	}
}

func TestWebQueryEachLevelOff(t *testing.T) {
	f := newFixture(t)
	f.enableAll()
	provs := []providers.EgressPolicyWebQueryProvider{providers.EgressPolicyWebQueryProviderSearxng}
	f.enableWebQuery(provs)
	for _, lv := range []providers.EgressLevel{
		providers.EgressLevelDeployment, providers.EgressLevelApplication,
		providers.EgressLevelTenant, providers.EgressLevelWorkspace,
	} {
		f.put(lv, func(in *PolicyInput) {
			in.WebQuery = &providers.EgressPolicyWebQueryEgress{Enabled: false, AllowedProviders: provs}
		})
		d, err := f.svc.WebQuery(context.Background(), f.scope, providers.EgressPolicyWebQueryProviderSearxng)
		if err != nil {
			t.Fatal(err)
		}
		if d.Allowed {
			t.Fatalf("%s off still allowed: %+v", lv, d)
		}
		f.enableWebQuery(provs)
	}
}

func TestWebQueryIntersection(t *testing.T) {
	f := newFixture(t)
	f.enableAll()
	all := []providers.EgressPolicyWebQueryProvider{
		providers.EgressPolicyWebQueryProviderSearxng,
		providers.EgressPolicyWebQueryProviderBrave,
		providers.EgressPolicyWebQueryProviderProviderNative,
	}
	f.enableWebQuery(all)
	f.put(providers.EgressLevelWorkspace, func(in *PolicyInput) {
		in.WebQuery = &providers.EgressPolicyWebQueryEgress{
			Enabled: true,
			AllowedProviders: []providers.EgressPolicyWebQueryProvider{
				providers.EgressPolicyWebQueryProviderSearxng,
			},
		}
	})
	d, err := f.svc.WebQuery(context.Background(), f.scope, providers.EgressPolicyWebQueryProviderSearxng)
	if err != nil || !d.Allowed {
		t.Fatalf("searxng: %+v %v", d, err)
	}
	d, err = f.svc.WebQuery(context.Background(), f.scope, providers.EgressPolicyWebQueryProviderBrave)
	if err != nil || d.Allowed || d.Reason != "web_query_provider_not_listed" {
		t.Fatalf("brave: %+v %v", d, err)
	}
}

func TestWebQuerySourceLevelRejected(t *testing.T) {
	f := newFixture(t)
	f.enableAll()
	id := providers.SourceID(ids.New("src"))
	cls := providers.DataClassificationPublic
	_, err := f.svc.PutPolicy(context.Background(), f.scope, PolicyInput{
		Level: providers.EgressLevelSource, SourceID: id,
		Enabled: true, SourceMode: providers.EgressModeLocalOnly, SourceClass: &cls,
		WebQuery: &providers.EgressPolicyWebQueryEgress{
			Enabled: true, AllowedProviders: []providers.EgressPolicyWebQueryProvider{providers.EgressPolicyWebQueryProviderSearxng},
		},
	})
	if !contracts.HasCode(err, contracts.ErrValidationFailed) {
		t.Fatalf("want validation_failed, got %v", err)
	}
}

func TestWebQueryNoApprovalNeeded(t *testing.T) {
	f := newFixture(t)
	f.enableAll()
	for _, lv := range []providers.EgressLevel{
		providers.EgressLevelDeployment, providers.EgressLevelApplication,
		providers.EgressLevelTenant, providers.EgressLevelWorkspace,
	} {
		f.put(lv, func(in *PolicyInput) {
			in.AllowedClasses = []providers.EgressEligibleClass{providers.EgressEligibleClassPublic}
			in.WebQuery = &providers.EgressPolicyWebQueryEgress{
				Enabled: true, AllowedProviders: []providers.EgressPolicyWebQueryProvider{providers.EgressPolicyWebQueryProviderTavily},
			}
		})
	}
	d, err := f.svc.WebQuery(context.Background(), f.scope, providers.EgressPolicyWebQueryProviderTavily)
	if err != nil || !d.Allowed {
		t.Fatalf("got %+v %v", d, err)
	}
}
