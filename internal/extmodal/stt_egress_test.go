package extmodal

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/internal/providers/voice"
	vfake "github.com/buxee/alix-ai/internal/providers/voice/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// 48000 bytes of PCM16 mono at 16 kHz last 1.5 s: the reservation rounds up to 2 audio seconds.
func sttInput() VoiceInput {
	return VoiceInput{Audio: pcm(48000), Format: pcm16k, VADSegmented: true, Class: "internal"}
}

// Raw audio for an external recogniser is a governed part: the data-egress seam decides on it first (with the class
// the session states and its size, never its bytes), and only an allowed utterance is reserved on the provider budget,
// sent, and settled, against the egress decision that allowed it.
func TestExternalSpeechToTextIsGovernedThenBudgeted(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	r.on(true, ModSTT)
	r.stt.Script("book me a visit")

	// The guard refuses: nothing is reserved and nothing is sent.
	r.aeg.deny = true
	_, err := r.svc.Transcribe(ctx, scopeA, r.target, sttInput(), nil, r.stt)
	wantCode(t, err, contracts.ErrPermissionDenied, ReasonAudioDenied)
	seen, _ := r.aeg.calls()
	if len(seen) != 1 || seen[0].Class != "internal" || seen[0].Bytes != 48000 {
		t.Fatalf("the seam saw %+v", seen)
	}
	if calls, _, _ := r.budget.calls(); len(calls) != 0 || r.stt.CallCount() != 0 {
		t.Fatalf("a refused utterance was reserved (%d) or sent (%d)", len(calls), r.stt.CallCount())
	}

	// Allowed: reserved at the profile's price per input audio second times the rounded-up length, sent, settled.
	r.aeg.deny = false
	res, err := r.svc.Transcribe(ctx, scopeA, r.target, sttInput(), nil, r.stt)
	if err != nil || res.Route != RouteExternalSTT || res.Text != "book me a visit" || res.BytesSent != 48000 {
		t.Fatalf("Transcribe: %+v, %v", res, err)
	}
	_, decided := r.aeg.calls()
	calls, settled, released := r.budget.calls()
	if len(decided) != 1 || len(calls) != 1 || len(settled) != 1 || released != 0 {
		t.Fatalf("decisions %d, reserved %d, settled %d, released %d", len(decided), len(calls), len(settled), released)
	}
	wantCall(t, calls[0], pv.RoleSTT, decided[0], providerbudget.Units{AudioSecondsIn: 2, Requests: 1})
	if a := settled[0]; a.Outcome != pv.UsageRecordOutcomeSucceeded || a.Metering.AudioSecondsIn != 2 || a.Metering.Requests != 1 {
		t.Fatalf("settled %+v", a)
	}
	if r.stt.CallCount() != 1 {
		t.Fatalf("stt calls = %d", r.stt.CallCount())
	}

	// A call that fails ends its reservation: given back or settled, never left open.
	r.budget = newRecBudget()
	r.svc.d.Budgets = r.budget
	r.stt.FailNext(vfake.FailUnavailable)
	if _, err := r.svc.Transcribe(ctx, scopeA, r.target, sttInput(), nil, r.stt); err == nil {
		t.Fatal("a failing recogniser succeeded")
	}
	if calls, settled, released := r.budget.calls(); len(calls) != 1 || len(settled)+released != 1 {
		t.Fatalf("reserved %d, settled %d, released %d", len(calls), len(settled), released)
	}
}

// Without an audio egress seam, a provider budget, a price per input audio second or a known length, raw audio
// does not leave.
func TestExternalSpeechToTextWithoutGuardOrBudgetIsNotMade(t *testing.T) {
	ctx := context.Background()
	try := func(r *rig, in VoiceInput) error {
		t.Helper()
		r.on(true, ModSTT)
		r.stt.Script("hello")
		_, err := r.svc.Transcribe(ctx, scopeA, r.target, in, nil, r.stt)
		if r.stt.CallCount() != 0 || r.stt.AudioBytesReceived() != 0 {
			t.Fatalf("audio reached the recogniser: %d calls", r.stt.CallCount())
		}
		return err
	}

	r := newRig(t, func(_ *rig, d *Deps) { d.Audio = nil })
	wantCode(t, try(r, sttInput()), contracts.ErrPermissionDenied, ReasonAudioDenied)

	r = newRig(t, func(_ *rig, d *Deps) { d.Budgets = nil })
	wantCode(t, try(r, sttInput()), contracts.ErrResourceExhausted, ReasonNoBudget)

	r = newRig(t)
	r.budget.refuse = contracts.New(contracts.ErrResourceExhausted, "exhausted").WithDetails("reason", providerbudget.ReasonBudgetExhausted)
	wantCode(t, try(r, sttInput()), contracts.ErrResourceExhausted, Reason(providerbudget.ReasonBudgetExhausted))

	r = newRig(t)
	r.target.Profile.Pricing.Prices.AudioSecondsIn = nil
	wantCode(t, try(r, sttInput()), contracts.ErrResourceExhausted, ReasonNotEstimable)
	if calls, _, _ := r.budget.calls(); len(calls) != 0 {
		t.Fatalf("an unpriced call was reserved: %d", len(calls))
	}

	r = newRig(t)
	in := sttInput()
	in.Format = voice.AudioFormat{}
	wantCode(t, try(r, in), contracts.ErrResourceExhausted, ReasonNotEstimable)
}

// NewSTT hands the seam the class the session put on the call's context, and no class when it put none.
func TestNewSTTCarriesTheSessionClassToTheGuard(t *testing.T) {
	r := newRig(t)
	r.on(true, ModSTT)
	r.stt.Script("hello")
	gate := NewSTT(r.svc, r.target, nil, r.stt)
	req := voice.TranscribeRequest{Audio: pcm(32000), Format: pcm16k}
	if _, err := gate.Transcribe(WithAudioClass(WithVADSegmented(context.Background()), "confidential"), scopeA, req); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Transcribe(WithVADSegmented(context.Background()), scopeA, req); err != nil {
		t.Fatal(err)
	}
	if seen, _ := r.aeg.calls(); len(seen) != 2 || seen[0].Class != "confidential" || seen[1].Class != "" {
		t.Fatalf("the seam saw %+v", seen)
	}
}
