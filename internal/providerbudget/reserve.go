package providerbudget

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Call is what the router tells the budget before an external call: which profile and role, on which
// channel, the envelope and egress decision it belongs to, the price table of the model and the upper
// bound of what the call can consume.
type Call struct {
	Provider   providers.ProviderRef      `json:"provider"`
	Role       providers.Role             `json:"role"`
	Channel    providers.Channel          `json:"channel"`
	EnvelopeID providers.EnvelopeID       `json:"envelope_id"`
	DecisionID providers.EgressDecisionID `json:"decision_id"`
	// Prices is the profile's price table. Its currency must be the currency of every applicable
	// budget.
	Prices providers.PriceTable `json:"prices"`
	// Units is the upper bound of the call: the bounded input, the declared output cap, any media.
	// Requests defaults to 1.
	Units Units `json:"units"`
	// EstimateFloor is an optional estimate computed elsewhere (for example from the request shape by
	// the provider contract's own estimator); the reserved amount is never below it.
	EstimateFloor providers.MicroUnits `json:"estimate_floor"`
	// FallbackFrom is the profile this call replaced when policy named a fallback, else nil.
	FallbackFrom *providers.ProfileID `json:"fallback_from"`
	// TTL is how long the reservation holds its amounts when not settled; zero takes the Config
	// default, and it never exceeds MaxReservationTTL.
	TTL time.Duration `json:"-"`
}

// Warning says a budget window is at or past its warn_at_percent after the reservation. It does not
// stop the call.
type Warning struct {
	BudgetID        providers.BudgetID
	Window          providers.BudgetStateWindowWindow
	UsedMicroUnits  providers.MicroUnits // spent plus reserved, including this call
	LimitMicroUnits providers.MicroUnits
}

// Reservation is an admitted call's hold on its budgets.
type Reservation struct {
	ID        providers.ReservationID
	Provider  providers.ProviderID
	Channel   providers.Channel
	Currency  providers.Currency
	Estimated providers.MicroUnits // the reserved upper bound
	Units     Units                // the reserved units
	BudgetIDs []providers.BudgetID
	CreatedAt time.Time
	ExpiresAt time.Time
	Warnings  []Warning
}

// refusal is the internal form of a refused reservation, turned into a typed error after the
// transaction (the fallback hook may do I/O and must not run inside it).
type refusal struct {
	reason                           string
	window, dimension                string
	limit, used, reserved, requested int64
	state                            providers.BudgetState
	retryAfter                       time.Duration
}

func (r *refusal) Error() string { return "providerbudget: refused: " + r.reason }

func (s *Service) refusalError(ctx context.Context, scope contracts.Scope, provider providers.ProviderID, r *refusal) error {
	var e *contracts.Error
	switch r.reason {
	case ReasonRateLimited:
		e = contracts.New(contracts.ErrRateLimited, "providerbudget: the request rate limit of a budget is reached").
			WithDetails("retry_after_ms", r.retryAfter.Milliseconds())
	case ReasonNoBudget:
		e = contracts.New(contracts.ErrResourceExhausted, "providerbudget: no budget applies to this call on its channel, so no external call is made")
	default:
		e = contracts.Newf(contracts.ErrResourceExhausted, "providerbudget: the %s %s of a budget is exhausted, so no external call is made", r.window, r.dimension)
	}
	e = e.WithDetails("reason", r.reason).
		WithDetails("alternate_provider", "none").
		WithDetails("local_fallback_available", s.hint(ctx, scope, provider))
	if r.reason != ReasonNoBudget {
		e = e.WithDetails("budget_id", string(r.state.BudgetID)).
			WithDetails("window", r.window).
			WithDetails("dimension", r.dimension).
			WithDetails("limit", r.limit).
			WithDetails("used", r.used).
			WithDetails("reserved", r.reserved).
			WithDetails("requested", r.requested).
			WithDetails("budget_state", r.state)
		if r.reason != ReasonRateLimited {
			// the state object carries the fallback hint too
			st := r.state
			st.LocalFallbackAvailable = s.hint(ctx, scope, provider)
			e.Details["budget_state"] = st
		}
	}
	return e
}

func validateCall(c Call) error {
	p := c.Provider
	if !providerIDOK(string(p.ProviderID)) || p.ProfileID == "" || p.ModelID == "" || p.ModelVersion == "" {
		return invalid("bad_call", "providerbudget: the call names no valid provider, profile and model")
	}
	if !c.Role.Valid() || !c.Channel.Valid() {
		return invalid("bad_call", "providerbudget: the call role or channel is not valid")
	}
	if !ids.Valid(string(c.EnvelopeID), prefixEnvelope) || !ids.Valid(string(c.DecisionID), prefixDecision) {
		return invalid("bad_call", "providerbudget: the call names no valid envelope and egress decision")
	}
	if c.TTL < 0 || c.TTL > MaxReservationTTL {
		return invalid("bad_call", "providerbudget: the reservation timeout must be between 0 and one hour")
	}
	if c.EstimateFloor < 0 || int64(c.EstimateFloor) > MaxAmount {
		return invalid("bad_call", "providerbudget: the estimate floor is outside 0..2^53-1")
	}
	return nil
}

func unitCount(u Units, unit providers.MeterUnit) int64 {
	for _, a := range unitList(u) {
		if a.unit == unit {
			return a.n
		}
	}
	return 0
}

// expireOpen tidies the application's reservations whose timeout passed: their holds go and they are
// marked expired. Holds are ignored once expired whether or not this has run.
func expireOpen(ctx context.Context, q store.Querier, app string, now time.Time) (int64, error) {
	res, err := q.ExecContext(ctx,
		`UPDATE providerbudget_reservations SET state = 'expired', ended_at = expires_at
		  WHERE application_id = ? AND state = 'reserved' AND expires_at <= ?`, app, now.UnixMilli())
	if err != nil {
		return 0, wrapStore(err, "providerbudget: expiring reservations failed")
	}
	n, _ := res.RowsAffected()
	if _, err := q.ExecContext(ctx,
		`DELETE FROM providerbudget_holds WHERE application_id = ? AND expires_at <= ?`, app, now.UnixMilli()); err != nil {
		return 0, wrapStore(err, "providerbudget: releasing expired holds failed")
	}
	return n, nil
}

// Sweep marks the application's timed-out reservations expired and drops their holds, and returns how
// many it expired. It changes no figure: an expired hold is already ignored. Run it from a
// maintenance job (class 6) or not at all; Reserve does the same lazily.
func (s *Service) Sweep(ctx context.Context, scope contracts.Scope) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	var n int64
	err := s.db.Tx(ctx, func(tx *store.Tx) (err error) {
		n, err = expireOpen(ctx, tx, scope.ApplicationID, s.clk.Now())
		return err
	})
	return int(n), err
}

// Reserve admits or refuses one external call. In a single write transaction it resolves every budget
// that applies to the caller's scope, the provider and the channel, estimates the call as an upper
// bound, and checks for each budget the rate and concurrency limits, the daily and monthly cost limit
// and the unit quotas, counting what is already spent and what open reservations hold. When every
// budget admits the call it records the reservation and its holds; when any refuses, nothing is taken
// and the error is typed:
//
//	resource_exhausted  reason budget_exhausted, quota_exhausted, concurrency_limit or no_budget
//	rate_limited        reason rate_limited, with retry_after_ms
//
// The error carries the budget-state object and a local-fallback hint, and never names another
// provider. The caller must settle or release the reservation; if it never does, the hold returns
// when its timeout passes.
func (s *Service) Reserve(ctx context.Context, scope contracts.Scope, call Call) (Reservation, error) {
	if err := scope.Validate(); err != nil {
		return Reservation{}, err
	}
	if err := validateCall(call); err != nil {
		return Reservation{}, err
	}
	if call.Units.Requests == 0 {
		call.Units.Requests = 1
	}
	est, err := Estimate(call.Prices, call.Units)
	if err != nil {
		return Reservation{}, err
	}
	micro := max(int64(est), int64(call.EstimateFloor))
	ttl := call.TTL
	if ttl == 0 {
		ttl = s.ttl
	}
	callDoc, err := json.Marshal(call)
	if err != nil {
		return Reservation{}, contracts.Wrap(contracts.ErrInternal, err, "providerbudget: a call cannot be encoded")
	}
	now := s.clk.Now()
	ws := windowsAt(now)
	expires := now.Add(ttl)
	rsvID := providers.ReservationID(s.ids.New(prefixReservation))

	var res Reservation
	err = s.db.Tx(ctx, func(tx *store.Tx) error {
		res = Reservation{}
		if _, err := expireOpen(ctx, tx, scope.ApplicationID, now); err != nil {
			return err
		}
		rows, err := applicable(ctx, tx, scope, call.Provider.ProviderID, call.Channel)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return &refusal{reason: ReasonNoBudget}
		}
		type loaded struct {
			row budgetRow
			sn  snapshot
		}
		var all []loaded
		for _, r := range rows {
			if r.b.Currency != call.Prices.Currency {
				return invalid("currency_mismatch", "providerbudget: the price table currency differs from a budget currency").
					WithDetails("budget_id", string(r.b.BudgetID))
			}
			sn, err := loadSnapshot(ctx, tx, scope.ApplicationID, r, ws, now)
			if err != nil {
				return err
			}
			all = append(all, loaded{r, sn})
		}
		for _, l := range all {
			if err := s.admit(ctx, tx, scope, l.row, l.sn, call.Units, micro, ws, now); err != nil {
				return err
			}
		}
		budgetIDs := make([]providers.BudgetID, len(all))
		idStrings := make([]string, len(all))
		for i, l := range all {
			budgetIDs[i] = l.row.b.BudgetID
			idStrings[i] = string(l.row.b.BudgetID)
		}
		idsDoc, _ := json.Marshal(idStrings)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO providerbudget_reservations
			   (application_id, tenant_id, workspace_id, principal_id, reservation_id, provider_id, channel, state,
			    budget_ids, call, reserved_micro, daily_start, monthly_start, created_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, 'reserved', ?, ?, ?, ?, ?, ?, ?)`,
			scope.ApplicationID, scope.TenantID, scope.WorkspaceID, scope.PrincipalID, string(rsvID),
			string(call.Provider.ProviderID), string(call.Channel), string(idsDoc), string(callDoc), micro,
			ws[0].start.UnixMilli(), ws[1].start.UnixMilli(), now.UnixMilli(), expires.UnixMilli()); err != nil {
			return wrapStore(err, "providerbudget: recording a reservation failed")
		}
		holds := []unitAmount{{unit: dimCost, n: micro}}
		for _, a := range unitList(call.Units) {
			if a.n > 0 {
				holds = append(holds, a)
			}
		}
		for _, l := range all {
			for _, h := range holds {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO providerbudget_holds
					   (application_id, tenant_id, workspace_id, budget_id, reservation_id, dimension, amount,
					    daily_start, monthly_start, expires_at)
					 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					scope.ApplicationID, l.row.tenant, l.row.workspace, string(l.row.b.BudgetID), string(rsvID),
					string(h.unit), h.n, ws[0].start.UnixMilli(), ws[1].start.UnixMilli(), expires.UnixMilli()); err != nil {
					return wrapStore(err, "providerbudget: recording a hold failed")
				}
			}
			if l.row.b.RateLimits != nil && l.row.b.RateLimits.RequestsPerMin != nil {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO providerbudget_rate_events (application_id, tenant_id, workspace_id, budget_id, at)
					 VALUES (?, ?, ?, ?, ?)`,
					scope.ApplicationID, l.row.tenant, l.row.workspace, string(l.row.b.BudgetID), now.UnixMilli()); err != nil {
					return wrapStore(err, "providerbudget: recording a request failed")
				}
			}
			for _, w := range ws {
				limit := limitOf(l.row.b, w.kind)
				used := l.sn.get(l.sn.spent, w.kind, dimCost) + l.sn.get(l.sn.reserved, w.kind, dimCost) + micro
				if _, st := windowStatus(limit, used, 0, l.row.b.WarnAtPercent); st != providers.BudgetStatusOk {
					name := providers.BudgetStateWindowWindowDaily
					if w.kind == "monthly" {
						name = providers.BudgetStateWindowWindowMonthly
					}
					res.Warnings = append(res.Warnings, Warning{
						BudgetID: l.row.b.BudgetID, Window: name,
						UsedMicroUnits: providers.MicroUnits(min(used, MaxAmount)), LimitMicroUnits: providers.MicroUnits(limit),
					})
				}
			}
		}
		res.ID, res.Provider, res.Channel, res.Currency = rsvID, call.Provider.ProviderID, call.Channel, call.Prices.Currency
		res.Estimated, res.Units, res.BudgetIDs = providers.MicroUnits(micro), call.Units, budgetIDs
		res.CreatedAt, res.ExpiresAt = now, expires
		return nil
	})
	var ref *refusal
	if errors.As(err, &ref) {
		return Reservation{}, s.refusalError(ctx, scope, call.Provider.ProviderID, ref)
	}
	if err != nil {
		return Reservation{}, err
	}
	return res, nil
}

// admit checks one budget: concurrency, rate, cost windows and quotas. It returns a *refusal, a typed
// error, or nil.
func (s *Service) admit(ctx context.Context, tx *store.Tx, scope contracts.Scope, r budgetRow, sn snapshot,
	units Units, micro int64, ws [2]window, now time.Time) error {
	b := r.b
	id := string(b.BudgetID)
	refuse := func(reason, win, dim string, limit, used, reserved, requested int64) *refusal {
		return &refusal{
			reason: reason, window: win, dimension: dim, limit: limit, used: used, reserved: reserved, requested: requested,
			state: stateOf(r, sn, ws, now, false),
		}
	}
	if rl := b.RateLimits; rl != nil {
		if rl.ConcurrentRequests != nil {
			var live int64
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM providerbudget_holds
				  WHERE application_id = ? AND tenant_id = ? AND workspace_id = ? AND budget_id = ?
				    AND dimension = 'cost' AND expires_at > ?`,
				scope.ApplicationID, r.tenant, r.workspace, id, now.UnixMilli()).Scan(&live); err != nil {
				return wrapStore(err, "providerbudget: counting concurrent requests failed")
			}
			if live >= *rl.ConcurrentRequests {
				return refuse(ReasonConcurrencyLimit, "", "concurrent_requests", *rl.ConcurrentRequests, live, 0, 1)
			}
		}
		if rl.RequestsPerMin != nil {
			cutoff := now.Add(-rateWindow).UnixMilli()
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM providerbudget_rate_events
				  WHERE application_id = ? AND tenant_id = ? AND workspace_id = ? AND budget_id = ? AND at <= ?`,
				scope.ApplicationID, r.tenant, r.workspace, id, cutoff); err != nil {
				return wrapStore(err, "providerbudget: pruning request events failed")
			}
			var n, oldest int64
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*), COALESCE(MIN(at), 0) FROM providerbudget_rate_events
				  WHERE application_id = ? AND tenant_id = ? AND workspace_id = ? AND budget_id = ?`,
				scope.ApplicationID, r.tenant, r.workspace, id).Scan(&n, &oldest); err != nil {
				return wrapStore(err, "providerbudget: counting requests failed")
			}
			if n >= *rl.RequestsPerMin {
				ref := refuse(ReasonRateLimited, "", "requests_per_min", *rl.RequestsPerMin, n, 0, 1)
				ref.retryAfter = max(fromMillis(oldest).Add(rateWindow).Sub(now), time.Millisecond)
				return ref
			}
		}
	}
	for _, w := range ws {
		limit := limitOf(b, w.kind)
		spent, reserved := sn.get(sn.spent, w.kind, dimCost), sn.get(sn.reserved, w.kind, dimCost)
		if remaining, _ := windowStatus(limit, spent, reserved, b.WarnAtPercent); remaining == 0 || micro > remaining {
			return refuse(ReasonBudgetExhausted, w.kind, dimCost, limit, spent, reserved, micro)
		}
	}
	for _, q := range quotaList(b) {
		n := unitCount(units, q.unit)
		if n == 0 {
			continue
		}
		for _, f := range []struct {
			kind string
			c    *providers.Counter
		}{{"daily", q.q.Daily}, {"monthly", q.q.Monthly}} {
			if f.c == nil {
				continue
			}
			spent, reserved := sn.get(sn.spent, f.kind, string(q.unit)), sn.get(sn.reserved, f.kind, string(q.unit))
			if spent+reserved+n > int64(*f.c) {
				return refuse(ReasonQuotaExhausted, f.kind, string(q.unit), int64(*f.c), spent, reserved, n)
			}
		}
	}
	return nil
}
