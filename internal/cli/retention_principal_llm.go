//go:build !noexternal

package cli

import (
	"context"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// retentionErasePrincipalLLM revokes principal credentials and deletes bindings (ADR 0041).
func retentionErasePrincipalLLM(ctx context.Context, e *retentionEngine, application, principal string) (creds, bindings int, err error) {
	dep := identity.CurrentConfig().DeploymentID
	if dep == "" {
		dep = "dep_retention"
	}
	scope := contracts.Scope{
		DeploymentID: dep, ApplicationID: application, TenantID: "ten_erase",
		WorkspaceID: audit.OperatorWorkspace, PrincipalID: principal,
		Roles: []string{"operator"}, CapabilityScopes: []string{"retention"}, SessionID: "ses_erase",
	}
	if principalLLMHolder != nil {
		return ErasePrincipalLLM(ctx, principalLLMHolder, scope, principal)
	}
	if e == nil || e.db == nil {
		return 0, 0, nil
	}
	reg, rerr := providerreg.New(e.db, providerreg.Config{Clock: e.clk, Credentials: retentionCredStub{}})
	if rerr != nil {
		return 0, 0, rerr
	}
	n, err := reg.ErasePrincipalBindings(ctx, scope, principal)
	return 0, n, err
}

type retentionCredStub struct{}

func (retentionCredStub) CredentialStatus(context.Context, contracts.Scope, pv.CredentialRefID) (pv.CredentialRefStatus, error) {
	return pv.CredentialRefStatusRevoked, nil
}
