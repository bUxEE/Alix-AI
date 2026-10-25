package egress

import (
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// maxApplicationIDLen mirrors contracts.MaxScopeValueLen.
const maxApplicationIDLen = contracts.MaxScopeValueLen

// Registry holds the destination allowlist of each application. An application
// with no entry reaches nothing. It is safe for concurrent use.
type Registry struct {
	set   settings
	mu    sync.RWMutex
	lists map[string]*Allowlist
}

// NewRegistry returns an empty registry: every application is denied until
// Set gives it an allowlist. The options apply to every Dialer it hands out.
func NewRegistry(opts ...Option) *Registry {
	r := &Registry{set: newSettings(opts), lists: map[string]*Allowlist{}}
	if r.set.platform == nil {
		// Always present, so the composition root can fill it after the fact.
		r.set.platform = NewPlatformDeny()
	}
	return r
}

func validApplicationID(id string) bool {
	if id == "" || utf8.RuneCountInString(id) > maxApplicationIDLen || !utf8.ValidString(id) {
		return false
	}
	for _, r := range id {
		if r == '*' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Set installs (or replaces) the allowlist of an application. A nil allowlist
// revokes it. The change applies to the next dial of every Dialer already
// handed out for that application.
func (r *Registry) Set(applicationID string, a *Allowlist) error {
	if !validApplicationID(applicationID) {
		return contracts.New(contracts.ErrScopeViolation, "egress: application id is empty or malformed")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if a == nil {
		delete(r.lists, applicationID)
		return nil
	}
	r.lists[applicationID] = a
	return nil
}

// Revoke removes the allowlist of an application, so it reaches nothing.
func (r *Registry) Revoke(applicationID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.lists, applicationID)
}

// Allowlist returns the current allowlist of an application, or nil when it
// has none.
func (r *Registry) Allowlist(applicationID string) *Allowlist {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lists[applicationID]
}

// Dialer returns a Dialer bound to one application. It reads the registry on
// every dial, so Set and Revoke take effect at once; an application that has
// no allowlist, or an empty id, gets a Dialer that refuses everything.
func (r *Registry) Dialer(applicationID string) *Dialer {
	if r == nil {
		return &Dialer{}
	}
	return &Dialer{
		app:    applicationID,
		set:    r.set,
		source: func() *Allowlist { return r.Allowlist(applicationID) },
	}
}
