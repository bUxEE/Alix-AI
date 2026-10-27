package providerreg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"time"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

const ProviderIDSelfHosted providers.ProviderID = "self_hosted"

var privateEndpointIDRE = regexp.MustCompile(`^ple_[0-9A-HJKMNP-TV-Z]{26}$`)

// PrivateEndpoint is one persisted self-hosted endpoint row (ADR 0045).
type PrivateEndpoint struct {
	EndpointID       providers.PrivateEndpointID
	BaseURL          string
	APIDialect       string
	ModelIDs         []providers.ModelID
	Roles            []providers.PrincipalLlmRole
	CredentialRef    *providers.CredentialRefID
	OwnerKind        providers.PrivateEndpointOwnerKind
	OwnerPrincipalID *common.Identifier
	OwnerTenantID    *common.Identifier
	HealthURL        *string
	Lifecycle        providers.PrivateLlmLifecycle
	ProfileID        providers.ProfileID
	PriceIn          int64
	PriceOut         int64
	UpdatedAt        time.Time
}

// PrincipalPrivateBinding is one operator or self-service private role binding.
type PrincipalPrivateBinding struct {
	Role       providers.PrincipalLlmRole
	EndpointID providers.PrivateEndpointID
	ModelID    providers.ModelID
	UpdatedAt  time.Time
}

func validPrivateEndpointID(id providers.PrivateEndpointID) bool {
	return privateEndpointIDRE.MatchString(string(id))
}

func validatePrivateEndpointUpdate(in providers.PrivateLlmEndpointUpdate) error {
	if !endpointRE.MatchString(in.Origin) {
		return invalid("the base URL must be https (or loopback http) with no credentials, query or fragment")
	}
	if in.HealthURL != nil && *in.HealthURL != "" && !endpointRE.MatchString(*in.HealthURL) {
		return invalid("the health URL must be https (or loopback http) with no credentials, query or fragment")
	}
	if len(in.ModelIDs) == 0 || len(in.Roles) == 0 {
		return invalid("model_ids and roles are required")
	}
	for _, m := range in.ModelIDs {
		if !modelIDRE.MatchString(string(m)) {
			return invalid("a model id is malformed")
		}
	}
	for _, r := range in.Roles {
		if !r.Valid() {
			return invalid("unknown role")
		}
	}
	if !in.OwnerKind.Valid() {
		return invalid("owner_kind is required")
	}
	switch in.OwnerKind {
	case providers.PrivateEndpointOwnerKindPrincipal:
		if in.OwnerPrincipalID == nil || *in.OwnerPrincipalID == "" {
			return invalid("owner_principal_id is required when owner_kind is principal")
		}
	case providers.PrivateEndpointOwnerKindTenant:
		if in.OwnerTenantID == nil || *in.OwnerTenantID == "" {
			return invalid("owner_tenant_id is required when owner_kind is tenant")
		}
	}
	if in.CredRef != nil && *in.CredRef != "" && !validCredRef(*in.CredRef) {
		return invalid("cred_ref is not a valid reference")
	}
	if in.Lifecycle != nil && !in.Lifecycle.Valid() {
		return invalid("unknown lifecycle")
	}
	return nil
}

// PutPrivateEndpoint upserts one endpoint (ADR 0045).
func (r *Registry) PutPrivateEndpoint(ctx context.Context, scope contracts.Scope, id providers.PrivateEndpointID, in providers.PrivateLlmEndpointUpdate) (providers.PrivateLlmEndpoint, error) {
	app, err := appOf(scope)
	if err != nil {
		return providers.PrivateLlmEndpoint{}, err
	}
	if !validPrivateEndpointID(id) {
		return providers.PrivateLlmEndpoint{}, invalid("the endpoint id must be a ple_ ULID")
	}
	if err := validatePrivateEndpointUpdate(in); err != nil {
		return providers.PrivateLlmEndpoint{}, err
	}
	lc := providers.PrivateLlmLifecycleProvisioning
	if in.Lifecycle != nil {
		lc = *in.Lifecycle
	}
	now := r.clk.Now().UnixMilli()
	modelsJSON, _ := json.Marshal(in.ModelIDs)
	rolesJSON, _ := json.Marshal(in.Roles)
	var cred sql.NullString
	if in.CredRef != nil && *in.CredRef != "" {
		cred = sql.NullString{String: string(*in.CredRef), Valid: true}
	}
	var ownerPr, ownerTen sql.NullString
	if in.OwnerPrincipalID != nil {
		ownerPr = sql.NullString{String: string(*in.OwnerPrincipalID), Valid: true}
	}
	if in.OwnerTenantID != nil {
		ownerTen = sql.NullString{String: string(*in.OwnerTenantID), Valid: true}
	}
	var health sql.NullString
	if in.HealthURL != nil && *in.HealthURL != "" {
		health = sql.NullString{String: *in.HealthURL, Valid: true}
	}
	priceIn, priceOut := int64(0), int64(0)
	if in.InternalPriceMicroPerMtokIn != nil {
		priceIn = int64(*in.InternalPriceMicroPerMtokIn)
	}
	if in.InternalPriceMicroPerMtokOut != nil {
		priceOut = int64(*in.InternalPriceMicroPerMtokOut)
	}
	err = r.db.Tx(ctx, func(tx *store.Tx) error {
		var profileID, prevLC string
		err := tx.QueryRowContext(ctx, `
			SELECT profile_id, lifecycle FROM providerreg_private_endpoints
			 WHERE `+appWhere+` AND endpoint_id = ?`, app, string(id)).Scan(&profileID, &prevLC)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			profileID = r.ids.New("pprf")
		case err != nil:
			return err
		default:
			if in.Lifecycle == nil {
				lc = providers.PrivateLlmLifecycle(prevLC)
			}
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO providerreg_private_endpoints
			  (application_id, tenant_id, workspace_id, endpoint_id, base_url, api_dialect,
			   model_ids_json, roles_json, credential_ref, owner_kind, owner_principal_id, owner_tenant_id,
			   health_url, lifecycle, profile_id, price_in, price_out, updated_at)
			VALUES (?, '', '', ?, ?, 'openai-compatible', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (application_id, tenant_id, workspace_id, endpoint_id) DO UPDATE SET
			  base_url = excluded.base_url,
			  model_ids_json = excluded.model_ids_json,
			  roles_json = excluded.roles_json,
			  credential_ref = excluded.credential_ref,
			  owner_kind = excluded.owner_kind,
			  owner_principal_id = excluded.owner_principal_id,
			  owner_tenant_id = excluded.owner_tenant_id,
			  health_url = excluded.health_url,
			  lifecycle = excluded.lifecycle,
			  price_in = excluded.price_in,
			  price_out = excluded.price_out,
			  updated_at = excluded.updated_at`,
			app, string(id), in.Origin, string(modelsJSON), string(rolesJSON), cred,
			string(in.OwnerKind), ownerPr, ownerTen, health, string(lc), profileID, priceIn, priceOut, now)
		return err
	})
	if err != nil {
		return providers.PrivateLlmEndpoint{}, wrapStore(err, "private endpoint could not be stored")
	}
	ep, found, err := r.GetPrivateEndpoint(ctx, scope, id)
	if err != nil || !found {
		return providers.PrivateLlmEndpoint{}, wrapStore(err, "private endpoint could not be read back")
	}
	return ep.ToContract(), nil
}

// GetPrivateEndpoint loads one endpoint by id.
func (r *Registry) GetPrivateEndpoint(ctx context.Context, scope contracts.Scope, id providers.PrivateEndpointID) (PrivateEndpoint, bool, error) {
	app, err := appOf(scope)
	if err != nil {
		return PrivateEndpoint{}, false, err
	}
	ep, ok, err := loadPrivateEndpoint(ctx, r.db, app, id)
	return ep, ok, wrapStore(err, "private endpoint could not be read")
}

// DeletePrivateEndpoint removes the endpoint and clears pins and bindings that named it.
func (r *Registry) DeletePrivateEndpoint(ctx context.Context, scope contracts.Scope, id providers.PrivateEndpointID) error {
	app, err := appOf(scope)
	if err != nil {
		return err
	}
	return wrapStore(r.db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM providerreg_private_endpoints WHERE `+appWhere+` AND endpoint_id = ?`, app, string(id)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM providerreg_tenant_private_llm WHERE application_id = ? AND endpoint_id = ?`, app, string(id)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			DELETE FROM providerreg_principal_private_bindings WHERE `+appWhere+` AND endpoint_id = ?`, app, string(id))
		return err
	}), "private endpoint could not be deleted")
}

// SetPrivateEndpointLifecycle updates lifecycle (health checker or host provisioner).
func (r *Registry) SetPrivateEndpointLifecycle(ctx context.Context, scope contracts.Scope, id providers.PrivateEndpointID, lc providers.PrivateLlmLifecycle) error {
	if !lc.Valid() {
		return invalid("unknown lifecycle")
	}
	app, err := appOf(scope)
	if err != nil {
		return err
	}
	now := r.clk.Now().UnixMilli()
	res, err := r.db.ExecContext(ctx, `
		UPDATE providerreg_private_endpoints SET lifecycle = ?, updated_at = ?
		 WHERE `+appWhere+` AND endpoint_id = ?`, string(lc), now, app, string(id))
	if err != nil {
		return wrapStore(err, "private endpoint lifecycle could not be updated")
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fail(contracts.ErrValidationFailed, ReasonInvalidProfile, "the private endpoint does not exist").
			WithDetails("reason", "not_found")
	}
	return nil
}

// PutTenantPrivateLLMPin pins a tenant to an endpoint.
func (r *Registry) PutTenantPrivateLLMPin(ctx context.Context, scope contracts.Scope, tenantID string, endpointID providers.PrivateEndpointID) (providers.PrivateLlmTenantPin, error) {
	app, err := appOf(scope)
	if err != nil {
		return providers.PrivateLlmTenantPin{}, err
	}
	if tenantID == "" {
		return providers.PrivateLlmTenantPin{}, invalid("tenant_id is required")
	}
	if _, ok, err := r.GetPrivateEndpoint(ctx, scope, endpointID); err != nil {
		return providers.PrivateLlmTenantPin{}, err
	} else if !ok {
		return providers.PrivateLlmTenantPin{}, fail(contracts.ErrValidationFailed, ReasonInvalidProfile, "the private endpoint does not exist").
			WithDetails("reason", "not_found")
	}
	now := r.clk.Now().UnixMilli()
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO providerreg_tenant_private_llm (application_id, tenant_id, workspace_id, endpoint_id, updated_at)
		VALUES (?, ?, '', ?, ?)
		ON CONFLICT (application_id, tenant_id) DO UPDATE SET
		  endpoint_id = excluded.endpoint_id,
		  updated_at = excluded.updated_at`, app, tenantID, string(endpointID), now)
	if err = wrapStore(err, "tenant private LLM pin could not be stored"); err != nil {
		return providers.PrivateLlmTenantPin{}, err
	}
	return r.TenantPrivateLLMPinView(ctx, scope, tenantID)
}

// TenantPrivateLLMPinView returns the tenant pin with endpoint lifecycle.
func (r *Registry) TenantPrivateLLMPinView(ctx context.Context, scope contracts.Scope, tenantID string) (providers.PrivateLlmTenantPin, error) {
	pin, ok, err := r.GetTenantPrivateLLMPin(ctx, scope, tenantID)
	if err != nil {
		return providers.PrivateLlmTenantPin{}, err
	}
	if !ok {
		return providers.PrivateLlmTenantPin{}, fail(contracts.ErrValidationFailed, ReasonInvalidProfile, "the tenant pin does not exist").
			WithDetails("reason", "not_found")
	}
	ep, _, err := r.GetPrivateEndpoint(ctx, scope, pin.EndpointID)
	if err != nil {
		return providers.PrivateLlmTenantPin{}, err
	}
	lc := ep.Lifecycle
	return providers.PrivateLlmTenantPin{
		TenantID: common.Identifier(tenantID), EndpointID: pin.EndpointID, Lifecycle: &lc,
		UpdatedAt: tsCommon(pin.UpdatedAt),
	}, nil
}

// GetTenantPrivateLLMPin returns the pin for tenantID.
func (r *Registry) GetTenantPrivateLLMPin(ctx context.Context, scope contracts.Scope, tenantID string) (struct {
	EndpointID providers.PrivateEndpointID
	UpdatedAt  time.Time
}, bool, error) {
	app, err := appOf(scope)
	if err != nil {
		return struct {
			EndpointID providers.PrivateEndpointID
			UpdatedAt  time.Time
		}{}, false, err
	}
	var ep string
	var updated int64
	err = r.db.QueryRowContext(ctx, `
		SELECT endpoint_id, updated_at FROM providerreg_tenant_private_llm
		 WHERE application_id = ? AND tenant_id = ?`, app, tenantID).Scan(&ep, &updated)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return struct {
			EndpointID providers.PrivateEndpointID
			UpdatedAt  time.Time
		}{}, false, nil
	case err != nil:
		return struct {
			EndpointID providers.PrivateEndpointID
			UpdatedAt  time.Time
		}{}, false, wrapStore(err, "tenant private LLM pin could not be read")
	}
	return struct {
		EndpointID providers.PrivateEndpointID
		UpdatedAt  time.Time
	}{EndpointID: providers.PrivateEndpointID(ep), UpdatedAt: time.UnixMilli(updated).UTC()}, true, nil
}

// DeleteTenantPrivateLLMPin clears a tenant pin.
func (r *Registry) DeleteTenantPrivateLLMPin(ctx context.Context, scope contracts.Scope, tenantID string) error {
	app, err := appOf(scope)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `
		DELETE FROM providerreg_tenant_private_llm WHERE application_id = ? AND tenant_id = ?`, app, tenantID)
	return wrapStore(err, "tenant private LLM pin could not be deleted")
}

// PutPrincipalPrivateBindings replaces operator bindings for principal (all roles in one PUT).
func (r *Registry) PutPrincipalPrivateBindings(ctx context.Context, scope contracts.Scope, principal string, endpointID providers.PrivateEndpointID, roles []providers.PrincipalLlmRole) (providers.PrivateLlmPrincipalBinding, error) {
	app, err := appOf(scope)
	if err != nil {
		return providers.PrivateLlmPrincipalBinding{}, err
	}
	if principal == "" {
		return providers.PrivateLlmPrincipalBinding{}, invalid("principal_id is required")
	}
	ep, ok, err := r.GetPrivateEndpoint(ctx, scope, endpointID)
	if err != nil {
		return providers.PrivateLlmPrincipalBinding{}, err
	}
	if !ok {
		return providers.PrivateLlmPrincipalBinding{}, fail(contracts.ErrValidationFailed, ReasonInvalidProfile, "the private endpoint does not exist").
			WithDetails("reason", "not_found")
	}
	use := scope
	use.PrincipalID = principal
	if err := EndpointOwnerPermitsUse(use, ep); err != nil {
		return providers.PrivateLlmPrincipalBinding{}, err
	}
	if len(roles) == 0 {
		return providers.PrivateLlmPrincipalBinding{}, invalid("roles are required")
	}
	model := ep.ModelIDs[0]
	now := r.clk.Now().UnixMilli()
	err = wrapStore(r.db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM providerreg_principal_private_bindings
			 WHERE `+appWhere+` AND principal_id = ?`, app, principal); err != nil {
			return err
		}
		for _, role := range roles {
			if !role.Valid() || !slices.Contains(ep.Roles, role) {
				return invalid("the endpoint does not serve this role")
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO providerreg_principal_private_bindings
				  (application_id, tenant_id, workspace_id, principal_id, role, endpoint_id, model_id, updated_at)
				VALUES (?, '', '', ?, ?, ?, ?, ?)`,
				app, principal, string(role), string(endpointID), string(model), now); err != nil {
				return err
			}
		}
		return nil
	}), "principal private LLM binding could not be stored")
	if err != nil {
		return providers.PrivateLlmPrincipalBinding{}, err
	}
	return r.principalPrivateContract(ctx, scope, principal)
}

// PutPrincipalPrivateBinding stores one role binding (self-service /me).
func (r *Registry) PutPrincipalPrivateBinding(ctx context.Context, scope contracts.Scope, principal string, b PrincipalPrivateBinding) error {
	app, err := appOf(scope)
	if err != nil {
		return err
	}
	if !b.Role.Valid() || b.EndpointID == "" || b.ModelID == "" {
		return invalid("role, endpoint_id and model_id are required")
	}
	ep, ok, err := r.GetPrivateEndpoint(ctx, scope, b.EndpointID)
	if err != nil {
		return err
	}
	if !ok {
		return fail(contracts.ErrValidationFailed, ReasonInvalidProfile, "the private endpoint does not exist").
			WithDetails("reason", "not_found")
	}
	use := scope
	use.PrincipalID = principal
	if err := EndpointOwnerPermitsUse(use, ep); err != nil {
		return err
	}
	now := r.clk.Now().UnixMilli()
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO providerreg_principal_private_bindings
		  (application_id, tenant_id, workspace_id, principal_id, role, endpoint_id, model_id, updated_at)
		VALUES (?, '', '', ?, ?, ?, ?, ?)
		ON CONFLICT (application_id, tenant_id, workspace_id, principal_id, role) DO UPDATE SET
		  endpoint_id = excluded.endpoint_id,
		  model_id = excluded.model_id,
		  updated_at = excluded.updated_at`,
		app, principal, string(b.Role), string(b.EndpointID), string(b.ModelID), now)
	return wrapStore(err, "principal private binding could not be stored")
}

func (r *Registry) principalPrivateContract(ctx context.Context, scope contracts.Scope, principal string) (providers.PrivateLlmPrincipalBinding, error) {
	bindings, err := r.ListPrincipalPrivateBindings(ctx, scope, principal)
	if err != nil {
		return providers.PrivateLlmPrincipalBinding{}, err
	}
	if len(bindings) == 0 {
		return providers.PrivateLlmPrincipalBinding{}, fail(contracts.ErrValidationFailed, ReasonInvalidProfile, "the principal private binding does not exist").
			WithDetails("reason", "not_found")
	}
	epID := bindings[0].EndpointID
	ep, ok, err := r.GetPrivateEndpoint(ctx, scope, epID)
	if err != nil {
		return providers.PrivateLlmPrincipalBinding{}, err
	}
	if !ok {
		return providers.PrivateLlmPrincipalBinding{}, fail(contracts.ErrInternal, ReasonCorrupt, "private endpoint missing for binding")
	}
	roles := make([]providers.PrincipalLlmRole, 0, len(bindings))
	var latest time.Time
	for _, b := range bindings {
		roles = append(roles, b.Role)
		if b.UpdatedAt.After(latest) {
			latest = b.UpdatedAt
		}
	}
	lc := ep.Lifecycle
	return providers.PrivateLlmPrincipalBinding{
		PrincipalID: common.Identifier(principal), EndpointID: epID, Roles: roles, Lifecycle: &lc,
		UpdatedAt: tsCommon(latest),
	}, nil
}

// GetPrincipalPrivateBinding returns operator view of principal private bindings.
func (r *Registry) GetPrincipalPrivateBinding(ctx context.Context, scope contracts.Scope, principal string) (providers.PrivateLlmPrincipalBinding, bool, error) {
	b, err := r.principalPrivateContract(ctx, scope, principal)
	if err != nil {
		var ce *contracts.Error
		if errors.As(err, &ce) && ce != nil {
			if reason, _ := ce.Details["reason"].(string); reason == "not_found" {
				return providers.PrivateLlmPrincipalBinding{}, false, nil
			}
		}
		return providers.PrivateLlmPrincipalBinding{}, false, err
	}
	return b, true, nil
}

// DeletePrincipalPrivateBindings clears all private bindings for principal.
func (r *Registry) DeletePrincipalPrivateBindings(ctx context.Context, scope contracts.Scope, principal string) error {
	app, err := appOf(scope)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `
		DELETE FROM providerreg_principal_private_bindings
		 WHERE `+appWhere+` AND principal_id = ?`, app, principal)
	return wrapStore(err, "principal private bindings could not be deleted")
}

// DeletePrincipalPrivateBinding clears one role private binding.
func (r *Registry) DeletePrincipalPrivateBinding(ctx context.Context, scope contracts.Scope, principal string, role providers.PrincipalLlmRole) error {
	app, err := appOf(scope)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `
		DELETE FROM providerreg_principal_private_bindings
		 WHERE `+appWhere+` AND principal_id = ? AND role = ?`, app, principal, string(role))
	return wrapStore(err, "principal private binding could not be deleted")
}

// ListPrincipalPrivateBindings lists private bindings for a principal.
func (r *Registry) ListPrincipalPrivateBindings(ctx context.Context, scope contracts.Scope, principal string) ([]PrincipalPrivateBinding, error) {
	app, err := appOf(scope)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT role, endpoint_id, model_id, updated_at
		  FROM providerreg_principal_private_bindings
		 WHERE `+appWhere+` AND principal_id = ?
		 ORDER BY role`, app, principal)
	if err != nil {
		return nil, wrapStore(err, "principal private bindings could not be listed")
	}
	defer rows.Close()
	var out []PrincipalPrivateBinding
	for rows.Next() {
		var role, ep, model string
		var updated int64
		if err := rows.Scan(&role, &ep, &model, &updated); err != nil {
			return nil, wrapStore(err, "principal private bindings could not be listed")
		}
		out = append(out, PrincipalPrivateBinding{
			Role: providers.PrincipalLlmRole(role), EndpointID: providers.PrivateEndpointID(ep),
			ModelID: providers.ModelID(model), UpdatedAt: time.UnixMilli(updated).UTC(),
		})
	}
	return out, wrapStore(rows.Err(), "principal private bindings could not be listed")
}

// PrincipalPrivateBindingFor returns one role binding.
func (r *Registry) PrincipalPrivateBindingFor(ctx context.Context, scope contracts.Scope, principal string, role providers.PrincipalLlmRole) (PrincipalPrivateBinding, bool, error) {
	all, err := r.ListPrincipalPrivateBindings(ctx, scope, principal)
	if err != nil {
		return PrincipalPrivateBinding{}, false, err
	}
	for _, b := range all {
		if b.Role == role {
			return b, true, nil
		}
	}
	return PrincipalPrivateBinding{}, false, nil
}

// ErasePrincipalPrivateBindings deletes private bindings for principal (erasure).
func (r *Registry) ErasePrincipalPrivateBindings(ctx context.Context, scope contracts.Scope, principal string) (int, error) {
	app, err := appOf(scope)
	if err != nil {
		return 0, err
	}
	res, err := r.db.ExecContext(ctx, `
		DELETE FROM providerreg_principal_private_bindings
		 WHERE `+appWhere+` AND principal_id = ?`, app, principal)
	if err != nil {
		return 0, wrapStore(err, "principal private bindings could not be erased")
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// EraseTenantPrivateLLMPin removes the tenant pin (erasure).
func (r *Registry) EraseTenantPrivateLLMPin(ctx context.Context, scope contracts.Scope, tenantID string) error {
	return r.DeleteTenantPrivateLLMPin(ctx, scope, tenantID)
}

// EndpointOwnerPermitsUse reports whether principal may use endpoint (bind or resolve).
func EndpointOwnerPermitsUse(scope contracts.Scope, ep PrivateEndpoint) error {
	switch ep.OwnerKind {
	case providers.PrivateEndpointOwnerKindShared:
		return nil
	case providers.PrivateEndpointOwnerKindPrincipal:
		if ep.OwnerPrincipalID == nil || string(*ep.OwnerPrincipalID) != scope.PrincipalID {
			return fail(contracts.ErrPermissionDenied, ReasonScopeNotPermitted, "this private endpoint belongs to another principal")
		}
	case providers.PrivateEndpointOwnerKindTenant:
		if ep.OwnerTenantID == nil || string(*ep.OwnerTenantID) != scope.TenantID {
			return fail(contracts.ErrPermissionDenied, ReasonScopeNotPermitted, "this private endpoint belongs to another tenant")
		}
	}
	return nil
}

// SynthesizeProfile builds a qualified ProviderProfile for routing when lifecycle is ready (ADR 0045).
func SynthesizeProfile(app string, ep PrivateEndpoint, model providers.ModelID, role providers.Role) providers.ProviderProfile {
	if ep.Lifecycle != providers.PrivateLlmLifecycleReady {
		return providers.ProviderProfile{}
	}
	roles := []providers.Role{role}
	classes := []providers.EgressEligibleClass{
		providers.EgressEligibleClassPublic,
		providers.EgressEligibleClassInternal,
		providers.EgressEligibleClassConfidential,
	}
	residency := providers.ResidencyPinned
	regions := []providers.RegionCode{"global"}
	if ep.BaseURL != "" && (len(ep.BaseURL) >= 16 && ep.BaseURL[:16] == "http://127.0.0.1" || len(ep.BaseURL) >= 15 && ep.BaseURL[:15] == "http://localhost") {
		residency = providers.ResidencyGlobal
	}
	p := providers.ProviderProfile{
		ProfileID:    ep.ProfileID,
		ProviderID:   ProviderIDSelfHosted,
		Endpoint:     ep.BaseURL,
		ModelID:      model,
		ModelVersion: providers.ModelVersion("private:" + string(ep.EndpointID)),
		Lifecycle:    providers.ProfileLifecycleQualified,
		Roles:        roles,
		CredRef:      ep.CredentialRef,
		Modalities: providers.ProviderProfileModalities{
			Input: []providers.Modality{providers.ModalityText}, Output: []providers.Modality{providers.ModalityText},
		},
		Streaming:        true,
		StructuredOutput: providers.ProviderProfileStructuredOutputJSONMode,
		ToolCalling:      providers.ProviderProfileToolCalling{Supported: true, StrictArguments: false},
		Limits: providers.ProviderProfileLimits{
			MaxContextToks: 128000, MaxOutputToks: 8192, MaxRequestBytes: 1 << 20,
		},
		ImageGeneration: providers.ProviderProfileImageGeneration{OutputRetention: providers.RetentionModeUnknown},
		Voice:           providers.ProviderProfileVoice{AudioRetention: providers.RetentionModeUnknown},
		Terms: providers.ProviderProfileTerms{
			Retention: providers.RetentionModeZero, TrainingUse: providers.TrainingUseNever,
			TermsHash:  providers.SHA256("sha256:" + "0000000000000000000000000000000000000000000000000000000000000000"),
			RecordedOn: "2026-01-01",
		},
		Region: providers.ProviderProfileRegion{Residency: residency, ProcessingRegions: regions},
		RateLimits: providers.ProviderProfileRateLimits{
			Retry: providers.ProviderProfileRateLimitsRetry{MaxAttempts: 2, InitialBackoffMS: 100, MaxBackoffMS: 1000, HonorsRetryAfter: true},
		},
		Pricing: providers.PriceTable{
			Currency: "USD", EffectiveOn: "2026-01-01",
		},
		UsageMetering: providers.ProviderProfileUsageMeteringPerResponse,
		PermittedScopes: []providers.ProviderProfileScopeGrant{
			{ApplicationID: common.Identifier(app)},
		},
		AllowedDataClasses: classes,
		LastPolicyReview:   "2026-01-01",
		ReviewIntervalDays: 30,
		BillingNotice: providers.ProviderProfileBillingNotice{
			Version: "private-self-hosted",
			Text:    "Self-hosted endpoint: no third-party provider billing.",
		},
	}
	if ep.PriceIn > 0 || ep.PriceOut > 0 {
		p.Pricing.Prices.ToksIn = &providers.PriceTablePrice{PerUnits: 1000000, MicroUnits: providers.MicroUnits(ep.PriceIn)}
		p.Pricing.Prices.ToksOut = &providers.PriceTablePrice{PerUnits: 1000000, MicroUnits: providers.MicroUnits(ep.PriceOut)}
	}
	return p
}

func principalRoleToProviderRole(r providers.PrincipalLlmRole) (providers.Role, bool) {
	switch r {
	case providers.PrincipalLlmRoleConversationalController:
		return providers.RoleConversationalController, true
	case providers.PrincipalLlmRoleLanguageRealization:
		return providers.RoleLanguageRealization, true
	case providers.PrincipalLlmRoleSTT:
		return providers.RoleSTT, true
	case providers.PrincipalLlmRoleTTS:
		return providers.RoleTTS, true
	}
	return "", false
}

func loadPrivateEndpoint(ctx context.Context, q store.Reader, app string, id providers.PrivateEndpointID) (PrivateEndpoint, bool, error) {
	var row PrivateEndpoint
	var modelsJSON, rolesJSON string
	var cred, ownerPr, ownerTen, health sql.NullString
	var updated int64
	err := q.QueryRowContext(ctx, `
		SELECT endpoint_id, base_url, api_dialect, model_ids_json, roles_json, credential_ref,
		       owner_kind, owner_principal_id, owner_tenant_id, health_url, lifecycle, profile_id,
		       price_in, price_out, updated_at
		  FROM providerreg_private_endpoints
		 WHERE application_id = ? AND tenant_id = '' AND workspace_id = '' AND endpoint_id = ?`,
		app, string(id)).Scan(
		&row.EndpointID, &row.BaseURL, &row.APIDialect, &modelsJSON, &rolesJSON, &cred,
		&row.OwnerKind, &ownerPr, &ownerTen, &health, &row.Lifecycle, &row.ProfileID,
		&row.PriceIn, &row.PriceOut, &updated,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return PrivateEndpoint{}, false, nil
	case err != nil:
		return PrivateEndpoint{}, false, err
	}
	if err := json.Unmarshal([]byte(modelsJSON), &row.ModelIDs); err != nil {
		return PrivateEndpoint{}, false, fail(contracts.ErrInternal, ReasonCorrupt, "private endpoint models are corrupt")
	}
	if err := json.Unmarshal([]byte(rolesJSON), &row.Roles); err != nil {
		return PrivateEndpoint{}, false, fail(contracts.ErrInternal, ReasonCorrupt, "private endpoint roles are corrupt")
	}
	if cred.Valid && cred.String != "" {
		ref := providers.CredentialRefID(cred.String)
		row.CredentialRef = &ref
	}
	if ownerPr.Valid {
		id := common.Identifier(ownerPr.String)
		row.OwnerPrincipalID = &id
	}
	if ownerTen.Valid {
		id := common.Identifier(ownerTen.String)
		row.OwnerTenantID = &id
	}
	if health.Valid && health.String != "" {
		h := health.String
		row.HealthURL = &h
	}
	row.UpdatedAt = time.UnixMilli(updated).UTC()
	return row, true, nil
}

// ToContract maps a stored endpoint to the public schema document.
func (ep PrivateEndpoint) ToContract() providers.PrivateLlmEndpoint {
	out := providers.PrivateLlmEndpoint{
		EndpointID: ep.EndpointID, Origin: ep.BaseURL, APIDialect: ep.APIDialect,
		ModelIDs: ep.ModelIDs, Roles: ep.Roles, OwnerKind: ep.OwnerKind,
		Lifecycle: ep.Lifecycle, UpdatedAt: tsCommon(ep.UpdatedAt),
	}
	if ep.CredentialRef != nil {
		out.CredRef = ep.CredentialRef
	}
	if ep.OwnerPrincipalID != nil {
		out.OwnerPrincipalID = ep.OwnerPrincipalID
	}
	if ep.OwnerTenantID != nil {
		out.OwnerTenantID = ep.OwnerTenantID
	}
	if ep.HealthURL != nil {
		out.HealthURL = ep.HealthURL
	}
	pid := ep.ProfileID
	out.ProfileID = &pid
	if ep.PriceIn > 0 {
		v := providers.MicroUnits(ep.PriceIn)
		out.InternalPriceMicroPerMtokIn = &v
	}
	if ep.PriceOut > 0 {
		v := providers.MicroUnits(ep.PriceOut)
		out.InternalPriceMicroPerMtokOut = &v
	}
	return out
}

// TenantPrivateEndpoint resolves a tenant pin for role (ADR 0045).
func (r *Registry) TenantPrivateEndpoint(ctx context.Context, scope contracts.Scope, role providers.PrincipalLlmRole) (PrivateEndpoint, providers.ModelID, bool, error) {
	pin, ok, err := r.GetTenantPrivateLLMPin(ctx, scope, scope.TenantID)
	if err != nil || !ok {
		return PrivateEndpoint{}, "", false, err
	}
	ep, ok, err := r.GetPrivateEndpoint(ctx, scope, pin.EndpointID)
	if err != nil || !ok {
		return PrivateEndpoint{}, "", false, err
	}
	if !slices.Contains(ep.Roles, role) {
		return PrivateEndpoint{}, "", false, nil
	}
	model := ep.ModelIDs[0]
	return ep, model, true, nil
}

// PrincipalPrivateEndpoint resolves an operator private binding for role.
func (r *Registry) PrincipalPrivateEndpoint(ctx context.Context, scope contracts.Scope, role providers.PrincipalLlmRole) (PrivateEndpoint, providers.ModelID, bool, error) {
	if scope.PrincipalID == "" {
		return PrivateEndpoint{}, "", false, nil
	}
	b, ok, err := r.PrincipalPrivateBindingFor(ctx, scope, scope.PrincipalID, role)
	if err != nil || !ok {
		return PrivateEndpoint{}, "", false, err
	}
	ep, ok, err := r.GetPrivateEndpoint(ctx, scope, b.EndpointID)
	if err != nil || !ok {
		return PrivateEndpoint{}, "", false, err
	}
	return ep, b.ModelID, true, nil
}

// PrivateEndpointByProfileID finds an endpoint by its synthetic profile id.
func (r *Registry) PrivateEndpointByProfileID(ctx context.Context, scope contracts.Scope, profileID providers.ProfileID) (PrivateEndpoint, bool, error) {
	app, err := appOf(scope)
	if err != nil {
		return PrivateEndpoint{}, false, err
	}
	var id string
	err = r.db.QueryRowContext(ctx, `
		SELECT endpoint_id FROM providerreg_private_endpoints
		 WHERE `+appWhere+` AND profile_id = ?`, app, string(profileID)).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return PrivateEndpoint{}, false, nil
	case err != nil:
		return PrivateEndpoint{}, false, wrapStore(err, "private endpoint could not be read")
	}
	return loadPrivateEndpoint(ctx, r.db, app, providers.PrivateEndpointID(id))
}

// PrivateProfileFor implements modes.PrivateProfileSource.
func (r *Registry) PrivateProfileFor(ctx context.Context, scope contracts.Scope, profileID providers.ProfileID, role providers.Role) (PrivateEndpoint, providers.ModelID, bool, error) {
	ep, ok, err := r.PrivateEndpointByProfileID(ctx, scope, profileID)
	if err != nil || !ok {
		return PrivateEndpoint{}, "", false, err
	}
	prRole, ok := providerRoleToPrincipalRole(role)
	if !ok {
		return PrivateEndpoint{}, "", false, nil
	}
	model := ep.ModelIDs[0]
	if b, found, _ := r.PrincipalPrivateBindingFor(ctx, scope, scope.PrincipalID, prRole); found && b.EndpointID == ep.EndpointID {
		model = b.ModelID
	}
	return ep, model, true, nil
}

func providerRoleToPrincipalRole(role providers.Role) (providers.PrincipalLlmRole, bool) {
	switch role {
	case providers.RoleConversationalController:
		return providers.PrincipalLlmRoleConversationalController, true
	case providers.RoleLanguageRealization:
		return providers.PrincipalLlmRoleLanguageRealization, true
	case providers.RoleSTT:
		return providers.PrincipalLlmRoleSTT, true
	case providers.RoleTTS:
		return providers.PrincipalLlmRoleTTS, true
	}
	return "", false
}

// ProfileIDForPrivateEndpoint returns the synthetic profile id for endpoint when present.
func (r *Registry) ProfileIDForPrivateEndpoint(ctx context.Context, scope contracts.Scope, id providers.PrivateEndpointID) (providers.ProfileID, bool, error) {
	ep, ok, err := r.GetPrivateEndpoint(ctx, scope, id)
	if err != nil || !ok {
		return "", ok, err
	}
	return ep.ProfileID, true, nil
}

// CheckPrivateProfile runs capability gates for a synthesized self-hosted profile.
func (r *Registry) CheckPrivateProfile(ctx context.Context, scope contracts.Scope, p providers.ProviderProfile, req Requirement) (Admission, error) {
	if p.ProviderID != ProviderIDSelfHosted || p.Lifecycle != providers.ProfileLifecycleQualified {
		return Admission{}, fail(contracts.ErrPermissionDenied, ReasonNotQualified, "the private profile is not ready")
	}
	if !permits(p.PermittedScopes, scope) {
		return Admission{}, fail(contracts.ErrPermissionDenied, ReasonScopeNotPermitted, "the provider profile is not permitted for this application, tenant and workspace")
	}
	if !classAllowed(p, req.DataClass) {
		return Admission{}, fail(contracts.ErrPermissionDenied, ReasonDataClassDenied, "the provider profile may not receive this data class")
	}
	if missing := gateCapabilities(p, req); len(missing) > 0 {
		return Admission{}, fail(contracts.ErrValidationFailed, ReasonCapability, "the provider profile cannot serve this request").
			WithDetails("reasons", reasonStrings(missing))
	}
	if p.CredRef != nil && validCredRef(*p.CredRef) {
		if err := r.credentialUsable(ctx, scope, p); err != nil {
			return Admission{}, err
		}
	}
	return Admission{Profile: p, passed: true}, nil
}
