//go:build !noexternal

package cli

import (
	"context"

	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// MeUsage implements gateway.MeUsageBudgetService for the caller's own usage.
func (s *principalLLM) MeUsage(ctx context.Context, scope contracts.Scope, q gateway.MeUsageQuery) (pv.MeUsage, error) {
	if s == nil || s.admin == nil || s.admin.budget == nil {
		return pv.MeUsage{}, contracts.New(contracts.ErrUnavailable, "principal usage is not available on this server").
			WithDetails("reason", "usage_not_configured")
	}
	page, err := s.admin.budget.MeUsage(ctx, scope, providerbudget.MeUsageQuery{
		From: q.From, To: q.To, TenantID: q.TenantID, Limit: q.Limit, Cursor: q.Cursor,
	})
	if err != nil {
		return pv.MeUsage{}, err
	}
	out := pv.MeUsage{Groups: make([]pv.MeUsageGroup, 0, len(page.Groups)), NextCursor: page.NextCursor}
	for _, g := range page.Groups {
		out.Groups = append(out.Groups, pv.MeUsageGroup{
			Day: g.Day, ProviderID: g.ProviderID, Role: g.Role, TenantID: g.TenantID,
			ToksIn: g.ToksIn, ToksOut: g.ToksOut, AudioSecondsIn: g.AudioSecondsIn,
			CharactersIn: g.CharactersIn, Requests: g.Requests, Currency: g.Currency,
			ChargedMicroUnits: g.ChargedMicroUnits,
		})
	}
	return out, nil
}

// MeBudget implements gateway.MeUsageBudgetService for the caller's own principal budgets.
func (s *principalLLM) MeBudget(ctx context.Context, scope contracts.Scope) (pv.MeBudget, error) {
	if s == nil || s.admin == nil || s.admin.budget == nil {
		return pv.MeBudget{}, contracts.New(contracts.ErrUnavailable, "principal budget is not available on this server").
			WithDetails("reason", "usage_not_configured")
	}
	view, err := s.admin.budget.MeBudget(ctx, scope)
	if err != nil {
		return pv.MeBudget{}, err
	}
	return pv.MeBudget{Budgets: view.Budgets, States: view.States}, nil
}
