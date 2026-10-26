package providerreg

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

const ReasonCatalogNotFound Reason = "catalog_not_found"
const ReasonCorrupt Reason = "corrupt"

// reservedPrincipalCredentialTenant mirrors the ADR 0041 sentinel (also in internal/credentials).
// Duplicated here so providerreg does not import credentials (import tier).
const reservedPrincipalCredentialTenant = "alixai-principal-credential"

// CatalogEntryInput is what an operator supplies when marking a profile for the principal catalog.
type CatalogEntryInput struct {
	ProfileID            providers.ProfileID
	DisplayName          string
	TermsSummary         string
	BillingNoticeVersion string
	Roles                []providers.PrincipalLlmRole
	ModelIDs             []providers.ModelID
	PriceHint            *providers.ProviderCatalogEntryPriceHint
	OAuthAvailable       bool
}

// CatalogAdd registers or updates a catalog entry for a qualified profile (ADR 0041).
func (r *Registry) CatalogAdd(ctx context.Context, scope contracts.Scope, in CatalogEntryInput) (providers.ProviderCatalogEntry, error) {
	if _, err := appOf(scope); err != nil {
		return providers.ProviderCatalogEntry{}, err
	}
	if err := refuseReservedTenantID(scope.TenantID); err != nil {
		return providers.ProviderCatalogEntry{}, err
	}
	p, err := r.Get(ctx, scope, in.ProfileID)
	if err != nil {
		return providers.ProviderCatalogEntry{}, err
	}
	if p.Lifecycle != providers.ProfileLifecycleQualified {
		return providers.ProviderCatalogEntry{}, fail(contracts.ErrValidationFailed, ReasonNotQualified, "only a qualified profile may enter the catalog")
	}
	if in.DisplayName == "" {
		if p.DisplayName != nil && *p.DisplayName != "" {
			in.DisplayName = *p.DisplayName
		} else {
			in.DisplayName = string(p.ProviderID) + " " + string(p.ModelID)
		}
	}
	if in.TermsSummary == "" {
		in.TermsSummary = "See the operator billing notice and provider terms for this profile."
	}
	if in.BillingNoticeVersion == "" {
		in.BillingNoticeVersion = p.BillingNotice.Version
	}
	if len(in.Roles) == 0 {
		in.Roles = principalRolesOf(p)
	}
	if len(in.Roles) == 0 {
		return providers.ProviderCatalogEntry{}, invalid("the profile has no principal-bindable role")
	}
	if len(in.ModelIDs) == 0 {
		in.ModelIDs = []providers.ModelID{p.ModelID}
	}
	rolesJSON, err := json.Marshal(in.Roles)
	if err != nil {
		return providers.ProviderCatalogEntry{}, fail(contracts.ErrInternal, ReasonCorrupt, "catalog roles could not be encoded")
	}
	modelsJSON, err := json.Marshal(in.ModelIDs)
	if err != nil {
		return providers.ProviderCatalogEntry{}, fail(contracts.ErrInternal, ReasonCorrupt, "catalog models could not be encoded")
	}
	var priceJSON sql.NullString
	if in.PriceHint != nil {
		b, err := json.Marshal(in.PriceHint)
		if err != nil {
			return providers.ProviderCatalogEntry{}, fail(contracts.ErrInternal, ReasonCorrupt, "catalog price hint could not be encoded")
		}
		priceJSON = sql.NullString{String: string(b), Valid: true}
	}
	now := r.clk.Now().UnixMilli()
	err = r.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO providerreg_catalog
			  (application_id, tenant_id, workspace_id, profile_id, display_name, terms_summary,
			   billing_notice_version, roles_json, model_ids_json, price_hint_json, oauth_available, created_at, updated_at)
			VALUES (?, '', '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (application_id, tenant_id, workspace_id, profile_id) DO UPDATE SET
			  display_name = excluded.display_name,
			  terms_summary = excluded.terms_summary,
			  billing_notice_version = excluded.billing_notice_version,
			  roles_json = excluded.roles_json,
			  model_ids_json = excluded.model_ids_json,
			  price_hint_json = excluded.price_hint_json,
			  oauth_available = excluded.oauth_available,
			  updated_at = excluded.updated_at`,
			scope.ApplicationID, string(in.ProfileID), in.DisplayName, in.TermsSummary, in.BillingNoticeVersion,
			string(rolesJSON), string(modelsJSON), priceJSON, b2i(in.OAuthAvailable), now, now)
		return wrapStore(err, "catalog entry could not be stored")
	})
	if err != nil {
		return providers.ProviderCatalogEntry{}, err
	}
	return r.catalogEntryOf(p, in)
}

// CatalogRemove drops a catalog entry. Bindings that still name it fail at Put/resolve time.
func (r *Registry) CatalogRemove(ctx context.Context, scope contracts.Scope, id providers.ProfileID) error {
	if _, err := appOf(scope); err != nil {
		return err
	}
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM providerreg_catalog WHERE `+appWhere+` AND profile_id = ?`,
		scope.ApplicationID, string(id))
	if err != nil {
		return wrapStore(err, "catalog entry could not be removed")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fail(contracts.ErrValidationFailed, ReasonCatalogNotFound, "no such catalog entry")
	}
	return nil
}

// Catalog lists operator-approved entries the principal may pick. No secrets. Only profiles that
// are still qualified appear as qualified=true; others are omitted so a principal cannot bind them.
func (r *Registry) Catalog(ctx context.Context, scope contracts.Scope) (providers.ProviderCatalog, error) {
	if _, err := appOf(scope); err != nil {
		return providers.ProviderCatalog{}, err
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT profile_id, display_name, terms_summary, billing_notice_version, roles_json, model_ids_json,
		       price_hint_json, oauth_available
		  FROM providerreg_catalog WHERE `+appWhere+` ORDER BY seq`, scope.ApplicationID)
	if err != nil {
		return providers.ProviderCatalog{}, wrapStore(err, "catalog could not be listed")
	}
	defer rows.Close()
	out := providers.ProviderCatalog{Entries: []providers.ProviderCatalogEntry{}}
	for rows.Next() {
		var (
			profileID, display, terms, notice, rolesJSON, modelsJSON string
			priceJSON                                                sql.NullString
			oauth                                                    int
		)
		if err := rows.Scan(&profileID, &display, &terms, &notice, &rolesJSON, &modelsJSON, &priceJSON, &oauth); err != nil {
			return providers.ProviderCatalog{}, wrapStore(err, "catalog could not be listed")
		}
		p, err := r.Get(ctx, scope, providers.ProfileID(profileID))
		if err != nil || p.Lifecycle != providers.ProfileLifecycleQualified {
			continue
		}
		var roles []providers.PrincipalLlmRole
		var models []providers.ModelID
		if err := json.Unmarshal([]byte(rolesJSON), &roles); err != nil {
			return providers.ProviderCatalog{}, fail(contracts.ErrInternal, ReasonCorrupt, "catalog roles could not be decoded")
		}
		if err := json.Unmarshal([]byte(modelsJSON), &models); err != nil {
			return providers.ProviderCatalog{}, fail(contracts.ErrInternal, ReasonCorrupt, "catalog models could not be decoded")
		}
		oauthAvail := oauth == 1
		e := providers.ProviderCatalogEntry{
			ProfileID:            p.ProfileID,
			ProviderID:           p.ProviderID,
			ModelID:              p.ModelID,
			ModelIDs:             models,
			Roles:                roles,
			AccountMode:          "per_principal",
			DisplayName:          display,
			TermsSummary:         terms,
			BillingNoticeVersion: notice,
			Qualified:            true,
			OauthAvailable:       &oauthAvail,
		}
		if priceJSON.Valid && priceJSON.String != "" {
			var hint providers.ProviderCatalogEntryPriceHint
			if err := json.Unmarshal([]byte(priceJSON.String), &hint); err != nil {
				return providers.ProviderCatalog{}, fail(contracts.ErrInternal, ReasonCorrupt, "catalog price hint could not be decoded")
			}
			e.PriceHint = &hint
		}
		out.Entries = append(out.Entries, e)
	}
	return out, wrapStore(rows.Err(), "catalog could not be listed")
}

// CatalogEntry returns one catalog entry for profile id, or found=false.
func (r *Registry) CatalogEntry(ctx context.Context, scope contracts.Scope, id providers.ProfileID) (providers.ProviderCatalogEntry, bool, error) {
	cat, err := r.Catalog(ctx, scope)
	if err != nil {
		return providers.ProviderCatalogEntry{}, false, err
	}
	for _, e := range cat.Entries {
		if e.ProfileID == id {
			return e, true, nil
		}
	}
	return providers.ProviderCatalogEntry{}, false, nil
}

func (r *Registry) catalogEntryOf(p providers.ProviderProfile, in CatalogEntryInput) (providers.ProviderCatalogEntry, error) {
	oauth := in.OAuthAvailable
	return providers.ProviderCatalogEntry{
		ProfileID:            p.ProfileID,
		ProviderID:           p.ProviderID,
		ModelID:              p.ModelID,
		ModelIDs:             in.ModelIDs,
		Roles:                in.Roles,
		AccountMode:          "per_principal",
		DisplayName:          in.DisplayName,
		TermsSummary:         in.TermsSummary,
		BillingNoticeVersion: in.BillingNoticeVersion,
		PriceHint:            in.PriceHint,
		Qualified:            p.Lifecycle == providers.ProfileLifecycleQualified,
		OauthAvailable:       &oauth,
	}, nil
}

func principalRolesOf(p providers.ProviderProfile) []providers.PrincipalLlmRole {
	var out []providers.PrincipalLlmRole
	for _, role := range p.Roles {
		switch role {
		case providers.RoleConversationalController:
			out = append(out, providers.PrincipalLlmRoleConversationalController)
		case providers.RoleLanguageRealization:
			out = append(out, providers.PrincipalLlmRoleLanguageRealization)
		case providers.RoleSTT:
			out = append(out, providers.PrincipalLlmRoleSTT)
		}
	}
	return out
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func refuseReservedTenantID(tenant string) error {
	if tenant != reservedPrincipalCredentialTenant {
		return nil
	}
	return contracts.New(contracts.ErrScopeViolation, "providerreg: the reserved principal-credential tenant is not a request scope").
		WithDetails("reason", "reserved_tenant")
}

// refuseReservedScopeValue refuses the reserved tenant wherever a tenant id is configured.
func refuseReservedScopeValue(tenant *common.Identifier) error {
	if tenant == nil {
		return nil
	}
	return refuseReservedTenantID(string(*tenant))
}
