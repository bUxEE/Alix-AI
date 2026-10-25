package gateway

import (
	"net/http"
	"testing"
)

// The egress policy body takes the optional files and audio settings (an enabled switch and the highest class); a
// body without them is what it was. Each member is closed and restricted is not a class it can name: both are
// refused before the service sees them.
func TestProvidersEgressPolicyFilesSetting(t *testing.T) {
	e, _ := providersEnv(t)
	providersOperator(e, ProvidersOperatorDeployment)
	for _, member := range []string{"files", "audio"} {
		body := func(setting any) map[string]any {
			b := map[string]any{"level": "deployment", "expected_revision": 0, "enabled": true, "source_mode": "local_only"}
			if setting != nil {
				b[member] = setting
			}
			return b
		}
		wantStatus(t, e.call(http.MethodPut, "/v1/providers/egress-policy", body(nil)), http.StatusOK)
		wantStatus(t, e.call(http.MethodPut, "/v1/providers/egress-policy", body(map[string]any{"enabled": true, "max_class": "confidential"})), http.StatusOK)
		for name, setting := range map[string]any{
			"restricted":      map[string]any{"enabled": true, "max_class": "restricted"},
			"no class":        map[string]any{"enabled": true},
			"an extra member": map[string]any{"enabled": true, "max_class": "public", "providers": []any{"openai"}},
			"not an object":   true,
		} {
			if rec := e.call(http.MethodPut, "/v1/providers/egress-policy", body(setting)); rec.Code != http.StatusBadRequest {
				t.Errorf("%s %s: status %d (%s), want 400", member, name, rec.Code, rec.Body.String())
			}
		}
	}
}
