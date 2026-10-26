//go:build noexternal

package cli

import "context"

// retentionErasePrincipalLLMHook is replaced by tests to observe application-wide credential erasure.
var retentionErasePrincipalLLMHook func(ctx context.Context, e *retentionEngine, application, principal string) (creds, bindings int, err error)

func retentionErasePrincipalLLM(ctx context.Context, e *retentionEngine, application, principal string) (int, int, error) {
	if retentionErasePrincipalLLMHook != nil {
		return retentionErasePrincipalLLMHook(ctx, e, application, principal)
	}
	return 0, 0, nil
}
