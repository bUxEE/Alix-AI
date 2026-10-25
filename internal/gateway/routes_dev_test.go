package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// devFinishGen is a runtime that reports a fixed finish reason (and no JSON
// member) for every answer.
type devFinishGen struct {
	*fake.Provider
	finish providers.FinishReason
}

func (g devFinishGen) Chat(ctx context.Context, scope contracts.Scope, req providers.ChatRequest) (providers.ChatResult, error) {
	res, err := g.Provider.Chat(ctx, scope, req)
	res.FinishReason = g.finish
	return res, err
}

// The dev route sends the output grammar and never a json_schema, and reads the
// answer's text (the provider fills res.JSON only for a json_schema request).
func TestDevRouteUsesTheGrammarRequest(t *testing.T) {
	h := newHarness(t, withSpike())
	grammarRule := h.fake.On(fake.Constrained(providers.ConstraintGrammar)).ReplyJSON(proposalJSON(nil)).Usage(210, 40)
	schemaRule := h.fake.On(fake.Constrained(providers.ConstraintJSONSchema)).ReplyJSON(proposalJSON(nil))

	rec := h.do(http.MethodPost, proposePath, demoRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s = %d, want 200: %s", proposePath, rec.Code, rec.Body.String())
	}
	if grammarRule.Hits() != 1 || schemaRule.Hits() != 0 {
		t.Fatalf("grammar rule hits = %d, json_schema rule hits = %d: the route must send a grammar and no json_schema", grammarRule.Hits(), schemaRule.Hits())
	}
	out := h.fake.Prompts()[0].Chat.Output
	if out.Kind() != providers.ConstraintGrammar || out.Validate() != nil || len(out.JSONSchema) != 0 {
		t.Fatalf("the model received constraint %q (valid: %v) with a schema of %d bytes", out.Kind(), out.Validate(), len(out.JSONSchema))
	}
	var resp ProposeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.SchemaValid || resp.Proposal.CapabilityID == nil || *resp.Proposal.CapabilityID != "demo.create" || resp.Proposal.TraceID != "fake-000001" {
		t.Errorf("response = %+v", resp)
	}
}

// An answer that did not finish normally is a 502 schema_invalid and never a
// proposal, even when its text is a complete, valid proposal (proposeDecode).
func TestDevRouteRefusesAnAnswerThatDidNotStop(t *testing.T) {
	for _, tc := range []struct {
		name      string
		finish    providers.FinishReason
		wantStage string
	}{
		{"cut at the token cap", providers.FinishLength, "truncated"},
		{"any other finish", providers.FinishReason("content_filter"), "finish"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fk := &countingProvider{Provider: fake.New()}
			fk.On(fake.Constrained(providers.ConstraintGrammar)).ReplyJSON(proposalJSON(nil))
			h := newHarness(t, withSpike(), func(o *Options) {
				reg := providers.NewRegistry(nil)
				if err := reg.Register(providers.RoleController, devFinishGen{Provider: fk.Provider, finish: tc.finish}); err != nil {
					t.Fatal(err)
				}
				o.Providers = reg
			})
			rec := h.do(http.MethodPost, proposePath, demoRequest())
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
			}
			env := mustEnvelope(t, rec)
			if env.Code != "schema_invalid" || env.Details["stage"] != tc.wantStage {
				t.Errorf("envelope = %+v, want schema_invalid at stage %q", env, tc.wantStage)
			}
			if body := rec.Body.String(); strings.Contains(body, "demo.create") || strings.Contains(body, `"proposal"`) {
				t.Errorf("the refusal carries model output: %s", body)
			}
		})
	}
}
