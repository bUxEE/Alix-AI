package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/dialogue/scopegate"
	"github.com/buxee/alix-ai/internal/providers/fake"
)

// oracleProvider answers every case with the proposal it expects, so the only difference a
// gated run can make is the gate's.
func oracleProvider(corpus *Corpus) *fake.Provider {
	return newOracle(corpus)
}

func gatedIDs(corpus *Corpus) map[string]bool {
	out := map[string]bool{}
	for _, c := range corpus.Cases {
		if _, refused := gateAnswer(c); refused {
			out[c.ID] = true
		}
	}
	return out
}

func runWithGate(t *testing.T, corpus *Corpus, gate bool) (*Result, *fake.Provider) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))
	p := oracleProvider(corpus)
	cfg := testRunConfig(t, corpus, p, clk)
	cfg.Constraint, cfg.Prompt, cfg.Gate = ConstraintGrammar, PromptV1, gate
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res, p
}

func TestGateRunSkipsTheModelAndScoresTheRefusal(t *testing.T) {
	corpus := realCorpus(t)
	gated := gatedIDs(corpus)
	if len(gated) == 0 {
		t.Fatal("the gate refused nothing in the corpus: this test would measure nothing")
	}
	res, p := runWithGate(t, corpus, true)

	// No request was sent for a gated case, and exactly one was sent for every other case
	// plus the warm-up (which bypasses the gate).
	if want := len(corpus.Cases) - len(gated) + 1; p.Calls() != want {
		t.Fatalf("%d requests reached the model, want %d (%d cases, %d gated, 1 warm-up)", p.Calls(), want, len(corpus.Cases), len(gated))
	}
	sent := map[string]int{}
	for _, pr := range p.Prompts() {
		sent[pr.Scope.SessionID]++
	}
	for id := range gated {
		if sent[id] != 0 {
			t.Errorf("gated case %s was sent to the model", id)
		}
	}

	refusal := string(scopegate.Refusal{}.ProposalJSON())
	proposal, resultSchema, _ := realSchemas(t)
	falseRefusals := 0
	for _, o := range res.Cases {
		if !gated[o.ID] {
			continue
		}
		if o.Status != StatusOK || o.Output == nil || *o.Output != refusal {
			t.Errorf("%s: gated case output %v", o.ID, o.Output)
		}
		if o.ToksIn == nil || *o.ToksIn != 0 || o.ToksOut == nil || *o.ToksOut != 0 || o.ToksEstimated || o.PromptToksEst != 0 {
			t.Errorf("%s: a gated case reports tokens: in %v out %v estimated %v prompt %d", o.ID, o.ToksIn, o.ToksOut, o.ToksEstimated, o.PromptToksEst)
		}
		if o.FinishReason == nil || *o.FinishReason != "stop" || !o.SchemaValid {
			t.Errorf("%s: finish %v, schema valid %v (%v)", o.ID, o.FinishReason, o.SchemaValid, o.SchemaError)
		}
		if o.CapabilityOK != nil {
			// A refused case that expects a capability is a routing miss: that is the cost the
			// protocol counts against the gate.
			if *o.CapabilityOK {
				t.Errorf("%s: a refusal credited as a capability match", o.ID)
			}
			falseRefusals++
		}
	}
	// Re-score the typed refusal with the real scorer against the real schema: it is a valid
	// out_of_scope proposal, which is what makes a gated refusal count as a refusal.
	for _, c := range corpus.Cases {
		if c.Category != CatRefusal {
			continue
		}
		s := refusal
		if o := ScoreCase(c, &s, "", proposal); !o.SchemaValid || o.RefusalOK == nil || !*o.RefusalOK {
			t.Fatalf("the typed refusal is not a correct refusal of %s: %+v", c.ID, o)
		}
		break
	}

	// The oracle is perfect, so every miss in this run is the gate's: routing loses exactly the
	// false refusals, refusals and injection keep every case.
	m := res.Metrics
	if want := 54 - falseRefusals; m.CapabilityRouting.Total != 54 || m.CapabilityRouting.Passed != want {
		t.Errorf("routing %d/%d, want %d/54 (%d false refusals)", m.CapabilityRouting.Passed, m.CapabilityRouting.Total, want, falseRefusals)
	}
	if m.RefusalCorrectness.Passed != 16 || m.InjectionResistance.Passed != 16 || m.SchemaValid.Passed != 80 {
		t.Errorf("refusal %d, injection %d, schema %d: a refusal must never cost those lines", m.RefusalCorrectness.Passed, m.InjectionResistance.Passed, m.SchemaValid.Passed)
	}
	if problems := VerifyResult(res); len(problems) > 0 {
		t.Fatalf("a gated result fails its own check: %v", problems)
	}
	data, err := marshalResult(res)
	if err != nil {
		t.Fatal(err)
	}
	if problems := validateResultBytes(resultSchema, data); len(problems) > 0 {
		t.Fatalf("a gated result does not validate: %v", problems)
	}
}

func TestGateOffAndOnSendTheSameRequestToTheModel(t *testing.T) {
	corpus := realCorpus(t)
	gated := gatedIDs(corpus)
	_, off := runWithGate(t, corpus, false)
	_, on := runWithGate(t, corpus, true)
	if off.Calls() != len(corpus.Cases)+1 {
		t.Fatalf("with the gate off, %d requests reached the model, want %d", off.Calls(), len(corpus.Cases)+1)
	}
	byCase := func(p *fake.Provider) map[string]fake.Prompt {
		m := map[string]fake.Prompt{}
		for _, pr := range p.Prompts() {
			m[pr.Scope.SessionID] = pr // the last request of a case wins; the warm-up case is sent twice with the same content
		}
		return m
	}
	a, b := byCase(off), byCase(on)
	checked := 0
	for id, pa := range a {
		if gated[id] {
			continue
		}
		pb, ok := b[id]
		if !ok {
			t.Fatalf("case %s was sent with the gate off and not with it on, and it is not gated", id)
		}
		if d := cmp.Diff(pa.Chat, pb.Chat); d != "" {
			t.Fatalf("the request for %s differs with the gate on (the gate must not change what the model sees):\n%s", id, d)
		}
		checked++
	}
	if checked < 60 {
		t.Fatalf("only %d requests were compared", checked)
	}
}

func TestGateWarmupBypassesTheGate(t *testing.T) {
	full := realCorpus(t)
	var first, second Case
	for _, c := range full.Cases {
		switch c.ID {
		case "rf-en-01":
			first = c
		case "rt-en-01":
			second = c
		}
	}
	if _, refused := gateAnswer(first); !refused {
		t.Fatal("rf-en-01 is not refused by the gate: pick another case for this test")
	}
	corpus := &Corpus{ID: full.ID, SHA256: full.SHA256, Cases: []Case{first, second}}
	clk := clock.NewFake(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))
	p := oracleProvider(corpus)
	cfg := testRunConfig(t, corpus, p, clk)
	cfg.Constraint, cfg.Prompt, cfg.Gate = ConstraintGrammar, PromptV1, true
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, pr := range p.Prompts() {
		ids = append(ids, pr.Scope.SessionID)
	}
	// The warm-up is case 0 and is sent although the gate refuses it; the measured case 0 is
	// not sent; case 1 is.
	if got, want := strings.Join(ids, ","), "rf-en-01,rt-en-01"; got != want {
		t.Fatalf("requests %q, want %q", got, want)
	}
}

func TestGateReportCountsEveryCaseOnce(t *testing.T) {
	corpus := realCorpus(t)
	rep := BuildGateReport(corpus, 0)
	total, refused := 0, 0
	for name, g := range rep.Groups {
		if g.Refused+g.Passed+g.Abstained != g.Cases {
			t.Errorf("group %s: %d refused + %d passed + %d abstained != %d cases", name, g.Refused, g.Passed, g.Abstained, g.Cases)
		}
		if len(g.RefusedID) != g.Refused || len(g.MissedID) != g.Passed+g.Abstained {
			t.Errorf("group %s: %d refused ids for %d refusals, %d not-refused ids for %d cases", name, len(g.RefusedID), g.Refused, len(g.MissedID), g.Passed+g.Abstained)
		}
		total += g.Cases
		refused += g.Refused
	}
	if total != 80 || len(rep.Cases) != 80 {
		t.Fatalf("the report covers %d cases (%d rows), want 80", total, len(rep.Cases))
	}
	want := map[string]int{gateGroupRefusal: 16, gateGroupInjectionOS: 6, gateGroupCapability: 44, gateGroupInjectionTK: 10, gateGroupControl: 4}
	for name, n := range want {
		if rep.Groups[name].Cases != n {
			t.Errorf("group %s has %d cases, want %d", name, rep.Groups[name].Cases, n)
		}
	}
	falseRefusals := rep.Groups[gateGroupCapability].Refused + rep.Groups[gateGroupInjectionTK].Refused
	if rep.FalseRefusalsAgainstRouting != falseRefusals {
		t.Errorf("false refusals %d, groups say %d", rep.FalseRefusalsAgainstRouting, falseRefusals)
	}
	if rep.Gate.ID != scopegate.ID || rep.Gate.Digest != scopegate.Digest() {
		t.Error("the report does not name the gate that ran")
	}
	// Deterministic: two reports (without the host timing) are equal.
	if d := cmp.Diff(rep, BuildGateReport(corpus, 0)); d != "" {
		t.Errorf("the gate report is not deterministic:\n%s", d)
	}
}

func TestGateCommandPrintsTheReport(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"gate", "--corpus", repoPath(t, corpusRel), "--timing-passes", "0"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var rep GateReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("the report is not JSON: %v", err)
	}
	if rep.Corpus.Cases != 80 || len(rep.Cases) != 80 || rep.Gate.ID != scopegate.ID {
		t.Fatalf("report: %d cases, gate %q", len(rep.Cases), rep.Gate.ID)
	}
	if code := run([]string{"gate"}, &out, &errb); code != 2 {
		t.Fatalf("a missing --corpus gave exit %d, want 2", code)
	}
}
