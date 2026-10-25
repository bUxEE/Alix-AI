package extmodal

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// wantCall checks one reservation: the profile, the role, the interactive channel, the egress decision of the text
// that left, the profile's prices, and the upper bound of the call.
func wantCall(t *testing.T, got providerbudget.Call, role pv.Role, d TextDecision, units providerbudget.Units) {
	t.Helper()
	p := fullProfile()
	ref := pv.ProviderRef{ProfileID: p.ProfileID, ProviderID: p.ProviderID, ModelID: p.ModelID, ModelVersion: p.ModelVersion}
	if got.Provider != ref || got.Role != role || got.Channel != pv.ChannelInteractive || got.EnvelopeID != d.EnvelopeID ||
		got.DecisionID != d.DecisionID || got.Prices.Currency != p.Pricing.Currency || got.Units != units {
		t.Fatalf("reservation %+v, want role %s, decision %+v, units %+v", got, role, d, units)
	}
}

// Every external call of a multimodal role is reserved on the provider budget before it is made, as the text router
// does, and settled with what it consumed.
func TestExternalMultimodalCallsAreReservedThenSettled(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	r.on(false, ModVision, ModImageGeneration, ModTTS)

	// Vision: the prompt, a fixed estimate per image and the output cap (the smaller of the profile's and the
	// adapter's), which is also what the provider is asked for.
	img := r.upload("a.png", "image/png", pngBytes())
	r.appr.Grant(scopeA, string(img.FileID), provID, Approval{Approved: true})
	r.gen.On(fake.Any()).Reply("a small square")
	if _, err := r.svc.Describe(ctx, scopeA, r.target, r.gen, VisionRequest{Prompt: "describe it", Files: []FileUse{{FileID: string(img.FileID)}}}, IngestOptions{}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	sent := r.gen.Prompts()[0].Rich
	if sent.MaxToks != 1024 {
		t.Fatalf("the provider was asked for %d toks, want the bound that was reserved", sent.MaxToks)
	}
	calls, settled, released := r.budget.calls()
	if len(calls) != 1 || len(settled) != 1 || released != 0 {
		t.Fatalf("reserved %d, settled %d, released %d", len(calls), len(settled), released)
	}
	wantCall(t, calls[0], pv.RoleVision, r.eg.decided()[0], providerbudget.Units{
		ToksIn: pv.Counter(providers.Budget{}.EstimateRich(sent)), ToksOut: 1024, ImagesIn: 1, Requests: 1,
	})
	if a := settled[0]; a.Outcome != pv.UsageRecordOutcomeSucceeded || a.Metering.ImagesIn != 1 || a.Metering.Requests != 1 {
		t.Fatalf("settled %+v", a)
	}

	// Image generation: the prompt, the most images the profile lets a reply carry and, since the profile prices
	// text output, the output cap.
	r.budget = newRecBudget()
	r.svc.d.Budgets = r.budget
	r.gen.On(fake.Any()).ReplyParts(imagePart(pngBytes()))
	if _, err := r.svc.GenerateImage(ctx, scopeA, r.target, r.gen, "a small square", IngestOptions{}); err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	sent = r.gen.Prompts()[1].Rich
	calls, settled, _ = r.budget.calls()
	if len(calls) != 1 || len(settled) != 1 {
		t.Fatalf("reserved %d, settled %d", len(calls), len(settled))
	}
	wantCall(t, calls[0], pv.RoleImageGeneration, r.eg.decided()[1], providerbudget.Units{
		ToksIn: pv.Counter(providers.Budget{}.EstimateRich(sent)), ToksOut: 1024, ImagesOut: 2, Requests: 1,
	})
	if a := settled[0]; a.Outcome != pv.UsageRecordOutcomeSucceeded || a.Metering.ImagesOut != 1 {
		t.Fatalf("settled %+v", a)
	}

	// Speech: the text, reserved before the provider is called and settled when the stream ends.
	r.budget = newRecBudget()
	r.svc.d.Budgets = r.budget
	const spoken = "Your visit is booked."
	if err := r.svc.Attest(scopeA, verifiedOutcome(spoken)); err != nil {
		t.Fatal(err)
	}
	st, err := NewTTS(r.svc, r.target, r.tts).SynthesizeStream(ctx, scopeA, voice.SynthesizeRequest{Text: spoken})
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}
	calls, settled, _ = r.budget.calls()
	if len(calls) != 1 || len(settled) != 0 {
		t.Fatalf("before the stream ends: reserved %d, settled %d", len(calls), len(settled))
	}
	wantCall(t, calls[0], pv.RoleTTS, r.eg.decided()[2], providerbudget.Units{ToksIn: pv.Counter(providers.EstimateTokens(spoken)), Requests: 1})
	if _, err := voice.CollectAudio(st, 1<<20); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if _, settled, _ = r.budget.calls(); len(settled) != 1 || settled[0].Outcome != pv.UsageRecordOutcomeSucceeded {
		t.Fatalf("after the stream: settled %+v", settled)
	}
}

// No budget, a budget that refuses, or a call that cannot be bounded: nothing reaches the provider, as for text.
func TestExternalMultimodalCallWithoutBudgetIsNotMade(t *testing.T) {
	ctx := context.Background()
	const spoken = "Your visit is booked."
	try := func(r *rig) (describe, generate, speak error) {
		t.Helper()
		img := r.upload("a.png", "image/png", pngBytes())
		r.appr.Grant(scopeA, string(img.FileID), provID, Approval{Approved: true})
		r.gen.On(fake.Any()).Reply("a small square")
		_ = r.svc.Attest(scopeA, verifiedOutcome(spoken))
		_, describe = r.svc.Describe(ctx, scopeA, r.target, r.gen, VisionRequest{Prompt: "describe it", Files: []FileUse{{FileID: string(img.FileID)}}}, IngestOptions{})
		_, generate = r.svc.GenerateImage(ctx, scopeA, r.target, r.gen, "a small square", IngestOptions{})
		_, speak = NewTTS(r.svc, r.target, r.tts).SynthesizeStream(ctx, scopeA, voice.SynthesizeRequest{Text: spoken})
		if r.gen.Calls() != 0 || r.tts.CallCount() != 0 {
			t.Fatalf("a provider was called: gen=%d tts=%d", r.gen.Calls(), r.tts.CallCount())
		}
		return describe, generate, speak
	}

	r := newRig(t, func(_ *rig, d *Deps) { d.Budgets = nil })
	r.on(false, ModVision, ModImageGeneration, ModTTS)
	for _, err := range func() []error { a, b, c := try(r); return []error{a, b, c} }() {
		wantCode(t, err, contracts.ErrResourceExhausted, ReasonNoBudget)
	}

	r = newRig(t)
	r.on(false, ModVision, ModImageGeneration, ModTTS)
	r.budget.refuse = contracts.New(contracts.ErrResourceExhausted, "exhausted").WithDetails("reason", providerbudget.ReasonBudgetExhausted)
	for _, err := range func() []error { a, b, c := try(r); return []error{a, b, c} }() {
		wantCode(t, err, contracts.ErrResourceExhausted, Reason(providerbudget.ReasonBudgetExhausted))
	}

	// The audio a speech call returns has no bound before the call, so a provider that prices it is not called.
	r = newRig(t)
	r.on(false, ModTTS)
	r.target.Profile.Pricing.Prices.AudioSecondsOut = &pv.PriceTablePrice{PerUnits: 60, MicroUnits: 15000}
	_ = r.svc.Attest(scopeA, verifiedOutcome(spoken))
	_, err := NewTTS(r.svc, r.target, r.tts).SynthesizeStream(ctx, scopeA, voice.SynthesizeRequest{Text: spoken})
	wantCode(t, err, contracts.ErrResourceExhausted, ReasonNotEstimable)
	if calls, _, _ := r.budget.calls(); len(calls) != 0 || r.tts.CallCount() != 0 {
		t.Fatalf("reserved %d, tts %d", len(calls), r.tts.CallCount())
	}
}

// failingGen is a generator whose every call fails with err.
type failingGen struct{ err error }

func (g failingGen) Generate(context.Context, contracts.Scope, providers.RichRequest) (providers.RichResult, error) {
	return providers.RichResult{}, g.err
}

func (g failingGen) GenerateStream(context.Context, contracts.Scope, providers.RichRequest) (providers.RichStream, error) {
	return nil, g.err
}

// A failed call that may have reached the provider settles at the reserved bound (uncertain); one the adapter
// refused before sending gives the reservation back.
func TestFailedExternalMultimodalCallSettlesOrReleases(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	r.on(false, ModImageGeneration)
	r.gen.On(fake.Any()).Fail(fake.FailUnavailable)
	if _, err := r.svc.GenerateImage(ctx, scopeA, r.target, r.gen, "a small square", IngestOptions{}); err == nil {
		t.Fatal("a failed call succeeded")
	}
	calls, settled, released := r.budget.calls()
	if len(calls) != 1 || len(settled) != 1 || released != 0 || settled[0].Outcome != pv.UsageRecordOutcomeUncertain ||
		settled[0].ErrorKind == nil || *settled[0].ErrorKind != pv.ProviderErrorKindUnavailable {
		t.Fatalf("reserved %d, settled %+v, released %d", len(calls), settled, released)
	}

	r.budget = newRecBudget()
	r.svc.d.Budgets = r.budget
	bad := failingGen{contracts.New(contracts.ErrValidationFailed, "refused before sending")}
	if _, err := r.svc.GenerateImage(ctx, scopeA, r.target, bad, "a small square", IngestOptions{}); err == nil {
		t.Fatal("a failed call succeeded")
	}
	if calls, settled, released := r.budget.calls(); len(calls) != 1 || len(settled) != 0 || released != 1 {
		t.Fatalf("reserved %d, settled %d, released %d", len(calls), len(settled), released)
	}
}
