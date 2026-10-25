//go:build !noexternal

package cli

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/extmodal"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/providerbudget"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/providers/voice"
	vfake "github.com/buxee/alix-ai/internal/providers/voice/fake"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// extAudioSpyGuard is the real guard, with every decision it made kept for the test.
type extAudioSpyGuard struct {
	inner     dataegress.Guard
	mu        sync.Mutex
	decisions []pv.EgressDecision
}

func (g *extAudioSpyGuard) Evaluate(ctx context.Context, c dataegress.Candidate) (dataegress.Result, error) {
	res, err := g.inner.Evaluate(ctx, c)
	g.mu.Lock()
	g.decisions = append(g.decisions, res.Decision)
	g.mu.Unlock()
	return res, err
}

func (g *extAudioSpyGuard) last() pv.EgressDecision {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.decisions[len(g.decisions)-1]
}

// External speech to text over the real guard and the real budget ledger: a policy that lets text leave does not let
// audio leave, audio of no stated class never leaves, and audio the audio setting allows is reserved, sent and settled,
// with a usage record that names the egress decision of the audio.
func TestExternalSpeechToTextAudioIsGovernedThenBudgeted(t *testing.T) {
	a := externalTestAdmin(t, filepath.Join(t.TempDir(), "external"), nil)
	ctx := context.Background()
	scope := externalTestScope("ten_acme", "usr_member")
	prof := extEgressProfile(t)
	prof.Modalities.Input = append(prof.Modalities.Input, pv.ModalityAudio)
	prof.Media = &pv.ProviderProfileMedia{Audio: &pv.ProviderProfileMediaHandling{MaxBytes: 1 << 20, MimeTypes: []string{"audio/wav"}, Retention: pv.RetentionModeTransient}}
	prof.Voice = pv.ProviderProfileVoice{STT: true, AudioRetention: pv.RetentionModeTransient}
	prof.Pricing.Prices.AudioSecondsIn = &pv.PriceTablePrice{PerUnits: 60, MicroUnits: 6000}

	spy := &extAudioSpyGuard{inner: a.egress}
	mp := extmodal.NewMemoryPolicy()
	mp.SetProvider(string(prof.ProviderID), extmodal.Enable(true, extmodal.ModSTT))
	mp.SetTenant(scope, extmodal.Enable(true, extmodal.ModSTT))
	svc, err := extmodal.New(extmodal.Deps{Policy: mp, Egress: extWireTextEgress{guard: spy}, Audio: extAudioEgress{guard: spy}, Budgets: a.budget})
	if err != nil {
		t.Fatal(err)
	}
	target := extmodal.Target{Profile: prof, Caps: pfake.DefaultCapabilities(string(prof.ProviderID), string(prof.ModelID))}
	stt := vfake.New()
	stt.Script("book me a visit")
	in := extmodal.VoiceInput{Audio: make([]byte, 48000), Format: voice.PCM16Mono(16000), VADSegmented: true, Class: "internal"}
	if _, err := a.PutBudget(ctx, externalTestScope("ten_acme", "usr_admin"), pv.Budget{
		Subject: pv.BudgetSubject{Kind: pv.BudgetSubjectKindTenant, ApplicationID: new(common.Identifier("app_demo")), TenantID: new(common.Identifier("ten_acme"))},
		Channel: pv.ChannelInteractive, Currency: prof.Pricing.Currency, WarnAtPercent: 80,
		Limits: pv.BudgetLimits{DailyMicroUnits: 1_000_000, MonthlyMicroUnits: 10_000_000},
	}); err != nil {
		t.Fatal(err)
	}
	usage := func() []providerbudget.UsageRow {
		t.Helper()
		exp, err := a.budget.ExportUsage(ctx, scope, providerbudget.UsageFilter{})
		if err != nil {
			t.Fatal(err)
		}
		return exp.Rows
	}

	// The policy lets internal text go; there is no audio setting, so the audio stays, with a recorded denial.
	extEgressPolicies(t, a, prof, nil)
	if _, err := (extWireTextEgress{guard: spy}).AllowText(ctx, scope, target, extmodal.PurposeVisionAsk, "book me a visit"); err != nil {
		t.Fatalf("the text: %v", err)
	}
	_, err = svc.Transcribe(ctx, scope, target, in, nil, stt)
	if extmodal.ReasonOf(err) != extmodal.ReasonAudioDenied || stt.CallCount() != 0 || len(usage()) != 0 {
		t.Fatalf("audio under a text-only policy: %v, stt %d, usage %d", err, stt.CallCount(), len(usage()))
	}
	if d := spy.last(); d.Outcome != pv.EgressDecisionOutcomeDeny || d.Checks[len(d.Checks)-1].Name != pv.EgressDecisionCheckNameAudio {
		t.Fatalf("decision %+v", d)
	}

	// The audio setting lets internal audio go: reserved, sent, settled, recorded against its decision.
	extEgressPolicies(t, a, prof, func(in *gateway.ProvidersEgressPolicyInput) {
		in.Audio = &pv.EgressPolicyContentEgress{Enabled: true, MaxClass: pv.EgressEligibleClassInternal}
	})
	res, err := svc.Transcribe(ctx, scope, target, in, nil, stt)
	if err != nil || res.Route != extmodal.RouteExternalSTT || stt.CallCount() != 1 {
		t.Fatalf("Transcribe: %+v, %v, stt %d", res, err, stt.CallCount())
	}
	allowed := spy.last()
	rows := usage()
	if allowed.Outcome != pv.EgressDecisionOutcomeAllow || len(rows) != 1 {
		t.Fatalf("decision %s, usage %d", allowed.Outcome, len(rows))
	}
	if r := rows[0].Record; r.Role != pv.RoleSTT || r.DecisionID != allowed.DecisionID || r.Outcome != pv.UsageRecordOutcomeSucceeded ||
		r.Metering.AudioSecondsIn != 2 || rows[0].ChargedMicroUnits <= 0 {
		t.Fatalf("usage %+v, charged %d, want the decision %s", r, rows[0].ChargedMicroUnits, allowed.DecisionID)
	}

	// Audio whose session states no class is unclassified: refused, and nothing more is reserved or sent.
	in.Class = ""
	_, err = svc.Transcribe(ctx, scope, target, in, nil, stt)
	if extmodal.ReasonOf(err) != extmodal.ReasonAudioDenied || stt.CallCount() != 1 || len(usage()) != 1 {
		t.Fatalf("unclassified audio: %v, stt %d, usage %d", err, stt.CallCount(), len(usage()))
	}
	if d := spy.last(); d.Reason != pv.EgressDecisionReasonUnclassifiedData {
		t.Fatalf("decision %+v", d)
	}
}
