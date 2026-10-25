package history

import (
	"context"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The sentinel scope values of a retention policy row (migration 0001). A real
// scope value can never be a wildcard, so these cannot collide with a tenant or
// a workspace.
const anyScope = "*"

// Retention is one retention policy: how long transcript turns are kept.
type Retention struct {
	// Days is how many days a transcript turn is kept, 1 to MaxRetainDays.
	Days      int
	UpdatedBy string
	UpdatedAt time.Time
}

// RetentionPolicy is the retention in force for a tenant (master plan 19).
type RetentionPolicy struct {
	// Application is the application-wide policy and Tenant the tenant's own; nil
	// when not set.
	Application *Retention
	Tenant      *Retention
	// EffectiveDays is the shorter of the two that are set, or 0 when neither is:
	// with no policy nothing is deleted.
	EffectiveDays int
	// Source is "tenant" or "application" (the one that sets EffectiveDays), or
	// "none".
	Source string
}

func effectivePolicy(app, tenant *Retention) RetentionPolicy {
	p := RetentionPolicy{Application: app, Tenant: tenant, Source: "none"}
	if app != nil {
		p.EffectiveDays, p.Source = app.Days, "application"
	}
	if tenant != nil && (app == nil || tenant.Days < app.Days) {
		p.EffectiveDays, p.Source = tenant.Days, "tenant"
	}
	return p
}

// loadRetention reads the application and tenant policies of an application and
// tenant.
func (s *Service) loadRetention(ctx context.Context, q store.Reader, application, tenant string) (RetentionPolicy, error) {
	read := func(t string) (*Retention, error) {
		var days, at int64
		var by string
		err := q.QueryRowContext(ctx,
			"SELECT retain_days, updated_by, updated_at FROM history_retention WHERE application_id = ? AND tenant_id = ? AND workspace_id = ?",
			application, t, anyScope).Scan(&days, &by, &at)
		switch {
		case err == nil:
			return &Retention{Days: int(days), UpdatedBy: by, UpdatedAt: fromMS(at)}, nil
		case isNoRows(err):
			return nil, nil
		}
		return nil, wrapDB(err)
	}
	app, err := read(anyScope)
	if err != nil {
		return RetentionPolicy{}, err
	}
	var ten *Retention
	if tenant != "" {
		if ten, err = read(tenant); err != nil {
			return RetentionPolicy{}, err
		}
	}
	return effectivePolicy(app, ten), nil
}

// RetentionPolicy returns the retention in force for the caller's tenant. The
// caller must hold CapRetention.
func (s *Service) RetentionPolicy(ctx context.Context, scope contracts.Scope) (RetentionPolicy, error) {
	if err := s.authorize(ctx, scope, ActionRetention, CapRetention); err != nil {
		return RetentionPolicy{}, err
	}
	return s.loadRetention(ctx, s.db, scope.ApplicationID, scope.TenantID)
}

// SetRetention sets the retention of the caller's tenant to days (1 to
// MaxRetainDays). The caller must hold CapRetention. A tenant cannot keep history
// longer than its application's policy allows: days over it is validation_failed.
// The change and its settings.changed audit event are one transaction.
func (s *Service) SetRetention(ctx context.Context, scope contracts.Scope, days int) (RetentionPolicy, error) {
	if err := s.authorize(ctx, scope, ActionRetention, CapRetention); err != nil {
		return RetentionPolicy{}, err
	}
	if days < 1 {
		return RetentionPolicy{}, contracts.New(contracts.ErrValidationFailed, "history: a retention policy keeps history for at least one day")
	}
	return s.setPolicy(ctx, scope, scope.TenantID, "tenant", days)
}

// ClearRetention removes the tenant's own policy, so the application's (if any)
// applies. The caller must hold CapRetention.
func (s *Service) ClearRetention(ctx context.Context, scope contracts.Scope) (RetentionPolicy, error) {
	if err := s.authorize(ctx, scope, ActionRetention, CapRetention); err != nil {
		return RetentionPolicy{}, err
	}
	return s.setPolicy(ctx, scope, scope.TenantID, "tenant", 0)
}

// SetApplicationRetention sets the retention policy of the whole application
// that scope belongs to. No tenant can change it: it is an operator action, and
// the Operator hook of Deps must accept it (with none it is denied). Tenants
// whose own policy is longer are held to the shorter one: the effective retention
// is always the minimum.
func (s *Service) SetApplicationRetention(ctx context.Context, scope contracts.Scope, days int) (RetentionPolicy, error) {
	if err := s.authorizeOperator(ctx, scope); err != nil {
		return RetentionPolicy{}, err
	}
	if days < 1 {
		return RetentionPolicy{}, contracts.New(contracts.ErrValidationFailed, "history: a retention policy keeps history for at least one day")
	}
	return s.setPolicy(ctx, scope, anyScope, "application", days)
}

// ClearApplicationRetention removes the application-wide policy. It is an
// operator action like SetApplicationRetention.
func (s *Service) ClearApplicationRetention(ctx context.Context, scope contracts.Scope) (RetentionPolicy, error) {
	if err := s.authorizeOperator(ctx, scope); err != nil {
		return RetentionPolicy{}, err
	}
	return s.setPolicy(ctx, scope, anyScope, "application", 0)
}

// SetRetentionTx is SetRetention, or ClearRetention when days is 0, inside the
// caller's transaction tx, which must be on the service's store: the policy, its
// settings.changed event and whatever else the caller writes in tx commit together
// or not at all. It writes nothing when the tenant's policy already is days. Its
// checks and refusals are SetRetention's; on a refusal the caller rolls tx back.
// The Authorizer runs inside tx, so it must not write to the store.
func (s *Service) SetRetentionTx(ctx context.Context, tx *store.Tx, scope contracts.Scope, days int) (RetentionPolicy, error) {
	if err := s.authorize(ctx, scope, ActionRetention, CapRetention); err != nil {
		return RetentionPolicy{}, err
	}
	out, err := s.setPolicyIn(ctx, tx, scope, scope.TenantID, "tenant", days, true)
	if err != nil {
		return RetentionPolicy{}, wrapDB(err)
	}
	return out, nil
}

// SetApplicationRetentionTx is SetApplicationRetention, or ClearApplicationRetention
// when days is 0, inside the caller's transaction tx, as SetRetentionTx. The
// Operator hook runs inside tx, so it must not write to the store.
func (s *Service) SetApplicationRetentionTx(ctx context.Context, tx *store.Tx, scope contracts.Scope, days int) (RetentionPolicy, error) {
	if err := s.authorizeOperator(ctx, scope); err != nil {
		return RetentionPolicy{}, err
	}
	out, err := s.setPolicyIn(ctx, tx, scope, anyScope, "application", days, true)
	if err != nil {
		return RetentionPolicy{}, wrapDB(err)
	}
	return out, nil
}

func (s *Service) authorizeOperator(ctx context.Context, scope contracts.Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if s.operator == nil {
		return contracts.New(contracts.ErrPermissionDenied, "history: no operator authorisation is configured for the application retention policy")
	}
	return s.extra(ctx, s.operator, scope, ActionApplicationRetention)
}

// setPolicy writes (days > 0) or removes (days == 0) the policy row of level
// tenantKey, and the audit event, in one transaction.
func (s *Service) setPolicy(ctx context.Context, scope contracts.Scope, tenantKey, level string, days int) (RetentionPolicy, error) {
	var out RetentionPolicy
	err := s.db.Tx(ctx, func(tx *store.Tx) error {
		var err error
		out, err = s.setPolicyIn(ctx, tx, scope, tenantKey, level, days, false)
		return err
	})
	if err != nil {
		return RetentionPolicy{}, wrapDB(err)
	}
	return out, nil
}

// setPolicyIn is setPolicy inside tx. With sameIsNoop, setting the days the level
// already has writes nothing.
func (s *Service) setPolicyIn(ctx context.Context, tx *store.Tx, scope contracts.Scope, tenantKey, level string, days int, sameIsNoop bool) (RetentionPolicy, error) {
	if days < 0 || days > MaxRetainDays {
		return RetentionPolicy{}, contracts.Newf(contracts.ErrValidationFailed, "history: retention must be between 1 and %d days", MaxRetainDays)
	}
	var loc audit.Locator
	var err error
	if level == "application" {
		loc, err = audit.ForApplication(scope.ApplicationID, scope.PrincipalID)
	} else {
		loc, err = audit.ForScope(scope)
	}
	if err != nil {
		return RetentionPolicy{}, err
	}
	now := s.now()
	cur, err := s.loadRetention(ctx, tx, scope.ApplicationID, scope.TenantID)
	if err != nil {
		return RetentionPolicy{}, err
	}
	prev := cur.Tenant
	if level == "application" {
		prev = cur.Application
	}
	// A tenant may keep history for no longer than its application allows.
	if level == "tenant" && days > 0 && cur.Application != nil && days > cur.Application.Days {
		return RetentionPolicy{}, contracts.Newf(contracts.ErrValidationFailed, "history: the application keeps history for %d days at most", cur.Application.Days).
			WithDetails("max_days", cur.Application.Days)
	}
	if (days == 0 && prev == nil) || (sameIsNoop && prev != nil && prev.Days == days) {
		return cur, nil
	}
	change := map[string]any{"setting": "history.retention", "level": level, "days": days}
	if prev != nil {
		change["previous_days"] = prev.Days
	}
	if _, err := s.aud.AppendTx(ctx, tx, loc, audit.NewEvent{Type: audit.EventSettingsChanged, Data: change}); err != nil {
		return RetentionPolicy{}, err
	}
	if days == 0 {
		_, err = tx.ExecContext(ctx,
			"DELETE FROM history_retention WHERE application_id = ? AND tenant_id = ? AND workspace_id = ?",
			scope.ApplicationID, tenantKey, anyScope)
	} else {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO history_retention (application_id, tenant_id, workspace_id, retain_days, updated_by, updated_at)
 VALUES (?, ?, ?, ?, ?, ?)
 ON CONFLICT (application_id, tenant_id, workspace_id)
 DO UPDATE SET retain_days = excluded.retain_days, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
			scope.ApplicationID, tenantKey, anyScope, days, scope.PrincipalID, toMS(now))
	}
	if err != nil {
		return RetentionPolicy{}, err
	}
	return s.loadRetention(ctx, tx, scope.ApplicationID, scope.TenantID)
}

// retentionCutoff is the time before which a workspace's turns are due for
// deletion under days, or the zero time when no policy applies.
func (s *Service) retentionCutoff(days int) time.Time {
	if days <= 0 {
		return time.Time{}
	}
	return s.now().Add(-time.Duration(days) * 24 * time.Hour)
}

// applyRetention deletes the transcript turns of scope's workspace that are past
// the retention in force for its tenant, and reports how many rows went. It takes
// no capability: it is the work of the retention job, which runs under the
// service identity of the deployment and is reached through the job queue, never
// through a request. With no policy nothing is deleted.
func (s *Service) applyRetention(ctx context.Context, scope contracts.Scope) (ClearResult, error) {
	pol, err := s.loadRetention(ctx, s.db, scope.ApplicationID, scope.TenantID)
	if err != nil {
		return ClearResult{}, err
	}
	if pol.EffectiveDays == 0 {
		return ClearResult{}, nil
	}
	return s.prune(ctx, scope, s.retentionCutoff(pol.EffectiveDays), reasonRetention, pol.EffectiveDays)
}
