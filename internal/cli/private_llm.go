//go:build !noexternal

package cli

import (
	"context"

	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

type privateLLMService struct {
	admin *externalAdmin
	reg   *providerreg.Registry
}

func newPrivateLLMService(a *externalAdmin) *privateLLMService {
	return &privateLLMService{admin: a, reg: a.reg}
}

func (s *privateLLMService) AuthorizeRead(ctx context.Context, scope contracts.Scope) error {
	if s.admin == nil {
		return contracts.New(contracts.ErrPermissionDenied, "private LLM ops require a deployment operator")
	}
	if err := s.admin.AuthorizeOperator(ctx, scope, gateway.ProvidersOperatorDeployment); err == nil {
		return nil
	}
	return contracts.New(contracts.ErrPermissionDenied, "private LLM read is for deployment operators")
}

func (s *privateLLMService) AuthorizeWrite(ctx context.Context, scope contracts.Scope, tenantID string) error {
	if s.admin == nil {
		return contracts.New(contracts.ErrPermissionDenied, "private LLM ops require a deployment operator")
	}
	if err := s.admin.AuthorizeOperator(ctx, scope, gateway.ProvidersOperatorDeployment); err == nil {
		return nil
	}
	if tenantID != "" && tenantID == scope.TenantID {
		if err := s.admin.AuthorizeOperator(ctx, scope, gateway.ProvidersOperatorTenant); err == nil {
			return nil
		}
	}
	return contracts.New(contracts.ErrPermissionDenied, "private LLM write is for deployment operators or the tenant's administrator")
}

func (s *privateLLMService) PutEndpoint(ctx context.Context, scope contracts.Scope, id pv.PrivateEndpointID, in pv.PrivateLlmEndpointUpdate) (pv.PrivateLlmEndpoint, error) {
	ep, err := s.reg.PutPrivateEndpoint(ctx, scope, id, in)
	if err != nil {
		return pv.PrivateLlmEndpoint{}, err
	}
	s.auditEndpoint(ctx, scope, "upsert", id, string(ep.Lifecycle))
	s.startHealth(scope, id, ep.Origin, ep.HealthURL)
	return ep, nil
}

func (s *privateLLMService) GetEndpoint(ctx context.Context, scope contracts.Scope, id pv.PrivateEndpointID) (pv.PrivateLlmEndpoint, error) {
	ep, ok, err := s.reg.GetPrivateEndpoint(ctx, scope, id)
	if err != nil {
		return pv.PrivateLlmEndpoint{}, err
	}
	if !ok {
		return pv.PrivateLlmEndpoint{}, contracts.New(contracts.ErrValidationFailed, "unknown private endpoint").
			WithDetails("reason", "not_found")
	}
	return ep.ToContract(), nil
}

func (s *privateLLMService) DeleteEndpoint(ctx context.Context, scope contracts.Scope, id pv.PrivateEndpointID) error {
	s.stopHealth(id)
	if err := s.reg.DeletePrivateEndpoint(ctx, scope, id); err != nil {
		return err
	}
	s.auditEndpoint(ctx, scope, "delete", id, "")
	return nil
}

func (s *privateLLMService) PutTenantPin(ctx context.Context, scope contracts.Scope, tenantID string, in pv.PrivateLlmTenantPinUpdate) (pv.PrivateLlmTenantPin, error) {
	pin, err := s.reg.PutTenantPrivateLLMPin(ctx, scope, tenantID, in.EndpointID)
	if err != nil {
		return pv.PrivateLlmTenantPin{}, err
	}
	s.auditPin(ctx, scope, "pin", tenantID, in.EndpointID)
	return pin, nil
}

func (s *privateLLMService) GetTenantPin(ctx context.Context, scope contracts.Scope, tenantID string) (pv.PrivateLlmTenantPin, error) {
	_, ok, err := s.reg.GetTenantPrivateLLMPin(ctx, scope, tenantID)
	if err != nil {
		return pv.PrivateLlmTenantPin{}, err
	}
	if !ok {
		return pv.PrivateLlmTenantPin{}, contracts.New(contracts.ErrValidationFailed, "no tenant private LLM pin").
			WithDetails("reason", "not_found")
	}
	return s.reg.TenantPrivateLLMPinView(ctx, scope, tenantID)
}

func (s *privateLLMService) DeleteTenantPin(ctx context.Context, scope contracts.Scope, tenantID string) error {
	if err := s.reg.DeleteTenantPrivateLLMPin(ctx, scope, tenantID); err != nil {
		return err
	}
	s.auditPin(ctx, scope, "unpin", tenantID, "")
	return nil
}

func (s *privateLLMService) PutPrincipalBinding(ctx context.Context, scope contracts.Scope, principalID string, in pv.PrivateLlmPrincipalBindingUpdate) (pv.PrivateLlmPrincipalBinding, error) {
	b, err := s.reg.PutPrincipalPrivateBindings(ctx, scope, principalID, in.EndpointID, in.Roles)
	if err != nil {
		return pv.PrivateLlmPrincipalBinding{}, err
	}
	s.auditBinding(ctx, scope, "bound", principalID, in.EndpointID)
	return b, nil
}

func (s *privateLLMService) GetPrincipalBinding(ctx context.Context, scope contracts.Scope, principalID string) (pv.PrivateLlmPrincipalBinding, error) {
	b, ok, err := s.reg.GetPrincipalPrivateBinding(ctx, scope, principalID)
	if err != nil {
		return pv.PrivateLlmPrincipalBinding{}, err
	}
	if !ok {
		return pv.PrivateLlmPrincipalBinding{}, contracts.New(contracts.ErrValidationFailed, "no principal private LLM binding").
			WithDetails("reason", "not_found")
	}
	return b, nil
}

func (s *privateLLMService) DeletePrincipalBinding(ctx context.Context, scope contracts.Scope, principalID string) error {
	if err := s.reg.DeletePrincipalPrivateBindings(ctx, scope, principalID); err != nil {
		return err
	}
	s.auditBinding(ctx, scope, "cleared", principalID, "")
	return nil
}

var _ gateway.PrivateLLMService = (*privateLLMService)(nil)
