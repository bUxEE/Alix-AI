package providerbudget

import (
	"context"
	"errors"
	"time"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// dimCost is the counter dimension of money; the other dimensions are the meter units.
const dimCost = "cost"

// snapshot is the consumption of one budget in the two windows around an instant: settled counters
// and the live (unexpired) holds of open reservations, by "window/dimension".
type snapshot struct {
	spent    map[string]int64
	reserved map[string]int64
}

func (sn snapshot) get(m map[string]int64, kind, dim string) int64 { return m[kind+"/"+dim] }

// loadSnapshot reads the counters of the windows containing now and the holds that are still live.
func loadSnapshot(ctx context.Context, q store.Reader, app string, r budgetRow, ws [2]window, now time.Time) (snapshot, error) {
	sn := snapshot{spent: map[string]int64{}, reserved: map[string]int64{}}
	id := string(r.b.BudgetID)
	rows, err := q.QueryContext(ctx,
		`SELECT dimension, window_kind, used FROM providerbudget_counters
		  WHERE application_id = ? AND tenant_id = ? AND workspace_id = ? AND budget_id = ?
		    AND ((window_kind = 'daily' AND window_start = ?) OR (window_kind = 'monthly' AND window_start = ?))`,
		app, r.tenant, r.workspace, id, ws[0].start.UnixMilli(), ws[1].start.UnixMilli())
	if err != nil {
		return sn, wrapStore(err, "providerbudget: reading counters failed")
	}
	for rows.Next() {
		var dim, kind string
		var used int64
		if err := rows.Scan(&dim, &kind, &used); err != nil {
			rows.Close()
			return sn, wrapStore(err, "providerbudget: reading counters failed")
		}
		sn.spent[kind+"/"+dim] = used
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return sn, wrapStore(err, "providerbudget: reading counters failed")
	}
	for i, col := range []string{"daily_start", "monthly_start"} {
		hr, err := q.QueryContext(ctx,
			`SELECT dimension, SUM(amount) FROM providerbudget_holds
			  WHERE application_id = ? AND tenant_id = ? AND workspace_id = ? AND budget_id = ?
			    AND expires_at > ? AND `+col+` = ? GROUP BY dimension`,
			app, r.tenant, r.workspace, id, now.UnixMilli(), ws[i].start.UnixMilli())
		if err != nil {
			return sn, wrapStore(err, "providerbudget: reading holds failed")
		}
		for hr.Next() {
			var dim string
			var sum int64
			if err := hr.Scan(&dim, &sum); err != nil {
				hr.Close()
				return sn, wrapStore(err, "providerbudget: reading holds failed")
			}
			sn.reserved[ws[i].kind+"/"+dim] = sum
		}
		if err := errors.Join(hr.Err(), hr.Close()); err != nil {
			return sn, wrapStore(err, "providerbudget: reading holds failed")
		}
	}
	return sn, nil
}

// limitOf is the cost limit of the window.
func limitOf(b providers.Budget, kind string) int64 {
	if kind == "daily" {
		return int64(b.Limits.DailyMicroUnits)
	}
	return int64(b.Limits.MonthlyMicroUnits)
}

// windowStatus derives remaining and the status of one window: exhausted exactly when nothing
// remains, warning from warn_at_percent of the limit (spent plus reserved), else ok.
func windowStatus(limit, spent, reserved, warn int64) (remaining int64, st providers.BudgetStatus) {
	used := spent + reserved
	remaining = max(limit-used, 0)
	switch {
	case remaining == 0:
		return 0, providers.BudgetStatusExhausted
	case used*100 >= limit*warn:
		return remaining, providers.BudgetStatusWarning
	}
	return remaining, providers.BudgetStatusOk
}

func worse(a, b providers.BudgetStatus) providers.BudgetStatus {
	rank := func(s providers.BudgetStatus) int {
		switch s {
		case providers.BudgetStatusExhausted:
			return 2
		case providers.BudgetStatusWarning:
			return 1
		}
		return 0
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// stateOf builds the budget-state object of schemas/providers/budget-state.
func stateOf(r budgetRow, sn snapshot, ws [2]window, now time.Time, fallback bool) providers.BudgetState {
	st := providers.BudgetState{
		BudgetID: r.b.BudgetID, Subject: r.b.Subject, Channel: r.b.Channel, Currency: r.b.Currency,
		AsOf: timestamp(now), State: providers.BudgetStatusOk, LocalFallbackAvailable: fallback,
		Windows: make([]providers.BudgetStateWindow, 0, 2),
	}
	for _, w := range ws {
		limit := limitOf(r.b, w.kind)
		spent, reserved := sn.get(sn.spent, w.kind, dimCost), sn.get(sn.reserved, w.kind, dimCost)
		remaining, status := windowStatus(limit, spent, reserved, r.b.WarnAtPercent)
		name := providers.BudgetStateWindowWindowDaily
		if w.kind == "monthly" {
			name = providers.BudgetStateWindowWindowMonthly
		}
		st.Windows = append(st.Windows, providers.BudgetStateWindow{
			Window: name, StartsAt: timestamp(w.start), EndsAt: timestamp(w.end),
			LimitMicroUnits:     providers.MicroUnits(limit),
			SpentMicroUnits:     providers.MicroUnits(min(spent, MaxAmount)),
			ReservedMicroUnits:  providers.MicroUnits(reserved),
			RemainingMicroUnits: providers.MicroUnits(remaining),
			State:               status,
		})
		st.State = worse(st.State, status)
	}
	return st
}

func (s *Service) hint(ctx context.Context, scope contracts.Scope, provider providers.ProviderID) bool {
	return s.fallback != nil && s.fallback(ctx, scope, provider)
}

// State returns the budget-state object of one budget of the caller's application: both UTC windows
// with limit, spent (settled), reserved (in flight), remaining and state, the overall state and the
// local-fallback hint. Operator function; a host shows the budgets that apply to a caller with
// States.
func (s *Service) State(ctx context.Context, scope contracts.Scope, budgetID providers.BudgetID) (providers.BudgetState, error) {
	if err := scope.Validate(); err != nil {
		return providers.BudgetState{}, err
	}
	now := s.clk.Now()
	var st providers.BudgetState
	var prov providers.ProviderID
	err := s.db.View(ctx, func(tx *store.Tx) error {
		r, found, err := loadBudget(ctx, tx, scope.ApplicationID, string(budgetID))
		if err != nil {
			return err
		}
		if !found {
			return invalid("unknown_budget", "providerbudget: no such budget")
		}
		ws := windowsAt(now)
		sn, err := loadSnapshot(ctx, tx, scope.ApplicationID, r, ws, now)
		if err != nil {
			return err
		}
		prov = providers.ProviderID(r.provider())
		st = stateOf(r, sn, ws, now, false)
		return nil
	})
	if err != nil {
		return providers.BudgetState{}, err
	}
	st.LocalFallbackAvailable = s.hint(ctx, scope, prov)
	return st, nil
}

// StatesOf is State for several budgets of the caller's application, read in one snapshot: the
// budget-state objects in the order of ids. A budget that is not the application's fails the whole
// read, as it fails State. Operator function.
func (s *Service) StatesOf(ctx context.Context, scope contracts.Scope, ids []providers.BudgetID) ([]providers.BudgetState, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	now := s.clk.Now()
	var out []providers.BudgetState
	var provs []providers.ProviderID
	err := s.db.View(ctx, func(tx *store.Tx) error {
		out, provs = make([]providers.BudgetState, 0, len(ids)), make([]providers.ProviderID, 0, len(ids))
		ws := windowsAt(now)
		for _, id := range ids {
			r, found, err := loadBudget(ctx, tx, scope.ApplicationID, string(id))
			if err != nil {
				return err
			}
			if !found {
				return invalid("unknown_budget", "providerbudget: no such budget")
			}
			sn, err := loadSnapshot(ctx, tx, scope.ApplicationID, r, ws, now)
			if err != nil {
				return err
			}
			provs = append(provs, providers.ProviderID(r.provider()))
			out = append(out, stateOf(r, sn, ws, now, false))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].LocalFallbackAvailable = s.hint(ctx, scope, provs[i])
	}
	return out, nil
}

// States returns the budget state of every budget that applies to a call of scope to provider on
// channel (the provider's and the scope's own tenant, workspace and principal budgets): what the
// host shows the user. A budget of another tenant, workspace or principal is never included.
func (s *Service) States(ctx context.Context, scope contracts.Scope, provider providers.ProviderID, channel providers.Channel) ([]providers.BudgetState, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if !channel.Valid() {
		return nil, invalid("bad_call", "providerbudget: the channel is not valid")
	}
	now := s.clk.Now()
	var out []providers.BudgetState
	err := s.db.View(ctx, func(tx *store.Tx) error {
		rows, err := applicable(ctx, tx, scope, provider, channel)
		if err != nil {
			return err
		}
		ws := windowsAt(now)
		for _, r := range rows {
			sn, err := loadSnapshot(ctx, tx, scope.ApplicationID, r, ws, now)
			if err != nil {
				return err
			}
			out = append(out, stateOf(r, sn, ws, now, false))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	hint := s.hint(ctx, scope, provider)
	for i := range out {
		out[i].LocalFallbackAvailable = hint
	}
	return out, nil
}
