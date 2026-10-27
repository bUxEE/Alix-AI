package sdkgo

import (
	"context"
	"encoding/json"

	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// PrivateLLMOps is the typed private-LLM operator surface (1.5.0, ADR 0045).
type PrivateLLMOps struct{ c *Client }

// PrivateLLM returns the typed private-LLM sub-client.
func (o *OpsClient) PrivateLLM() *PrivateLLMOps { return &PrivateLLMOps{c: o.c} }

// PutEndpoint registers or updates a private self-hosted endpoint.
func (p *PrivateLLMOps) PutEndpoint(ctx context.Context, endpointID string, body pv.PrivateLlmEndpointUpdate, options ...CallOption) (*pv.PrivateLlmEndpoint, error) {
	if endpointID == "" {
		return nil, invalidArgument("endpointID", "it is required")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req := request{op: opPutPrivateLlmEndpoint, params: map[string]string{"endpoint_id": endpointID}, accept: "application/json", contentType: "application/json", body: raw}
	return doJSON(ctx, p.c.t, req, applyOptions(options).deadline(p.c.timeout), func(*pv.PrivateLlmEndpoint) error { return nil })
}

// GetEndpoint reads a private endpoint.
func (p *PrivateLLMOps) GetEndpoint(ctx context.Context, endpointID string, options ...CallOption) (*pv.PrivateLlmEndpoint, error) {
	if endpointID == "" {
		return nil, invalidArgument("endpointID", "it is required")
	}
	req := request{op: opGetPrivateLlmEndpoint, params: map[string]string{"endpoint_id": endpointID}, accept: "application/json"}
	return doJSON(ctx, p.c.t, req, applyOptions(options).deadline(p.c.timeout), func(*pv.PrivateLlmEndpoint) error { return nil })
}

// DeleteEndpoint removes a private endpoint.
func (p *PrivateLLMOps) DeleteEndpoint(ctx context.Context, endpointID string, options ...CallOption) error {
	if endpointID == "" {
		return invalidArgument("endpointID", "it is required")
	}
	req := request{op: opDeletePrivateLlmEndpoint, params: map[string]string{"endpoint_id": endpointID}}
	return doEmpty(ctx, p.c.t, req, applyOptions(options).deadline(p.c.timeout))
}

// PutTenantPin pins a tenant to a private endpoint.
func (p *PrivateLLMOps) PutTenantPin(ctx context.Context, tenantID string, body pv.PrivateLlmTenantPinUpdate, options ...CallOption) (*pv.PrivateLlmTenantPin, error) {
	if tenantID == "" {
		return nil, invalidArgument("tenantID", "it is required")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req := request{op: opPutTenantPrivateLlm, params: map[string]string{"tenant_id": tenantID}, accept: "application/json", contentType: "application/json", body: raw}
	return doJSON(ctx, p.c.t, req, applyOptions(options).deadline(p.c.timeout), func(*pv.PrivateLlmTenantPin) error { return nil })
}

// GetTenantPin reads a tenant pin.
func (p *PrivateLLMOps) GetTenantPin(ctx context.Context, tenantID string, options ...CallOption) (*pv.PrivateLlmTenantPin, error) {
	if tenantID == "" {
		return nil, invalidArgument("tenantID", "it is required")
	}
	req := request{op: opGetTenantPrivateLlm, params: map[string]string{"tenant_id": tenantID}, accept: "application/json"}
	return doJSON(ctx, p.c.t, req, applyOptions(options).deadline(p.c.timeout), func(*pv.PrivateLlmTenantPin) error { return nil })
}

// DeleteTenantPin clears a tenant pin.
func (p *PrivateLLMOps) DeleteTenantPin(ctx context.Context, tenantID string, options ...CallOption) error {
	if tenantID == "" {
		return invalidArgument("tenantID", "it is required")
	}
	req := request{op: opDeleteTenantPrivateLlm, params: map[string]string{"tenant_id": tenantID}}
	return doEmpty(ctx, p.c.t, req, applyOptions(options).deadline(p.c.timeout))
}

// PutPrincipalBinding activates a principal's private LLM binding.
func (p *PrivateLLMOps) PutPrincipalBinding(ctx context.Context, principalID string, body pv.PrivateLlmPrincipalBindingUpdate, options ...CallOption) (*pv.PrivateLlmPrincipalBinding, error) {
	if principalID == "" {
		return nil, invalidArgument("principalID", "it is required")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req := request{op: opPutPrincipalPrivateLlm, params: map[string]string{"principal_id": principalID}, accept: "application/json", contentType: "application/json", body: raw}
	return doJSON(ctx, p.c.t, req, applyOptions(options).deadline(p.c.timeout), func(*pv.PrivateLlmPrincipalBinding) error { return nil })
}

// GetPrincipalBinding reads a principal's operator private LLM binding.
func (p *PrivateLLMOps) GetPrincipalBinding(ctx context.Context, principalID string, options ...CallOption) (*pv.PrivateLlmPrincipalBinding, error) {
	if principalID == "" {
		return nil, invalidArgument("principalID", "it is required")
	}
	req := request{op: opGetPrincipalPrivateLlm, params: map[string]string{"principal_id": principalID}, accept: "application/json"}
	return doJSON(ctx, p.c.t, req, applyOptions(options).deadline(p.c.timeout), func(*pv.PrivateLlmPrincipalBinding) error { return nil })
}

// DeletePrincipalBinding clears a principal's operator private LLM binding.
func (p *PrivateLLMOps) DeletePrincipalBinding(ctx context.Context, principalID string, options ...CallOption) error {
	if principalID == "" {
		return invalidArgument("principalID", "it is required")
	}
	req := request{op: opDeletePrincipalPrivateLlm, params: map[string]string{"principal_id": principalID}}
	return doEmpty(ctx, p.c.t, req, applyOptions(options).deadline(p.c.timeout))
}

func mapFromTyped[T any](v *T) (*map[string]any, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func typedFromMap[T any](body map[string]any) (T, error) {
	var zero T
	raw, err := json.Marshal(body)
	if err != nil {
		return zero, err
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return zero, err
	}
	return out, nil
}

// PutPrivateLlmEndpoint registers or updates a private self-hosted endpoint (ADR 0045).
//
// Deprecated: use OpsClient.PrivateLLM().PutEndpoint, typed (1.5.0).
func (o *OpsClient) PutPrivateLlmEndpoint(ctx context.Context, endpointID string, body map[string]any, options ...CallOption) (*map[string]any, error) {
	typed, err := typedFromMap[pv.PrivateLlmEndpointUpdate](body)
	if err != nil {
		return nil, err
	}
	got, err := o.PrivateLLM().PutEndpoint(ctx, endpointID, typed, options...)
	if err != nil {
		return nil, err
	}
	return mapFromTyped(got)
}

// GetPrivateLlmEndpoint reads a private endpoint.
//
// Deprecated: use OpsClient.PrivateLLM().GetEndpoint, typed (1.5.0).
func (o *OpsClient) GetPrivateLlmEndpoint(ctx context.Context, endpointID string, options ...CallOption) (*map[string]any, error) {
	got, err := o.PrivateLLM().GetEndpoint(ctx, endpointID, options...)
	if err != nil {
		return nil, err
	}
	return mapFromTyped(got)
}

// DeletePrivateLlmEndpoint removes a private endpoint.
//
// Deprecated: use OpsClient.PrivateLLM().DeleteEndpoint, typed (1.5.0).
func (o *OpsClient) DeletePrivateLlmEndpoint(ctx context.Context, endpointID string, options ...CallOption) error {
	return o.PrivateLLM().DeleteEndpoint(ctx, endpointID, options...)
}

// PutTenantPrivateLlm pins a tenant to a private endpoint.
//
// Deprecated: use OpsClient.PrivateLLM().PutTenantPin, typed (1.5.0).
func (o *OpsClient) PutTenantPrivateLlm(ctx context.Context, tenantID string, body map[string]any, options ...CallOption) (*map[string]any, error) {
	typed, err := typedFromMap[pv.PrivateLlmTenantPinUpdate](body)
	if err != nil {
		return nil, err
	}
	got, err := o.PrivateLLM().PutTenantPin(ctx, tenantID, typed, options...)
	if err != nil {
		return nil, err
	}
	return mapFromTyped(got)
}

// GetTenantPrivateLlm reads a tenant pin.
//
// Deprecated: use OpsClient.PrivateLLM().GetTenantPin, typed (1.5.0).
func (o *OpsClient) GetTenantPrivateLlm(ctx context.Context, tenantID string, options ...CallOption) (*map[string]any, error) {
	got, err := o.PrivateLLM().GetTenantPin(ctx, tenantID, options...)
	if err != nil {
		return nil, err
	}
	return mapFromTyped(got)
}

// DeleteTenantPrivateLlm clears a tenant pin.
//
// Deprecated: use OpsClient.PrivateLLM().DeleteTenantPin, typed (1.5.0).
func (o *OpsClient) DeleteTenantPrivateLlm(ctx context.Context, tenantID string, options ...CallOption) error {
	return o.PrivateLLM().DeleteTenantPin(ctx, tenantID, options...)
}

// PutPrincipalPrivateLlm activates a principal's private LLM binding.
//
// Deprecated: use OpsClient.PrivateLLM().PutPrincipalBinding, typed (1.5.0).
func (o *OpsClient) PutPrincipalPrivateLlm(ctx context.Context, principalID string, body map[string]any, options ...CallOption) (*map[string]any, error) {
	typed, err := typedFromMap[pv.PrivateLlmPrincipalBindingUpdate](body)
	if err != nil {
		return nil, err
	}
	got, err := o.PrivateLLM().PutPrincipalBinding(ctx, principalID, typed, options...)
	if err != nil {
		return nil, err
	}
	return mapFromTyped(got)
}

// GetPrincipalPrivateLlm reads a principal's operator private LLM binding.
//
// Deprecated: use OpsClient.PrivateLLM().GetPrincipalBinding, typed (1.5.0).
func (o *OpsClient) GetPrincipalPrivateLlm(ctx context.Context, principalID string, options ...CallOption) (*map[string]any, error) {
	got, err := o.PrivateLLM().GetPrincipalBinding(ctx, principalID, options...)
	if err != nil {
		return nil, err
	}
	return mapFromTyped(got)
}

// DeletePrincipalPrivateLlm clears a principal's operator private LLM binding.
//
// Deprecated: use OpsClient.PrivateLLM().DeletePrincipalBinding, typed (1.5.0).
func (o *OpsClient) DeletePrincipalPrivateLlm(ctx context.Context, principalID string, options ...CallOption) error {
	return o.PrivateLLM().DeletePrincipalBinding(ctx, principalID, options...)
}
