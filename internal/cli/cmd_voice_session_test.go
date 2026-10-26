package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	vfake "github.com/buxee/alix-ai/internal/providers/voice/fake"
	"github.com/buxee/alix-ai/internal/voice/packs"
	"github.com/buxee/alix-ai/internal/voice/profile"
	vsession "github.com/buxee/alix-ai/internal/voice/session"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The tests of M4-08: serve builds the voice session service over the loaded voice profile,
// provides it to the voice route, and keeps the profile's pack, the activation marker and the
// live streams in step. They drive voiceServeProvide on the rig of cmd_voice_test.go with a
// service registry, the way wireV1 does, and the fake speech runtime stands in for the adapter.

// voiceSessionRig is a serve rig whose factory hands out the fake speech runtime.
type voiceSessionRig struct {
	*voiceServeRig
	stt, tts *vfake.Provider
	// defaultFactory is the factory of the binary, before the rig replaced it.
	defaultFactory profile.ProviderFactory
	// onClose runs when the profile releases what the factory connected.
	onClose func()
}

// voiceSessionSetup prepares a serve run: the test packs are approved, voice is enabled with
// pin as voice.active_pack, the packs in installed are installed, and activated (when not "")
// is recorded as the operator's activation, as `alixai voice activate` does before serve starts.
// It does not start the provider.
func voiceSessionSetup(t *testing.T, pin, activated string, installed ...string) *voiceSessionRig {
	t.Helper()
	voiceUseTestManifests(t)
	voiceResetConfig(t)
	voiceEnableConfig(t, pin)
	r := &voiceSessionRig{voiceServeRig: newVoiceServeRig(t), stt: vfake.New(), tts: vfake.New(), defaultFactory: voiceProviderFactory}
	r.deps.Services = gateway.NewServices()
	voiceUseFactory(t, func(context.Context) (profile.Providers, error) {
		return profile.Providers{STT: r.stt, TTS: r.tts, VAD: vfake.New(vfake.WithName("fake-vad")), Close: func() error {
			if r.onClose != nil {
				r.onClose()
			}
			return nil
		}}, nil
	})
	for _, id := range installed {
		r.installVoicePack(t, id, id == activated)
	}
	t.Cleanup(func() {
		for _, stop := range r.stops {
			stop()
		}
	})
	return r
}

func (r *voiceSessionRig) provide(t *testing.T) {
	t.Helper()
	if err := voiceServeProvide(context.Background(), r.deps); err != nil {
		t.Fatalf("voiceServeProvide: %v\nlog: %s", err, r.log.String())
	}
}

func (r *voiceSessionRig) store(t *testing.T) *packs.Store {
	t.Helper()
	st, err := voiceOpenStore(filepath.Join(r.dataDr, voicePackDirName))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func voiceSessionService(r *voiceSessionRig) (gateway.VoiceService, bool) {
	return gateway.Lookup[gateway.VoiceService](r.deps.Services)
}

func voiceSessionScope() contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep-prod-3301", ApplicationID: "demo-app", TenantID: "tnt-acme-7781", WorkspaceID: "wsp-north-2290",
		PrincipalID: "usr-jane-4410", Roles: []string{"receptionist"}, CapabilityScopes: []string{"appointments.read"},
		SessionID: "ses-voice-1",
	}
}

// voiceSessionNoTurns is the turn runner of a test that runs no turn.
type voiceSessionNoTurns struct{}

func (voiceSessionNoTurns) RunTextTurn(context.Context, contracts.Scope, string, string) (dialogue.Result, error) {
	return dialogue.Result{}, errors.New("no turn is run in this test")
}

// voiceSessionConn is the connection of a test: the client's messages go in through in, and the
// JSON events of the session are kept.
type voiceSessionConn struct {
	in     chan []byte
	mu     sync.Mutex
	events []map[string]any
}

func newVoiceSessionConn() *voiceSessionConn { return &voiceSessionConn{in: make(chan []byte, 16)} }

func (c *voiceSessionConn) Read(ctx context.Context) (bool, []byte, error) {
	select {
	case b := <-c.in:
		return false, b, nil
	case <-ctx.Done():
		return false, nil, ctx.Err()
	}
}

func (c *voiceSessionConn) Write(_ context.Context, binary bool, data []byte) error {
	if binary {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	c.mu.Lock()
	c.events = append(c.events, m)
	c.mu.Unlock()
	return nil
}

func (c *voiceSessionConn) find(typ string) (map[string]any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.events {
		if m["type"] == typ {
			return m, true
		}
	}
	return nil, false
}

// voiceSessionAgree fails unless the activation marker and the live pack both name want.
func voiceSessionAgree(t *testing.T, st *packs.Store, reg profile.PackRegistry, want string) {
	t.Helper()
	marker, _, err := st.ActiveID()
	if err != nil {
		t.Fatal(err)
	}
	live := ""
	if p, ok := reg.Active(); ok {
		live = p.ID
	}
	if marker != want || live != want {
		t.Fatalf("the activation marker names %q and the live pack is %q, want both %q", marker, live, want)
	}
}

// With voice enabled, a pack installed and activated and a provider that connects, serve provides
// the voice service, so the voice route stops answering 503, and the service opens a session.
func TestServeProvidesTheVoiceServiceWhenAPackIsActive(t *testing.T) {
	r := voiceSessionSetup(t, "", "en", "en")
	r.provide(t)

	svc, ok := voiceSessionService(r)
	if !ok {
		t.Fatalf("serve did not provide gateway.VoiceService\nlog: %s", r.log.String())
	}
	sess, err := svc.Open(context.Background(), voiceSessionScope(), vsession.Options{Turns: voiceSessionNoTurns{}})
	if err != nil {
		t.Fatalf("Open with a pack and a provider in place: %v", err)
	}
	sess.Close(vsession.ReasonClientClosed)
	if _, ok := gateway.Lookup[profile.PackRegistry](r.deps.Services); !ok {
		t.Error("serve did not provide the pack registry an operator surface changes packs through")
	}
	if len(r.stops) != 1 {
		t.Errorf("serve registered %d services, want one that stops the voice stack", len(r.stops))
	}

	// Retention is the voice_session section. A tenant that retains has no audio store to retain
	// into: its session is refused rather than opened without the retention it was promised.
	t.Run("a retaining tenant cannot open a session without an audio store", func(t *testing.T) {
		if err := config.Default.LoadFrom(writeConfig(t, "voice:\n  enabled: true\nvoice_session:\n  retain_raw_audio: true\n"), voiceNoEnv); err != nil {
			t.Fatal(err)
		}
		_, err := svc.Open(context.Background(), voiceSessionScope(), vsession.Options{Turns: voiceSessionNoTurns{}})
		if contracts.CodeOf(err) != contracts.ErrUnavailable || !strings.Contains(err.Error(), "no audio store") {
			t.Fatalf("Open for a retaining tenant = %v, want unavailable (no audio store)", err)
		}
	})
}

// The voice_session section is accepted by the configuration once this package is linked.
func TestVoiceSessionConfigSectionIsAccepted(t *testing.T) {
	voiceResetConfig(t)
	if code, _, errb := voiceRun(t, "config", "check", "-config", writeConfig(t, "voice_session:\n  retain_raw_audio: false\n  retain_tenants: [tnt-a]\n")); code != ExitOK {
		t.Fatalf("config check with a voice_session section = %d: %s", code, errb)
	}
	if code, _, _ := voiceRun(t, "config", "check", "-config", writeConfig(t, "voice_session:\n  surprise: 1\n")); code != ExitError {
		t.Errorf("a voice_session section with an unknown key = %d, want 1", code)
	}
}

// Whenever voice cannot come up, nothing is provided and the voice route keeps answering 503 with
// the text_input_only effect; a pack installed but not active leaves the service present and its
// sessions refused with that same effect.
func TestServeStaysTextOnlyWithoutAVoicePack(t *testing.T) {
	t.Run("voice disabled", func(t *testing.T) {
		r := voiceSessionSetup(t, "", "", "en")
		voiceResetConfig(t)
		r.provide(t)
		if _, ok := voiceSessionService(r); ok {
			t.Error("a disabled voice profile provided a voice service")
		}
	})
	t.Run("no pack installed", func(t *testing.T) {
		r := voiceSessionSetup(t, "", "")
		r.provide(t)
		if _, ok := voiceSessionService(r); ok {
			t.Error("a voice service was provided with no pack installed")
		}
		if _, ok := gateway.Lookup[profile.PackRegistry](r.deps.Services); ok {
			t.Error("a pack registry was provided with no voice profile")
		}
	})
	t.Run("no runtime adapter is linked", func(t *testing.T) {
		r := voiceSessionSetup(t, "", "en", "en")
		voiceProviderFactory = r.defaultFactory // the rig's cleanup puts it back as it was
		r.provide(t)
		if _, ok := voiceSessionService(r); ok {
			t.Error("a voice service was provided although no provider connects")
		}
		if !strings.Contains(r.log.String(), "running text-only") {
			t.Errorf("log: %s", r.log.String())
		}
	})
	t.Run("the configuration and the recorded activation disagree", func(t *testing.T) {
		r := voiceSessionSetup(t, "it", "en", "en", "it")
		r.provide(t)
		if _, ok := voiceSessionService(r); ok {
			t.Error("a voice service was provided although the activation is ambiguous")
		}
	})
	t.Run("a pack is installed and none is active", func(t *testing.T) {
		r := voiceSessionSetup(t, "", "", "en")
		r.provide(t)
		svc, ok := voiceSessionService(r)
		if !ok {
			t.Fatalf("no voice service although the profile is loaded\nlog: %s", r.log.String())
		}
		_, err := svc.Open(context.Background(), voiceSessionScope(), vsession.Options{Turns: voiceSessionNoTurns{}})
		var ce *contracts.Error
		if !errors.As(err, &ce) || ce.Code != contracts.ErrUnavailable || ce.Details["effect"] != providers.EffectTextInputOnly {
			t.Fatalf("Open with no active pack = %v, want unavailable with effect %s", err, providers.EffectTextInputOnly)
		}
	})
}

// On shutdown the live streams are cancelled, with the pack_changed reason of a pack taken away,
// before the profile closes and releases the runtime under them.
func TestShutdownCancelsLiveVoiceStreamsBeforeTheProfileCloses(t *testing.T) {
	r := voiceSessionSetup(t, "", "en", "en")
	conn := newVoiceSessionConn()
	var atClose struct {
		open      int
		cancelled map[string]any
		ran       bool
	}
	r.onClose = func() {
		atClose.ran = true
		atClose.open = r.stt.OpenStreams()
		atClose.cancelled, _ = conn.find("capture.cancelled")
	}
	r.provide(t)
	svc, ok := voiceSessionService(r)
	if !ok {
		t.Fatalf("no voice service\nlog: %s", r.log.String())
	}
	sess, err := svc.Open(context.Background(), voiceSessionScope(), vsession.Options{Turns: voiceSessionNoTurns{}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan vsession.CloseReason, 1)
	go func() { done <- sess.Serve(context.Background(), conn) }()
	r.stt.Script("book a table") // what the recogniser hears once the capture is ended
	format, _ := json.Marshal(voice.PCM16Mono(16000))
	conn.in <- []byte(`{"type":"capture.start","format":` + string(format) + `}`)
	pollUntil(t, 5*time.Second, "the capture to start", func() bool {
		_, ok := conn.find("capture.started")
		return ok
	})
	if n := r.stt.OpenStreams(); n != 1 {
		t.Fatalf("%d recognition streams are open, want the live capture's one", n)
	}

	r.stops[0]()

	if !atClose.ran {
		t.Fatal("shutdown did not close the profile")
	}
	if atClose.open != 0 || atClose.cancelled == nil {
		t.Errorf("when the profile closed, %d streams were open and capture.cancelled was %v: the streams must be cancelled first", atClose.open, atClose.cancelled)
	} else if atClose.cancelled["reason"] != "pack_changed" {
		t.Errorf("capture.cancelled = %v, want the pack_changed reason", atClose.cancelled)
	}
	select {
	case reason := <-done:
		if reason != vsession.ReasonServerClosing {
			t.Errorf("the session ended with %q, want %q", reason, vsession.ReasonServerClosing)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the session was still open after shutdown")
	}
	// Stopping serve is not an operator's deactivation: the recorded activation survives it.
	if id, _, err := r.store(t).ActiveID(); err != nil || id != "en" {
		t.Errorf("after shutdown the activation marker names %q (%v), want it kept", id, err)
	}
	if _, err := svc.Open(context.Background(), voiceSessionScope(), vsession.Options{Turns: voiceSessionNoTurns{}}); err == nil {
		t.Error("a session opened after shutdown")
	}
}

// voiceSessionInner is a profile.PackRegistry whose changes can be made to fail.
type voiceSessionInner struct {
	profile.PackRegistry
	activateErr, deactivateErr error
}

func (f voiceSessionInner) Activate(ctx context.Context, id string) (profile.Pack, error) {
	if f.activateErr != nil {
		return profile.Pack{}, f.activateErr
	}
	return f.PackRegistry.Activate(ctx, id)
}

func (f voiceSessionInner) Deactivate(ctx context.Context) error {
	if f.deactivateErr != nil {
		return f.deactivateErr
	}
	return f.PackRegistry.Deactivate(ctx)
}

// A pack changed while serve runs is recorded in the activation marker too (or refused), so the
// live pack and the marker never differ, and the next start finds what the operator left.
func TestLivePackActivationAndTheMarkerAgree(t *testing.T) {
	ctx := context.Background()

	t.Run("live changes move the marker with them", func(t *testing.T) {
		r := voiceSessionSetup(t, "", "en", "en", "it")
		r.provide(t)
		reg, ok := gateway.Lookup[profile.PackRegistry](r.deps.Services)
		if !ok {
			t.Fatalf("no pack registry\nlog: %s", r.log.String())
		}
		st := r.store(t)
		voiceSessionAgree(t, st, reg, "en") // what `alixai voice activate en` recorded is what serve runs

		// A second pack is refused while one is active, and nothing changes.
		if _, err := reg.Activate(ctx, "it"); contracts.CodeOf(err) != contracts.ErrConflict {
			t.Fatalf("Activate it while en is active = %v, want conflict", err)
		}
		voiceSessionAgree(t, st, reg, "en")

		if err := reg.Deactivate(ctx); err != nil {
			t.Fatal(err)
		}
		voiceSessionAgree(t, st, reg, "")
		svc, _ := voiceSessionService(r)
		if _, err := svc.Open(ctx, voiceSessionScope(), vsession.Options{Turns: voiceSessionNoTurns{}}); contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Errorf("Open after the pack was deactivated = %v, want unavailable", err)
		}

		// A pack whose files no longer verify is refused, and the marker is not written.
		if err := os.WriteFile(filepath.Join(r.dataDr, voicePackDirName, "en", "stt-en.bin"), []byte("tampered"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := reg.Activate(ctx, "en"); err == nil {
			t.Fatal("a pack whose files do not verify was activated")
		}
		voiceSessionAgree(t, st, reg, "")

		if _, err := reg.Activate(ctx, "it"); err != nil {
			t.Fatal(err)
		}
		voiceSessionAgree(t, st, reg, "it")
		sess, err := svc.Open(ctx, voiceSessionScope(), vsession.Options{Turns: voiceSessionNoTurns{}})
		if err != nil {
			t.Fatalf("Open after a live activation: %v", err)
		}
		sess.Close(vsession.ReasonClientClosed)

		// The next start runs the pack the live activation recorded.
		next := newVoiceServeRig(t)
		next.deps.DataDir, next.deps.Services = r.dataDr, gateway.NewServices()
		if err := voiceServeProvide(ctx, next.deps); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			for _, stop := range next.stops {
				stop()
			}
		})
		reg2, ok := gateway.Lookup[profile.PackRegistry](next.deps.Services)
		if !ok {
			t.Fatalf("the next start provided no pack registry\nlog: %s", next.log.String())
		}
		voiceSessionAgree(t, voiceSessionStoreOf(t, next), reg2, "it")
	})

	t.Run("a configured pack is recorded at the start, and no other can be activated live", func(t *testing.T) {
		r := voiceSessionSetup(t, "en", "", "en", "it") // voice.active_pack: en, nothing recorded yet
		r.provide(t)
		reg, ok := gateway.Lookup[profile.PackRegistry](r.deps.Services)
		if !ok {
			t.Fatalf("no pack registry\nlog: %s", r.log.String())
		}
		st := r.store(t)
		voiceSessionAgree(t, st, reg, "en")

		if err := reg.Deactivate(ctx); err != nil {
			t.Fatal(err)
		}
		voiceSessionAgree(t, st, reg, "")
		// The configuration names en: it would disagree with a marker naming it, at the next start.
		if _, err := reg.Activate(ctx, "it"); contracts.CodeOf(err) != contracts.ErrConflict {
			t.Fatalf("Activate it against voice.active_pack: en = %v, want conflict", err)
		}
		voiceSessionAgree(t, st, reg, "")
		if _, err := reg.Activate(ctx, "en"); err != nil {
			t.Fatal(err)
		}
		voiceSessionAgree(t, st, reg, "en")
	})

	t.Run("a refused live step leaves the marker as it was", func(t *testing.T) {
		r := voiceSessionSetup(t, "", "", "en")
		st := r.store(t)
		packsOfProfile, err := profile.NewPacks(st.Catalog(), st.Loader())
		if err != nil {
			t.Fatal(err)
		}
		boom := errors.New("the runtime refused")

		// The marker this call wrote is taken back.
		k := &voiceSessionPacks{store: st, inner: voiceSessionInner{PackRegistry: packsOfProfile, activateErr: boom}}
		if _, err := k.Activate(ctx, "en"); !errors.Is(err, boom) {
			t.Fatalf("Activate = %v, want the live refusal", err)
		}
		if id, has, _ := st.ActiveID(); has {
			t.Errorf("a refused activation left the marker naming %q", id)
		}
		// A marker the operator recorded earlier is not this call's to take back.
		if _, err := st.Activate(ctx, "en"); err != nil {
			t.Fatal(err)
		}
		if _, err := k.Activate(ctx, "en"); !errors.Is(err, boom) {
			t.Fatalf("Activate = %v, want the live refusal", err)
		}
		if id, _, _ := st.ActiveID(); id != "en" {
			t.Errorf("a refused live step cleared the operator's marker (now %q)", id)
		}
		// A pack that cannot be unloaded stays active and keeps its marker.
		if _, err := packsOfProfile.Activate(ctx, "en"); err != nil {
			t.Fatal(err)
		}
		k = &voiceSessionPacks{store: st, inner: voiceSessionInner{PackRegistry: packsOfProfile, deactivateErr: boom}}
		if err := k.Deactivate(ctx); !errors.Is(err, boom) {
			t.Fatalf("Deactivate = %v, want the live refusal", err)
		}
		voiceSessionAgree(t, st, packsOfProfile, "en")
	})
}

// voiceSessionStoreOf opens the pack store of the rig that restarted serve.
func voiceSessionStoreOf(t *testing.T, r *voiceServeRig) *packs.Store {
	t.Helper()
	st, err := voiceOpenStore(filepath.Join(r.deps.DataDir, voicePackDirName))
	if err != nil {
		t.Fatal(err)
	}
	return st
}
