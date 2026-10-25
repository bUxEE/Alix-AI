package egress

import (
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Deployment-level provider destinations (additive plan section 12, task X-10).
//
// A Registry holds one allowlist per APPLICATION: the places an application's
// connectors may reach. A hosted model provider is not an application's
// connector. It is reached by the deployment, for whichever application the
// provider router admitted, so its destination belongs to the deployment: one
// allowlist per provider id, kept in a ProviderRegistry that is separate from
// the application Registry. The two never mix: an application's allowlist does
// not grant a provider host, and a provider's does not grant an application
// anything. Default deny holds here as well: a provider with no entry reaches
// nothing, and so does a nil or zero ProviderRegistry.

var providerIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// ProviderRegistry holds the destination allowlist of each external provider.
// It is safe for concurrent use.
type ProviderRegistry struct {
	set   settings
	mu    sync.RWMutex
	lists map[string]*Allowlist
}

// NewProviderRegistry returns an empty registry: every provider is denied
// until Set gives it an allowlist. The options apply to every Dialer it hands
// out.
func NewProviderRegistry(opts ...Option) *ProviderRegistry {
	return &ProviderRegistry{set: newSettings(opts), lists: map[string]*Allowlist{}}
}

// Set installs (or replaces) the allowlist of a provider. A nil allowlist
// revokes it. The change applies to the next dial of every Dialer already
// handed out for that provider.
func (r *ProviderRegistry) Set(providerID string, a *Allowlist) error {
	if r == nil || r.lists == nil || !providerIDPattern.MatchString(providerID) {
		return contracts.New(contracts.ErrValidationFailed, "egress: provider registry is not built or the provider id is malformed")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if a == nil {
		delete(r.lists, providerID)
		return nil
	}
	r.lists[providerID] = a
	return nil
}

// Revoke removes the allowlist of a provider, so it reaches nothing.
func (r *ProviderRegistry) Revoke(providerID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.lists, providerID)
}

// Allowlist returns the current allowlist of a provider, or nil.
func (r *ProviderRegistry) Allowlist(providerID string) *Allowlist {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lists[providerID]
}

// Dialer returns a Dialer bound to one provider. It reads the registry on every
// dial, so Set and Revoke take effect at once. A provider with no allowlist
// gets a Dialer that refuses everything. Denials are reported with the
// application name "provider:<id>".
func (r *ProviderRegistry) Dialer(providerID string) *Dialer {
	if r == nil {
		return &Dialer{}
	}
	return &Dialer{
		app:    "provider:" + providerID,
		set:    r.set,
		source: func() *Allowlist { return r.Allowlist(providerID) },
	}
}

// DestinationFromURL derives the allowlist entry that exactly covers one base
// URL: its host, its effective port and its scheme. It refuses anything but a
// bare origin (a path, query, fragment or userinfo), and the entry is then
// validated by NewAllowlist like any other (plain http only to loopback).
func DestinationFromURL(raw string) (Destination, error) {
	bad := func(msg string) error { return contracts.New(contracts.ErrValidationFailed, "egress: "+msg) }
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") {
		return Destination{}, bad("destination URL must be a bare origin such as https://host")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != SchemeHTTPS && scheme != SchemeHTTP {
		return Destination{}, bad("destination URL scheme must be https or http")
	}
	port := effectivePort(u)
	if port == 0 {
		return Destination{}, bad("destination URL has a malformed port")
	}
	return Destination{Host: u.Hostname(), Ports: []int{port}, Scheme: scheme}, nil
}
