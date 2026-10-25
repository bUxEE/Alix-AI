//go:build live

package spike

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// TestSpikeEndToEnd is the M0 exit gate's first two lines: one text request runs locally through AlixAI and LocalAI,
// and the model returns a schema-constrained proposal that validates.
//
//	test -> alixai serve (spike endpoint) -> recording proxy -> LocalAI -> model
//
// It records the request, the raw model output as the runtime generated it, the validated proposal, the latency and the
// container memory to docs/evidence/m0-spike/e2e.json. "schema_valid" there is this test's own verdict, reached by
// compiling schemas/conversation/task-proposal.schema.json itself; the gateway's own claim is recorded separately.
func TestSpikeEndToEnd(t *testing.T) {
	env := needEnv(t)
	env.needServe(t)
	regID, localaiName := controllerUnderTest(t)
	variant, shippedStatus := registryVariant(t, env, regID)

	px, rec := newRecordingProxy(t, env.runtimeURL)
	srv := startAlixAI(t, env, serveOptions{runtimeEndpoint: px.URL, registryModel: regID, localaiModel: localaiName, modelsDir: env.modelsDir})

	readyBody, err := srv.waitReady(t, 90*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	request := proposeRequest{
		Text:  "Book a table for four people tomorrow at 8pm, the name is Rossi.",
		Today: "2026-10-12",
		Capabilities: []proposeCapability{
			{ID: "bookings.create", Description: "Create a table booking", Fields: []proposeField{
				{Name: "party_size", Type: "integer", Required: true},
				{Name: "date", Type: "date", Required: true},
				{Name: "time", Type: "time", Required: true},
				{Name: "customer_name", Type: "string"},
			}},
			{ID: "bookings.cancel", Description: "Cancel an existing booking", Fields: []proposeField{
				{Name: "booking_ref", Type: "string", Required: true},
			}},
			{ID: "menu.search", Description: "Search the menu", Fields: []proposeField{
				{Name: "query", Type: "string", Required: true},
			}},
		},
	}
	const wantCapability = "bookings.create"
	// What a correct proposal holds for this request (today is Monday 2026-10-12, so "tomorrow" is 2026-10-13). Recorded next
	// to the model's answer and never asserted: the task is the plumbing proof, and a small model's mistakes are the finding.
	wantFields := map[string]any{"party_size": float64(4), "date": "2026-10-13", "time": "20:00", "customer_name": "Rossi"}
	const wantIntent = "task"
	reqBody, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}

	// Warm the model with one direct runtime call, outside the request under test. LocalAI keeps one backend resident
	// (LOCALAI_MAX_ACTIVE_BACKENDS=1), so the first request for a model unloads the previous one and loads this one from disk;
	// on the dev box that took about 50 s, longer than the gateway's 60 s call budget (logs/e2e-cold-start-timeout.log). The
	// call's duration is recorded as the cold load; the request through the gateway is then a warm one.
	warmBody, _ := json.Marshal(map[string]any{
		"model": localaiName, "max_tokens": 1, "temperature": 0,
		"messages": []map[string]string{{"role": "user", "content": "ping"}},
	})
	warmStart := time.Now()
	warmCode, warmResp, warmErr := httpDo(http.MethodPost, env.runtimeURL+"/v1/chat/completions", warmBody, map[string]string{"Authorization": "Bearer " + env.key})
	warmMS := time.Since(warmStart).Milliseconds()
	if warmErr != nil || warmCode != http.StatusOK {
		t.Fatalf("warm-up call to the runtime failed: status %d err %v body %s", warmCode, warmErr, truncate(string(warmResp), 300))
	}

	stopMem := env.trackMem()
	start := time.Now()
	code, respBody, err := httpDo(http.MethodPost, srv.base+"/v0/dev/propose", reqBody, nil)
	gatewayMS := time.Since(start).Milliseconds()
	mem := stopMem()
	if err != nil {
		t.Fatalf("POST /v0/dev/propose: %v\nserve log:\n%s", err, srv.stderr.String())
	}
	if code != http.StatusOK {
		t.Fatalf("POST /v0/dev/propose = %d: %s\nserve log:\n%s", code, respBody, srv.stderr.String())
	}

	var resp struct {
		DevOnly     bool            `json:"dev_only"`
		SchemaValid bool            `json:"schema_valid"`
		Proposal    json.RawMessage `json:"proposal"`
		Model       string          `json:"model"`
		Usage       json.RawMessage `json:"usage"`
		PromptID    string          `json:"prompt_id"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		t.Fatalf("the gateway's answer is not JSON: %v\n%s", err, respBody)
	}

	// The test's own verdict on the proposal.
	schema := compileTaskProposal(t, env.root)
	var validationErrs []string
	valid := true
	doc, perr := jsonschema.UnmarshalJSON(strings.NewReader(string(resp.Proposal)))
	if perr != nil {
		valid, validationErrs = false, append(validationErrs, "the proposal is not JSON: "+perr.Error())
	} else if verr := schema.Validate(doc); verr != nil {
		valid, validationErrs = false, append(validationErrs, verr.Error())
	}
	var proposal struct {
		CapabilityID *string        `json:"capability_id"`
		Intent       string         `json:"intent"`
		Fields       map[string]any `json:"fields"`
		TraceID      string         `json:"trace_id"`
	}
	if perr == nil {
		_ = json.Unmarshal(resp.Proposal, &proposal)
	}
	var fieldDiffs []string
	for k, want := range wantFields {
		if got, ok := proposal.Fields[k]; !ok {
			fieldDiffs = append(fieldDiffs, k+": missing, want "+fmt.Sprint(want))
		} else if got != want {
			fieldDiffs = append(fieldDiffs, k+": got "+fmt.Sprint(got)+", want "+fmt.Sprint(want))
		}
	}
	for k := range proposal.Fields {
		if _, ok := wantFields[k]; !ok {
			fieldDiffs = append(fieldDiffs, k+": not expected")
		}
	}
	slices.Sort(fieldDiffs)
	inCatalogue := proposal.CapabilityID == nil
	for _, c := range request.Capabilities {
		if proposal.CapabilityID != nil && *proposal.CapabilityID == c.ID {
			inCatalogue = true
		}
	}

	// What went over the wire to the runtime and what came back raw.
	chat, ok := rec.lastChat()
	if !ok {
		t.Fatal("the runtime saw no chat completion: the request did not reach the model")
	}
	var wire struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	_ = json.Unmarshal(chat.Response, &wire)
	rawOutput := ""
	if len(wire.Choices) > 0 {
		rawOutput = wire.Choices[0].Message.Content
	}

	paths := rec.paths()
	slices.Sort(paths)
	allowed := []string{"/readyz", "/v1/chat/completions", "/v1/models", "/version"}
	var outside []string
	for _, p := range paths {
		if !slices.Contains(allowed, p) {
			outside = append(outside, p)
		}
	}

	// The gateway serves no runtime surface: models, settings, backends, agents and the like are plain 404s.
	gw := map[string]int{}
	for _, p := range []string{"/models/available", "/backends", "/api/settings", "/api/agents", "/v1/mcp/servers/x", "/swagger/index.html", "/metrics"} {
		c, _, err := httpDo(http.MethodGet, srv.base+p, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		gw[p] = c
	}

	version, verr := env.runtimeVersion()
	if verr != nil {
		version = "unreadable: " + verr.Error()
	}

	out := map[string]any{
		"task":      "M0-13",
		"what":      "one text request through `alixai serve` (dev spike endpoint) and LocalAI to a validated TaskProposal",
		"emulated":  true,
		"emulation": "LocalAI ran in a container under the lite profile (8 GB, 4 CPU, no swap) on an i7-10700K host; not a certified machine",
		"runtime": map[string]any{
			"name":    "localai",
			"version": version,
		},
		"model": map[string]any{
			"registry_id":                regID,
			"localai_name":               localaiName,
			"status_in_shipped_registry": shippedStatus,
			"qualified_for_lite":         false,
			"qualification_note":         "this request proves the plumbing only; the model did not pass the Lite bar (report.md), so nothing here qualifies it",
			"verified_by":                "alixai serve hashed the model file against the registry entry's sha256 and size (models.Verify) before inference was enabled",
		},
		"registry_variant": variant,
		"serve_readyz":     readyBody,
		"gateway_request":  json.RawMessage(reqBody),
		"gateway_status":   code,
		"gateway_response": json.RawMessage(respBody),
		"runtime_request": map[string]any{
			"method": chat.Method, "path": chat.Path, "status": chat.Status,
			"body_note": "exactly what the gateway's LocalAI adapter sent (Authorization header omitted)",
			"body":      chat.Request,
		},
		"raw_model_output":   rawOutput,
		"raw_runtime_usage":  wire.Usage,
		"validated_proposal": json.RawMessage(resp.Proposal),
		// schema_valid is the verdict of this test's own validation against the published schema.
		"schema_valid":                          valid,
		"gateway_reported_schema_valid":         resp.SchemaValid,
		"schema":                                "schemas/conversation/task-proposal.schema.json (urn:alixai:schema:conversation:task-proposal:v1)",
		"schema_errors":                         validationErrs,
		"capability_in_request_catalogue":       inCatalogue,
		"expected_capability":                   wantCapability,
		"routing_matches_expectation":           proposal.CapabilityID != nil && *proposal.CapabilityID == wantCapability,
		"expected_intent":                       wantIntent,
		"intent_matches_expectation":            proposal.Intent == wantIntent,
		"expected_fields":                       wantFields,
		"fields_match_expectation":              len(fieldDiffs) == 0,
		"field_differences":                     fieldDiffs,
		"correctness_note":                      "schema_valid is about shape. Whether the values are right is recorded separately; this model is rejected for Lite (report.md) and a wrong value here is part of why",
		"proposal_intent":                       proposal.Intent,
		"constraint_sent":                       "json_schema (the gateway's choice in M0-12; see report.md for whether the runtime enforces it)",
		"latency_ms":                            map[string]any{"gateway_request_total": gatewayMS, "runtime_chat_call": chat.DurationMS},
		"cold_load_note":                        "a direct warm-up call to the runtime preceded the request (see warmup_direct_call_ms); the first end-to-end attempt without it timed out at the gateway's 60 s budget, see logs/e2e-cold-start-timeout.log",
		"warmup_direct_call_ms":                 warmMS,
		"memory":                                mem,
		"runtime_paths_called_by_alixai":        paths,
		"runtime_paths_outside_allowlist":       outside,
		"gateway_probe_status_of_runtime_paths": gw,
		"dev_only_endpoint":                     resp.DevOnly,
		"measured_at":                           time.Now().UTC().Format(time.RFC3339),
	}
	path := writeEvidence(t, env, "e2e.json", out)
	t.Logf("wrote %s", path)

	if !valid {
		t.Errorf("the proposal does not satisfy the TaskProposal schema: %v\n%s", validationErrs, resp.Proposal)
	}
	if !resp.SchemaValid {
		t.Error("the gateway did not report the proposal as schema-valid")
	}
	if !inCatalogue {
		t.Errorf("the proposal names a capability outside the request's catalogue: %s", resp.Proposal)
	}
	if len(outside) > 0 {
		t.Errorf("alixai called runtime paths outside the allowlist: %v", outside)
	}
	for p, c := range gw {
		if c == http.StatusOK {
			t.Errorf("the gateway answers GET %s with 200: a runtime surface is exposed", p)
		}
	}
	if proposal.CapabilityID == nil || *proposal.CapabilityID != wantCapability {
		t.Logf("note: the model routed to %v, not %s (recorded as routing_matches_expectation=false)", proposal.CapabilityID, wantCapability)
	}
}

// The request shapes of POST /v0/dev/propose (internal/gateway/propose.go), repeated here because the test drives the
// binary over HTTP like any other client.
type proposeRequest struct {
	Text         string              `json:"text"`
	Today        string              `json:"today,omitempty"`
	Capabilities []proposeCapability `json:"capabilities"`
}

type proposeCapability struct {
	ID          string         `json:"id"`
	Description string         `json:"description"`
	Fields      []proposeField `json:"fields"`
}

type proposeField struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Required bool     `json:"required"`
	Enum     []string `json:"enum,omitempty"`
}
