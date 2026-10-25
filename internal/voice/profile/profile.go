package profile

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Providers is what a ProviderFactory connects. Every field is optional (voice
// components are independently installable) but at least one must be set.
type Providers struct {
	STT voice.STT
	TTS voice.TTS
	VAD voice.VAD
	// Close releases whatever the factory connected (nil when there is
	// nothing to release). It is called once, by Profile.Close or when Load
	// fails after the factory succeeded.
	Close func() error
}

// ProviderFactory connects the voice providers. It is the only place a runtime
// connection is made, and Load calls it only for a profile that is installed
// and enabled.
type ProviderFactory func(ctx context.Context) (Providers, error)

// Deps is everything Load needs. They are interfaces and a function so the
// composition root (not this package) decides what the concrete runtime is:
// nothing here imports an adapter.
type Deps struct {
	Config Config
	// Catalog lists the installed packs; Loader loads the active one. Both are
	// required once voice is enabled and unused before.
	Catalog PackCatalog
	Loader  PackLoader
	// Factory connects the providers, called only when voice is enabled and a
	// pack is installed.
	Factory ProviderFactory
	// Registry is where the gated voice roles are registered.
	Registry *providers.Registry
}

// Absence says why a profile is not loaded.
type Absence string

const (
	// AbsentDisabled: voice is not enabled. Nothing was consulted or built.
	AbsentDisabled Absence = "disabled"
	// AbsentNotInstalled: voice is enabled but no language pack is installed.
	AbsentNotInstalled Absence = "not_installed"
)

// Status describes a profile for readiness and diagnostics.
type Status struct {
	// Loaded is whether a profile exists. When false, Absent says why.
	Loaded bool
	Absent Absence
	// Ready is whether voice can serve right now: loaded, open, and a pack is
	// active.
	Ready bool
	// ActivePack is the active pack's id ("" when none).
	ActivePack string
	// Roles are the voice roles this profile registered.
	Roles []providers.Role
}

// Profile is a loaded voice profile. Obtain one from Load; an absent profile is
// a nil *Profile.
type Profile struct {
	packs   *Packs
	roles   []providers.Role
	release func() error
	closed  atomic.Bool
}

// Load builds the voice profile, or decides not to (see the package comment).
// It returns:
//
//   - (nil, Status{Absent: AbsentDisabled}, nil) when voice is not enabled,
//     having done nothing at all;
//   - (nil, Status{Absent: AbsentNotInstalled}, nil) when it is enabled and no
//     pack is installed;
//   - a loaded Profile when it is both;
//   - an error when it is wanted but cannot come up. Nothing is left
//     registered or connected after an error.
//
// Load is meant to run once per process: the registry cannot unregister a role,
// so a second Load fails with conflict before it connects anything.
func Load(ctx context.Context, d Deps) (*Profile, Status, error) {
	if !d.Config.Enabled {
		return nil, Status{Absent: AbsentDisabled}, nil
	}
	// Voice is wanted from here on: a defect is an error, not an absence.
	if d.Catalog == nil || d.Loader == nil || d.Factory == nil || d.Registry == nil {
		return nil, Status{}, contracts.New(contracts.ErrInternal, "voice: profile is enabled but the composition root did not supply a catalog, loader, provider factory and registry")
	}
	if err := d.Config.Validate(); err != nil {
		return nil, Status{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, Status{}, providers.ContextError(err)
	}
	for _, role := range voice.Roles() {
		if d.Registry.Has(role) {
			return nil, Status{}, contracts.Newf(contracts.ErrConflict, "voice: role %s is already registered; the voice profile loads once per process", role)
		}
	}

	packs, err := NewPacks(d.Catalog, d.Loader)
	if err != nil {
		return nil, Status{}, err
	}
	installed, err := packs.Installed(ctx)
	if err != nil {
		return nil, Status{}, err
	}
	if len(installed) == 0 {
		return nil, Status{Absent: AbsentNotInstalled}, nil
	}
	if want := d.Config.ActivePack; want != "" && !hasPack(installed, want) {
		return nil, Status{}, contracts.Newf(contracts.ErrValidationFailed, "voice: configured active pack %q is not installed", want).
			WithDetails("pack", want)
	}

	// Installed and enabled: the first and only place anything is connected.
	prov, err := d.Factory(ctx)
	if err != nil {
		return nil, Status{}, wrapFactoryError(err)
	}
	p := &Profile{packs: packs, release: prov.Close}
	fail := func(err error) (*Profile, Status, error) {
		p.closed.Store(true)
		if rerr := p.releaseOnce(); rerr != nil {
			err = errors.Join(err, rerr)
		}
		return nil, Status{}, err
	}

	type wire struct {
		role providers.Role
		impl any
	}
	var wires []wire
	if prov.STT != nil {
		wires = append(wires, wire{providers.RoleSTT, sttGate{p: p, in: prov.STT}})
	}
	if prov.TTS != nil {
		wires = append(wires, wire{providers.RoleTTS, ttsGate{p: p, in: prov.TTS}})
	}
	if prov.VAD != nil {
		wires = append(wires, wire{providers.RoleVAD, vadGate{p: p, in: prov.VAD}})
	}
	for _, w := range wires {
		p.roles = append(p.roles, w.role)
	}
	if len(wires) == 0 {
		return fail(contracts.New(contracts.ErrInternal, "voice: the provider factory returned no STT, TTS or VAD provider"))
	}

	if id := d.Config.ActivePack; id != "" {
		if _, err := packs.Activate(ctx, id); err != nil {
			return fail(err)
		}
	}
	for _, w := range wires {
		if err := voice.Register(d.Registry, w.role, w.impl); err != nil {
			// A role registered before the failure stays in the registry, but
			// closing the profile makes its gate refuse every call.
			_ = packs.Deactivate(ctx)
			return fail(err)
		}
	}
	return p, p.Status(), nil
}

func hasPack(packs []Pack, id string) bool {
	for _, p := range packs {
		if p.ID == id {
			return true
		}
	}
	return false
}

func wrapFactoryError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return providers.ContextError(err)
	}
	code := contracts.ErrUnavailable
	var ce *contracts.Error
	if errors.As(err, &ce) {
		code = ce.Code
	}
	return contracts.Wrap(code, err, "voice: could not connect the voice providers")
}

// Loaded reports whether p is a loaded profile (a nil *Profile is not).
func (p *Profile) Loaded() bool { return p != nil }

// Ready reports whether voice can serve right now: the profile is open and a
// pack is active. A nil *Profile is never ready.
func (p *Profile) Ready() bool {
	if p == nil || p.closed.Load() {
		return false
	}
	_, ok := p.packs.Active()
	return ok
}

// Packs returns the language-pack registry, through which an operator
// activates and deactivates packs. It is nil for an absent profile.
func (p *Profile) Packs() PackRegistry {
	if p == nil {
		return nil
	}
	return p.packs
}

// Status returns the profile's live status. For a nil *Profile it reports an
// absent profile without a reason; Load's own Status carries the reason.
func (p *Profile) Status() Status {
	if p == nil {
		return Status{}
	}
	s := Status{Loaded: true, Ready: p.Ready(), Roles: append([]providers.Role(nil), p.roles...)}
	if pack, ok := p.packs.Active(); ok {
		s.ActivePack = pack.ID
	}
	return s
}

// Close takes the profile out of service: the gated providers refuse every call
// from now on, the active pack is unloaded and the connections the factory made
// are released. It is idempotent, and safe on a nil *Profile. The roles stay
// registered (the registry cannot remove them) but report unavailable.
func (p *Profile) Close(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if p.closed.Swap(true) {
		return nil
	}
	var errs []error
	if err := p.packs.Deactivate(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := p.releaseOnce(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// releaseOnce calls the factory's release function (at most once).
func (p *Profile) releaseOnce() error {
	rel := p.release
	p.release = nil
	if rel == nil {
		return nil
	}
	return rel()
}
