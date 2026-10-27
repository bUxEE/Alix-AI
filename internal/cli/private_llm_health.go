//go:build !noexternal

package cli

import (
	"context"
	"sync"

	"github.com/buxee/alix-ai/adapters/external/selfhosted"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

var (
	privateLLMHealthMu     sync.Mutex
	privateLLMHealthCancel = map[pv.PrivateEndpointID]context.CancelFunc{}

	privateLLMEventEndpoint = audit.MustRegister[privateLLMEndpointRec](audit.TypeSpec{
		Name:        "providers.private_llm_endpoint",
		Description: "a private LLM endpoint was registered, updated, removed or changed lifecycle (ADR 0045); never a secret",
		Levels:      audit.LevelTenant,
		Required:    []string{"action", "endpoint_id"},
	})
	privateLLMEventPin = audit.MustRegister[privateLLMPinRec](audit.TypeSpec{
		Name:        "providers.private_llm_tenant_pin",
		Description: "a tenant was pinned to or unpinned from a private LLM endpoint (ADR 0045)",
		Levels:      audit.LevelTenant,
		Required:    []string{"action", "tenant_id", "endpoint_id"},
	})
	privateLLMEventBinding = audit.MustRegister[privateLLMBindingRec](audit.TypeSpec{
		Name:        "providers.private_llm_principal_binding",
		Description: "a principal private LLM binding was set or cleared (ADR 0045)",
		Levels:      audit.LevelTenant,
		Required:    []string{"action", "principal_id", "endpoint_id"},
	})
)

type privateLLMEndpointRec struct {
	Action     string `json:"action"`
	EndpointID string `json:"endpoint_id"`
	Lifecycle  string `json:"lifecycle,omitempty"`
}

type privateLLMPinRec struct {
	Action     string `json:"action"`
	TenantID   string `json:"tenant_id"`
	EndpointID string `json:"endpoint_id"`
}

type privateLLMBindingRec struct {
	Action      string `json:"action"`
	PrincipalID string `json:"principal_id"`
	EndpointID  string `json:"endpoint_id"`
}

func (s *privateLLMService) startHealth(scope contracts.Scope, id pv.PrivateEndpointID, baseURL string, healthURL *string) {
	if s.reg == nil {
		return
	}
	privateLLMHealthMu.Lock()
	if cancel, ok := privateLLMHealthCancel[id]; ok {
		cancel()
		delete(privateLLMHealthCancel, id)
	}
	ctx, cancel := context.WithCancel(context.Background())
	privateLLMHealthCancel[id] = cancel
	privateLLMHealthMu.Unlock()

	hu := ""
	if healthURL != nil {
		hu = *healthURL
	}
	appScope := scope
	go (&selfhosted.Checker{
		Set: func(ctx context.Context, ready bool) error {
			lc := pv.PrivateLlmLifecycleFailed
			if ready {
				lc = pv.PrivateLlmLifecycleReady
			}
			if err := s.reg.SetPrivateEndpointLifecycle(ctx, appScope, id, lc); err != nil {
				return err
			}
			s.auditEndpoint(ctx, appScope, "lifecycle", id, string(lc))
			return nil
		},
	}).Run(ctx, hu, baseURL)
}

func (s *privateLLMService) stopHealth(id pv.PrivateEndpointID) {
	privateLLMHealthMu.Lock()
	defer privateLLMHealthMu.Unlock()
	if cancel, ok := privateLLMHealthCancel[id]; ok {
		cancel()
		delete(privateLLMHealthCancel, id)
	}
}

func (s *privateLLMService) auditEndpoint(ctx context.Context, scope contracts.Scope, action string, id pv.PrivateEndpointID, lifecycle string) {
	if s.admin == nil || s.admin.audit == nil {
		return
	}
	loc, err := audit.ForScope(scope)
	if err != nil {
		return
	}
	_, _ = privateLLMEventEndpoint.Append(ctx, s.admin.audit, loc, privateLLMEndpointRec{
		Action: action, EndpointID: string(id), Lifecycle: lifecycle,
	})
}

func (s *privateLLMService) auditPin(ctx context.Context, scope contracts.Scope, action, tenant string, id pv.PrivateEndpointID) {
	if s.admin == nil || s.admin.audit == nil {
		return
	}
	loc, err := audit.ForScope(scope)
	if err != nil {
		return
	}
	_, _ = privateLLMEventPin.Append(ctx, s.admin.audit, loc, privateLLMPinRec{
		Action: action, TenantID: tenant, EndpointID: string(id),
	})
}

func (s *privateLLMService) auditBinding(ctx context.Context, scope contracts.Scope, action, principal string, id pv.PrivateEndpointID) {
	if s.admin == nil || s.admin.audit == nil {
		return
	}
	loc, err := audit.ForScope(scope)
	if err != nil {
		return
	}
	_, _ = privateLLMEventBinding.Append(ctx, s.admin.audit, loc, privateLLMBindingRec{
		Action: action, PrincipalID: principal, EndpointID: string(id),
	})
}
