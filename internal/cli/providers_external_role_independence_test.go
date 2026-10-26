//go:build !noexternal

package cli

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/extmodal"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/providerreg"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/providers/voice"
	vfake "github.com/buxee/alix-ai/internal/providers/voice/fake"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// ADR 0043: each role resolves independently — same provider or different ones, no cross-principal use.

type roleIndepBindings map[pv.PrincipalLlmRole]pv.ProfileID

func (b roleIndepBindings) PrincipalBindingFor(_ context.Context, _ contracts.Scope, role pv.PrincipalLlmRole) (pv.ProfileID, bool, error) {
	id, ok := b[role]
	return id, ok, nil
}

type roleIndepMultiProfiles struct {
	byID map[pv.ProfileID]pv.ProviderProfile
}

func (p roleIndepMultiProfiles) Get(_ context.Context, _ contracts.Scope, id pv.ProfileID) (pv.ProviderProfile, error) {
	prof, ok := p.byID[id]
	if !ok {
		return pv.ProviderProfile{}, contracts.New(contracts.ErrUnavailable, "profile missing")
	}
	return prof, nil
}

func (p roleIndepMultiProfiles) Check(_ context.Context, _ contracts.Scope, id pv.ProfileID, _ providerreg.Requirement) (providerreg.Admission, error) {
	prof, ok := p.byID[id]
	if !ok {
		return providerreg.Admission{}, contracts.New(contracts.ErrUnavailable, "profile missing")
	}
	return providerreg.Admission{Profile: prof}, nil
}

func (p roleIndepMultiProfiles) Fallback(context.Context, contracts.Scope, pv.ProfileID, providerreg.FallbackCondition, providerreg.Requirement) (providerreg.FallbackRecord, providerreg.Admission, error) {
	return providerreg.FallbackRecord{}, providerreg.Admission{}, contracts.New(contracts.ErrUnavailable, "no fallback")
}

type roleIndepKitMap map[pv.ProfileID]extFinalVoiceKit

func (m roleIndepKitMap) Kit(_ context.Context, _ contracts.Scope, p pv.ProviderProfile) (extFinalVoiceKit, bool) {
	k, ok := m[p.ProfileID]
	return k, ok
}

func roleIndepProfile(id, provider, model string, roles ...pv.Role) pv.ProviderProfile {
	return pv.ProviderProfile{
		ProfileID: pv.ProfileID(id), ProviderID: pv.ProviderID(provider), ModelID: pv.ModelID(model),
		Roles: roles,
		Pricing: pv.PriceTable{Currency: "USD", Prices: pv.PriceTablePrices{
			CharactersIn: &pv.PriceTablePrice{PerUnits: 1, MicroUnits: 10},
			ToksIn:       &pv.PriceTablePrice{PerUnits: 1000, MicroUnits: 1000},
		}},
		Modalities: pv.ProviderProfileModalities{Input: []pv.Modality{pv.ModalityText, pv.ModalityAudio}, Output: []pv.Modality{pv.ModalityText, pv.ModalityAudio}},
		Media: &pv.ProviderProfileMedia{
			Audio: &pv.ProviderProfileMediaHandling{MaxBytes: 1 << 20, MimeTypes: []string{"audio/wav"}, Retention: pv.RetentionModeTransient},
		},
		Voice: pv.ProviderProfileVoice{STT: true, TTS: true, AudioRetention: pv.RetentionModeTransient},
	}
}

func roleIndepPolicyStack(t *testing.T, mode modes.Mode, inner modes.PolicySource, bindings modes.PrincipalBindingSource) (modes.PolicySource, contracts.Scope, *modes.Resolver) {
	t.Helper()
	scope := externalTestScope("ten_acme", "usr_member")
	mem := modes.NewMemoryModes()
	for _, lv := range []modes.Level{modes.LevelDeployment, modes.LevelApplication, modes.LevelTenant} {
		if err := mem.Set(lv, scope, mode); err != nil {
			t.Fatal(err)
		}
	}
	res, err := modes.NewResolver(mem, &extFinalKS{})
	if err != nil {
		t.Fatal(err)
	}
	pol := modes.PolicySource(inner)
	if bindings != nil {
		pol = modes.PrincipalPolicy{Inner: inner, Bindings: bindings, Resolver: res, RequireConfigured: true}
	}
	return pol, scope, res
}

func roleIndepVoiceStack(t *testing.T, mode modes.Mode, policy modes.PolicySource, profiles modes.Profiles, source extFinalVoiceSource, bindings modes.PrincipalBindingSource) (*extFinalVoice, contracts.Scope) {
	t.Helper()
	pol, scope, res := roleIndepPolicyStack(t, mode, policy, bindings)
	mp := extmodal.NewMemoryPolicy()
	for _, p := range []string{"openai", "elevenlabs", "deepgram", "acme"} {
		mp.SetProvider(p, extmodal.Enable(true, extmodal.ModTTS, extmodal.ModSTT))
	}
	mp.SetTenant(scope, extmodal.Enable(true, extmodal.ModTTS, extmodal.ModSTT))
	svc, err := extmodal.New(extmodal.Deps{Policy: mp, Egress: extFinalAllowText{}, Budgets: extFinalAdmitBudget{}})
	if err != nil {
		t.Fatal(err)
	}
	v := &extFinalVoice{
		svc: svc, modes: res, policy: pol, profiles: profiles, source: source,
		log: slog.New(slog.DiscardHandler),
	}
	extFinalVoiceHolder.Store(v)
	t.Cleanup(func() { extFinalVoiceHolder.Store(nil) })
	return v, scope
}

func TestPerRoleIndependenceSameProviderOneCredential(t *testing.T) {
	// (a) all four roles bound to ONE provider with ONE credential work.
	const spoken = "Booked for Tuesday."
	prof := roleIndepProfile("pprf_"+strings.Repeat("0", 25)+"A", "openai", "omni",
		pv.RoleConversationalController, pv.RoleLanguageRealization, pv.RoleSTT, pv.RoleTTS)
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
		rp, err := v.policy.RolePolicy(context.Background(), scope, role)
		if err != nil || len(rp.Profiles) != 1 || rp.Profiles[0] != prof.ProfileID {
			t.Fatalf("role %s: %+v %v", role, rp, err)
		}
	}
	rtSTT, err := v.route(context.Background(), scope, modes.RoleSTT)
	if err != nil || !rtSTT.external || string(rtSTT.target.Profile.ProviderID) != "openai" {
		t.Fatalf("stt route: %+v %v", rtSTT, err)
	}
	rtTTS, err := v.route(context.Background(), scope, modes.RoleTTS)
	if err != nil || !rtTTS.external || string(rtTTS.target.Profile.ProviderID) != "openai" {
		t.Fatalf("tts route: %+v %v", rtTTS, err)
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
		t.Fatalf("tts calls = %d", extTTS.CallCount())
	}
}

func TestPerRoleIndependenceFourDifferentProviders(t *testing.T) {
	// (b) four DIFFERENT providers, one per role, work in the same session.
	const spoken = "Booked for Tuesday."
	ids := []string{
		"pprf_" + strings.Repeat("0", 25) + "B",
		"pprf_" + strings.Repeat("0", 25) + "C",
		"pprf_" + strings.Repeat("0", 25) + "D",
		"pprf_" + strings.Repeat("0", 25) + "E",
	}
	profs := map[pv.ProfileID]pv.ProviderProfile{
		pv.ProfileID(ids[0]): roleIndepProfile(ids[0], "openai", "gpt", pv.RoleConversationalController),
		pv.ProfileID(ids[1]): roleIndepProfile(ids[1], "elevenlabs", "el", pv.RoleLanguageRealization),
		pv.ProfileID(ids[2]): roleIndepProfile(ids[2], "deepgram", "nova", pv.RoleSTT),
		pv.ProfileID(ids[3]): roleIndepProfile(ids[3], "acme", "speak", pv.RoleTTS),
	}
	extTTS := vfake.New(vfake.WithName("acme-tts"))
	extSTT := vfake.New(vfake.WithName("deepgram-stt"))
	inner := modes.StaticPolicy{
		modes.RoleController:  {Profiles: []pv.ProfileID{pv.ProfileID(ids[0])}},
		modes.RoleRealisation: {Profiles: []pv.ProfileID{pv.ProfileID(ids[1])}},
		modes.RoleSTT:         {Profiles: []pv.ProfileID{pv.ProfileID(ids[2])}},
		modes.RoleTTS:         {Profiles: []pv.ProfileID{pv.ProfileID(ids[3])}},
	}
	bind := roleIndepBindings{
		pv.PrincipalLlmRoleConversationalController: pv.ProfileID(ids[0]),
		pv.PrincipalLlmRoleLanguageRealization:      pv.ProfileID(ids[1]),
		pv.PrincipalLlmRoleSTT:                      pv.ProfileID(ids[2]),
		pv.PrincipalLlmRoleTTS:                      pv.ProfileID(ids[3]),
	}
	kits := roleIndepKitMap{
		pv.ProfileID(ids[2]): {STT: extSTT, Caps: pfake.DefaultCapabilities("deepgram", "nova")},
		pv.ProfileID(ids[3]): {TTS: extTTS, Caps: pfake.DefaultCapabilities("acme", "speak")},
	}
	v, scope := roleIndepVoiceStack(t, modes.ModeExternal, inner, roleIndepMultiProfiles{byID: profs}, kits, bind)
	wantPolicy := map[modes.Role]string{
		modes.RoleController: ids[0], modes.RoleRealisation: ids[1],
		modes.RoleSTT: ids[2], modes.RoleTTS: ids[3],
	}
	for role, id := range wantPolicy {
		rp, err := v.policy.RolePolicy(context.Background(), scope, role)
		if err != nil || len(rp.Profiles) != 1 || string(rp.Profiles[0]) != id {
			t.Fatalf("role %s: %+v %v", role, rp, err)
		}
	}
	wantProv := map[modes.Role]string{modes.RoleSTT: "deepgram", modes.RoleTTS: "acme"}
	for role, provider := range wantProv {
		rt, err := v.route(context.Background(), scope, role)
		if err != nil || !rt.external || string(rt.target.Profile.ProviderID) != provider {
			t.Fatalf("role %s: provider=%s err=%v", role, rt.target.Profile.ProviderID, err)
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
		t.Fatalf("tts=%d", extTTS.CallCount())
	}
}

func TestPerRoleIndependenceMixedLocalAndExternal(t *testing.T) {
	// (c) controller external A, STT local, TTS provider B.
	const spoken = "Booked for Tuesday."
	ctrl := roleIndepProfile("pprf_"+strings.Repeat("0", 25)+"F", "openai", "gpt", pv.RoleConversationalController)
	ttsProf := roleIndepProfile("pprf_"+strings.Repeat("0", 25)+"G", "acme", "speak", pv.RoleTTS)
	extTTS, locTTS, locSTT := vfake.New(), vfake.New(), vfake.New()
	locSTT.Script("local transcript")
	inner := modes.StaticPolicy{
		modes.RoleController: {Profiles: []pv.ProfileID{ctrl.ProfileID}},
		modes.RoleTTS:        {Profiles: []pv.ProfileID{ttsProf.ProfileID}},
	}
	v, scope := roleIndepVoiceStack(t, modes.ModeExternal, inner,
		roleIndepMultiProfiles{byID: map[pv.ProfileID]pv.ProviderProfile{ctrl.ProfileID: ctrl, ttsProf.ProfileID: ttsProf}},
		roleIndepKitMap{ttsProf.ProfileID: {TTS: extTTS, Caps: pfake.DefaultCapabilities("acme", "speak")}},
		nil,
	)
	rpCtrl, err := v.policy.RolePolicy(context.Background(), scope, modes.RoleController)
	if err != nil || len(rpCtrl.Profiles) != 1 || rpCtrl.Profiles[0] != ctrl.ProfileID {
		t.Fatalf("controller policy: %+v %v", rpCtrl, err)
	}
	rtSTT, err := v.route(context.Background(), scope, modes.RoleSTT)
	if err != nil || rtSTT.external {
		t.Fatalf("stt must stay local: %+v %v", rtSTT, err)
	}
	rtTTS, err := v.route(context.Background(), scope, modes.RoleTTS)
	if err != nil || !rtTTS.external || string(rtTTS.target.Profile.ProviderID) != "acme" {
		t.Fatalf("tts: %+v %v", rtTTS, err)
	}
	if err := v.svc.Attest(scope, verify.Outcome{Text: spoken, Source: verify.SourceModel, Reason: verify.ReasonVerified}); err != nil {
		t.Fatal(err)
	}
	st, err := extFinalTTS{local: locTTS}.SynthesizeStream(context.Background(), scope, voice.SynthesizeRequest{Text: spoken})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	_, err = extFinalSTT{local: locSTT}.Transcribe(context.Background(), scope, voice.TranscribeRequest{
		Audio: make([]byte, 3200), Format: voice.AudioFormat{Encoding: voice.EncodingPCM16, SampleRate: 16000, Channels: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if extTTS.CallCount() != 1 || locTTS.CallCount() != 0 || locSTT.CallCount() != 1 {
		t.Fatalf("extTTS=%d locTTS=%d locSTT=%d", extTTS.CallCount(), locTTS.CallCount(), locSTT.CallCount())
	}
}

func TestPerRoleIndependencePerUserOppositeBindings(t *testing.T) {
	// (d) user X: chat A + TTS B; user Y: chat B + TTS A; same tenant; no cross-use.
	chatA := roleIndepProfile("pprf_"+strings.Repeat("0", 25)+"H", "openai", "gpt", pv.RoleConversationalController)
	ttsB := roleIndepProfile("pprf_"+strings.Repeat("0", 25)+"I", "acme", "speak", pv.RoleTTS)
	chatB := roleIndepProfile("pprf_"+strings.Repeat("0", 25)+"J", "acme", "chat", pv.RoleConversationalController)
	ttsA := roleIndepProfile("pprf_"+strings.Repeat("0", 25)+"K", "openai", "tts", pv.RoleTTS)
	profs := roleIndepMultiProfiles{byID: map[pv.ProfileID]pv.ProviderProfile{
		chatA.ProfileID: chatA, ttsB.ProfileID: ttsB, chatB.ProfileID: chatB, ttsA.ProfileID: ttsA,
	}}
	ttsX, ttsY := vfake.New(vfake.WithName("tts-x")), vfake.New(vfake.WithName("tts-y"))
	kits := roleIndepKitMap{
		ttsB.ProfileID: {TTS: ttsX, Caps: pfake.DefaultCapabilities("acme", "speak")},
		ttsA.ProfileID: {TTS: ttsY, Caps: pfake.DefaultCapabilities("openai", "tts")},
	}

	runUser := func(principal string, bind roleIndepBindings, wantChat, wantTTS string, ttsFake *vfake.Provider) {
		t.Helper()
		scope := externalTestScope("ten_acme", principal)
		mem := modes.NewMemoryModes()
		for _, lv := range []modes.Level{modes.LevelDeployment, modes.LevelApplication, modes.LevelTenant} {
			if err := mem.Set(lv, scope, modes.ModeExternal); err != nil {
				t.Fatal(err)
			}
		}
		res, err := modes.NewResolver(mem, &extFinalKS{})
		if err != nil {
			t.Fatal(err)
		}
		pol := modes.PrincipalPolicy{Inner: modes.StaticPolicy{}, Bindings: bind, Resolver: res, RequireConfigured: true}
		mp := extmodal.NewMemoryPolicy()
		mp.SetProvider("openai", extmodal.Enable(true, extmodal.ModTTS))
		mp.SetProvider("acme", extmodal.Enable(true, extmodal.ModTTS))
		mp.SetTenant(scope, extmodal.Enable(true, extmodal.ModTTS))
		svc, err := extmodal.New(extmodal.Deps{Policy: mp, Egress: extFinalAllowText{}, Budgets: extFinalAdmitBudget{}})
		if err != nil {
			t.Fatal(err)
		}
		v := &extFinalVoice{svc: svc, modes: res, policy: pol, profiles: profs, source: kits, log: slog.New(slog.DiscardHandler)}
		extFinalVoiceHolder.Store(v)
		t.Cleanup(func() { extFinalVoiceHolder.Store(nil) })

		rpChat, err := pol.RolePolicy(context.Background(), scope, modes.RoleController)
		if err != nil || len(rpChat.Profiles) != 1 {
			t.Fatalf("%s chat policy: %+v %v", principal, rpChat, err)
		}
		chatProf := profs.byID[rpChat.Profiles[0]]
		if string(chatProf.ProviderID) != wantChat {
			t.Fatalf("%s chat provider=%s want %s", principal, chatProf.ProviderID, wantChat)
		}
		rtTTS, err := v.route(context.Background(), scope, modes.RoleTTS)
		if err != nil || !rtTTS.external || string(rtTTS.target.Profile.ProviderID) != wantTTS {
			t.Fatalf("%s tts: provider=%s err=%v", principal, rtTTS.target.Profile.ProviderID, err)
		}
		const spoken = "hello"
		if err := v.svc.Attest(scope, verify.Outcome{Text: spoken, Source: verify.SourceModel, Reason: verify.ReasonVerified}); err != nil {
			t.Fatal(err)
		}
		st, err := extFinalTTS{}.SynthesizeStream(context.Background(), scope, voice.SynthesizeRequest{Text: spoken})
		if err != nil {
			t.Fatal(err)
		}
		_ = st.Close()
		if ttsFake.CallCount() != 1 {
			t.Fatalf("%s: wrong TTS fake called (%d)", principal, ttsFake.CallCount())
		}
	}

	runUser("usr_x", roleIndepBindings{
		pv.PrincipalLlmRoleConversationalController: chatA.ProfileID,
		pv.PrincipalLlmRoleTTS:                      ttsB.ProfileID,
	}, "openai", "acme", ttsX)
	runUser("usr_y", roleIndepBindings{
		pv.PrincipalLlmRoleConversationalController: chatB.ProfileID,
		pv.PrincipalLlmRoleTTS:                      ttsA.ProfileID,
	}, "acme", "openai", ttsY)
	if ttsX.CallCount() != 1 || ttsY.CallCount() != 1 {
		t.Fatalf("cross-use? x=%d y=%d", ttsX.CallCount(), ttsY.CallCount())
	}
}

func TestPerRoleIndependenceDeploymentRoutingDifferentProfiles(t *testing.T) {
	// (e) deployment routing with different profiles per role.
	sttP := roleIndepProfile("pprf_"+strings.Repeat("0", 25)+"L", "deepgram", "nova", pv.RoleSTT)
	ttsP := roleIndepProfile("pprf_"+strings.Repeat("0", 25)+"M", "acme", "speak", pv.RoleTTS)
	policy := modes.StaticPolicy{
		modes.RoleSTT: {Profiles: []pv.ProfileID{sttP.ProfileID}},
		modes.RoleTTS: {Profiles: []pv.ProfileID{ttsP.ProfileID}},
	}
	extSTT, extTTS := vfake.New(), vfake.New()
	v, scope := roleIndepVoiceStack(t, modes.ModeHybrid, policy,
		roleIndepMultiProfiles{byID: map[pv.ProfileID]pv.ProviderProfile{sttP.ProfileID: sttP, ttsP.ProfileID: ttsP}},
		roleIndepKitMap{
			sttP.ProfileID: {STT: extSTT, Caps: pfake.DefaultCapabilities("deepgram", "nova")},
			ttsP.ProfileID: {TTS: extTTS, Caps: pfake.DefaultCapabilities("acme", "speak")},
		},
		nil,
	)
	rtSTT, _ := v.route(context.Background(), scope, modes.RoleSTT)
	rtTTS, _ := v.route(context.Background(), scope, modes.RoleTTS)
	if string(rtSTT.target.Profile.ProviderID) != "deepgram" || string(rtTTS.target.Profile.ProviderID) != "acme" {
		t.Fatalf("stt=%s tts=%s", rtSTT.target.Profile.ProviderID, rtTTS.target.Profile.ProviderID)
	}
	if rtSTT.target.Profile.ProfileID == rtTTS.target.Profile.ProfileID {
		t.Fatal("roles must keep distinct deployment profiles")
	}
}
