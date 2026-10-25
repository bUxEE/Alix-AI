//go:build live

package spike

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
)

// TestSpikeRuntimeSurface probes the running LocalAI directly, with and without the API key, and records the status of
// every route that deploy/container/localai/hardening.md and docs/security/localai-hardening-checklist.md section 5 make a
// claim about. It writes docs/evidence/m0-spike/surface-probe.json. The test fails only where a claim the deployment relies
// on is false (a surface that should be gone answers with content); routes that the documents say stay reachable are
// recorded, not asserted, so the evidence also shows where the documents are wrong.
func TestSpikeRuntimeSurface(t *testing.T) {
	env := needEnv(t)
	auth := map[string]string{"Authorization": "Bearer " + env.key}

	type probe struct {
		Method string
		Path   string
		Body   string
		// Why is the claim the probe checks.
		Why string
		// MustBeGone: with the key this must not answer 2xx (the surface is disabled). Everything else is recorded only.
		MustBeGone bool
		// Expect inspects the body of the answer with the key and returns a violation message, or "" when it holds. It is for
		// routes that exist but must be inert: with the feature off they answer 200 and say nothing.
		Expect func(body []byte) string
	}
	emptyList := func(body []byte) string {
		if strings.TrimSpace(string(body)) != "[]" {
			return "expected an empty list, got " + truncate(string(body), 200)
		}
		return ""
	}
	probes := []probe{
		// disabled surfaces (hardening.md section 2)
		{http.MethodGet, "/app", "", "web UI off (LOCALAI_DISABLE_WEBUI)", true, nil},
		{http.MethodGet, "/", "", "web UI root redirect", true, nil},
		{http.MethodGet, "/browse", "", "web UI gallery browser", true, nil},
		{http.MethodGet, "/api/settings", "", "runtime settings API (HC-13)", true, nil},
		{http.MethodGet, "/models/available", "", "model gallery listing (HC-14)", true, nil},
		{http.MethodPost, "/models/apply", `{"id":"x"}`, "model install (HC-14)", true, nil},
		{http.MethodPost, "/models/import-uri", `{"uri":"x"}`, "model import by URI (HC-14)", true, nil},
		{http.MethodGet, "/backends", "", "backend listing (HC-14)", true, nil},
		{http.MethodGet, "/backends/available", "", "backend gallery listing (HC-14)", true, nil},
		{http.MethodPost, "/backends/apply", `{"id":"x"}`, "backend install (HC-14)", true, nil},
		{http.MethodGet, "/api/agents", "", "agent pool (HC-15)", true, nil},
		{http.MethodGet, "/api/agent/tasks", "", "MCP/agent jobs API (HC-15/HC-16)", true, nil},
		{http.MethodGet, "/v1/mcp/servers/x", "", "MCP (HC-16)", true, nil},
		{http.MethodPost, "/v1/mcp/chat/completions", `{}`, "MCP chat (HC-16)", true, nil},
		{http.MethodGet, "/api/cors-proxy?url=http://127.0.0.1:1/", "", "authenticated open HTTP proxy (HC-16)", true, nil},
		// Tracing off: the routes are registered and answer 200, but they must hold nothing. A chat request is sent first (below)
		// so that a tracer that was on would have captured it.
		{http.MethodGet, "/api/traces", "", "request-body tracing off: an empty list although a request was made (hardening.md)", false, emptyList},
		{http.MethodGet, "/api/backend-traces", "", "backend tracing off: an empty list although a request was made", false, emptyList},
		// Public by design in v4.10.0 (auth/public_routes.go); it must say the user system is off.
		{http.MethodGet, "/api/auth/status", "", "LocalAI user system off (HC-09): public status route reports authEnabled=false", false, func(b []byte) string {
			if !strings.Contains(string(b), `"authEnabled":false`) {
				return "the user system is not reported as off: " + truncate(string(b), 200)
			}
			return ""
		}},
		{http.MethodGet, "/api/nodes", "", "distributed mode off", true, nil},
		// kept (hardening.md section 3 and 5): recorded, not asserted
		{http.MethodGet, "/metrics", "", "Prometheus metrics (HC-21 says disable)", false, nil},
		{http.MethodGet, "/system", "", "system info", false, nil},
		{http.MethodGet, "/backend/monitor?model=x", "", "backend monitor", false, nil},
		{http.MethodGet, "/api/p2p", "", "p2p list (empty while off)", false, nil},
		{http.MethodGet, "/api/fine-tuning/jobs", "", "fine-tuning API (no off switch)", false, nil},
		{http.MethodGet, "/api/quantization/jobs", "", "quantization API (no off switch)", false, nil},
		{http.MethodGet, "/stores/get", "", "vector store route (no off switch)", false, nil},
		{http.MethodGet, "/v1/models", "", "model list (used by AlixAI)", false, nil},
		{http.MethodGet, "/version", "", "version (used by AlixAI)", false, nil},
		// anonymous by upstream design (HC-11)
		{http.MethodGet, "/readyz", "", "readiness (exempt)", false, nil},
		{http.MethodGet, "/healthz", "", "liveness (exempt)", false, nil},
		{http.MethodGet, "/swagger/index.html", "", "API docs (exempt)", false, nil},
		{http.MethodGet, "/api/instructions", "", "instructions (exempt)", false, nil},
		{http.MethodGet, "/.well-known/localai.json", "", "discovery (exempt)", false, nil},
	}

	type row struct {
		Method        string `json:"method"`
		Path          string `json:"path"`
		Why           string `json:"claim"`
		StatusNoKey   int    `json:"status_without_key"`
		StatusWithKey int    `json:"status_with_key"`
		Gone          bool   `json:"gone_with_key"`
		Violation     bool   `json:"violates_claim,omitempty"`
	}
	// A request the tracer would capture if it were on.
	// (The model comes from the environment only: the registry has no lite default now that every controller is rejected.)
	localaiName := os.Getenv("ALIXAI_SPIKE_LOCALAI_MODEL")
	if localaiName == "" {
		localaiName = os.Getenv("ALIXAI_SPIKE_MODEL")
	}
	if localaiName != "" {
		body, _ := json.Marshal(map[string]any{"model": localaiName, "max_tokens": 4, "temperature": 0,
			"messages": []map[string]string{{"role": "user", "content": "CANARY-TRACE-5d2e say ready"}}})
		if c, resp, err := httpDo(http.MethodPost, env.runtimeURL+"/v1/chat/completions", body, auth); err != nil || c != http.StatusOK {
			t.Logf("canary chat request did not succeed (status %d, err %v, body %s): the tracing check below is weaker", c, err, truncate(string(resp), 200))
		}
	}
	var rows []row
	for _, p := range probes {
		var body []byte
		if p.Body != "" {
			body = []byte(p.Body)
		}
		noKey, _, err := httpDo(p.Method, env.runtimeURL+p.Path, body, nil)
		if err != nil {
			t.Fatalf("%s %s: %v", p.Method, p.Path, err)
		}
		withKey, withKeyBody, err := httpDo(p.Method, env.runtimeURL+p.Path, body, auth)
		if err != nil {
			t.Fatalf("%s %s: %v", p.Method, p.Path, err)
		}
		r := row{Method: p.Method, Path: p.Path, Why: p.Why, StatusNoKey: noKey, StatusWithKey: withKey, Gone: withKey == 404 || withKey == 405}
		if p.MustBeGone && (withKey >= 200 && withKey < 300) {
			r.Violation = true
			t.Errorf("%s %s answers %d with the key: %s", p.Method, p.Path, withKey, p.Why)
		}
		if p.Expect != nil {
			if msg := p.Expect(withKeyBody); msg != "" {
				r.Violation = true
				t.Errorf("%s %s: %s", p.Method, p.Path, msg)
			}
		}
		// Whatever is not exempt must demand the key.
		if noKey >= 200 && noKey < 300 && !isExempt(p.Path) {
			r.Violation = true
			t.Errorf("%s %s answers %d WITHOUT a key", p.Method, p.Path, noKey)
		}
		rows = append(rows, r)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
	path := writeEvidence(t, env, "surface-probe.json", map[string]any{
		"task":     "M0-13",
		"what":     "status of LocalAI routes with and without the API key (hardening checklist section 5, items 3 and 4)",
		"endpoint": "loopback only; the container publishes 127.0.0.1:8080",
		"rows":     rows,
	})
	t.Logf("wrote %s", path)
}

// isExempt lists the routes LocalAI answers without a key by design (HC-11).
func isExempt(path string) bool {
	switch path {
	case "/readyz", "/healthz", "/api/instructions", "/.well-known/localai.json", "/swagger/index.html", "/api/auth/status":
		return true
	}
	return false
}
