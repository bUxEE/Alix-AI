package sdkgo

import (
	"context"
	"testing"
)

// Compile-time: 1.4 map[string]any signatures must still exist on OpsClient.
func TestPrivateLlmMapSignaturesStillCompile(t *testing.T) {
	var o *OpsClient
	var (
		_ func(context.Context, string, map[string]any, ...CallOption) (*map[string]any, error) = o.PutPrivateLlmEndpoint
		_ func(context.Context, string, ...CallOption) (*map[string]any, error)                 = o.GetPrivateLlmEndpoint
		_ func(context.Context, string, ...CallOption) error                                    = o.DeletePrivateLlmEndpoint
		_ func(context.Context, string, map[string]any, ...CallOption) (*map[string]any, error) = o.PutTenantPrivateLlm
		_ func(context.Context, string, ...CallOption) (*map[string]any, error)                 = o.GetTenantPrivateLlm
		_ func(context.Context, string, ...CallOption) error                                    = o.DeleteTenantPrivateLlm
		_ func(context.Context, string, map[string]any, ...CallOption) (*map[string]any, error) = o.PutPrincipalPrivateLlm
		_ func(context.Context, string, ...CallOption) (*map[string]any, error)                 = o.GetPrincipalPrivateLlm
		_ func(context.Context, string, ...CallOption) error                                    = o.DeletePrincipalPrivateLlm
		_ func() *PrivateLLMOps                                                                 = o.PrivateLLM
	)
}
