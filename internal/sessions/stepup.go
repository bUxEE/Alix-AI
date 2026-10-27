package sessions

import (
	"context"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// ApplyStepUpReceipt ends the active goal that still references preparationID
// after a host step-up outcome (ADR 0048), matching an in-turn commit:
// committed / queued / partial may record a ResultCommit, then Complete with
// outcome and ReceiptID; rejected ends with cancelled (declined) or failure.
// When the goal no longer references the preparation, state is unchanged.
func (s *State) ApplyStepUpReceipt(now time.Time, preparationID string, rc contracts.ActionReceipt, outcome Outcome, cfg Config) (bool, error) {
	if s == nil || preparationID == "" || s.Goal == nil || s.Goal.PreparedID != preparationID {
		return false, nil
	}
	if !outcome.Valid() {
		return false, contracts.Newf(contracts.ErrValidationFailed, "sessions: %q is not an outcome", string(outcome))
	}
	cfg = cfg.withDefaults()
	switch rc.Status {
	case act.ActionReceiptStatusCommitted, act.ActionReceiptStatusQueued, act.ActionReceiptStatusPartial:
		if len(rc.Records) > 0 {
			items := make([]ResultItem, 0, len(rc.Records))
			for _, ref := range rc.Records {
				items = append(items, ResultItem{EntityType: string(ref.EntityType), EntityID: string(ref.EntityID)})
			}
			s.RecordResult(ResultSummary{
				CapabilityID: string(rc.CapabilityID),
				Kind:         ResultCommit,
				ReceiptID:    string(rc.ReceiptID),
				Items:        items,
				At:           now,
			}, cfg)
		}
	}
	// Clear before Complete so Impact.Voided stays empty: the preparation is
	// already terminal from RecordStepUpOutcome.
	s.Goal.ClearPrepared()
	s.Goal.Pending = nil
	_, _, err := s.Complete(now, CompleteOptions{
		Outcome:   outcome,
		ReceiptID: string(rc.ReceiptID),
	}, cfg)
	if err != nil {
		return false, err
	}
	return true, nil
}

// ApplyStepUpReceipt opens the session, applies the step-up receipt to its goal
// when the preparation is still referenced, and saves with one retry on a
// version conflict.
func (st *Store) ApplyStepUpReceipt(ctx context.Context, scope contracts.Scope, preparationID string, rc contracts.ActionReceipt, outcome Outcome) error {
	if st == nil {
		return nil
	}
	apply := func() error {
		sess, err := st.Open(ctx, scope)
		if err != nil {
			return err
		}
		changed, err := sess.State.ApplyStepUpReceipt(st.clk.Now(), preparationID, rc, outcome, st.cfg)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}
		_, err = st.Save(ctx, sess, nil)
		return err
	}
	err := apply()
	if contracts.CodeOf(err) == contracts.ErrConflict {
		return apply()
	}
	return err
}
