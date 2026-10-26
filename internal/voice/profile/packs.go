package profile

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Pack is one installed language pack: the speech models for one language.
// Models and their files are the installer's and the loader's business; the
// profile only needs to know what a pack is called, what language it serves
// and which voice roles it provides.
type Pack struct {
	// ID names the pack: lowercase, digits and hyphens ("en", "it-fast").
	ID string `json:"id"`
	// Language is the BCP 47 tag the pack serves ("en", "it-IT").
	Language string `json:"language"`
	// Version is the installed version, informational.
	Version string `json:"version,omitempty"`
	// Roles are the voice roles the pack provides. A pack that provides only
	// STT leaves the deployment on text output (the section 18 behaviour of a
	// missing TTS role), so voice components stay independently installable.
	Roles []providers.Role `json:"roles"`
}

var packIDRE = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

const maxPackIDLen = 48

// ValidPackID reports whether id is a well-formed pack id.
func ValidPackID(id string) bool { return len(id) <= maxPackIDLen && packIDRE.MatchString(id) }

// Validate rejects a pack that must not be loaded: bad id, language or role
// list.
func (p Pack) Validate() error {
	switch {
	case !ValidPackID(p.ID):
		return contracts.Newf(contracts.ErrSchemaInvalid, "voice: language pack id %q is malformed", p.ID)
	case !voice.ValidLanguage(p.Language):
		return contracts.Newf(contracts.ErrSchemaInvalid, "voice: language pack %q has a malformed language %q", p.ID, p.Language)
	case len(p.Roles) == 0:
		return contracts.Newf(contracts.ErrSchemaInvalid, "voice: language pack %q provides no voice role", p.ID)
	}
	seen := map[providers.Role]bool{}
	for _, r := range p.Roles {
		if r != providers.RoleSTT && r != providers.RoleTTS && r != providers.RoleVAD {
			return contracts.Newf(contracts.ErrSchemaInvalid, "voice: language pack %q lists %q, which is not a voice role", p.ID, string(r))
		}
		if seen[r] {
			return contracts.Newf(contracts.ErrSchemaInvalid, "voice: language pack %q lists role %s twice", p.ID, r)
		}
		seen[r] = true
	}
	return nil
}

// Provides reports whether the pack serves role.
func (p Pack) Provides(role providers.Role) bool {
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// DictationOK reports whether the pack can serve record-to-text dictation: it needs STT.
// VAD is not required (ADR 0039); the user's record and stop are the segmentation.
func (p Pack) DictationOK() bool { return p.Provides(providers.RoleSTT) }

// ConversationOK reports whether the pack can serve voice-conversation mode: STT with a VAD
// in front (ADR 0015). An STT-only pack is refused here and remains valid for dictation.
func (p Pack) ConversationOK() error {
	if !p.Provides(providers.RoleSTT) {
		return contracts.New(contracts.ErrUnavailable, "voice: the pack does not provide speech recognition").
			WithDetails("role", providers.RoleSTT.String()).WithDetails("effect", providers.RoleSTT.Effect())
	}
	if !p.Provides(providers.RoleVAD) {
		return contracts.New(contracts.ErrUnavailable, "voice: voice conversation needs a VAD in front of STT; an STT-only pack is for dictation").
			WithDetails("role", providers.RoleVAD.String()).WithDetails("effect", providers.RoleVAD.Effect())
	}
	return nil
}

func (p Pack) clone() Pack {
	p.Roles = append([]providers.Role(nil), p.Roles...)
	return p
}

// PackCatalog lists the installed packs. It is what the installer (M4-05)
// implements. List reads metadata only: it must not load a model, start a
// worker or open a runtime connection.
type PackCatalog interface {
	List(ctx context.Context) ([]Pack, error)
}

// PackLoader makes a pack's models ready and releases them. It is what a
// runtime adapter implements. Load must leave nothing resident when it fails;
// Unload must be safe to retry when it fails.
type PackLoader interface {
	Load(ctx context.Context, pack Pack) error
	Unload(ctx context.Context, pack Pack) error
}

// NopLoader is a PackLoader for runtimes that load models on demand and unload
// them when idle: there is nothing to do at activation.
type NopLoader struct{}

func (NopLoader) Load(context.Context, Pack) error   { return nil }
func (NopLoader) Unload(context.Context, Pack) error { return nil }

// PackRegistry is the language-pack registry: what is installed and which one
// pack, if any, is active.
type PackRegistry interface {
	// Installed lists the installed packs in ID order.
	Installed(ctx context.Context) ([]Pack, error)
	// Active returns the active pack. At most one is ever active.
	Active() (Pack, bool)
	// Activate loads the pack with the given id and makes it the active one.
	// It fails with validation_failed when the id is malformed or the pack is
	// not installed, and with conflict when a different pack is already
	// active. Activating the active pack again is a no-op.
	Activate(ctx context.Context, id string) (Pack, error)
	// Deactivate unloads the active pack. With none active it is a no-op.
	Deactivate(ctx context.Context) error
}

// Packs is the PackRegistry over a PackCatalog and a PackLoader. It enforces
// the activation rule in one place: Activate and Deactivate run one at a time,
// and a second pack is never loaded while another is.
type Packs struct {
	catalog PackCatalog
	loader  PackLoader
	sem     chan struct{} // one slot: held for the whole of an Activate or Deactivate

	mu     sync.RWMutex
	active *Pack
}

var _ PackRegistry = (*Packs)(nil)

// NewPacks returns a registry over catalog and loader. It activates nothing.
func NewPacks(catalog PackCatalog, loader PackLoader) (*Packs, error) {
	if catalog == nil || loader == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "voice: a language pack registry needs a catalog and a loader")
	}
	return &Packs{catalog: catalog, loader: loader, sem: make(chan struct{}, 1)}, nil
}

// Installed implements PackRegistry. A catalog that lists a malformed pack or
// the same id twice is a schema_invalid failure: packs are not guessed at.
func (k *Packs) Installed(ctx context.Context) ([]Pack, error) {
	if err := ctx.Err(); err != nil {
		return nil, providers.ContextError(err)
	}
	listed, err := k.catalog.List(ctx)
	if err != nil {
		return nil, wrapCatalogError(err)
	}
	out := make([]Pack, 0, len(listed))
	seen := make(map[string]bool, len(listed))
	for _, p := range listed {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if seen[p.ID] {
			return nil, contracts.Newf(contracts.ErrSchemaInvalid, "voice: language pack %q is listed twice", p.ID)
		}
		seen[p.ID] = true
		out = append(out, p.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Active implements PackRegistry.
func (k *Packs) Active() (Pack, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.active == nil {
		return Pack{}, false
	}
	return k.active.clone(), true
}

// lock takes the single activation slot, giving up when ctx ends.
func (k *Packs) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return providers.ContextError(err)
	}
	select {
	case k.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return providers.ContextError(ctx.Err())
	}
}

func (k *Packs) unlock() { <-k.sem }

// Activate implements PackRegistry.
func (k *Packs) Activate(ctx context.Context, id string) (Pack, error) {
	if !ValidPackID(id) {
		return Pack{}, contracts.Newf(contracts.ErrValidationFailed, "voice: %q is not a language pack id", id)
	}
	if err := k.lock(ctx); err != nil {
		return Pack{}, err
	}
	defer k.unlock()

	if cur, ok := k.Active(); ok {
		if cur.ID == id {
			return cur, nil
		}
		return Pack{}, contracts.Newf(contracts.ErrConflict,
			"voice: language pack %q is active; deactivate it before activating %q", cur.ID, id).
			WithDetails("active", cur.ID).WithDetails("requested", id)
	}

	installed, err := k.Installed(ctx)
	if err != nil {
		return Pack{}, err
	}
	var pack *Pack
	for i := range installed {
		if installed[i].ID == id {
			pack = &installed[i]
			break
		}
	}
	if pack == nil {
		return Pack{}, contracts.Newf(contracts.ErrValidationFailed, "voice: language pack %q is not installed", id).
			WithDetails("pack", id)
	}
	if err := k.loader.Load(ctx, pack.clone()); err != nil {
		return Pack{}, loaderError(err, "load", id)
	}
	k.mu.Lock()
	k.active = pack
	k.mu.Unlock()
	return pack.clone(), nil
}

// Deactivate implements PackRegistry. When the loader fails to unload, the
// pack stays active: it may still be resident, and treating it as gone would
// let a second pack load beside it.
func (k *Packs) Deactivate(ctx context.Context) error {
	if err := k.lock(ctx); err != nil {
		return err
	}
	defer k.unlock()
	cur, ok := k.Active()
	if !ok {
		return nil
	}
	if err := k.loader.Unload(ctx, cur); err != nil {
		return loaderError(err, "unload", cur.ID)
	}
	k.mu.Lock()
	k.active = nil
	k.mu.Unlock()
	return nil
}

// loaderError types a loader failure. A typed error keeps its code (a model
// that does not fit in memory stays resource_exhausted); anything else is
// unavailable. Cancellation stays a plain error wrapping the context's.
func loaderError(err error, op, id string) error {
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("voice: %s language pack %q: %w", op, id, err)
	}
	code := contracts.ErrUnavailable
	var ce *contracts.Error
	if errors.As(err, &ce) {
		code = ce.Code
	}
	return contracts.Wrap(code, err, fmt.Sprintf("voice: could not %s language pack %q", op, id)).WithDetails("pack", id)
}

func wrapCatalogError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return providers.ContextError(err)
	}
	code := contracts.ErrUnavailable
	var ce *contracts.Error
	if errors.As(err, &ce) {
		code = ce.Code
	}
	return contracts.Wrap(code, err, "voice: could not list installed language packs")
}

// MemoryCatalog is an in-memory PackCatalog for tests and for hosts that
// declare their packs in code. It is safe for concurrent use.
type MemoryCatalog struct {
	mu    sync.Mutex
	packs []Pack
	lists int
}

var _ PackCatalog = (*MemoryCatalog)(nil)

// NewMemoryCatalog returns a catalog holding packs.
func NewMemoryCatalog(packs ...Pack) *MemoryCatalog {
	c := &MemoryCatalog{}
	for _, p := range packs {
		c.packs = append(c.packs, p.clone())
	}
	return c
}

// List implements PackCatalog.
func (c *MemoryCatalog) List(ctx context.Context) ([]Pack, error) {
	if err := ctx.Err(); err != nil {
		return nil, providers.ContextError(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lists++
	out := make([]Pack, len(c.packs))
	for i, p := range c.packs {
		out[i] = p.clone()
	}
	return out, nil
}

// Calls returns how many times List was called: a profile that is not loaded
// must leave it at zero.
func (c *MemoryCatalog) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lists
}
