package cli

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/voice/packs"
	"github.com/buxee/alix-ai/internal/voice/profile"
	vsession "github.com/buxee/alix-ai/internal/voice/session"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The voice session service of `alixai serve` (master plan 13 and 14.2; M4-04, M4-08).
//
// voiceServeProvide (cmd_voice.go) loads the voice profile; once it is loaded, voiceSessionWire
// builds the session service over it and gives it to the gateway:
//
//   - gateway.VoiceService is provided whenever the profile is loaded, so GET
//     /v1/sessions/{id}/voice stops answering 503 once a voice pack and a provider are in place.
//     With a pack installed and none active the service is present and refuses to open a session
//     (unavailable, effect text_input_only), which is also what lets a live activation work. When
//     the profile is not loaded nothing is provided and the route keeps answering 503 with the
//     text_input_only effect: the text API never notices.
//   - Retention is the voice_session configuration section (nothing is retained by default).
//     serve configures no audio store, so a tenant that retains cannot open a session: Open
//     fails closed for it.
//   - The pack registry the session service holds is voiceSessionPacks: a change of the active
//     pack made through it (the only registry provided, as profile.PackRegistry) writes the
//     activation marker as well, so the marker `alixai voice activate` writes and the live
//     pack never disagree. Service.Packs() wraps it, so taking a pack away also cancels the
//     live streams first. Nothing outside this file touches Profile.Packs() directly.
//   - On shutdown the live streams are cancelled, then the sessions are closed, and only then is
//     the profile closed, which makes the gated providers refuse and releases the runtime. Close
//     deactivates the profile's own registry, not the guarded one: stopping serve must never
//     clear the activation the operator recorded.

// voiceSessionPacks keeps the activation marker of the pack store in step with the live
// registry of the profile (profile.Packs()), which knows nothing of the marker. Without it an
// activation made while serve runs would be lost silently at the next start, which takes the
// pack from the marker.
type voiceSessionPacks struct {
	store *packs.Store
	inner profile.PackRegistry
	// pin is voice.active_pack of the configuration. A different pack cannot be activated
	// live: the next start would find the configuration and the marker naming two packs and run
	// text-only.
	pin string
	// mu makes the two halves of a change (the marker and the live registry) one step.
	mu sync.Mutex
}

var _ profile.PackRegistry = (*voiceSessionPacks)(nil)

// Installed implements profile.PackRegistry.
func (k *voiceSessionPacks) Installed(ctx context.Context) ([]profile.Pack, error) {
	return k.inner.Installed(ctx)
}

// Active implements profile.PackRegistry.
func (k *voiceSessionPacks) Active() (profile.Pack, bool) { return k.inner.Active() }

// Activate records the activation (which verifies the pack's files, refuses a second pack
// and writes the marker exclusively) and then activates the pack live. When the live
// activation fails, a marker this call wrote is taken back, so a refusal leaves both as they
// were.
func (k *voiceSessionPacks) Activate(ctx context.Context, id string) (profile.Pack, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.pin != "" && id != k.pin {
		return profile.Pack{}, contracts.Newf(contracts.ErrConflict,
			"voice: voice.active_pack pins %q; activating %q would leave the configuration and the activation marker naming different packs", k.pin, id).
			WithDetails("pinned", k.pin).WithDetails("requested", id)
	}
	_, had, err := k.store.ActiveID()
	if err != nil {
		return profile.Pack{}, err
	}
	if _, err := k.store.Activate(ctx, id); err != nil {
		return profile.Pack{}, err
	}
	pack, err := k.inner.Activate(ctx, id)
	if err != nil {
		if !had {
			if cerr := k.store.Deactivate(context.WithoutCancel(ctx)); cerr != nil {
				err = errors.Join(err, cerr)
			}
		}
		return profile.Pack{}, err
	}
	return pack, nil
}

// Deactivate unloads the live pack and then clears the marker. A pack that cannot be unloaded
// stays active and keeps its marker.
func (k *voiceSessionPacks) Deactivate(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.inner.Deactivate(ctx); err != nil {
		return err
	}
	if err := k.store.Deactivate(context.WithoutCancel(ctx)); err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err,
			"voice: the pack is deactivated for this run but its activation marker could not be cleared; run 'alixai voice deactivate'")
	}
	return nil
}

// voiceSessionRecordActivation makes the marker say what the profile just activated. The
// configuration may name a pack (voice.active_pack) that no `alixai voice activate` recorded;
// it is recorded now, so that from the first request on the marker and the live pack agree.
// A marker naming another pack than the live one is a disagreement and an error.
func voiceSessionRecordActivation(ctx context.Context, st *packs.Store, p *profile.Profile) error {
	live, active := p.Packs().Active()
	recorded, has, err := st.ActiveID()
	switch {
	case err != nil:
		return err
	case active && !has:
		_, err := st.Activate(ctx, live.ID)
		return err
	case active && recorded != live.ID:
		return contracts.Newf(contracts.ErrConflict, "voice: pack %q is active but the activation marker names %q", live.ID, recorded)
	case !active && has:
		return contracts.Newf(contracts.ErrConflict, "voice: the activation marker names pack %q but none is active", recorded)
	}
	return nil
}

// voiceSessionWire builds the session service over the loaded profile p, provides it and
// registers the shutdown. A defect that leaves voice unusable is logged and voice runs
// text-only (the profile is closed again), like every other reason voice cannot come up; only a
// wiring mistake in the service registry stops serve.
func voiceSessionWire(ctx context.Context, d serveDeps, st *packs.Store, p *profile.Profile, status profile.Status, pin string, log *slog.Logger) error {
	closeProfile := func() {
		cctx, cancel := context.WithTimeout(context.Background(), voiceCloseTimeout)
		defer cancel()
		if err := p.Close(cctx); err != nil {
			log.Warn("voice profile did not close cleanly", slog.String("reason", err.Error()))
		}
	}
	textOnly := func(err error) error {
		closeProfile()
		log.Warn("voice profile not loaded: running text-only", slog.String("reason", err.Error()))
		return nil
	}

	if err := voiceSessionRecordActivation(ctx, st, p); err != nil {
		return textOnly(err)
	}
	// Policy and Retainer stay nil: retention follows the voice_session section, and a tenant
	// that retains has no audio store to retain into, so its sessions are refused.
	caps := vsession.CurrentConfig()
	svc, err := vsession.New(vsession.Deps{
		Providers: d.Providers,
		// The caps of the voice_session section; 0 takes the defaults (4, 2 per tenant, 1 per principal).
		MaxSessions:             caps.MaxSessions,
		MaxSessionsPerTenant:    caps.MaxSessionsPerTenant,
		MaxSessionsPerPrincipal: caps.MaxSessionsPerPrincipal,
		Packs:                   &voiceSessionPacks{store: st, inner: p.Packs(), pin: pin},
		Clock:                   d.Clock,
		Logger:                  log,
	})
	if err != nil {
		return textOnly(err)
	}
	if d.Services != nil {
		if err := gateway.Provide[gateway.VoiceService](d.Services, svc); err != nil {
			closeProfile()
			return err
		}
		// The only registry an operator surface may use to change a pack while serve runs.
		if err := gateway.Provide[profile.PackRegistry](d.Services, svc.Packs()); err != nil {
			closeProfile()
			return err
		}
	}
	if d.AddService != nil {
		d.AddService(func(context.Context) func() {
			return func() {
				svc.CancelLiveStreams()
				svc.Close()
				closeProfile()
			}
		})
	}
	log.Info("voice profile loaded", slog.Bool("ready", status.Ready), slog.String("active_pack", status.ActivePack), slog.Int("roles", len(status.Roles)))
	return nil
}
