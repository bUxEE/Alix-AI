package dataegress

import (
	"context"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// WebQueryDecision is the typed outcome of Service.WebQuery.
type WebQueryDecision struct {
	Allowed bool
	// Reason is a stable machine code when Allowed is false
	// (web_query_absent, web_query_off_<level>, web_query_provider_not_listed, …).
	Reason string
	// AllowedProviders is the intersection of enabled levels' allowed_providers
	// when Allowed is true (or empty when refused).
	AllowedProviders []providers.EgressPolicyWebQueryProvider
}

// WebQuery reports whether a vetted web search query may leave to provider
// under the caller's scope. Absent or enabled:false at any of the four chain
// levels refuses. Effective allowed_providers is the intersection; provider
// must be in that set. Kill switch and disabled external mode refuse.
func (s *Service) WebQuery(ctx context.Context, scope contracts.Scope, provider providers.EgressPolicyWebQueryProvider) (WebQueryDecision, error) {
	if err := scope.Validate(); err != nil {
		return WebQueryDecision{}, err
	}
	if !provider.Valid() {
		return WebQueryDecision{Reason: "web_query_unknown_provider"}, nil
	}
	ks, err := s.KillSwitch(ctx, scope.DeploymentID)
	if err != nil {
		return WebQueryDecision{}, err
	}
	if ks.Engaged {
		return WebQueryDecision{Reason: "kill_switch"}, nil
	}
	ch, err := s.loadChain(ctx, scope)
	if err != nil {
		return WebQueryDecision{}, err
	}
	levels := ch.all()
	levelNames := [4]string{"deployment", "application", "tenant", "workspace"}
	var inter []providers.EgressPolicyWebQueryProvider
	first := true
	for i, p := range levels {
		if p == nil || !p.Enabled {
			return WebQueryDecision{Reason: "external_off_" + levelNames[i]}, nil
		}
		wq := p.WebQuery
		if wq == nil {
			return WebQueryDecision{Reason: "web_query_absent_" + levelNames[i]}, nil
		}
		if !wq.Enabled {
			return WebQueryDecision{Reason: "web_query_off_" + levelNames[i]}, nil
		}
		if first {
			inter = append([]providers.EgressPolicyWebQueryProvider{}, wq.AllowedProviders...)
			first = false
			continue
		}
		inter = intersectWebProviders(inter, wq.AllowedProviders)
	}
	if !containsWebProvider(inter, provider) {
		return WebQueryDecision{Reason: "web_query_provider_not_listed", AllowedProviders: inter}, nil
	}
	return WebQueryDecision{Allowed: true, AllowedProviders: inter}, nil
}

func intersectWebProviders(a, b []providers.EgressPolicyWebQueryProvider) []providers.EgressPolicyWebQueryProvider {
	set := map[providers.EgressPolicyWebQueryProvider]bool{}
	for _, p := range b {
		set[p] = true
	}
	out := make([]providers.EgressPolicyWebQueryProvider, 0, len(a))
	for _, p := range a {
		if set[p] {
			out = append(out, p)
		}
	}
	return out
}

func containsWebProvider(list []providers.EgressPolicyWebQueryProvider, p providers.EgressPolicyWebQueryProvider) bool {
	for _, x := range list {
		if x == p {
			return true
		}
	}
	return false
}
