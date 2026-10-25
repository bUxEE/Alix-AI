package capabilities

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// DefaultMaxAge is how long the registry trusts an entry it loaded from a Source
// before it asks the Source again which package is active.
const DefaultMaxAge = 30 * time.Second

// Source is where the registry finds installed packages. *apppkg.Installer is
// wrapped by InstallerSource; tests supply their own.
type Source interface {
	// ActiveHash returns the content hash of the application's active package.
	// found is false when the application has none: nothing installed, or every
	// installed package is quarantined, rejected or superseded.
	ActiveHash(ctx context.Context, applicationID string) (hash string, found bool, err error)
	// Open loads the package with that hash.
	Open(ctx context.Context, applicationID, hash string) (Package, error)
}

// InstallerSource reads installed packages through an apppkg.Installer. The
// installer re-verifies every package it opens (signature, hash, checks), and the
// registry refuses anything that is not in the active state.
func InstallerSource(in *apppkg.Installer) Source { return installerSource{in} }

type installerSource struct{ in *apppkg.Installer }

func (s installerSource) ActiveHash(ctx context.Context, applicationID string) (string, bool, error) {
	recs, err := s.in.List(ctx, applicationID)
	if err != nil {
		return "", false, err
	}
	// The table allows at most one active package per application; if a list
	// ever shows two, the newest is the one the audit activated last.
	var hash string
	var found bool
	for _, r := range recs {
		if r.State == apppkg.StateActive {
			hash, found = r.ManifestHash, true
		}
	}
	return hash, found, nil
}

func (s installerSource) Open(ctx context.Context, applicationID, hash string) (Package, error) {
	p, _, err := s.in.Open(ctx, applicationID, hash)
	if err != nil {
		return nil, err
	}
	return FromApppkg(p), nil
}

// Registry holds, for each application, the capabilities of its active package
// (master plan 4.1 capability routing). It is the only place a router learns
// which capabilities exist: an application with no active package has none, so a
// quarantined, rejected or superseded package exposes nothing (SEC-060).
//
// An entry is keyed by application ID and found only by the application of the
// verified Scope, never by anything the user wrote (SEC-053). It is compiled when
// the package is activated and replaced whole when the package changes, so a
// reader never sees half of an update. The registry is safe for concurrent use.
//
// Entries come from two places. Activate takes a package the caller already
// holds; it stays until Deactivate or the next Activate. Refresh and Lookup load
// from the Source; such an entry is re-checked against the Source after MaxAge,
// and an application the Source no longer reports as active is dropped. A Source
// that fails is treated as unknown, never as "still active".
type Registry struct {
	src    Source
	clk    clock.Clock
	maxAge time.Duration

	mu   sync.RWMutex
	apps map[string]*index
	// absent remembers, for MaxAge, an application the Source reported as having
	// no active package, so a quarantined application does not cost a database
	// read on every turn. Refresh ignores it.
	absent map[string]time.Time
}

// RegistryOption configures NewRegistry.
type RegistryOption func(*Registry)

// WithClock sets the clock that ages entries loaded from a Source.
func WithClock(c clock.Clock) RegistryOption {
	return func(r *Registry) {
		if c != nil {
			r.clk = c
		}
	}
}

// WithMaxAge sets how long a loaded entry is trusted. Zero or less means
// DefaultMaxAge.
func WithMaxAge(d time.Duration) RegistryOption {
	return func(r *Registry) {
		if d > 0 {
			r.maxAge = d
		}
	}
}

// NewRegistry returns a registry. src may be nil, in which case only Activate
// adds applications.
func NewRegistry(src Source, opts ...RegistryOption) *Registry {
	r := &Registry{src: src, clk: clock.Real{}, maxAge: DefaultMaxAge, apps: map[string]*index{}, absent: map[string]time.Time{}}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Activate registers an active package, replacing the application's previous
// entry. It refuses a package that is not active, a package whose typed and
// policy views disagree, and a package with no application ID.
func (r *Registry) Activate(p Package) error {
	ix, err := compile(p)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.apps[ix.app] = ix
	delete(r.absent, ix.app)
	r.mu.Unlock()
	return nil
}

// Deactivate removes an application's entry. It is how the activation owner
// takes an application out of service at once, without waiting for MaxAge.
func (r *Registry) Deactivate(applicationID string) {
	r.mu.Lock()
	delete(r.apps, applicationID)
	r.mu.Unlock()
}

// Refresh asks the Source which package of the application is active and
// installs it. An application with no active package is removed. A failing Source
// removes the entry too and returns an unavailable error: the registry does not
// keep serving a package it can no longer confirm is active.
func (r *Registry) Refresh(ctx context.Context, applicationID string) error {
	if r.src == nil {
		return contracts.New(contracts.ErrUnavailable, "capabilities: the registry has no package source")
	}
	if applicationID == "" {
		return contracts.New(contracts.ErrScopeMissing, "capabilities: no application to refresh")
	}
	hash, found, err := r.src.ActiveHash(ctx, applicationID)
	if err != nil {
		r.Deactivate(applicationID)
		return contracts.Wrap(contracts.ErrUnavailable, err, "capabilities: cannot read the installed packages")
	}
	now := r.clk.Now()
	if !found {
		r.Deactivate(applicationID)
		r.mu.Lock()
		r.absent[applicationID] = now
		r.mu.Unlock()
		return nil
	}
	r.mu.RLock()
	cur := r.apps[applicationID]
	r.mu.RUnlock()
	if cur != nil && cur.hash == hash && cur.fromSource {
		// Same package: keep the compiled index (and its embedding cache), renew
		// its age. checkedAt is the registry's own field and is written under the
		// registry lock.
		r.mu.Lock()
		if r.apps[applicationID] == cur {
			cur.checkedAt = now
		}
		r.mu.Unlock()
		return nil
	}
	p, err := r.src.Open(ctx, applicationID, hash)
	if err != nil {
		r.Deactivate(applicationID)
		return contracts.Wrap(contracts.ErrUnavailable, err, "capabilities: cannot open the active package")
	}
	ix, err := compile(p)
	if err != nil {
		r.Deactivate(applicationID)
		return err
	}
	if ix.app != applicationID {
		r.Deactivate(applicationID)
		return contracts.New(contracts.ErrScopeViolation, "capabilities: the source returned a package of another application").
			WithDetails("application_id", applicationID)
	}
	ix.fromSource, ix.checkedAt = true, now
	r.mu.Lock()
	r.apps[applicationID] = ix
	delete(r.absent, applicationID)
	r.mu.Unlock()
	return nil
}

// lookup returns the compiled entry of an application, loading or re-checking
// it first when it came from the Source.
func (r *Registry) lookup(ctx context.Context, applicationID string) (*index, error) {
	r.mu.RLock()
	ix := r.apps[applicationID]
	r.mu.RUnlock()

	switch {
	case ix == nil && r.src != nil:
		r.mu.RLock()
		t, known := r.absent[applicationID]
		r.mu.RUnlock()
		if known && r.clk.Now().Sub(t) < r.maxAge && r.clk.Now().Sub(t) >= 0 {
			break // recently confirmed absent: no active package
		}
		if err := r.Refresh(ctx, applicationID); err != nil {
			return nil, err
		}
	case ix != nil && ix.fromSource && r.src != nil:
		r.mu.RLock()
		age := r.clk.Now().Sub(ix.checkedAt)
		r.mu.RUnlock()
		if age >= r.maxAge || age < 0 {
			if err := r.Refresh(ctx, applicationID); err != nil {
				return nil, err
			}
		}
	}
	r.mu.RLock()
	ix = r.apps[applicationID]
	r.mu.RUnlock()
	if ix == nil {
		return nil, contracts.New(contracts.ErrUnavailable, "capabilities: the application has no active package").
			WithDetails("application_id", applicationID)
	}
	// Defence in depth: a package that stopped being active under the entry
	// (a Package whose status can change) is not served.
	if ix.pkg.Status() != policy.StatusActive {
		r.Deactivate(applicationID)
		return nil, contracts.New(contracts.ErrUnavailable, "capabilities: the application's package is no longer active").
			WithDetails("application_id", applicationID)
	}
	return ix, nil
}

// Info describes an application's registered package.
type Info struct {
	ApplicationID  string
	PackageVersion string
	ContentHash    string
	// Capabilities are the user capabilities the package declares, sorted. They
	// are what exists, not what a given caller may see: visibility is the policy
	// gateway's decision per request.
	Capabilities []string
	Languages    []string
}

// Info returns the registered package of an application. The error is unavailable
// when the application has no active package.
func (r *Registry) Info(ctx context.Context, applicationID string) (Info, error) {
	ix, err := r.lookup(ctx, applicationID)
	if err != nil {
		return Info{}, err
	}
	return Info{
		ApplicationID:  ix.app,
		PackageVersion: ix.version,
		ContentHash:    ix.hash,
		Capabilities:   append([]string(nil), ix.ids...),
		Languages:      append([]string(nil), ix.languages...),
	}, nil
}

// Applications lists the applications that currently have a registered package,
// sorted.
func (r *Registry) Applications() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.apps))
	for id := range r.apps {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
