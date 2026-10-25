package credentials

import (
	"context"
	"slices"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Rotation replaces the value of a reference. The reference ID stays, so nothing that holds it
// changes, and the next Resolve returns the new value: no host application restarts.
type Rotation struct {
	// Key is the new key or access value. Required.
	Key Secret
	// Refresh is a new refresh value; empty keeps the one held (a provider need not issue a new one).
	Refresh Secret
	// ExpiresAt is the new expiry; nil keeps the old one.
	ExpiresAt *time.Time
	// Scopes replaces the permitted scopes (a re-authorization with changed consent); nil keeps them. Only a
	// rotation that names them makes a flagged OAuth reference active again: a refresh keeps the verdict.
	Scopes []string
	// Account replaces the safe account identifiers (account switching); nil keeps them.
	Account *providers.SafeAccount
}

// Rotate stores a new value under the same reference. A manual key is validated first, exactly as
// when it was added, and a key the provider does not accept changes nothing. An OAuth value is what the
// OAuth engine just obtained. A reference whose value is provisioned outside (environment or file) is
// rotated there, not here. A revoked reference stays revoked: the operator adds a new credential.
func (s *Service) Rotate(ctx context.Context, scope contracts.Scope, ref providers.CredentialRefID, rot Rotation) (providers.CredentialRef, error) {
	loc, err := s.begin(scope)
	if err != nil {
		return providers.CredentialRef{}, err
	}
	if err := s.checkDeployment(scope); err != nil {
		return providers.CredentialRef{}, err
	}
	r, err := s.find(ctx, scope, string(ref))
	if err != nil {
		return providers.CredentialRef{}, err
	}
	if r.ref.Status == providers.CredentialRefStatusRevoked {
		return providers.CredentialRef{}, fail(contracts.ErrUnauthenticated, ReasonRevoked, "credentials: a revoked credential cannot be rotated")
	}
	b, ok := s.backends[r.backend]
	switch {
	case !ok:
		return providers.CredentialRef{}, fail(contracts.ErrUnavailable, ReasonBackendUnknown, "credentials: the backend of the reference is not configured")
	case b.ReadOnly():
		return providers.CredentialRef{}, fail(contracts.ErrValidationFailed, ReasonUnsupported, "credentials: the value is provisioned outside the service and is rotated there")
	}
	if err := validValue(rot.Key); err != nil {
		return providers.CredentialRef{}, err
	}
	if !rot.Refresh.Empty() {
		if err := validValue(rot.Refresh); err != nil {
			return providers.CredentialRef{}, err
		}
	}
	expires, err := s.validExpiry(rot.ExpiresAt)
	if err != nil {
		return providers.CredentialRef{}, err
	}
	scopes := r.ref.Binding.Scopes
	if rot.Scopes != nil {
		if scopes, err = validScopes(rot.Scopes); err != nil {
			return providers.CredentialRef{}, err
		}
	}
	if r.ref.Kind == providers.CredentialKindOauth && len(scopes) == 0 {
		return providers.CredentialRef{}, fail(contracts.ErrValidationFailed, ReasonBadRequest, "credentials: an OAuth credential has at least one scope")
	}
	var res ValidationResult
	if r.ref.Kind == providers.CredentialKindAPIKey {
		if res, err = s.validate(ctx, r.ref.Binding.ProviderID, rot.Key, scopes); err != nil {
			_ = s.lifecycle(ctx, loc, lifecycleAudit{Ref: string(ref), Provider: string(r.ref.Binding.ProviderID), Action: "rejected", Reason: string(ReasonOf(err))})
			return providers.CredentialRef{}, err
		}
	}
	slot := r.slot()
	if err := b.Put(ctx, slot, rot.Key); err != nil {
		return providers.CredentialRef{}, fail(contracts.ErrUnavailable, ReasonBackendFailed, "credentials: the value could not be stored")
	}
	if !rot.Refresh.Empty() {
		if err := b.Put(ctx, slot.refresh(), rot.Refresh); err != nil {
			return providers.CredentialRef{}, fail(contracts.ErrUnavailable, ReasonBackendFailed, "credentials: the value could not be stored")
		}
	}
	now := s.clk.Now()
	var out row
	err = s.db.Tx(ctx, func(tx *store.Tx) error {
		cur, found, err := loadRow(ctx, tx, scope.ApplicationID, scope.TenantID, string(ref))
		if err != nil {
			return err
		}
		if !found || cur.ref.Status == providers.CredentialRefStatusRevoked {
			return fail(contracts.ErrUnauthenticated, ReasonRevoked, "credentials: the credential was revoked meanwhile")
		}
		from := cur.ref.Status
		if rot.Scopes != nil { // nil keeps the scopes as they are now, not as they were before the transaction
			cur.ref.Binding.Scopes = scopes
		}
		if rot.Account != nil {
			cur.ref.Account = sanitizeAccount(*rot.Account)
		}
		if expires != nil {
			cur.ref.Times.ExpiresAt = expires
		}
		cur.ref.Times.RotatedAt = tsPtr(now)
		cur.ref.UpdatedAt = ts(now)
		cur.hasRefresh = cur.hasRefresh || !rot.Refresh.Empty()
		if cur.ref.Kind == providers.CredentialKindAPIKey {
			fp := Fingerprint(rot.Key)
			cur.ref.Fingerprint = &fp
			cur.ref.Times.LastValidatedAt = tsPtr(now)
			cur.ref.Warnings = keyWarnings(res)
			cur.ref.Status = keyStatus(res)
		} else if rot.Scopes != nil || cur.ref.Status == providers.CredentialRefStatusActive {
			// A re-authorization (it names the consented scopes) makes the connection active again; a refresh
			// keeps a verdict (quarantine, invalid, reauthorization required) that landed while it was in flight.
			cur.ref.Status = providers.CredentialRefStatusActive
			cur.ref.Warnings = nil
		}
		if err := updateRow(ctx, tx, cur, now); err != nil {
			return err
		}
		out = cur
		_, err = s.aud.AppendTx(ctx, tx, loc, EventLifecycle.Event(lifecycleAudit{
			Ref: string(ref), Provider: string(cur.ref.Binding.ProviderID), Action: "rotated", From: string(from), To: string(cur.ref.Status)}))
		return err
	})
	if err != nil {
		if ReasonOf(err) == ReasonRevoked {
			// revoked while we wrote: leave nothing behind
			_ = b.Delete(ctx, slot)
			_ = b.Delete(ctx, slot.refresh())
			return providers.CredentialRef{}, err
		}
		return providers.CredentialRef{}, fail(contracts.ErrUnavailable, ReasonAuditFailed, "credentials: the rotation could not be recorded")
	}
	return s.view(out), nil
}

// Revoke revokes a reference at once and deletes its value. The status change is committed before the
// value is deleted, so the very next Resolve fails whatever the backend does. A revocation that cannot be
// recorded still deletes the value, so the reference releases nothing whatever the audit does. A revoked
// reference is never usable again; adding a credential makes a new reference. Revoking twice is not an error.
func (s *Service) Revoke(ctx context.Context, scope contracts.Scope, ref providers.CredentialRefID) (providers.CredentialRef, error) {
	loc, err := s.begin(scope)
	if err != nil {
		return providers.CredentialRef{}, err
	}
	if err := s.checkDeployment(scope); err != nil {
		return providers.CredentialRef{}, err
	}
	return s.revokeOne(ctx, scope, loc, string(ref))
}

// RevokeTenant revokes every reference of the scope's tenant: a tenant removed or a security reset. It
// returns how many references it revoked; it carries on past a failure and returns the first one.
func (s *Service) RevokeTenant(ctx context.Context, scope contracts.Scope) (int, error) {
	loc, err := s.begin(scope)
	if err != nil {
		return 0, err
	}
	if err := s.checkDeployment(scope); err != nil {
		return 0, err
	}
	rows, err := listRows(ctx, s.db, scope.ApplicationID, scope.TenantID, "")
	if err != nil {
		return 0, err
	}
	var first error
	n := 0
	for _, r := range rows {
		if r.ref.Status == providers.CredentialRefStatusRevoked {
			continue
		}
		if _, err := s.revokeOne(ctx, scope, loc, string(r.ref.RefID)); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		n++
	}
	return n, first
}

func (s *Service) revokeOne(ctx context.Context, scope contracts.Scope, loc audit.Locator, ref string) (providers.CredentialRef, error) {
	now := s.clk.Now()
	var out row
	already := false
	err := s.db.Tx(ctx, func(tx *store.Tx) error {
		cur, found, err := loadRow(ctx, tx, scope.ApplicationID, scope.TenantID, ref)
		if err != nil {
			return err
		}
		if !found {
			return deny(ReasonNotBound, "credentials: no such reference in this application and tenant")
		}
		out = cur
		if cur.ref.Status == providers.CredentialRefStatusRevoked {
			already = true
			return nil
		}
		from := cur.ref.Status
		cur.ref.Status = providers.CredentialRefStatusRevoked
		cur.ref.Times.RevokedAt = tsPtr(now)
		cur.ref.UpdatedAt = ts(now)
		if err := updateRow(ctx, tx, cur, now); err != nil {
			return err
		}
		out = cur
		_, err = s.aud.AppendTx(ctx, tx, loc, EventLifecycle.Event(lifecycleAudit{
			Ref: ref, Provider: string(cur.ref.Binding.ProviderID), Action: "revoked", From: string(from), To: string(cur.ref.Status), Reason: "operator"}))
		return err
	})
	if err != nil {
		if ReasonOf(err) == ReasonNotBound {
			return providers.CredentialRef{}, err
		}
		// The revocation could not be recorded, but it was asked for: the stored value is deleted all the same, so
		// whatever the audit or the store does, the reference releases nothing (it fails closed as secret_missing
		// until a revocation is recorded).
		if b, ok := s.backends[out.backend]; ok && !b.ReadOnly() && out.ref.RefID != "" {
			slot, dctx := out.slot(), context.WithoutCancel(ctx) // a cancelled call may be why the record failed
			_, _ = b.Delete(dctx, slot), b.Delete(dctx, slot.refresh())
			s.log.Warn("credential revocation was not recorded; its stored value was deleted", "ref", ref)
		}
		return providers.CredentialRef{}, fail(contracts.ErrUnavailable, ReasonAuditFailed, "credentials: the revocation could not be recorded")
	}
	if !already {
		if b, ok := s.backends[out.backend]; ok && !b.ReadOnly() {
			slot := out.slot()
			e1, e2 := b.Delete(ctx, slot), b.Delete(ctx, slot.refresh())
			if e1 != nil || e2 != nil {
				return s.view(out), fail(contracts.ErrPartialFailure, ReasonBackendFailed, "credentials: revoked, but the stored value could not be deleted")
			}
		}
	}
	return s.view(out), nil
}

// Flag is a verdict on a credential that arrived after it was added: the provider rejected it, reported
// excessive permissions, or the consent changed.
type Flag struct {
	// Status is invalid, quarantined or reauthorization_required.
	Status providers.CredentialRefStatus
	// Warning is required for quarantined: excessive_permissions_reported or unsupported_account_policy.
	Warning providers.ConnectionWarning
}

// Flag records the verdict. Afterwards Resolve refuses the reference with a typed error, and nothing
// substitutes another credential or provider. A revoked reference stays revoked.
func (s *Service) Flag(ctx context.Context, scope contracts.Scope, ref providers.CredentialRefID, f Flag) (providers.CredentialRef, error) {
	loc, err := s.begin(scope)
	if err != nil {
		return providers.CredentialRef{}, err
	}
	if err := s.checkDeployment(scope); err != nil {
		return providers.CredentialRef{}, err
	}
	switch f.Status {
	case providers.CredentialRefStatusInvalid, providers.CredentialRefStatusReauthorizationRequired:
	case providers.CredentialRefStatusQuarantined:
		if f.Warning != providers.ConnectionWarningExcessivePermissionsReported && f.Warning != providers.ConnectionWarningUnsupportedAccountPolicy {
			return providers.CredentialRef{}, fail(contracts.ErrValidationFailed, ReasonBadRequest, "credentials: a quarantine names why")
		}
	default:
		return providers.CredentialRef{}, fail(contracts.ErrValidationFailed, ReasonBadRequest, "credentials: that status cannot be flagged")
	}
	now := s.clk.Now()
	var out row
	err = s.db.Tx(ctx, func(tx *store.Tx) error {
		cur, found, err := loadRow(ctx, tx, scope.ApplicationID, scope.TenantID, string(ref))
		if err != nil {
			return err
		}
		if !found {
			return deny(ReasonNotBound, "credentials: no such reference in this application and tenant")
		}
		if cur.ref.Status == providers.CredentialRefStatusRevoked {
			return fail(contracts.ErrUnauthenticated, ReasonRevoked, "credentials: the credential has been revoked")
		}
		from := cur.ref.Status
		cur.ref.Status = f.Status
		if f.Status == providers.CredentialRefStatusQuarantined && !slices.Contains(cur.ref.Warnings, f.Warning) {
			cur.ref.Warnings = append(slices.Clone(cur.ref.Warnings), f.Warning)
		}
		cur.ref.UpdatedAt = ts(now)
		if err := updateRow(ctx, tx, cur, now); err != nil {
			return err
		}
		out = cur
		_, err = s.aud.AppendTx(ctx, tx, loc, EventLifecycle.Event(lifecycleAudit{
			Ref: string(ref), Provider: string(cur.ref.Binding.ProviderID), Action: "flagged", From: string(from), To: string(f.Status), Reason: string(f.Warning)}))
		return err
	})
	if err != nil {
		if r := ReasonOf(err); r == ReasonNotBound || r == ReasonRevoked {
			return providers.CredentialRef{}, err
		}
		return providers.CredentialRef{}, fail(contracts.ErrUnavailable, ReasonAuditFailed, "credentials: the change could not be recorded")
	}
	return s.view(out), nil
}
