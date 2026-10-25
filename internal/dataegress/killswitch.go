package dataegress

import (
	"context"
	"errors"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Fixed reasons recorded when the switch is released or engaged without a more
// specific code.
const (
	ReasonOperatorEmergency = "operator_emergency"
	ReasonOperatorReleased  = "operator_released"
)

// EngageKillSwitch engages the persistent global external-disable switch of the
// operator's deployment: from this call on every Evaluate of that deployment is
// denied with reason kill_switch, whatever any policy says, and the switch survives
// a restart (it is a row in the store). reason is a fixed code (lower snake_case),
// never free text; an empty reason is ReasonOperatorEmergency.
//
// It fails safe. The process stops sending first (an in-memory latch), then the
// switch is persisted and audited in one transaction; if either fails the error is
// returned and this process stays stopped, so a failing store or audit log can never
// leave the switch half way open. Engaging an engaged switch is not an error.
func (s *Service) EngageKillSwitch(ctx context.Context, op Operator, reason string) error {
	if err := op.validate(); err != nil {
		return err
	}
	if reason == "" {
		reason = ReasonOperatorEmergency
	}
	if !reasonCodeRE.MatchString(reason) {
		return contracts.New(contracts.ErrValidationFailed, "dataegress: the kill switch reason must be a fixed code (lower snake_case)")
	}
	s.latched.Store(op.DeploymentID, struct{}{})
	return s.writeKillSwitch(ctx, op, true, reason)
}

// ReleaseKillSwitch releases the switch of the operator's deployment. External mode
// is still off unless the policy chain enables it: releasing the switch enables
// nothing by itself. The process latch is cleared only after the release is persisted
// and audited.
func (s *Service) ReleaseKillSwitch(ctx context.Context, op Operator) error {
	if err := op.validate(); err != nil {
		return err
	}
	if err := s.writeKillSwitch(ctx, op, false, ReasonOperatorReleased); err != nil {
		return err
	}
	s.latched.Delete(op.DeploymentID)
	return nil
}

func (s *Service) writeKillSwitch(ctx context.Context, op Operator, engaged bool, reason string) error {
	on := int64(0)
	if engaged {
		on = 1
	}
	err := s.db.Tx(ctx, func(tx *store.Tx) error {
		now := s.clk.Now().UTC().UnixMilli()
		res, err := tx.ExecContext(ctx,
			"UPDATE dataegress_kill_switch SET engaged = ?, reason = ?, changed_by = ?, changed_at = ? WHERE deployment_id = ? AND application_id = '' AND tenant_id = '' AND workspace_id = ''",
			on, reason, op.PrincipalID, now, op.DeploymentID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO dataegress_kill_switch (application_id, tenant_id, workspace_id, deployment_id, engaged, reason, changed_by, changed_at) VALUES ('', '', '', ?, ?, ?, ?, ?)",
				op.DeploymentID, on, reason, op.PrincipalID, now); err != nil {
				return err
			}
		}
		if _, err := EventKillSwitch.AppendTx(ctx, s.audit, tx, audit.Platform(),
			KillSwitchChanged{DeploymentID: op.DeploymentID, Engaged: engaged, Reason: reason, ChangedBy: op.PrincipalID}); err != nil {
			return contracts.Wrap(contracts.ErrUnavailable, err, "the kill switch change could not be recorded")
		}
		return nil
	})
	return wrapStore(err, "dataegress: the kill switch could not be written")
}

// KillSwitch returns the persisted state of a deployment's switch.
func (s *Service) KillSwitch(ctx context.Context, deploymentID string) (KillSwitchState, error) {
	if deploymentID == "" {
		return KillSwitchState{}, contracts.New(contracts.ErrScopeMissing, "dataegress: deployment id is missing")
	}
	st, err := readKillSwitch(ctx, s.db, deploymentID)
	if err != nil {
		return KillSwitchState{}, wrapStore(err, "dataegress: the kill switch could not be read")
	}
	if _, ok := s.latched.Load(deploymentID); ok {
		st.Engaged = true
	}
	return st, nil
}

// killSwitchOn reports whether external requests of the deployment are disabled.
// It reads the store on every call (no cache, so another process's change takes
// effect at once) and fails closed: a store that cannot be read is an engaged switch.
func (s *Service) killSwitchOn(ctx context.Context, deploymentID string) (bool, error) {
	if _, ok := s.latched.Load(deploymentID); ok {
		return true, nil
	}
	st, err := readKillSwitch(ctx, s.db, deploymentID)
	if err != nil {
		return true, err
	}
	return st.Engaged, nil
}

// wrapStore types a failure that is not already typed.
func wrapStore(err error, msg string) error {
	if err == nil {
		return nil
	}
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return err
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, msg)
}
