package providerbudget

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Actual is what happened to a reserved call.
type Actual struct {
	// Outcome is succeeded, failed, cancelled or uncertain. An uncertain call (connection lost,
	// timeout) may have been billed: it settles at the reserved upper bound with cost basis none.
	Outcome providers.UsageRecordOutcome
	// ErrorKind is required for failed and uncertain and absent for succeeded and cancelled.
	ErrorKind *providers.ProviderErrorKind
	// Metering is the usage as the provider reported it, or as AlixAI estimated it. A zero Requests
	// counts the reserved request.
	Metering Units
	// MeteringBasis is provider_reported (the default) or estimated.
	MeteringBasis providers.UsageRecordMeteringBasis
	// ReportedMicroUnits is the cost the provider reported, when it reports one; it beats the price
	// table.
	ReportedMicroUnits *providers.MicroUnits
	Latency            providers.UsageRecordLatency
}

// Settlement is the result of Settle.
type Settlement struct {
	// Record is the usage record written for the call.
	Record providers.UsageRecord
	// ChargedMicroUnits is what the budgets were charged.
	ChargedMicroUnits providers.MicroUnits
}

// rsvRow is a stored reservation.
type rsvRow struct {
	principal, provider, channel, state string
	budgetIDs                           []string
	call                                Call
	reservedMicro                       int64
	dailyStart, monthlyStart            int64
}

func errUnknownReservation() error {
	return invalid("unknown_reservation", "providerbudget: no such reservation for this caller")
}

// loadReservation reads a reservation made by this exact scope and principal. Another tenant,
// workspace or principal gets the same answer as for an ID that never existed.
func loadReservation(ctx context.Context, q store.Reader, scope contracts.Scope, id providers.ReservationID) (rsvRow, error) {
	pred, args, err := store.ScopedWhere(scope)
	if err != nil {
		return rsvRow{}, err
	}
	var r rsvRow
	var idsDoc, callDoc string
	err = q.QueryRowContext(ctx,
		`SELECT principal_id, provider_id, channel, state, budget_ids, call, reserved_micro, daily_start, monthly_start
		   FROM providerbudget_reservations WHERE `+pred+` AND reservation_id = ?`,
		append(args, string(id))...).
		Scan(&r.principal, &r.provider, &r.channel, &r.state, &idsDoc, &callDoc, &r.reservedMicro, &r.dailyStart, &r.monthlyStart)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return rsvRow{}, errUnknownReservation()
	case err != nil:
		return rsvRow{}, wrapStore(err, "providerbudget: reading a reservation failed")
	}
	if r.principal != scope.PrincipalID {
		return rsvRow{}, errUnknownReservation()
	}
	if err := json.Unmarshal([]byte(idsDoc), &r.budgetIDs); err != nil {
		return rsvRow{}, contracts.Wrap(contracts.ErrInternal, err, "providerbudget: a stored reservation cannot be decoded")
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(callDoc)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r.call); err != nil {
		return rsvRow{}, contracts.Wrap(contracts.ErrInternal, err, "providerbudget: a stored reservation cannot be decoded")
	}
	return r, nil
}

// reservationBudgets loads the budgets a reservation was charged against.
func reservationBudgets(ctx context.Context, q store.Reader, app string, r rsvRow) ([]budgetRow, error) {
	var out []budgetRow
	for _, id := range r.budgetIDs {
		b, found, err := loadBudget(ctx, q, app, id)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, contracts.New(contracts.ErrInternal, "providerbudget: a reservation names a budget that does not exist")
		}
		out = append(out, b)
	}
	return out, nil
}

func dropHolds(ctx context.Context, q store.Querier, app string, budgets []budgetRow, id providers.ReservationID) error {
	for _, b := range budgets {
		if _, err := q.ExecContext(ctx,
			`DELETE FROM providerbudget_holds
			  WHERE application_id = ? AND tenant_id = ? AND workspace_id = ? AND budget_id = ? AND reservation_id = ?`,
			app, b.tenant, b.workspace, string(b.b.BudgetID), string(id)); err != nil {
			return wrapStore(err, "providerbudget: releasing holds failed")
		}
	}
	return nil
}

func (a Actual) validate() error {
	if !a.Outcome.Valid() {
		return invalid("bad_actual", "providerbudget: the outcome is not valid")
	}
	switch a.Outcome {
	case providers.UsageRecordOutcomeSucceeded, providers.UsageRecordOutcomeCancelled:
		if a.ErrorKind != nil {
			return invalid("bad_actual", "providerbudget: a succeeded or cancelled call has no error kind")
		}
	default:
		if a.ErrorKind == nil || !a.ErrorKind.Valid() {
			return invalid("bad_actual", "providerbudget: a failed or uncertain call names its error kind")
		}
	}
	if a.MeteringBasis != "" && !a.MeteringBasis.Valid() {
		return invalid("bad_actual", "providerbudget: the metering basis is not valid")
	}
	if err := validateUnits(a.Metering); err != nil {
		return err
	}
	if a.ReportedMicroUnits != nil && (*a.ReportedMicroUnits < 0 || int64(*a.ReportedMicroUnits) > MaxAmount) {
		return invalid("bad_actual", "providerbudget: the reported cost is outside 0..2^53-1")
	}
	if a.Latency.TotalMS < 0 || int64(a.Latency.TotalMS) > MaxAmount || (a.Latency.FirstDeltaMS != nil && *a.Latency.FirstDeltaMS < 0) {
		return invalid("bad_actual", "providerbudget: a latency is out of range")
	}
	return nil
}

// charge adds amount to the counter of one budget dimension and window.
func charge(ctx context.Context, q store.Querier, app string, b budgetRow, dim, kind string, start int64, amount int64, now time.Time) error {
	if amount <= 0 {
		return nil
	}
	_, err := q.ExecContext(ctx,
		`INSERT INTO providerbudget_counters
		   (application_id, tenant_id, workspace_id, budget_id, dimension, window_kind, window_start, used, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (application_id, tenant_id, workspace_id, budget_id, dimension, window_kind, window_start)
		 DO UPDATE SET used = MIN(used + excluded.used, 9007199254740991), updated_at = excluded.updated_at`,
		app, b.tenant, b.workspace, string(b.b.BudgetID), dim, kind, start, min(amount, MaxAmount), now.UnixMilli())
	return wrapStore(err, "providerbudget: charging a counter failed")
}

// Settle records what a reserved call really consumed. It charges the actual cost and units to the
// counters of every budget the reservation was charged against, in the windows the call was RESERVED
// in (a call reserved at 23:59 and settled at 00:01 counts in the first day), drops the holds and
// writes the usage record, all in one transaction. The actual is the provider-reported cost when
// given, else the metered units priced from the reservation's price table, else (an unpriced unit)
// the reserved upper bound with cost basis none; an uncertain outcome settles at the reserved upper
// bound with cost basis none. A reservation whose timeout already passed still settles: spend that
// happened is recorded, even when it takes a window past its limit (the next Reserve is then
// refused). A reservation settles once; only the principal that made it may settle it.
func (s *Service) Settle(ctx context.Context, scope contracts.Scope, id providers.ReservationID, a Actual) (Settlement, error) {
	if err := scope.Validate(); err != nil {
		return Settlement{}, err
	}
	if err := a.validate(); err != nil {
		return Settlement{}, err
	}
	now := s.clk.Now()
	var out Settlement
	err := s.db.Tx(ctx, func(tx *store.Tx) error {
		r, err := loadReservation(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		switch r.state {
		case "settled":
			return conflict("already_settled", "providerbudget: the reservation is already settled")
		case "released":
			return conflict("released", "providerbudget: the reservation was released")
		}
		budgets, err := reservationBudgets(ctx, tx, scope.ApplicationID, r)
		if err != nil {
			return err
		}

		units, charged := a.Metering, r.reservedMicro
		var actual *providers.MicroUnits
		basis, mbasis := providers.UsageRecordCostBasisNone, a.MeteringBasis
		if mbasis == "" {
			mbasis = providers.UsageRecordMeteringBasisProviderReported
		}
		switch {
		case a.Outcome == providers.UsageRecordOutcomeUncertain:
			units, mbasis = r.call.Units, providers.UsageRecordMeteringBasisEstimated
		case a.ReportedMicroUnits != nil:
			actual, basis = a.ReportedMicroUnits, providers.UsageRecordCostBasisProviderReported
			charged = int64(*a.ReportedMicroUnits)
		default:
			if c, err := Cost(r.call.Prices, withRequests(units, r.call.Units)); err == nil {
				actual, basis, charged = &c, providers.UsageRecordCostBasisPriceTable, int64(c)
			}
		}
		units = withRequests(units, r.call.Units)

		for _, b := range budgets {
			for _, w := range []struct {
				kind  string
				start int64
			}{{"daily", r.dailyStart}, {"monthly", r.monthlyStart}} {
				if err := charge(ctx, tx, scope.ApplicationID, b, dimCost, w.kind, w.start, charged, now); err != nil {
					return err
				}
				for _, u := range unitList(units) {
					if err := charge(ctx, tx, scope.ApplicationID, b, string(u.unit), w.kind, w.start, u.n, now); err != nil {
						return err
					}
				}
			}
		}
		if err := dropHolds(ctx, tx, scope.ApplicationID, budgets, id); err != nil {
			return err
		}

		rec := providers.UsageRecord{
			UsageID:    providers.UsageID(s.ids.New(prefixUsage)),
			RecordedAt: timestamp(now),
			Subject: providers.Subject{
				ApplicationID: common.Identifier(scope.ApplicationID), TenantID: common.Identifier(scope.TenantID),
				WorkspaceID: common.Identifier(scope.WorkspaceID), PrincipalID: common.Identifier(scope.PrincipalID),
				SessionID: common.Identifier(scope.SessionID),
			},
			Provider: r.call.Provider, Role: r.call.Role, Channel: r.call.Channel,
			EnvelopeID: r.call.EnvelopeID, DecisionID: r.call.DecisionID, ReservationID: &id,
			Outcome: a.Outcome, ErrorKind: a.ErrorKind, Metering: units, MeteringBasis: mbasis,
			Cost: providers.UsageRecordCost{
				Currency: r.call.Prices.Currency, ReservedMicroUnits: providers.MicroUnits(r.reservedMicro),
				ActualMicroUnits: actual, Basis: basis,
			},
			Latency: a.Latency, FallbackFrom: r.call.FallbackFrom,
		}
		doc, err := json.Marshal(rec)
		if err != nil {
			return contracts.Wrap(contracts.ErrInternal, err, "providerbudget: a usage record cannot be encoded")
		}
		var actualCol any
		if actual != nil {
			actualCol = int64(*actual)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO providerbudget_usage
			   (application_id, tenant_id, workspace_id, principal_id, usage_id, reservation_id, provider_id, channel,
			    currency, outcome, reserved_micro, actual_micro, charged_micro, charged_day, recorded_at, document)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			scope.ApplicationID, scope.TenantID, scope.WorkspaceID, scope.PrincipalID, string(rec.UsageID), string(id),
			r.provider, r.channel, string(rec.Cost.Currency), string(a.Outcome), r.reservedMicro, actualCol, charged,
			r.dailyStart, now.UnixMilli(), string(doc)); err != nil {
			return wrapStore(err, "providerbudget: recording usage failed")
		}
		pred, args, err := store.ScopedWhere(scope)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE providerbudget_reservations SET state = 'settled', ended_at = ? WHERE `+pred+` AND reservation_id = ?`,
			append([]any{now.UnixMilli()}, append(args, string(id))...)...); err != nil {
			return wrapStore(err, "providerbudget: settling a reservation failed")
		}
		out = Settlement{Record: rec, ChargedMicroUnits: providers.MicroUnits(charged)}
		return nil
	})
	if err != nil {
		return Settlement{}, err
	}
	return out, nil
}

// withRequests counts the reserved request when the metering names none.
func withRequests(u, reserved Units) Units {
	if u.Requests == 0 {
		u.Requests = reserved.Requests
	}
	return u
}

// Release gives a reservation back without charging anything: the call was never made. It is
// idempotent for a reservation already released; a settled one is refused with ErrConflict. Only the
// principal that made the reservation may release it.
func (s *Service) Release(ctx context.Context, scope contracts.Scope, id providers.ReservationID) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	now := s.clk.Now()
	return s.db.Tx(ctx, func(tx *store.Tx) error {
		r, err := loadReservation(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		switch r.state {
		case "released":
			return nil
		case "settled":
			return conflict("already_settled", "providerbudget: the reservation is already settled")
		}
		budgets, err := reservationBudgets(ctx, tx, scope.ApplicationID, r)
		if err != nil {
			return err
		}
		if err := dropHolds(ctx, tx, scope.ApplicationID, budgets, id); err != nil {
			return err
		}
		pred, args, err := store.ScopedWhere(scope)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE providerbudget_reservations SET state = 'released', ended_at = COALESCE(ended_at, ?) WHERE `+pred+` AND reservation_id = ?`,
			append([]any{now.UnixMilli()}, append(args, string(id))...)...)
		return wrapStore(err, "providerbudget: releasing a reservation failed")
	})
}
