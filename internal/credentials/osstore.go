package credentials

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// OSStore is the operating system's secret store: Windows Credential Manager,
// macOS Keychain, the Linux Secret Service. It is an interface so the encrypted
// file is always available where there is no store (a headless server) and so
// tests run on the in-memory fake. The implementation over
// github.com/zalando/go-keyring is in the subpackage oskeyring, which a
// composition root imports; this package imports no such module.
//
// The methods mirror go-keyring's. Get returns ErrOSStoreNotFound for an entry
// that does not exist, and Delete of an absent entry returns it too.
type OSStore interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

// ErrOSStoreNotFound is what an OSStore returns for an entry that does not exist.
var ErrOSStoreNotFound = errors.New("credentials: no such entry in the OS store")

// OSStoreService is the service name entries are filed under.
const OSStoreService = "alixai-credentials"

// OSBackend keeps each secret as one entry of an OSStore.
type OSBackend struct {
	store   OSStore
	service string
}

var _ Backend = (*OSBackend)(nil)

// NewOSBackend returns a backend over store. The service name is OSStoreService.
func NewOSBackend(store OSStore) *OSBackend {
	return &OSBackend{store: store, service: OSStoreService}
}

// Name implements Backend.
func (*OSBackend) Name() BackendName { return BackendOSStore }

// ReadOnly implements Backend.
func (*OSBackend) ReadOnly() bool { return false }

// Get implements Backend.
func (b *OSBackend) Get(ctx context.Context, slot Slot) (Secret, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	v, err := b.store.Get(b.service, slotKey(slot))
	switch {
	case errors.Is(err, ErrOSStoreNotFound):
		return "", ErrNotFound
	case err != nil:
		return "", errBackend // the text of an OS error is not passed on
	}
	return Secret(v), nil
}

// Put implements Backend.
func (b *OSBackend) Put(ctx context.Context, slot Slot, value Secret) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.store.Set(b.service, slotKey(slot), value.Reveal()) != nil {
		return errBackend
	}
	return nil
}

// Delete implements Backend. An entry that is already absent is not an error.
func (b *OSBackend) Delete(ctx context.Context, slot Slot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := b.store.Delete(b.service, slotKey(slot)); err != nil && !errors.Is(err, ErrOSStoreNotFound) {
		return errBackend
	}
	return nil
}

// errBackend is every other failure of a backend, with no cause attached.
var errBackend = errors.New("credentials: the backend failed")

// MemoryOSStore is an in-memory OSStore, the fake the backend tests run on. It
// holds values in the process only; a composition root never wires it into a
// production service.
type MemoryOSStore struct {
	mu sync.Mutex
	m  map[[2]string]string
	// Fail, when set, makes every call fail with it (tests of a locked or
	// unreachable store).
	Fail error
}

var _ OSStore = (*MemoryOSStore)(nil)

// Format implements fmt.Formatter: the fake holds values, so no verb prints its contents.
func (s *MemoryOSStore) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte("credentials.MemoryOSStore"))
}

// NewMemoryOSStore returns an empty fake store.
func NewMemoryOSStore() *MemoryOSStore { return &MemoryOSStore{m: map[[2]string]string{}} }

// Get implements OSStore.
func (s *MemoryOSStore) Get(service, user string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Fail != nil {
		return "", s.Fail
	}
	v, ok := s.m[[2]string{service, user}]
	if !ok {
		return "", ErrOSStoreNotFound
	}
	return v, nil
}

// Set implements OSStore.
func (s *MemoryOSStore) Set(service, user, password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Fail != nil {
		return s.Fail
	}
	s.m[[2]string{service, user}] = password
	return nil
}

// Delete implements OSStore.
func (s *MemoryOSStore) Delete(service, user string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Fail != nil {
		return s.Fail
	}
	k := [2]string{service, user}
	if _, ok := s.m[k]; !ok {
		return ErrOSStoreNotFound
	}
	delete(s.m, k)
	return nil
}

// Len returns the number of entries (tests).
func (s *MemoryOSStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// Dump returns every stored value joined (tests that look for a canary in the
// store's contents).
func (s *MemoryOSStore) Dump() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.m))
	for k, v := range s.m {
		out = append(out, k[0]+"/"+k[1]+"="+v)
	}
	return out
}
