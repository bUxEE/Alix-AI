//go:build !noexternal

package cli

import (
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/providerreg"
)

func privateLLMProvide(services *gateway.Services, admin *externalAdmin) error {
	if admin == nil || services == nil {
		return nil
	}
	s := newPrivateLLMService(admin)
	return gateway.Provide[gateway.PrivateLLMService](services, s)
}

func privateLLMPolicy(inner modes.PolicySource, res *modes.Resolver, reg *providerreg.Registry) modes.PolicySource {
	if reg == nil {
		return inner
	}
	return modes.PrivatePolicy{Inner: inner, Endpoints: reg, Resolver: res}
}

func privateLLMProfiles(inner modes.Profiles, pll *principalLLM) modes.Profiles {
	if pll == nil || pll.reg == nil {
		return principalLLMProfiles(inner, pll)
	}
	wrapped := principalLLMProfiles(inner, pll)
	return modes.PrivateProfiles{Inner: wrapped, Registry: pll.reg, Private: pll.reg, Creds: pll}
}
