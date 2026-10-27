package dialogue

import (
	"testing"

	"github.com/buxee/alix-ai/internal/analyst/loop"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
)

func TestAnalystConfigDefaultsOff(t *testing.T) {
	cfg := Loaded()
	if cfg.Analyst.Enabled || cfg.Analyst.Abilities.Loop || cfg.Analyst.Abilities.Compute ||
		cfg.Analyst.Abilities.WebSearch || cfg.StreamVerified {
		t.Fatalf("defaults not safe: %+v", cfg)
	}
}

func TestAnalystOffLeavesSinglePath(t *testing.T) {
	// With no Runner on Deps.Analyst the text path never enters the loop.
	e := newEnv(t)
	e.app.Seed(tenantA, workspace1, "item_1", "Alpha widget")
	e.model.On(pfake.Constrained(providers.ConstraintJSONSchema)).Reply(proposalJSON(capGet, "task", map[string]any{"item_id": "item_1"}))
	e.onAct("present_result", "Here is what I found. Name: Alpha widget.")
	res := e.say(memberScope(), "show me item item_1")
	wantAct(t, res, "present_result")
	if res.Trace.AnalystReason != "" {
		t.Fatalf("analyst reason set when off: %q", res.Trace.AnalystReason)
	}
	for _, s := range res.Trace.Stages {
		if s == StageAnalyst {
			t.Fatal("analyst stage present when runner is nil")
		}
	}
}

func TestClientCapabilitiesStoredForEmit(t *testing.T) {
	c := an.ClientCapabilitiesContract15
	caps := an.ClientCapabilities{Contract: &c}
	loop.RememberClientCapabilities("trn_emit", caps)
	got, ok := ClientCapabilitiesForTurn("trn_emit")
	if !ok || got.Contract == nil || *got.Contract != an.ClientCapabilitiesContract15 {
		t.Fatalf("accessor = %+v ok=%v", got, ok)
	}
	loop.ForgetClientCapabilities("trn_emit")
}

func TestDeploymentAbilityResolver(t *testing.T) {
	r := deploymentAbilityResolver{cfg: AnalystConfig{
		Enabled:   true,
		Abilities: AnalystAbilityFlags{Loop: true, Compute: true, WebSearch: true},
	}}
	off, err := r.Resolve(nil, memberScope(), false)
	if err != nil {
		t.Fatal(err)
	}
	if off.Abilities.Loop || !off.Wanted || off.WantedReason == "" {
		t.Fatalf("expected wanted-but-incapable: %+v", off)
	}
	on, err := r.Resolve(nil, memberScope(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !on.Enabled || !on.Abilities.Loop || !on.Abilities.Compute || !on.Abilities.WebSearch {
		t.Fatalf("expected enabled: %+v", on)
	}
}
