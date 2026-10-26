package providerbudget

import (
	"context"
	"sort"
	"strconv"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// MeUsageQuery selects the caller's own usage groups. PrincipalID is always the verified token's
// principal (set by the service); TenantID optionally narrows to one tenant of the application.
type MeUsageQuery struct {
	From, To time.Time
	TenantID string
	Limit    int    // 1..100; default 25
	Cursor   string // numeric offset into the sorted group list
}

// MeUsageGroup is one provider/role/day aggregate of the caller's usage.
type MeUsageGroup struct {
	Day               string               `json:"day"`
	ProviderID        providers.ProviderID `json:"provider_id"`
	Role              providers.Role       `json:"role"`
	TenantID          *common.Identifier   `json:"tenant_id,omitzero"`
	ToksIn            providers.Counter    `json:"toks_in"`
	ToksOut           providers.Counter    `json:"toks_out"`
	AudioSecondsIn    providers.Counter    `json:"audio_seconds_in"`
	CharactersIn      providers.Counter    `json:"characters_in"`
	Requests          providers.Counter    `json:"requests"`
	Currency          providers.Currency   `json:"currency"`
	ChargedMicroUnits providers.MicroUnits `json:"charged_micro_units"`
}

// MeUsagePage is a page of the caller's usage groups.
type MeUsagePage struct {
	Groups     []MeUsageGroup `json:"groups"`
	NextCursor *string        `json:"next_cursor"`
}

// MeBudgetView is the caller's own principal budgets with states.
type MeBudgetView struct {
	Budgets []providers.Budget      `json:"budgets"`
	States  []providers.BudgetState `json:"states"`
}

const meUsageDefaultLimit = 25
const meUsageMaxLimit = 100

// MeUsage returns the caller's usage across every tenant of the application (or one tenant when
// TenantID is set), grouped by provider, role and UTC day. Never another principal's rows.
func (s *Service) MeUsage(ctx context.Context, scope contracts.Scope, q MeUsageQuery) (MeUsagePage, error) {
	if err := scope.Validate(); err != nil {
		return MeUsagePage{}, err
	}
	limit := q.Limit
	if limit == 0 {
		limit = meUsageDefaultLimit
	}
	if limit < 1 || limit > meUsageMaxLimit {
		return MeUsagePage{}, invalid("bad_filter", "providerbudget: me usage limit is 1 to 100")
	}
	offset := 0
	if q.Cursor != "" {
		n, err := strconv.Atoi(q.Cursor)
		if err != nil || n < 0 {
			return MeUsagePage{}, invalid("bad_filter", "providerbudget: me usage cursor is not valid")
		}
		offset = n
	}
	f := UsageFilter{
		From: q.From, To: q.To,
		ApplicationWide: true,
		PrincipalID:     scope.PrincipalID,
		TenantID:        q.TenantID,
	}
	exp, err := s.ExportUsage(ctx, scope, f)
	if err != nil {
		return MeUsagePage{}, err
	}
	type key struct {
		day, provider, role, currency string
	}
	idx := map[key]*MeUsageGroup{}
	var keys []key
	for _, r := range exp.Rows {
		rec := r.Record
		k := key{r.ChargedDay, string(rec.Provider.ProviderID), string(rec.Role), string(rec.Cost.Currency)}
		g := idx[k]
		if g == nil {
			g = &MeUsageGroup{
				Day: r.ChargedDay, ProviderID: rec.Provider.ProviderID, Role: rec.Role,
				Currency: rec.Cost.Currency,
			}
			if q.TenantID != "" {
				tid := common.Identifier(q.TenantID)
				g.TenantID = &tid
			}
			idx[k] = g
			keys = append(keys, k)
		}
		m := rec.Metering
		g.ToksIn += m.ToksIn
		g.ToksOut += m.ToksOut
		g.AudioSecondsIn += m.AudioSecondsIn
		if m.CharactersIn != nil {
			g.CharactersIn += *m.CharactersIn
		}
		g.Requests += m.Requests
		g.ChargedMicroUnits += r.ChargedMicroUnits
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.day != b.day {
			return a.day < b.day
		}
		if a.provider != b.provider {
			return a.provider < b.provider
		}
		if a.role != b.role {
			return a.role < b.role
		}
		return a.currency < b.currency
	})
	out := MeUsagePage{Groups: []MeUsageGroup{}}
	if offset > len(keys) {
		offset = len(keys)
	}
	end := offset + limit
	if end > len(keys) {
		end = len(keys)
	}
	for _, k := range keys[offset:end] {
		out.Groups = append(out.Groups, *idx[k])
	}
	if end < len(keys) {
		c := strconv.Itoa(end)
		out.NextCursor = &c
	}
	return out, nil
}

// MeBudget returns every principal budget of the verified principal in the application (workspace-
// bound and application-wide) with their states.
func (s *Service) MeBudget(ctx context.Context, scope contracts.Scope) (MeBudgetView, error) {
	if err := scope.Validate(); err != nil {
		return MeBudgetView{}, err
	}
	all, err := s.ListBudgets(ctx, scope)
	if err != nil {
		return MeBudgetView{}, err
	}
	var list []providers.Budget
	var ids []providers.BudgetID
	for _, b := range all {
		switch b.Subject.Kind {
		case providers.BudgetSubjectKindPrincipal, providers.BudgetSubjectKindPrincipalApplication:
		default:
			continue
		}
		if b.Subject.PrincipalID == nil || string(*b.Subject.PrincipalID) != scope.PrincipalID {
			continue
		}
		list = append(list, b)
		ids = append(ids, b.BudgetID)
	}
	out := MeBudgetView{Budgets: list, States: []providers.BudgetState{}}
	if len(ids) == 0 {
		if out.Budgets == nil {
			out.Budgets = []providers.Budget{}
		}
		return out, nil
	}
	states, err := s.StatesOf(ctx, scope, ids)
	if err != nil {
		return MeBudgetView{}, err
	}
	out.States = states
	return out, nil
}
