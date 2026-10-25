package profile_test

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	vfake "github.com/buxee/alix-ai/internal/providers/voice/fake"
	"github.com/buxee/alix-ai/internal/voice/profile"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// settle lets goroutines that are exiting finish, so a count taken before and
// after a call compares like with like.
func settle() {
	for i := 0; i < 3; i++ {
		runtime.GC()
		runtime.Gosched()
	}
}

// TestVoiceDisabledLoadsNothing proves the text-only claim (master plan 2 and
// 13): a deployment that is not installed and enabled carries no voice worker,
// goroutine, buffer, provider connection or registered role. The last row is
// the positive control: with both conditions true the same rig does load, so
// the empty rows cannot pass because nothing was ever wired up.
func TestVoiceDisabledLoadsNothing(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name               string
		enabled, installed bool
		absent             profile.Absence
		wantCatalogCalls   int
	}{
		{"not installed, not enabled", false, false, profile.AbsentDisabled, 0},
		{"installed but not enabled", false, true, profile.AbsentDisabled, 0},
		{"enabled but not installed", true, false, profile.AbsentNotInstalled, 1},
		{"installed and enabled (positive control)", true, true, "", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var packs []profile.Pack
			if tc.installed {
				packs = []profile.Pack{packEN, packIT}
			}
			r := newRig(packs...)
			if got := r.registered(); len(got) != 0 {
				t.Fatalf("roles registered before Load: %v", got)
			}

			settle()
			before := runtime.NumGoroutine()
			p, st, err := profile.Load(ctx, r.deps(profile.Config{Enabled: tc.enabled}))
			settle()
			after := runtime.NumGoroutine()

			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if after != before {
				t.Fatalf("goroutines %d -> %d: the profile started work", before, after)
			}
			if got := r.catalog.Calls(); got != tc.wantCatalogCalls {
				t.Fatalf("catalog consulted %d times, want %d", got, tc.wantCatalogCalls)
			}

			if tc.absent == "" { // the positive control
				if p == nil || !p.Loaded() || !st.Loaded || st.Absent != "" {
					t.Fatalf("an installed, enabled profile did not load: %+v", st)
				}
				if got := r.factory.Load(); got != 1 {
					t.Fatalf("factory called %d times, want 1", got)
				}
				if got := r.registered(); len(got) != 3 {
					t.Fatalf("registered roles = %v, want stt, tts, vad", got)
				}
				if p.Ready() || st.Ready || st.ActivePack != "" {
					t.Fatalf("nothing is activated by default: %+v", st)
				}
				if err := p.Close(ctx); err != nil {
					t.Fatalf("Close: %v", err)
				}
				settle()
				if n := runtime.NumGoroutine(); n != before {
					t.Fatalf("goroutines %d -> %d after Close", before, n)
				}
				return
			}

			// Everything below is the absent case.
			if p != nil || p.Loaded() || p.Ready() || st.Loaded || st.Ready || st.Absent != tc.absent {
				t.Fatalf("profile = %v, status = %+v, want absent (%s)", p, st, tc.absent)
			}
			if got := r.factory.Load(); got != 0 {
				t.Fatalf("factory called %d times: a provider connection was made", got)
			}
			if r.loader.loads.Load() != 0 || r.loader.unloads.Load() != 0 {
				t.Fatal("a language pack was loaded")
			}
			if got := r.registered(); len(got) != 0 {
				t.Fatalf("roles registered: %v", got)
			}
			for _, s := range r.reg.Snapshot() {
				if s.Role.Reserved() && s.Configured {
					t.Fatalf("snapshot shows %s configured", s.Role)
				}
			}
			if r.rt.CallCount() != 0 || r.rt.OpenStreams() != 0 {
				t.Fatalf("the runtime was used: %d calls, %d streams", r.rt.CallCount(), r.rt.OpenStreams())
			}
			// Callers see the section 18 degradation, not a panic.
			for role, effect := range map[providers.Role]string{
				providers.RoleSTT: providers.EffectTextInputOnly,
				providers.RoleTTS: providers.EffectVerifiedTextOnly,
				providers.RoleVAD: providers.EffectManualTurnTaking,
			} {
				_, err := providers.Lookup[any](r.reg, role)
				var ce *contracts.Error
				if !errors.As(err, &ce) || ce.Code != contracts.ErrUnavailable || ce.Details["effect"] != effect {
					t.Fatalf("%s lookup = %v, want unavailable with effect %s", role, err, effect)
				}
			}
			if p.Packs() != nil || p.Close(ctx) != nil || p.Status().Loaded {
				t.Fatal("an absent profile must be inert")
			}
		})
	}

	t.Run("the disabled path does no work at all", func(t *testing.T) {
		r := newRig(packEN)
		d := r.deps(profile.Config{})
		settle()
		before := runtime.NumGoroutine()
		allocs, measured := disabledLoadAllocs(ctx, d)
		settle()
		if p, st, err := profile.Load(ctx, d); p != nil || err != nil || st.Absent != profile.AbsentDisabled {
			t.Fatalf("Load = %v, %+v, %v", p, st, err)
		}
		if measured && allocs != 0 {
			t.Fatalf("the disabled path allocated %v times per call: it is building something", allocs)
		}
		if after := runtime.NumGoroutine(); after != before {
			t.Fatalf("goroutines %d -> %d over 500 disabled loads", before, after)
		}
		if r.catalog.Calls() != 0 || r.factory.Load() != 0 || len(r.registered()) != 0 {
			t.Fatal("the disabled path touched the catalog, factory or registry")
		}
	})

	t.Run("a zero Deps is fine while voice is off", func(t *testing.T) {
		p, st, err := profile.Load(ctx, profile.Deps{})
		if p != nil || err != nil || st.Absent != profile.AbsentDisabled {
			t.Fatalf("Load(zero) = %v, %+v, %v", p, st, err)
		}
	})
}

func TestLoadedProfileGatesProvidersByActivePack(t *testing.T) {
	ctx := context.Background()
	r := newRig(packEN, packDE)
	r.rt.Script("hello there")
	p, _, err := profile.Load(ctx, r.deps(profile.Config{Enabled: true}))
	if err != nil {
		t.Fatal(err)
	}
	pcm := voice.PCM16Mono(16000)
	audio := vfake.Tone(pcm, 100*time.Millisecond, 3000)

	transcribe := func() error {
		stt, err := voice.LookupSTT(r.reg)
		if err != nil {
			t.Fatalf("LookupSTT: %v", err)
		}
		_, err = stt.Transcribe(ctx, scope(), voice.TranscribeRequest{Audio: audio, Format: pcm})
		return err
	}
	synthesize := func() error {
		tts, err := voice.LookupTTS(r.reg)
		if err != nil {
			t.Fatalf("LookupTTS: %v", err)
		}
		st, err := tts.SynthesizeStream(ctx, scope(), voice.SynthesizeRequest{Text: "hello"})
		if err == nil {
			st.Close()
		}
		return err
	}
	detect := func() error {
		vad, err := voice.LookupVAD(r.reg)
		if err != nil {
			t.Fatalf("LookupVAD: %v", err)
		}
		_, err = vad.DetectSpeech(ctx, scope(), voice.DetectRequest{Audio: audio, Format: pcm})
		return err
	}
	wantDenied := func(what string, err error, effect string) {
		t.Helper()
		var ce *contracts.Error
		if !errors.As(err, &ce) || ce.Code != contracts.ErrUnavailable || ce.Details["effect"] != effect {
			t.Fatalf("%s = %v, want unavailable with effect %s", what, err, effect)
		}
	}

	// Loaded but nothing activated: every role is registered and refuses.
	wantDenied("transcribe without a pack", transcribe(), providers.EffectTextInputOnly)
	wantDenied("synthesize without a pack", synthesize(), providers.EffectVerifiedTextOnly)
	wantDenied("detect without a pack", detect(), providers.EffectManualTurnTaking)
	if r.rt.CallCount() != 0 {
		t.Fatalf("a gated call reached the runtime: %d", r.rt.CallCount())
	}
	r.reg.Refresh(ctx)
	if st := r.reg.Status(providers.RoleSTT); st.Status != providers.HealthUnavailable || st.Effect != providers.EffectTextInputOnly {
		t.Fatalf("STT readiness without a pack = %+v", st)
	}
	degraded := map[providers.Role]bool{}
	for _, s := range r.reg.Degraded() {
		degraded[s.Role] = true
	}
	if !degraded[providers.RoleSTT] || !degraded[providers.RoleTTS] || !degraded[providers.RoleVAD] {
		t.Fatalf("a loaded profile that cannot serve must show as degraded, got %v", degraded)
	}

	// An STT-only pack serves STT and nothing else (independently installable).
	if _, err := p.Packs().Activate(ctx, "de"); err != nil {
		t.Fatal(err)
	}
	if !p.Ready() || p.Status().ActivePack != "de" {
		t.Fatalf("status = %+v", p.Status())
	}
	if err := transcribe(); err != nil {
		t.Fatalf("transcribe with an STT pack: %v", err)
	}
	wantDenied("synthesize with an STT-only pack", synthesize(), providers.EffectVerifiedTextOnly)
	wantDenied("detect with an STT-only pack", detect(), providers.EffectManualTurnTaking)
	r.reg.Refresh(ctx)
	if r.reg.Status(providers.RoleSTT).Status != providers.HealthOK || r.reg.Status(providers.RoleTTS).Status != providers.HealthUnavailable {
		t.Fatalf("readiness with an STT pack: %+v", r.reg.Snapshot())
	}

	// Switching packs is deactivate then activate; the gates follow.
	if err := p.Packs().Deactivate(ctx); err != nil {
		t.Fatal(err)
	}
	wantDenied("transcribe after deactivate", transcribe(), providers.EffectTextInputOnly)
	if _, err := p.Packs().Activate(ctx, "en"); err != nil {
		t.Fatal(err)
	}
	for what, f := range map[string]func() error{"transcribe": transcribe, "synthesize": synthesize, "detect": detect} {
		if err := f(); err != nil {
			t.Fatalf("%s with the full pack: %v", what, err)
		}
	}

	// A stream opened through the gate is the runtime's own stream.
	stt, _ := voice.LookupSTT(r.reg)
	st, err := stt.TranscribeStream(ctx, scope(), voice.STTStreamRequest{Format: pcm})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Send(audio); err != nil {
		t.Fatal(err)
	}
	if err := st.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if tr, err := voice.CollectTranscript(st); err != nil || tr.Text != "hello there" {
		t.Fatalf("stream through the gate = %+v, %v", tr, err)
	}

	// Close takes it out of service: the registration stays (the registry
	// cannot drop it) but every call is refused.
	calls := r.rt.CallCount()
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(ctx); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
	if p.Ready() || r.released.Load() != 1 || r.loader.loaded.Load() != 0 {
		t.Fatalf("after Close: ready=%v released=%d loaded=%d", p.Ready(), r.released.Load(), r.loader.loaded.Load())
	}
	wantDenied("transcribe after Close", transcribe(), providers.EffectTextInputOnly)
	r.reg.Refresh(ctx)
	if r.reg.Usable(providers.RoleSTT) {
		t.Fatal("a closed profile's role must be unusable")
	}
	if r.rt.CallCount() != calls {
		t.Fatal("a call reached the runtime after Close")
	}
}

func TestLoadActivatesTheConfiguredPack(t *testing.T) {
	ctx := context.Background()
	r := newRig(packEN, packIT)
	p, st, err := profile.Load(ctx, r.deps(profile.Config{Enabled: true, ActivePack: "it"}))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(ctx)
	if !st.Ready || st.ActivePack != "it" || len(st.Roles) != 3 {
		t.Fatalf("status = %+v", st)
	}
	if r.loader.loads.Load() != 1 || r.loader.maxLoaded.Load() != 1 {
		t.Fatalf("loads = %d, max loaded = %d", r.loader.loads.Load(), r.loader.maxLoaded.Load())
	}
}

func TestLoadFailsClosed(t *testing.T) {
	ctx := context.Background()
	enabled := profile.Config{Enabled: true}

	t.Run("a missing dependency is an error, not an absence", func(t *testing.T) {
		r := newRig(packEN)
		for name, mutate := range map[string]func(*profile.Deps){
			"catalog":  func(d *profile.Deps) { d.Catalog = nil },
			"loader":   func(d *profile.Deps) { d.Loader = nil },
			"factory":  func(d *profile.Deps) { d.Factory = nil },
			"registry": func(d *profile.Deps) { d.Registry = nil },
		} {
			d := r.deps(enabled)
			mutate(&d)
			p, _, err := profile.Load(ctx, d)
			wantCode(t, err, contracts.ErrInternal)
			if p != nil {
				t.Fatalf("%s: a profile came back with the error", name)
			}
		}
		if r.factory.Load() != 0 || len(r.registered()) != 0 {
			t.Fatal("a rejected Load connected or registered something")
		}
	})

	t.Run("a second load is refused before it connects anything", func(t *testing.T) {
		r := newRig(packEN)
		p, _, err := profile.Load(ctx, r.deps(enabled))
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close(ctx)
		_, _, err = profile.Load(ctx, r.deps(enabled))
		wantCode(t, err, contracts.ErrConflict)
		if r.factory.Load() != 1 {
			t.Fatalf("the refused Load called the factory (%d calls)", r.factory.Load())
		}
	})

	t.Run("a role already registered is a conflict", func(t *testing.T) {
		r := newRig(packEN)
		if err := voice.Register(r.reg, providers.RoleTTS, r.rt); err != nil {
			t.Fatal(err)
		}
		_, _, err := profile.Load(ctx, r.deps(enabled))
		wantCode(t, err, contracts.ErrConflict)
		if r.factory.Load() != 0 {
			t.Fatal("the factory ran although a role was taken")
		}
	})

	t.Run("a factory failure keeps its code and leaves nothing behind", func(t *testing.T) {
		r := newRig(packEN)
		d := r.deps(enabled)
		d.Factory = func(context.Context) (profile.Providers, error) {
			return profile.Providers{}, contracts.New(contracts.ErrResourceExhausted, "no memory for the STT model")
		}
		p, _, err := profile.Load(ctx, d)
		wantCode(t, err, contracts.ErrResourceExhausted)
		if p != nil || len(r.registered()) != 0 {
			t.Fatal("a failed Load left a profile or a registration")
		}
		d.Factory = func(context.Context) (profile.Providers, error) { return profile.Providers{}, errors.New("refused") }
		_, _, err = profile.Load(ctx, d)
		wantCode(t, err, contracts.ErrUnavailable)
	})

	t.Run("a factory that supplies no provider releases what it connected", func(t *testing.T) {
		r := newRig(packEN)
		d := r.deps(enabled)
		d.Factory = func(context.Context) (profile.Providers, error) {
			return profile.Providers{Close: func() error { r.released.Add(1); return nil }}, nil
		}
		_, _, err := profile.Load(ctx, d)
		wantCode(t, err, contracts.ErrInternal)
		if r.released.Load() != 1 || len(r.registered()) != 0 {
			t.Fatalf("released %d, registered %v", r.released.Load(), r.registered())
		}
	})

	t.Run("a configured pack that is not installed stops Load before the factory", func(t *testing.T) {
		r := newRig(packEN)
		_, _, err := profile.Load(ctx, r.deps(profile.Config{Enabled: true, ActivePack: "fr"}))
		wantCode(t, err, contracts.ErrValidationFailed)
		if r.factory.Load() != 0 {
			t.Fatal("the factory ran for a pack that is not installed")
		}
	})

	t.Run("a malformed pack id is rejected", func(t *testing.T) {
		r := newRig(packEN)
		_, _, err := profile.Load(ctx, r.deps(profile.Config{Enabled: true, ActivePack: "Bad Id"}))
		wantCode(t, err, contracts.ErrValidationFailed)
	})

	t.Run("a pack that will not activate unwinds the whole load", func(t *testing.T) {
		r := newRig(packEN)
		r.loader.set(func() { r.loader.failLoad["en"] = contracts.New(contracts.ErrResourceExhausted, "model does not fit") })
		p, _, err := profile.Load(ctx, r.deps(profile.Config{Enabled: true, ActivePack: "en"}))
		wantCode(t, err, contracts.ErrResourceExhausted)
		if p != nil || len(r.registered()) != 0 || r.released.Load() != 1 || r.loader.loaded.Load() != 0 {
			t.Fatalf("profile %v, registered %v, released %d, loaded %d", p, r.registered(), r.released.Load(), r.loader.loaded.Load())
		}
	})

	t.Run("a cancelled context stops Load", func(t *testing.T) {
		r := newRig(packEN)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		_, _, err := profile.Load(cctx, r.deps(enabled))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Load with a cancelled context = %v", err)
		}
		if r.factory.Load() != 0 {
			t.Fatal("the factory ran under a cancelled context")
		}
	})

	t.Run("a broken catalog is an error", func(t *testing.T) {
		r := newRig(packEN, profile.Pack{ID: "en", Language: "en", Roles: []providers.Role{providers.RoleSTT}}) // listed twice
		_, _, err := profile.Load(ctx, r.deps(enabled))
		wantCode(t, err, contracts.ErrSchemaInvalid)
		if r.factory.Load() != 0 {
			t.Fatal("the factory ran against a broken catalog")
		}
	})
}

func TestConfigSection(t *testing.T) {
	found := false
	for _, s := range config.Default.Sections() {
		found = found || s == profile.SectionName
	}
	if !found {
		t.Fatalf("section %q is not registered", profile.SectionName)
	}
	reset := func() {
		if err := config.Default.LoadFrom("", func(string) (string, bool) { return "", false }); err != nil {
			t.Fatalf("reset: %v", err)
		}
	}
	reset()
	defer reset()
	if got := profile.CurrentConfig(); got.Enabled || got.ActivePack != "" {
		t.Fatalf("the default must be voice off, got %+v", got)
	}
	env := map[string]string{"ALIXAI_VOICE_ENABLED": "true", "ALIXAI_VOICE_ACTIVE_PACK": "it"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	if err := config.Default.LoadFrom("", lookup); err != nil {
		t.Fatal(err)
	}
	if got := profile.CurrentConfig(); !got.Enabled || got.ActivePack != "it" {
		t.Fatalf("overrides not applied: %+v", got)
	}
	env["ALIXAI_VOICE_ACTIVE_PACK"] = "Not A Pack"
	if err := config.Default.LoadFrom("", lookup); err == nil {
		t.Fatal("a malformed active_pack must fail the load")
	}
	if got := profile.CurrentConfig(); got.ActivePack != "it" {
		t.Fatalf("a failed load must leave the section as it was, got %+v", got)
	}
}

var _ voice.STT = (*vfake.Provider)(nil)
