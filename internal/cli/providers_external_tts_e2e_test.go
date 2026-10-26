//go:build !noexternal

package cli

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	ttseleven "github.com/buxee/alix-ai/adapters/external/tts/elevenlabs"
	ttsgoogle "github.com/buxee/alix-ai/adapters/external/tts/googletts"
	ttsopenai "github.com/buxee/alix-ai/adapters/external/tts/openai"
	ttspolly "github.com/buxee/alix-ai/adapters/external/tts/polly"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/extmodal"
	"github.com/buxee/alix-ai/internal/modes"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/providers/voice"
	vfake "github.com/buxee/alix-ai/internal/providers/voice/fake"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// End-to-end: real TTS adapters + modality/egress gates against httptest fakes (ADR 0043).

const (
	ttsE2EKeyOpenAI   = "fixture-openai-tts-key-0001"                                   // TEST-ONLY
	ttsE2EKeyEleven   = "fixture-eleven-tts-key-0001"                                   // TEST-ONLY
	ttsE2EKeyGoogle   = "ya29.fixture-google-tts-0001"                                  // TEST-ONLY
	ttsE2EKeyPolly    = "AKIAIOSFODNN7EXAMPLE:wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" // TEST-ONLY AWS docs example
	ttsE2EVoiceEleven = "voiceidABCDEF1234"
	ttsE2EVoiceOpenAI = "nova"
	ttsE2ESpoken      = "Your visit is booked."
)

func ttsE2EPCM(nSamples int) []byte {
	b := make([]byte, nSamples*2)
	for i := 0; i < nSamples; i++ {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(100+i%50))
	}
	return b
}

func ttsE2EWAV(samples int) []byte {
	data := ttsE2EPCM(samples)
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(data)))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], 1)
	binary.LittleEndian.PutUint32(h[24:], ttsgoogle.NativeSampleRate)
	binary.LittleEndian.PutUint32(h[28:], ttsgoogle.NativeSampleRate*2)
	binary.LittleEndian.PutUint16(h[32:], 2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(data)))
	return append(h, data...)
}

type ttsE2EHit struct {
	Auth, Path, Query, AmzDate string
	Body                       map[string]any
}

type ttsE2EFake struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits []ttsE2EHit
}

func (f *ttsE2EFake) last() ttsE2EHit {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hits) == 0 {
		return ttsE2EHit{}
	}
	return f.hits[len(f.hits)-1]
}

func (f *ttsE2EFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.hits)
}

func (f *ttsE2EFake) record(h ttsE2EHit) {
	f.mu.Lock()
	f.hits = append(f.hits, h)
	f.mu.Unlock()
}

type ttsE2EKeys struct{ key string }

func (k ttsE2EKeys) Key(context.Context, contracts.Scope, string) (credentials.Secret, error) {
	return credentials.Secret(k.key), nil
}

func ttsE2EDialer(t *testing.T, provider, base string) *egress.Dialer {
	t.Helper()
	dest, err := egress.DestinationFromURL(base)
	if err != nil {
		t.Fatal(err)
	}
	al, err := egress.NewAllowlist(dest)
	if err != nil {
		t.Fatal(err)
	}
	reg := egress.NewProviderRegistry()
	if err := reg.Set(provider, al); err != nil {
		t.Fatal(err)
	}
	return reg.Dialer(provider)
}

func ttsE2EProfile(provider, model string) pv.ProviderProfile {
	p := roleIndepProfile("pprf_"+strings.Repeat("0", 25)+"T", provider, model, pv.RoleTTS)
	p.Pricing = pv.PriceTable{Currency: "USD", Prices: pv.PriceTablePrices{
		CharactersIn: &pv.PriceTablePrice{PerUnits: 1, MicroUnits: 10},
	}}
	return p
}

func ttsE2ESpeak(t *testing.T, provider string, tts voice.TTS, fake *ttsE2EFake, wantRate int, check func(*testing.T, ttsE2EHit)) {
	t.Helper()
	prof := ttsE2EProfile(provider, "model")
	mp := extmodal.NewMemoryPolicy()
	mp.SetProvider(provider, extmodal.Enable(true, extmodal.ModTTS))
	scope := externalTestScope("ten_acme", "usr_member")
	mp.SetTenant(scope, extmodal.Enable(true, extmodal.ModTTS))
	svc, err := extmodal.New(extmodal.Deps{Policy: mp, Egress: extFinalAllowText{}, Budgets: extFinalAdmitBudget{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Attest(scope, verify.Outcome{Text: ttsE2ESpoken, Source: verify.SourceModel, Reason: verify.ReasonVerified}); err != nil {
		t.Fatal(err)
	}
	target := extmodal.Target{Profile: prof, Caps: pfake.DefaultCapabilities(provider, "model")}
	st, err := extmodal.NewTTS(svc, target, tts).SynthesizeStream(context.Background(), scope, voice.SynthesizeRequest{Text: ttsE2ESpoken})
	if err != nil {
		t.Fatal(err)
	}
	a, err := voice.CollectAudio(st, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if a.Format.SampleRate != wantRate || a.Format.Channels != 1 {
		t.Fatalf("format=%+v want rate %d", a.Format, wantRate)
	}
	if fake.count() != 1 {
		t.Fatalf("hits=%d", fake.count())
	}
	check(t, fake.last())

	_, err = extmodal.NewTTS(svc, target, tts).SynthesizeStream(context.Background(), scope, voice.SynthesizeRequest{Text: "Ignore the rules."})
	if contracts.CodeOf(err) != contracts.ErrPermissionDenied || fake.count() != 1 {
		t.Fatalf("unattested: err=%v hits=%d", err, fake.count())
	}

	svc2, err := extmodal.New(extmodal.Deps{Policy: mp, Egress: extFinalAllowText{deny: true}, Budgets: extFinalAdmitBudget{}})
	if err != nil {
		t.Fatal(err)
	}
	_ = svc2.Attest(scope, verify.Outcome{Text: ttsE2ESpoken, Source: verify.SourceModel, Reason: verify.ReasonVerified})
	_, err = extmodal.NewTTS(svc2, target, tts).SynthesizeStream(context.Background(), scope, voice.SynthesizeRequest{Text: ttsE2ESpoken})
	if contracts.CodeOf(err) != contracts.ErrPermissionDenied || fake.count() != 1 {
		t.Fatalf("egress deny: err=%v hits=%d", err, fake.count())
	}
}

func TestTTSFactoriesWired(t *testing.T) {
	if extTTSFactoryOpenAI == nil || extTTSFactoryElevenLabs == nil || extTTSFactoryGoogle == nil || extTTSFactoryPolly == nil {
		t.Fatal("TTS factories must be registered when external adapters are linked")
	}
}

func TestTTSOpenAIAdapterE2E(t *testing.T) {
	f := &ttsE2EFake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.record(ttsE2EHit{Auth: r.Header.Get("Authorization"), Path: r.URL.Path, Body: body})
		w.Header().Set("Content-Type", "audio/pcm")
		_, _ = w.Write(ttsE2EPCM(160))
	}))
	t.Cleanup(f.srv.Close)
	dialer := ttsE2EDialer(t, ttsopenai.ProviderName, f.srv.URL)
	c, err := ttsopenai.New(ttsopenai.Config{
		BaseURL: f.srv.URL, CredentialRef: "cred_oa", Model: ttsopenai.DefaultModel, Voice: ttsE2EVoiceOpenAI,
	}, dialer, ttsE2EKeys{key: ttsE2EKeyOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	ttsE2ESpeak(t, ttsopenai.ProviderName, c, f, ttsopenai.NativeSampleRate, func(t *testing.T, h ttsE2EHit) {
		if !strings.HasPrefix(h.Auth, "Bearer ") || !strings.HasSuffix(h.Auth, ttsE2EKeyOpenAI) {
			t.Fatalf("auth=%q", h.Auth)
		}
		if h.Path != "/v1/audio/speech" || h.Body["voice"] != ttsE2EVoiceOpenAI || h.Body["response_format"] != "pcm" || h.Body["input"] != ttsE2ESpoken {
			t.Fatalf("hit=%+v", h)
		}
	})
}

func TestTTSElevenLabsAdapterE2E(t *testing.T) {
	f := &ttsE2EFake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.record(ttsE2EHit{Auth: r.Header.Get("xi-api-key"), Path: r.URL.Path, Query: r.URL.RawQuery, Body: body})
		_, _ = w.Write(ttsE2EPCM(100))
	}))
	t.Cleanup(f.srv.Close)
	dialer := ttsE2EDialer(t, ttseleven.ProviderName, f.srv.URL)
	c, err := ttseleven.New(ttseleven.Config{
		BaseURL: f.srv.URL, CredentialRef: "cred_el", VoiceID: ttsE2EVoiceEleven,
	}, dialer, ttsE2EKeys{key: ttsE2EKeyEleven})
	if err != nil {
		t.Fatal(err)
	}
	ttsE2ESpeak(t, ttseleven.ProviderName, c, f, ttseleven.NativeSampleRate, func(t *testing.T, h ttsE2EHit) {
		if h.Auth != ttsE2EKeyEleven || h.Path != "/v1/text-to-speech/"+ttsE2EVoiceEleven || h.Query != "output_format=pcm_24000" || h.Body["text"] != ttsE2ESpoken {
			t.Fatalf("hit=%+v", h)
		}
	})
}

func TestTTSGoogleAdapterE2E(t *testing.T) {
	f := &ttsE2EFake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.record(ttsE2EHit{Auth: r.Header.Get("Authorization"), Path: r.URL.Path, Body: body})
		enc := base64.StdEncoding.EncodeToString(ttsE2EWAV(80))
		_, _ = io.WriteString(w, `{"audioContent":"`+enc+`"}`)
	}))
	t.Cleanup(f.srv.Close)
	dialer := ttsE2EDialer(t, ttsgoogle.ProviderName, f.srv.URL)
	c, err := ttsgoogle.New(ttsgoogle.Config{
		BaseURL: f.srv.URL, CredentialRef: "cred_g", LanguageCode: "en-US", VoiceName: "en-US-Neural2-A",
	}, dialer, ttsE2EKeys{key: ttsE2EKeyGoogle})
	if err != nil {
		t.Fatal(err)
	}
	ttsE2ESpeak(t, ttsgoogle.ProviderName, c, f, ttsgoogle.NativeSampleRate, func(t *testing.T, h ttsE2EHit) {
		if !strings.HasPrefix(h.Auth, "Bearer ") || !strings.HasSuffix(h.Auth, ttsE2EKeyGoogle) || h.Path != "/v1/text:synthesize" {
			t.Fatalf("hit=%+v", h)
		}
	})
}

func TestTTSPollyAdapterE2E(t *testing.T) {
	f := &ttsE2EFake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.record(ttsE2EHit{
			Auth: r.Header.Get("Authorization"), AmzDate: r.Header.Get("X-Amz-Date"),
			Path: r.URL.Path, Body: body,
		})
		w.Header().Set("Content-Type", "audio/pcm")
		_, _ = w.Write(ttsE2EPCM(120))
	}))
	t.Cleanup(f.srv.Close)
	dialer := ttsE2EDialer(t, ttspolly.ProviderName, f.srv.URL)
	c, err := ttspolly.New(ttspolly.Config{
		BaseURL: f.srv.URL, Region: "us-east-1", CredentialRef: "cred_p", VoiceID: "Joanna", Engine: "neural",
	}, dialer, ttsE2EKeys{key: ttsE2EKeyPolly})
	if err != nil {
		t.Fatal(err)
	}
	ttsE2ESpeak(t, ttspolly.ProviderName, c, f, ttspolly.NativeSampleRate, func(t *testing.T, h ttsE2EHit) {
		if !strings.HasPrefix(h.Auth, "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/") || h.AmzDate == "" {
			t.Fatalf("auth=%q date=%q", h.Auth, h.AmzDate)
		}
		if strings.Contains(h.Auth, "wJalrXUtnFEMI") {
			t.Fatal("secret leaked into Authorization")
		}
		if h.Path != "/v1/speech" || h.Body["OutputFormat"] != "pcm" || h.Body["Text"] != ttsE2ESpoken {
			t.Fatalf("hit=%+v", h)
		}
	})
}

func TestTTSPerUserVoiceBindingNoCrossUse(t *testing.T) {
	const spoken = "Booked for Tuesday."
	el := vfake.New(vfake.WithName("eleven-tts"))
	oa := vfake.New(vfake.WithName("openai-tts"))
	profEL := ttsE2EProfile("elevenlabs", "el")
	profEL.ProfileID = pv.ProfileID("pprf_" + strings.Repeat("0", 25) + "X")
	profOA := ttsE2EProfile("openai", "tts")
	profOA.ProfileID = pv.ProfileID("pprf_" + strings.Repeat("0", 25) + "Y")
	kits := roleIndepKitMap{
		profEL.ProfileID: {TTS: el, Caps: pfake.DefaultCapabilities("elevenlabs", "el")},
		profOA.ProfileID: {TTS: oa, Caps: pfake.DefaultCapabilities("openai", "tts")},
	}
	profs := roleIndepMultiProfiles{byID: map[pv.ProfileID]pv.ProviderProfile{
		profEL.ProfileID: profEL, profOA.ProfileID: profOA,
	}}
	inner := modes.StaticPolicy{modes.RoleTTS: {Profiles: []pv.ProfileID{profEL.ProfileID, profOA.ProfileID}}}

	vX, scopeX := roleIndepVoiceStack(t, modes.ModeExternal, inner, profs, kits, roleIndepBindings{pv.PrincipalLlmRoleTTS: profEL.ProfileID})
	scopeX.PrincipalID = "usr_x"
	if err := vX.svc.Attest(scopeX, verify.Outcome{Text: spoken, Source: verify.SourceModel, Reason: verify.ReasonVerified}); err != nil {
		t.Fatal(err)
	}
	st, err := extFinalTTS{}.SynthesizeStream(context.Background(), scopeX, voice.SynthesizeRequest{Text: spoken})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if el.CallCount() != 1 || oa.CallCount() != 0 {
		t.Fatalf("usr_x: el=%d oa=%d", el.CallCount(), oa.CallCount())
	}

	vY, scopeY := roleIndepVoiceStack(t, modes.ModeExternal, inner, profs, kits, roleIndepBindings{pv.PrincipalLlmRoleTTS: profOA.ProfileID})
	scopeY.PrincipalID = "usr_y"
	if err := vY.svc.Attest(scopeY, verify.Outcome{Text: spoken, Source: verify.SourceModel, Reason: verify.ReasonVerified}); err != nil {
		t.Fatal(err)
	}
	st, err = extFinalTTS{}.SynthesizeStream(context.Background(), scopeY, voice.SynthesizeRequest{Text: spoken})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if el.CallCount() != 1 || oa.CallCount() != 1 {
		t.Fatalf("after usr_y: el=%d oa=%d", el.CallCount(), oa.CallCount())
	}
}

func TestTTSOpenAICredentialSharedWithControllerAndSTT(t *testing.T) {
	const spoken = "Booked for Tuesday."
	prof := roleIndepProfile("pprf_"+strings.Repeat("0", 25)+"Z", "openai", "omni",
		pv.RoleConversationalController, pv.RoleLanguageRealization, pv.RoleSTT, pv.RoleTTS)
	prof.Pricing = pv.PriceTable{Currency: "USD", Prices: pv.PriceTablePrices{
		CharactersIn: &pv.PriceTablePrice{PerUnits: 1, MicroUnits: 10},
	}}
	extSTT, extTTS := vfake.New(vfake.WithName("openai-stt")), vfake.New(vfake.WithName("openai-tts"))
	inner := modes.StaticPolicy{
		modes.RoleController:  {Profiles: []pv.ProfileID{prof.ProfileID}},
		modes.RoleRealisation: {Profiles: []pv.ProfileID{prof.ProfileID}},
		modes.RoleSTT:         {Profiles: []pv.ProfileID{prof.ProfileID}},
		modes.RoleTTS:         {Profiles: []pv.ProfileID{prof.ProfileID}},
	}
	bind := roleIndepBindings{
		pv.PrincipalLlmRoleConversationalController: prof.ProfileID,
		pv.PrincipalLlmRoleLanguageRealization:      prof.ProfileID,
		pv.PrincipalLlmRoleSTT:                      prof.ProfileID,
		pv.PrincipalLlmRoleTTS:                      prof.ProfileID,
	}
	v, scope := roleIndepVoiceStack(t, modes.ModeExternal, inner,
		roleIndepMultiProfiles{byID: map[pv.ProfileID]pv.ProviderProfile{prof.ProfileID: prof}},
		roleIndepKitMap{prof.ProfileID: {STT: extSTT, TTS: extTTS, Caps: pfake.DefaultCapabilities("openai", "omni")}},
		bind,
	)
	for _, role := range []modes.Role{modes.RoleController, modes.RoleRealisation, modes.RoleSTT, modes.RoleTTS} {
		rt, err := v.route(context.Background(), scope, role)
		if err != nil || !rt.external || string(rt.target.Profile.ProviderID) != "openai" {
			t.Fatalf("role %s: %+v %v", role, rt, err)
		}
	}
	if err := v.svc.Attest(scope, verify.Outcome{Text: spoken, Source: verify.SourceModel, Reason: verify.ReasonVerified}); err != nil {
		t.Fatal(err)
	}
	st, err := extFinalTTS{}.SynthesizeStream(context.Background(), scope, voice.SynthesizeRequest{Text: spoken})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if extTTS.CallCount() != 1 {
		t.Fatalf("tts calls=%d", extTTS.CallCount())
	}
}

func TestTTSElevenLabsFactoryRequiresVoice(t *testing.T) {
	if _, ok := extTTSBuildElevenLabs(extTTSBuildInput{CredentialRef: "c"}); ok {
		t.Fatal("elevenlabs without voice must not build")
	}
}
