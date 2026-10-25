//go:build live

package spike

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestSpikeErrorMapping checks the adapter's error mapping (M0-09) against what the real runtime answers, through the real
// binary: a failure is provoked in LocalAI, `alixai serve` makes the call, and the error envelope's code is compared with
// the code the mapping decisions of the task brief call for. A recording proxy between the two keeps the runtime's raw
// status and (bounded) message, so the evidence shows the input of every decision, not only the result.
//
// The fixtures are LocalAI models of tests/live/spike/localai/models.yaml: m0-corrupt (not a GGUF file: the backend cannot
// load it) and m0-tinyctx (a 256-token window: the gateway's prompt overflows it). A model the runtime lists but cannot
// run passes the adapter's readiness check, so the failure happens at the call, which is where the mapping applies.
//
// Run it once per runtime configuration and name the variant: the default stack (opaque errors OFF, the deployment's
// choice) and a stack started with LOCALAI_OPAQUE_ERRORS=true (hardening checklist HC-20):
//
//	ALIXAI_SPIKE_ERRORS_VARIANT=default          ... go test -tags live -run TestSpikeErrorMapping
//	ALIXAI_SPIKE_ERRORS_VARIANT=opaque-errors    ... (stack started with compose.opaque-errors.yaml)
//
// It writes docs/evidence/m0-spike/error-mapping-<variant>.json.
func TestSpikeErrorMapping(t *testing.T) {
	env := needEnv(t)
	env.needServe(t)
	regID, _ := controllerUnderTest(t)
	variant := os.Getenv("ALIXAI_SPIKE_ERRORS_VARIANT")
	if variant == "" {
		variant = "default"
	}
	opaque := strings.Contains(variant, "opaque")

	type scenario struct {
		name  string
		model string // LocalAI model name used as localai.models.controller
		// want is the code the mapping decisions call for with opaque errors off; wantOpaque with them on.
		want       string
		wantOpaque string
		why        string
		// recordOnly scenarios are recorded and never asserted (the mapping decisions of the brief do not name their code).
		recordOnly bool
	}
	scenarios := []scenario{
		{"model-cannot-load", "m0-corrupt", "resource_exhausted", "internal",
			"a model file the backend cannot load: LocalAI answers 500 with a load failure, mapped to resource_exhausted; with opaque errors the body is blank and the 500 is just internal (HC-20)", false},
		{"context-window-overflow", "m0-tinyctx", "validation_failed", "internal",
			"a prompt larger than the model's window: mapped to validation_failed when the message says so; opaque errors hide it", false},
	}
	// ALIXAI_SPIKE_ERRORS_OOM=1 adds the real out-of-memory case. It needs the stack started under the 4g profile: m0-oom asks
	// for a 131072-token window whose KV cache cannot fit the 4 GiB cap, so the kernel kills the backend while it loads.
	if os.Getenv("ALIXAI_SPIKE_ERRORS_OOM") == "1" {
		scenarios = append(scenarios, scenario{"backend-killed-by-cgroup-oom", "m0-oom", "", "internal",
			"the model cannot be loaded inside the container memory cap and the kernel kills the backend; recorded to see what the runtime answers and what the adapter makes of it", true})
	}

	type result struct {
		Scenario       string          `json:"scenario"`
		Why            string          `json:"why"`
		LocalAIModel   string          `json:"localai_model"`
		RuntimeStatus  int             `json:"runtime_status"`
		RuntimeBody    json.RawMessage `json:"runtime_body"`
		GatewayStatus  int             `json:"gateway_status"`
		GatewayCode    string          `json:"gateway_code"`
		ExpectedCode   string          `json:"expected_code"`
		Matches        bool            `json:"matches_expected"`
		RecordOnly     bool            `json:"record_only,omitempty"`
		GatewayMessage string          `json:"gateway_message,omitempty"`
		DurationMS     int64           `json:"duration_ms"`
	}
	var results []result

	request := proposeRequest{
		Text:  "Book a table for four people tomorrow at 8pm.",
		Today: "2026-10-12",
		Capabilities: []proposeCapability{
			{ID: "bookings.create", Description: "Create a table booking", Fields: []proposeField{{Name: "party_size", Type: "integer", Required: true}}},
			{ID: "menu.search", Description: "Search the menu", Fields: []proposeField{{Name: "query", Type: "string", Required: true}}},
		},
	}
	reqBody, _ := json.Marshal(request)

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			px, rec := newRecordingProxy(t, env.runtimeURL)
			srv := startAlixAI(t, env, serveOptions{runtimeEndpoint: px.URL, registryModel: regID, localaiModel: sc.model, modelsDir: env.modelsDir})
			if _, err := srv.waitReady(t, 120*time.Second); err != nil {
				t.Fatalf("the failure fixture %s must pass the readiness check (it is listed by the runtime): %v", sc.model, err)
			}
			start := time.Now()
			code, body, err := httpDo(http.MethodPost, srv.base+"/v0/dev/propose", reqBody, nil)
			dur := time.Since(start)
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			_ = json.Unmarshal(body, &envelope)
			chat, _ := rec.lastChat()
			want := sc.want
			if opaque {
				want = sc.wantOpaque
			}
			if sc.recordOnly {
				want = envelope.Code
			}
			r := result{Scenario: sc.name, Why: sc.why, LocalAIModel: sc.model, RuntimeStatus: chat.Status, RuntimeBody: chat.Response,
				GatewayStatus: code, GatewayCode: envelope.Code, ExpectedCode: want, Matches: envelope.Code == want, RecordOnly: sc.recordOnly, GatewayMessage: envelope.Message, DurationMS: dur.Milliseconds()}
			results = append(results, r)
			t.Logf("runtime answered %d %s; gateway: HTTP %d code %q (want %q)", chat.Status, truncate(string(chat.Response), 300), code, envelope.Code, want)
			if envelope.Code != want {
				t.Errorf("gateway code = %q, want %q (runtime status %d)", envelope.Code, want, chat.Status)
			}
		})
	}
	path := writeEvidence(t, env, "error-mapping-"+variant+".json", map[string]any{
		"task":          "M0-13",
		"variant":       variant,
		"opaque_errors": opaque,
		"decisions": map[string]string{
			"503 with 'load is in cooldown' or 'inference capacity reached', 429, 507": "resource_exhausted",
			"other 503, 404":        "unavailable",
			"401, 403, 400, 422":    "internal",
			"context overflow, 413": "validation_failed",
		},
		"results": results,
	})
	t.Logf("wrote %s", path)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
