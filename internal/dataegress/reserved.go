package dataegress

import "github.com/buxee/alix-ai/pkg/contracts"

// reservedPrincipalCredentialTenant is the ADR 0041 storage bucket; never a policy scope.
const reservedPrincipalCredentialTenant = "alixai-principal-credential"

func refuseReservedTenant(tenant string) error {
	if tenant != reservedPrincipalCredentialTenant {
		return nil
	}
	return contracts.New(contracts.ErrScopeViolation, "dataegress: the reserved principal-credential tenant is not a request scope").
		WithDetails("reason", "reserved_tenant")
}
