package settings

import (
	"context"

	"github.com/buxee/alix-ai/pkg/contracts"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// Sources supplies deployment config, package assistant and model capability for Resolve.
type Sources interface {
	DeploymentConfig(ctx context.Context, scope contracts.Scope) (an.Config, error)
	ApplicationAssistant(ctx context.Context, scope contracts.Scope) (*applicationpackage.ManifestAssistant, error)
	ControllerAnalystCapable(ctx context.Context, scope contracts.Scope) (bool, error)
	WebConfigured(ctx context.Context, scope contracts.Scope) (bool, error)
}

// Authorizer decides whether the caller may read/write another tenant's settings.
type Authorizer interface {
	AuthorizeTenantRead(ctx context.Context, scope contracts.Scope, tenantID string) error
	AuthorizeTenantWrite(ctx context.Context, scope contracts.Scope, tenantID string) error
}

// DefaultAuthorizer: same-tenant with capability already checked by gateway, or
// any tenant when scope.TenantID matches, else permission_denied. Deployment-wide
// operators are recognised when PrincipalID equals OperatorPrincipal (optional).
type DefaultAuthorizer struct {
	// OperatorPrincipals may read/write any tenant of the application.
	OperatorPrincipals map[string]bool
}

func (a DefaultAuthorizer) AuthorizeTenantRead(_ context.Context, scope contracts.Scope, tenantID string) error {
	return a.allow(scope, tenantID)
}

func (a DefaultAuthorizer) AuthorizeTenantWrite(_ context.Context, scope contracts.Scope, tenantID string) error {
	return a.allow(scope, tenantID)
}

func (a DefaultAuthorizer) allow(scope contracts.Scope, tenantID string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if a.OperatorPrincipals[scope.PrincipalID] {
		return nil
	}
	if tenantID == scope.TenantID {
		return nil
	}
	return contracts.New(contracts.ErrPermissionDenied, "analyst: tenant settings are only readable/writable for the caller's tenant or a deployment operator")
}

// Service is the analyst settings surface wired to gateway.AnalystService.
type Service struct {
	Store  *Store
	Auth   Authorizer
	Source Sources
}

// AuthorizeTenantRead delegates to Auth.
func (s *Service) AuthorizeTenantRead(ctx context.Context, scope contracts.Scope, tenantID string) error {
	if s.Auth == nil {
		return contracts.New(contracts.ErrPermissionDenied, "analyst: authorizer is not wired")
	}
	return s.Auth.AuthorizeTenantRead(ctx, scope, tenantID)
}

// AuthorizeTenantWrite delegates to Auth.
func (s *Service) AuthorizeTenantWrite(ctx context.Context, scope contracts.Scope, tenantID string) error {
	if s.Auth == nil {
		return contracts.New(contracts.ErrPermissionDenied, "analyst: authorizer is not wired")
	}
	return s.Auth.AuthorizeTenantWrite(ctx, scope, tenantID)
}

// GetTenant reads stored ceilings.
func (s *Service) GetTenant(ctx context.Context, scope contracts.Scope, tenantID string) (an.TenantSettings, error) {
	return s.Store.GetTenant(ctx, scope, tenantID)
}

// PutTenant writes after refusing any widen above deployment/application ceilings.
func (s *Service) PutTenant(ctx context.Context, scope contracts.Scope, tenantID string, in an.TenantSettings) (an.TenantSettings, error) {
	dep, err := s.Source.DeploymentConfig(ctx, scope)
	if err != nil {
		return an.TenantSettings{}, err
	}
	app, err := s.Source.ApplicationAssistant(ctx, scope)
	if err != nil {
		return an.TenantSettings{}, err
	}
	if bad := TenantWidens(in, dep, app); len(bad) > 0 {
		return an.TenantSettings{}, contracts.New(contracts.ErrPermissionDenied, "analyst: tenant may only narrow abilities").
			WithDetails("widened", string(bad[0]))
	}
	return s.Store.PutTenant(ctx, scope, tenantID, in)
}

// Effective resolves the caller's effective ability set.
func (s *Service) Effective(ctx context.Context, scope contracts.Scope) (an.Abilities, error) {
	if err := scope.Validate(); err != nil {
		return an.Abilities{}, err
	}
	dep, err := s.Source.DeploymentConfig(ctx, scope)
	if err != nil {
		return an.Abilities{}, err
	}
	app, err := s.Source.ApplicationAssistant(ctx, scope)
	if err != nil {
		return an.Abilities{}, err
	}
	ten, err := s.Store.GetTenant(ctx, scope, scope.TenantID)
	if err != nil {
		return an.Abilities{}, err
	}
	capable, err := s.Source.ControllerAnalystCapable(ctx, scope)
	if err != nil {
		return an.Abilities{}, err
	}
	webOK, err := s.Source.WebConfigured(ctx, scope)
	if err != nil {
		return an.Abilities{}, err
	}
	domain := app != nil && app.Domain != nil && app.Domain.Description != "" && len(app.Domain.InScope) > 0
	return AbilityResolver{}.Resolve(ResolveInput{
		Deployment:     dep,
		Application:    app,
		Tenant:         ten,
		AnalystCapable: capable,
		WebConfigured:  webOK,
		DomainDeclared: domain,
		Now:            s.Store.clk.Now(),
	}), nil
}
