package gateway

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

type opsPrivateLLMFake struct {
	mu sync.Mutex
}

func (f *opsPrivateLLMFake) AuthorizeRead(_ context.Context, _ contracts.Scope) error { return nil }
func (f *opsPrivateLLMFake) AuthorizeWrite(_ context.Context, _ contracts.Scope, _ string) error {
	return nil
}
func (f *opsPrivateLLMFake) PutEndpoint(_ context.Context, _ contracts.Scope, id pv.PrivateEndpointID, _ pv.PrivateLlmEndpointUpdate) (pv.PrivateLlmEndpoint, error) {
	return pv.PrivateLlmEndpoint{
		EndpointID: id, Origin: "http://127.0.0.1:1", APIDialect: "openai-compatible",
		ModelIDs: []pv.ModelID{"m1"}, Roles: []pv.PrincipalLlmRole{pv.PrincipalLlmRoleConversationalController},
		OwnerKind: pv.PrivateEndpointOwnerKindShared, Lifecycle: pv.PrivateLlmLifecycleReady,
		UpdatedAt: "2026-01-01T00:00:00.000Z",
	}, nil
}
func (f *opsPrivateLLMFake) GetEndpoint(_ context.Context, _ contracts.Scope, id pv.PrivateEndpointID) (pv.PrivateLlmEndpoint, error) {
	return f.PutEndpoint(context.Background(), contracts.Scope{}, id, pv.PrivateLlmEndpointUpdate{})
}
func (f *opsPrivateLLMFake) DeleteEndpoint(context.Context, contracts.Scope, pv.PrivateEndpointID) error {
	return nil
}
func (f *opsPrivateLLMFake) PutTenantPin(_ context.Context, _ contracts.Scope, tenant string, in pv.PrivateLlmTenantPinUpdate) (pv.PrivateLlmTenantPin, error) {
	return pv.PrivateLlmTenantPin{EndpointID: in.EndpointID}, nil
}
func (f *opsPrivateLLMFake) GetTenantPin(context.Context, contracts.Scope, string) (pv.PrivateLlmTenantPin, error) {
	return pv.PrivateLlmTenantPin{}, contracts.New(contracts.ErrValidationFailed, "missing").WithDetails("reason", "not_found")
}
func (f *opsPrivateLLMFake) DeleteTenantPin(context.Context, contracts.Scope, string) error {
	return nil
}
func (f *opsPrivateLLMFake) PutPrincipalBinding(context.Context, contracts.Scope, string, pv.PrivateLlmPrincipalBindingUpdate) (pv.PrivateLlmPrincipalBinding, error) {
	return pv.PrivateLlmPrincipalBinding{}, nil
}
func (f *opsPrivateLLMFake) GetPrincipalBinding(context.Context, contracts.Scope, string) (pv.PrivateLlmPrincipalBinding, error) {
	return pv.PrivateLlmPrincipalBinding{}, nil
}
func (f *opsPrivateLLMFake) DeletePrincipalBinding(context.Context, contracts.Scope, string) error {
	return nil
}

func init() {
	registerTestServices("v1_ops_private_llm", func(t testing.TB, e *apiEnv, s *Services) {
		f := &opsPrivateLLMFake{}
		if err := Provide[PrivateLLMService](s, f); err != nil {
			t.Fatal(err)
		}
	})
}

func TestOpsPrivateLLMRoutesRegistered(t *testing.T) {
	e := newAPIEnv(t)
	routes := map[string]bool{}
	for _, r := range e.srv.AreaRoutes("v1_ops_private_llm") {
		routes[r] = true
	}
	for _, r := range []string{
		"PUT /v1/ops/private-llm/endpoints/{endpoint_id}",
		"GET /v1/ops/private-llm/endpoints/{endpoint_id}",
	} {
		if !routes[r] {
			t.Errorf("missing %s (have %v)", r, e.srv.AreaRoutes("v1_ops_private_llm"))
		}
	}
}

func TestOpsPrivateLLMPutEndpoint(t *testing.T) {
	e := newAPIEnv(t)
	caps := append(append([]string{}, defaultCaps...), PrivateLLMCapabilityWrite)
	rec := e.call(http.MethodPut, "/v1/ops/private-llm/endpoints/ple_01J0000000000000000000000A", map[string]any{
		"origin": "http://127.0.0.1:8099", "api_dialect": "openai-compatible",
		"model_ids": []string{"m1"}, "roles": []string{"conversational_controller"}, "owner_kind": "shared",
	}, withScopes(caps...))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
}
