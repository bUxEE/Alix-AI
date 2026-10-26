//go:build noexternal

package cli

import "context"

func retentionErasePrincipalLLM(_ context.Context, _ *retentionEngine, _, _ string) (int, int, error) {
	return 0, 0, nil
}
