package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providers"
)

// TestPromptV1DefinesEveryIntent: bench-prompt-v1 exists because v0 named four
// intents and defined only one (M0-13 finding F3). It iterates the intents the
// template offers, so an intent added later without a rule fails here.
func TestPromptV1DefinesEveryIntent(t *testing.T) {
	if PromptID != PromptV1 {
		t.Fatalf("the default for new runs is %q, want %q", PromptID, PromptV1)
	}
	rules := systemRulesFor(PromptV1)
	if len(promptIntents) != 4 {
		t.Fatalf("the template offers %v, the brief names four intents", promptIntents)
	}
	for _, intent := range promptIntents {
		line := ""
		for _, l := range strings.Split(rules, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "- "+intent+": ") {
				line = strings.TrimSpace(l)
			}
		}
		if line == "" {
			t.Errorf("bench-prompt-v1 has no rule line for the intent %q:\n%s", intent, rules)
			continue
		}
		if len(line) < len(intent)+42 {
			t.Errorf("the rule for %q is a label, not a rule: %q", intent, line)
		}
		switch intent {
		case IntentTask:
			if !strings.Contains(line, "capability_id is that capability's id") {
				t.Errorf("the task rule does not say which capability_id goes with it: %q", line)
			}
		default:
			if !strings.Contains(line, "capability_id is null and fields is {}") {
				t.Errorf("the %s rule does not say that capability_id is null and fields is {}: %q", intent, line)
			}
		}
	}
	// The distinction that decides the cancel and out_of_scope cases is stated, not left to the model.
	for _, want := range []string{"cancel a record", "otherwise out_of_scope", "ignore that part"} {
		if !strings.Contains(rules, want) {
			t.Errorf("bench-prompt-v1 lacks %q", want)
		}
	}

	// v0 is the defect F3 measured: the intents are listed and only out_of_scope has a rule.
	v0 := systemRulesFor(PromptV0)
	for _, intent := range []string{IntentTask, IntentHelp, IntentCancel} {
		if strings.Contains(v0, "- "+intent+": ") {
			t.Errorf("bench-prompt-v0 defines %q; v0 must stay as M0-13 ran it", intent)
		}
	}

	// The rendered system message carries the rules, and nothing of the user's text.
	c := caseByID(t, "ex-en-01")
	sys := RenderSystemFor(PromptV1, c)
	if !strings.HasSuffix(sys, rules) || strings.Contains(sys, c.Text) {
		t.Errorf("the v1 system message is not the catalogue plus the v1 rules, or it carries the user text:\n%s", sys)
	}
	if RenderSystem(c) != sys {
		t.Error("RenderSystem is not the default template")
	}
	if PromptSHA256For(PromptV1) == PromptSHA256For(PromptV0) || PromptSHA256() != PromptSHA256For(PromptV1) {
		t.Error("the two templates must fingerprint differently and PromptSHA256 must be the default's")
	}
}

// promptV0RecordedSHA256 is the bench-prompt-v0 fingerprint every M0-13 result
// file recorded (the twelve runs of the M0-13 qualification).
const promptV0RecordedSHA256 = "351bf8500121b668dd25c37d64382f095c168713d7f8c31f153453ae389a0d8f"

// TestPromptV0StillReproducible: v0 is what the M0-13 results ran, so its
// fingerprint must equal the one those result files recorded and a run that
// asks for v0 must render, hash and record v0. The raw result files are not
// part of every checkout; where they are, each one is checked as well.
func TestPromptV0StillReproducible(t *testing.T) {
	want := PromptSHA256For(PromptV0)
	if want != promptV0RecordedSHA256 {
		t.Errorf("bench-prompt-v0 now renders to %s, the M0-13 results recorded %s: v0 is no longer reproducible", want, promptV0RecordedSHA256)
	}
	files, err := filepath.Glob(filepath.Join(repoPath(t, "benchmarks/results/m0"), "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var r struct {
			Config struct {
				Prompt struct {
					ID     string `json:"id"`
					SHA256 string `json:"sha256"`
				} `json:"prompt"`
			} `json:"config"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if r.Config.Prompt.ID != PromptV0 || r.Config.Prompt.SHA256 != want {
			t.Errorf("%s recorded prompt %s %s, bench-prompt-v0 now renders to %s: v0 is no longer reproducible",
				filepath.Base(f), r.Config.Prompt.ID, r.Config.Prompt.SHA256, want)
		}
	}

	// A request built for v0 carries the v0 rules, byte for byte.
	c := caseByID(t, "ex-en-01")
	req, err := BuildRequest(c, PromptConfig{Role: providers.RoleController, Constraint: ConstraintGrammar, MaxToks: 64, Prompt: PromptV0})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(req.Messages[0].Content, systemRulesV0) || strings.Contains(req.Messages[0].Content, "- task: ") {
		t.Errorf("the v0 request does not carry the v0 rules:\n%s", req.Messages[0].Content)
	}
	if _, err := BuildRequest(c, PromptConfig{Role: providers.RoleController, Constraint: ConstraintGrammar, MaxToks: 64, Prompt: "bench-prompt-v9"}); err == nil {
		t.Error("an unknown prompt must be refused")
	}

	// A whole run records the template it used and that template's hash.
	corpus := realCorpus(t)
	for _, tc := range []struct{ prompt, wantID string }{{"", PromptV1}, {PromptV0, PromptV0}, {PromptV1, PromptV1}} {
		clk := clock.NewFake(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))
		cfg := testRunConfig(t, corpus, newOracle(corpus), clk)
		cfg.Limit, cfg.Warmup, cfg.Prompt = 3, 0, tc.prompt
		res, err := Run(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if res.Config.Prompt.ID != tc.wantID || res.Config.Prompt.SHA256 != PromptSHA256For(tc.wantID) {
			t.Errorf("prompt %q: the result records %+v, want %s %s", tc.prompt, res.Config.Prompt, tc.wantID, PromptSHA256For(tc.wantID))
		}
	}
	cfg := testRunConfig(t, corpus, newOracle(corpus), clock.NewFake(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)))
	cfg.Prompt = "bench-prompt-v9"
	if _, err := Run(context.Background(), cfg); err == nil {
		t.Error("a run with an unknown prompt must be refused before any case is sent")
	}
}

// grammarGoldenSHA256 is the digest of Grammar(referenceCase).
// internal/gateway/propose_test.go pins the same value for the gateway copy of
// the builder (a main package cannot be imported), so the two cannot drift.
const grammarGoldenSHA256 = "eb6ac03bdeaa91799bdcc1a4c5429206c0351b0038c8d1c6d723bc42626b6c03"

func grammarSHA256(g string) string {
	sum := sha256.Sum256([]byte(g))
	return hex.EncodeToString(sum[:])
}

func TestGrammarGolden(t *testing.T) {
	if got := grammarSHA256(Grammar(referenceCase)); got != grammarGoldenSHA256 {
		t.Errorf("Grammar(referenceCase) digest = %s, want %s (the gateway test pins the same value)", got, grammarGoldenSHA256)
	}
}

// promptV1SystemGoldenSHA256 is the digest of the system message of
// bench-prompt-v1 for referenceCase. internal/gateway/propose_test.go pins the
// same value for the gateway's copy of the template (proposeSystemGoldenSHA256).
const promptV1SystemGoldenSHA256 = "3b085d3201bb490ac4e2b78f79fd5ea9328c9117b2eb9f700f66bdec8ed78c1e"

func TestPromptV1SystemMessageGolden(t *testing.T) {
	if got := grammarSHA256(RenderSystemFor(PromptV1, referenceCase)); got != promptV1SystemGoldenSHA256 {
		t.Errorf("system message digest = %s, want %s (the gateway test pins the same value)", got, promptV1SystemGoldenSHA256)
	}
}
