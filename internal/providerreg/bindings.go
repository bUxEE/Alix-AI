package providerreg

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// PrincipalBinding is one persisted role binding for a principal (ADR 0041).
type PrincipalBinding struct {
	Role       providers.PrincipalLlmRole
	ProfileID  providers.ProfileID
	ProviderID providers.ProviderID
	ModelID    providers.ModelID
	UpdatedAt  time.Time
}

// PutPrincipalBinding stores or replaces the binding for (application, principal, role).
func (r *Registry) PutPrincipalBinding(ctx context.Context, scope contracts.Scope, b PrincipalBinding) error {
	if _, err := appOf(scope); err != nil {
		return err
	}
	if scope.PrincipalID == "" {
		return contracts.New(contracts.ErrScopeMissing, "providerreg: a principal is required")
	}
	if !b.Role.Valid() {
		return invalid("role must be conversational_controller, language_realization or stt")
	}
	now := r.clk.Now().UnixMilli()
	return wrapStore(r.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO providerreg_principal_bindings
			  (application_id, tenant_id, workspace_id, principal_id, role, profile_id, provider_id, model_id, updated_at)
			VALUES (?, '', '', ?, ?, ?, ?, ?, ?)
			ON CONFLICT (application_id, tenant_id, workspace_id, principal_id, role) DO UPDATE SET
			  profile_id = excluded.profile_id,
			  provider_id = excluded.provider_id,
			  model_id = excluded.model_id,
			  updated_at = excluded.updated_at`,
			scope.ApplicationID, scope.PrincipalID, string(b.Role), string(b.ProfileID),
			string(b.ProviderID), string(b.ModelID), now)
		return err
	}), "principal binding could not be stored")
}

// DeletePrincipalBinding clears one role binding. Missing is not an error.
func (r *Registry) DeletePrincipalBinding(ctx context.Context, scope contracts.Scope, role providers.PrincipalLlmRole) error {
	if _, err := appOf(scope); err != nil {
		return err
	}
	_, err := r.db.ExecContext(ctx, `
		DELETE FROM providerreg_principal_bindings
		 WHERE `+appWhere+` AND principal_id = ? AND role = ?`,
		scope.ApplicationID, scope.PrincipalID, string(role))
	return wrapStore(err, "principal binding could not be deleted")
}

// ListPrincipalBindings returns every binding of the caller's principal in the application.
func (r *Registry) ListPrincipalBindings(ctx context.Context, scope contracts.Scope) ([]PrincipalBinding, error) {
	if _, err := appOf(scope); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT role, profile_id, provider_id, model_id, updated_at
		  FROM providerreg_principal_bindings
		 WHERE `+appWhere+` AND principal_id = ?
		 ORDER BY role`, scope.ApplicationID, scope.PrincipalID)
	if err != nil {
		return nil, wrapStore(err, "principal bindings could not be listed")
	}
	defer rows.Close()
	var out []PrincipalBinding
	for rows.Next() {
		var role, profile, provider, model string
		var updated int64
		if err := rows.Scan(&role, &profile, &provider, &model, &updated); err != nil {
			return nil, wrapStore(err, "principal bindings could not be listed")
		}
		out = append(out, PrincipalBinding{
			Role:       providers.PrincipalLlmRole(role),
			ProfileID:  providers.ProfileID(profile),
			ProviderID: providers.ProviderID(provider),
			ModelID:    providers.ModelID(model),
			UpdatedAt:  time.UnixMilli(updated).UTC(),
		})
	}
	return out, wrapStore(rows.Err(), "principal bindings could not be listed")
}

// PrincipalBindingFor returns the binding for role, or found=false.
func (r *Registry) PrincipalBindingFor(ctx context.Context, scope contracts.Scope, role providers.PrincipalLlmRole) (PrincipalBinding, bool, error) {
	all, err := r.ListPrincipalBindings(ctx, scope)
	if err != nil {
		return PrincipalBinding{}, false, err
	}
	for _, b := range all {
		if b.Role == role {
			return b, true, nil
		}
	}
	return PrincipalBinding{}, false, nil
}

// ErasePrincipalBindings deletes every binding of principal in the application (ADR 0041 erasure).
func (r *Registry) ErasePrincipalBindings(ctx context.Context, scope contracts.Scope, principal string) (int, error) {
	if _, err := appOf(scope); err != nil {
		return 0, err
	}
	if principal == "" {
		return 0, contracts.New(contracts.ErrValidationFailed, "providerreg: a principal is required")
	}
	res, err := r.db.ExecContext(ctx, `
		DELETE FROM providerreg_principal_bindings
		 WHERE `+appWhere+` AND principal_id = ?`, scope.ApplicationID, principal)
	if err != nil {
		return 0, wrapStore(err, "principal bindings could not be erased")
	}
	n, _ := res.RowsAffected()
	if _, err := r.db.ExecContext(ctx, `
		DELETE FROM providerreg_principal_billing_acks
		 WHERE `+appWhere+` AND principal_id = ?`, scope.ApplicationID, principal); err != nil {
		return int(n), wrapStore(err, "principal billing acks could not be erased")
	}
	return int(n), nil
}

// PutPrincipalBillingAck records that the principal acknowledged the notice version for provider.
func (r *Registry) PutPrincipalBillingAck(ctx context.Context, scope contracts.Scope, provider providers.ProviderID, noticeVersion string, at time.Time) error {
	if _, err := appOf(scope); err != nil {
		return err
	}
	if noticeVersion == "" || provider == "" {
		return invalid("provider and notice version are required")
	}
	return wrapStore(r.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO providerreg_principal_billing_acks
			  (application_id, tenant_id, workspace_id, principal_id, provider_id, notice_version, acknowledged_at)
			VALUES (?, '', '', ?, ?, ?, ?)
			ON CONFLICT (application_id, tenant_id, workspace_id, principal_id, provider_id) DO UPDATE SET
			  notice_version = excluded.notice_version,
			  acknowledged_at = excluded.acknowledged_at`,
			scope.ApplicationID, scope.PrincipalID, string(provider), noticeVersion, at.UTC().UnixMilli())
		return err
	}), "principal billing acknowledgement could not be stored")
}

// PrincipalBillingAcked reports whether the principal has acknowledged noticeVersion for provider.
func (r *Registry) PrincipalBillingAcked(ctx context.Context, scope contracts.Scope, provider providers.ProviderID, noticeVersion string) (bool, time.Time, error) {
	if _, err := appOf(scope); err != nil {
		return false, time.Time{}, err
	}
	var got string
	var at int64
	err := r.db.QueryRowContext(ctx, `
		SELECT notice_version, acknowledged_at FROM providerreg_principal_billing_acks
		 WHERE `+appWhere+` AND principal_id = ? AND provider_id = ?`,
		scope.ApplicationID, scope.PrincipalID, string(provider)).Scan(&got, &at)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, time.Time{}, nil
	case err != nil:
		return false, time.Time{}, wrapStore(err, "principal billing acknowledgement could not be read")
	}
	return got == noticeVersion, time.UnixMilli(at).UTC(), nil
}

// tsCommon formats t as the schema timestamp.
func tsCommon(t time.Time) common.Timestamp {
	return common.Timestamp(t.UTC().Format("2006-01-02T15:04:05.000Z"))
}
