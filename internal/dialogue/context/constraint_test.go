package dctx

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The output constraint (the proposal's schema, the "tool schema" of resource-budgets.md 6.2)
// counts against the prompt and has a cap of its own, which the builder meets by describing
// fewer capabilities.
func TestOutputConstraintCapLowersTheShortlist(t *testing.T) {
	full := mustBuild(t, newBuilder(t, ProfileLite), baseInput("please reserve The Leopard for me"))
	if full.Report.Shortlist != len(catalogue) || len(full.Report.Trimmed) != 0 {
		t.Fatalf("the fixture is not a clean five-capability build: %+v", full.Report)
	}
	five := full.Report.ConstraintToks
	if five < 100 {
		t.Fatalf("the constraint of five capabilities is %d estimated tokens; the fixture is too small to test a cap", five)
	}

	t.Run("a cap just under the full constraint drops the lowest-ranked capability", func(t *testing.T) {
		p := Lite()
		p.MaxConstraintTokens = five - 1
		b, err := New(p)
		if err != nil {
			t.Fatal(err)
		}
		c := mustBuild(t, b, baseInput("please reserve The Leopard for me"))
		if c.Report.Shortlist >= len(catalogue) || c.Report.Shortlist < 1 {
			t.Fatalf("shortlist = %d, want fewer than %d", c.Report.Shortlist, len(catalogue))
		}
		if c.Report.ConstraintToks > p.MaxConstraintTokens {
			t.Fatalf("the constraint is %d estimated tokens, over its cap %d", c.Report.ConstraintToks, p.MaxConstraintTokens)
		}
		if got := b.Budget().EstimateChat(providers.ChatRequest{Output: c.Output}); got > p.MaxConstraintTokens {
			t.Fatalf("the constraint counted again is %d, over its cap %d", got, p.MaxConstraintTokens)
		}
		trimmed := false
		for _, what := range c.Report.Trimmed {
			trimmed = trimmed || what == "capabilities"
		}
		if !trimmed {
			t.Fatalf("the cut is not reported: %v", c.Report.Trimmed)
		}
	})

	t.Run("a cap that one capability cannot meet is a budget violation and nothing is built", func(t *testing.T) {
		p := Lite()
		p.MaxConstraintTokens = 10
		b, err := New(p)
		if err != nil {
			t.Fatal(err)
		}
		c, err := b.Build(context.Background(), scopeOf(sessionID), baseInput("please reserve The Leopard for me"))
		if err == nil {
			t.Fatalf("a constraint over its cap was built: %+v", c.Report)
		}
		if which, ok := providers.IsBudgetViolation(err); !ok || which != "max_constraint_tokens" {
			t.Fatalf("error %v is not the max_constraint_tokens violation (%q, %v)", err, which, ok)
		}
		if contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Fatalf("code = %s, want validation_failed (not retryable)", contracts.CodeOf(err))
		}
		if len(c.Items) != 0 {
			t.Fatal("a context came back with the error")
		}
	})

	t.Run("every profile's constraint cap holds for the fixture", func(t *testing.T) {
		for _, name := range ProfileNames() {
			b := newBuilder(t, name)
			c := mustBuild(t, b, baseInput("please reserve The Leopard for me"))
			if c.Report.ConstraintToks > b.Profile().MaxConstraintTokens {
				t.Errorf("%s: constraint %d over its cap %d", name, c.Report.ConstraintToks, b.Profile().MaxConstraintTokens)
			}
		}
	})
}

func TestResponseBudgetIsTheProfilesWithTheResponseCap(t *testing.T) {
	for _, name := range ProfileNames() {
		p, err := ProfileByName(name)
		if err != nil {
			t.Fatal(err)
		}
		rb := p.ResponseBudget()
		if err := rb.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if rb.MaxPromptTokens != p.MaxResponsePromptTokens || rb.MaxPromptTokens > p.MaxPromptTokens || rb.MaxOutputTokens != p.MaxOutputTokens {
			t.Errorf("%s: response budget %+v against profile %v", name, rb, p)
		}
	}
	// A response cap above the total cap never widens it.
	p := Lite()
	p.MaxResponsePromptTokens = p.MaxPromptTokens * 2
	if got := p.ResponseBudget().MaxPromptTokens; got != p.MaxPromptTokens {
		t.Errorf("response budget cap = %d, want the profile's %d", got, p.MaxPromptTokens)
	}
}

// The caps of docs/operations/resource-budgets.md 6.2 are the table: a smaller machine never
// gets a larger cap, and a profile without the new caps is refused.
func TestProfileCapsMatchTheResourceBudgetDocument(t *testing.T) {
	type row struct{ constraint, response int }
	want := map[string]row{
		ProfileExperimental4G: {380, 512}, ProfileLite: {540, 512}, ProfileBalanced: {540, 768}, ProfilePerformance: {540, 1024},
	}
	var prev Profile
	for i, name := range []string{ProfileExperimental4G, ProfileLite, ProfileBalanced, ProfilePerformance} {
		p, err := ProfileByName(name)
		if err != nil {
			t.Fatal(err)
		}
		if p.MaxConstraintTokens != want[name].constraint || p.MaxResponsePromptTokens != want[name].response {
			t.Errorf("%s: constraint %d / response %d, the document says %d / %d", name, p.MaxConstraintTokens, p.MaxResponsePromptTokens, want[name].constraint, want[name].response)
		}
		if i > 0 && (p.MaxConstraintTokens < prev.MaxConstraintTokens || p.MaxResponsePromptTokens < prev.MaxResponsePromptTokens) {
			t.Errorf("%s has a smaller constraint or response cap than %s", name, prev.Name)
		}
		prev = p
	}
	for name, mod := range map[string]func(*Profile){
		"no constraint cap":      func(p *Profile) { p.MaxConstraintTokens = 0 },
		"no response prompt cap": func(p *Profile) { p.MaxResponsePromptTokens = -1 },
	} {
		p := Lite()
		mod(&p)
		if _, err := New(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
