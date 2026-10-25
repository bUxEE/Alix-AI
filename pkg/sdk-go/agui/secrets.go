package agui

import (
	"context"
	"regexp"
	"sort"
	"sync"
	"time"
)

// SecretKey names the confirmation secret of one prepared action: the thread (the session of the context token) and the
// preparation id the interrupt carries. A secret is never looked up, attached or kept under anything wider.
type SecretKey struct {
	ThreadID      string
	PreparationID string
}

// SecretStore keeps the single-use confirmation secrets of the actions a thread has prepared, on the host's server. A host
// that runs several replicas of its backend passes a store they share; the default is in memory and per process. The store
// holds a secret only until it is used or expires, and never logs one. Implementations must be safe for concurrent use.
type SecretStore interface {
	// Put keeps secret under key until expires.
	Put(ctx context.Context, key SecretKey, secret string, expires time.Time) error
	// Get returns the secret under key, if there is one that has not expired.
	Get(ctx context.Context, key SecretKey) (secret string, ok bool, err error)
	// Delete forgets the secret under key. Deleting what is not there is not an error.
	Delete(ctx context.Context, key SecretKey) error
}

// confirmationSecretPattern is the shape of the secret the gateway issues (pkg/sdk-go and schemas/actions); anything else is
// not stored, so a header that is not a secret never becomes one.
var confirmationSecretPattern = regexp.MustCompile(`^cfm_[A-Za-z0-9_-]{43}$`)

// DefaultMaxSecrets bounds the in-memory store.
const DefaultMaxSecrets = 1024

// MemorySecretStore is the in-process SecretStore: bounded, with expiry, safe for concurrent use.
type MemorySecretStore struct {
	now func() time.Time
	max int

	mu      sync.Mutex
	entries map[SecretKey]memorySecret
	order   int64
}

type memorySecret struct {
	secret  string
	expires time.Time
	order   int64
}

// NewMemorySecretStore returns an empty store. now is the clock (nil is time.Now) and max bounds the entries (zero is
// DefaultMaxSecrets); when full, expired entries go first and then the oldest.
func NewMemorySecretStore(now func() time.Time, max int) *MemorySecretStore {
	if now == nil {
		now = time.Now
	}
	if max <= 0 {
		max = DefaultMaxSecrets
	}
	return &MemorySecretStore{now: now, max: max, entries: map[SecretKey]memorySecret{}}
}

// Put implements SecretStore.
func (s *MemorySecretStore) Put(_ context.Context, key SecretKey, secret string, expires time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if !expires.After(now) {
		delete(s.entries, key)
		return nil
	}
	if _, replacing := s.entries[key]; !replacing && len(s.entries) >= s.max {
		for k, e := range s.entries {
			if !e.expires.After(now) {
				delete(s.entries, k)
			}
		}
		if len(s.entries) >= s.max {
			keys := make([]SecretKey, 0, len(s.entries))
			for k := range s.entries {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool { return s.entries[keys[i]].order < s.entries[keys[j]].order })
			delete(s.entries, keys[0])
		}
	}
	s.order++
	s.entries[key] = memorySecret{secret: secret, expires: expires, order: s.order}
	return nil
}

// Get implements SecretStore.
func (s *MemorySecretStore) Get(_ context.Context, key SecretKey) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return "", false, nil
	}
	if !e.expires.After(s.now()) {
		delete(s.entries, key)
		return "", false, nil
	}
	return e.secret, true, nil
}

// Delete implements SecretStore.
func (s *MemorySecretStore) Delete(_ context.Context, key SecretKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key)
	return nil
}

// Len reports how many secrets are held (expired ones that have not been swept included); for tests and metrics.
func (s *MemorySecretStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}
