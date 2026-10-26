package extmodal

import (
	"context"
	"testing"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func TestTTSReserveUnitsCharactersPreferredThenToksThenRefuse(t *testing.T) {
	price := func(per, micro int64) *pv.PriceTablePrice {
		return &pv.PriceTablePrice{PerUnits: per, MicroUnits: pv.MicroUnits(micro)}
	}
	const text = "Booked."
	chars := pv.Counter(utf8.RuneCountInString(text))

	u, ok := ttsReserveUnits(pv.PriceTable{Prices: pv.PriceTablePrices{
		CharactersIn: price(1, 10), ToksIn: price(1000, 1000),
	}}, text)
	if !ok || u.CharactersIn == nil || *u.CharactersIn != chars || u.ToksIn != 0 || u.Requests != 1 {
		t.Fatalf("characters preferred: %+v ok=%v", u, ok)
	}

	u, ok = ttsReserveUnits(pv.PriceTable{Prices: pv.PriceTablePrices{ToksIn: price(1000, 1000)}}, text)
	wantToks := pv.Counter(providers.EstimateTokens(text))
	if !ok || u.CharactersIn != nil || u.ToksIn != wantToks || u.Requests != 1 {
		t.Fatalf("legacy toks: %+v ok=%v", u, ok)
	}

	u, ok = ttsReserveUnits(pv.PriceTable{Prices: pv.PriceTablePrices{}}, text)
	if ok || u.CharactersIn != nil || u.ToksIn != 0 {
		t.Fatalf("unpriced must refuse: %+v ok=%v", u, ok)
	}
}

func TestExternalTTSRefusesUnpricedProfile(t *testing.T) {
	ctx := context.Background()
	const spoken = "Your visit is booked."
	r := newRig(t)
	r.on(false, ModTTS)
	r.target.Profile.Pricing.Prices = pv.PriceTablePrices{} // no characters_in, no toks_in
	if err := r.svc.Attest(scopeA, verifiedOutcome(spoken)); err != nil {
		t.Fatal(err)
	}
	_, err := NewTTS(r.svc, r.target, r.tts).SynthesizeStream(ctx, scopeA, voice.SynthesizeRequest{Text: spoken})
	wantCode(t, err, contracts.ErrResourceExhausted, ReasonNotEstimable)
	if r.tts.CallCount() != 0 {
		t.Fatalf("provider called %d times", r.tts.CallCount())
	}
	if calls, _, _ := r.budget.calls(); len(calls) != 0 {
		t.Fatalf("reserved %d", len(calls))
	}
}

func TestExternalTTSReservesCharactersWhenPriced(t *testing.T) {
	ctx := context.Background()
	const spoken = "Your visit is booked."
	r := newRig(t)
	r.on(false, ModTTS)
	price := &pv.PriceTablePrice{PerUnits: 1, MicroUnits: 15}
	r.target.Profile.Pricing.Prices = pv.PriceTablePrices{CharactersIn: price}
	if err := r.svc.Attest(scopeA, verifiedOutcome(spoken)); err != nil {
		t.Fatal(err)
	}
	st, err := NewTTS(r.svc, r.target, r.tts).SynthesizeStream(ctx, scopeA, voice.SynthesizeRequest{Text: spoken})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	calls, _, _ := r.budget.calls()
	if len(calls) != 1 {
		t.Fatalf("reserved %d", len(calls))
	}
	n := pv.Counter(utf8.RuneCountInString(spoken))
	got := calls[0].Units
	if got.CharactersIn == nil || *got.CharactersIn != n || got.ToksIn != 0 || got.Requests != 1 {
		t.Fatalf("units %+v, want characters_in=%d requests=1", got, n)
	}
}
