//go:build !noexternal

package cli

import (
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/modes"
)

// principalLLMHolder keeps the wired service for retention erase and STT resolution.
var principalLLMHolder *principalLLM

// principalLLMProvide wires PrincipalLLMService and MeUsageBudgetService from the external admin
// (called by providers_external).
func principalLLMProvide(services *gateway.Services, admin *externalAdmin) error {
	if admin == nil || services == nil {
		return nil
	}
	s := newPrincipalLLM(admin)
	principalLLMHolder = s
	if err := gateway.Provide[gateway.PrincipalLLMService](services, s); err != nil {
		return err
	}
	return gateway.Provide[gateway.MeUsageBudgetService](services, s)
}

// principalLLMPolicy wraps the static routing policy with per-principal bindings (ADR 0041).
func principalLLMPolicy(inner modes.PolicySource, res *modes.Resolver, s *principalLLM) modes.PolicySource {
	if s == nil {
		return inner
	}
	// RequireConfigured stays false: an empty deployment route still falls through to local
	// (1.1 behaviour). When true, a bindable role with no binding and no default is
	// llm_not_configured (covered by modes.TestPrincipalPolicyResolutionOrder).
	return modes.PrincipalPolicy{
		Inner: inner, Bindings: principalPolicyBinding{s: s}, Resolver: res,
	}
}

// principalLLMProfiles wraps registry Check so principal-bound profiles use the principal's credential.
func principalLLMProfiles(inner modes.Profiles, s *principalLLM) modes.Profiles {
	if s == nil || s.reg == nil {
		return inner
	}
	return modes.PrincipalProfiles{
		Inner: inner, Registry: s.reg, Bindings: principalBindingAdapter{s: s}, Creds: s,
	}
}
