package dataegress

import (
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func TestSelfHostedInternalWithoutChainApproval(t *testing.T) {
	f := newFixture(t)
	prof := providers.ProviderProfile{
		ProviderID: selfHostedProviderID,
		AllowedDataClasses: []providers.EgressEligibleClass{
			providers.EgressEligibleClassPublic,
			providers.EgressEligibleClassInternal,
		},
	}
	r := &run{s: f.svc, prof: prof, chain: chain{}}
	if !r.classAllowed(providers.EgressEligibleClassInternal, nil) {
		t.Fatal("internal should be allowed for self_hosted without chain approval")
	}
	if r.classAllowed(providers.EgressEligibleClass("restricted"), nil) {
		t.Fatal("restricted must never leave")
	}
}
