package credentials

import (
	"context"
	"strings"
)

// BackendName names a kind of backend. It is stored with the reference, so a
// later change of configuration cannot silently look in the wrong place.
type BackendName string

// The backends.
const (
	BackendOSStore       BackendName = "os_store"
	BackendEncryptedFile BackendName = "encrypted_file"
	BackendEnv           BackendName = "env"
	BackendFile          BackendName = "file"
)

// Slot says where one secret value lives in a backend.
type Slot struct {
	// Ref is the credential reference ID ("cred_..."): the key of the managed
	// backends. For a refresh value the service appends RefreshSuffix.
	Ref string
	// Locator is the environment variable or file name of a read-only backend;
	// "" for the managed ones.
	Locator string
	// Binding is the canonical binding string (provider, deployment, application,
	// tenant). The encrypted file authenticates it with the value, so a record
	// copied to another reference or another binding fails to open.
	Binding string
}

// RefreshSuffix is appended to a reference to name the slot of a refresh value.
const RefreshSuffix = "/refresh"

func (s Slot) refresh() Slot {
	s.Ref += RefreshSuffix
	return s
}

// Backend holds secret values. Implementations are safe for concurrent use,
// honour ctx, hold nothing decrypted between calls and never put a value, or the
// text of an operating system error that could echo one, in an error they return.
// Get returns ErrNotFound for a slot that holds nothing; Put and Delete on a
// read-only backend return ErrReadOnly (Delete may also succeed doing nothing).
type Backend interface {
	Name() BackendName
	// ReadOnly says that values are provisioned outside the service.
	ReadOnly() bool
	Get(ctx context.Context, slot Slot) (Secret, error)
	Put(ctx context.Context, slot Slot, value Secret) error
	Delete(ctx context.Context, slot Slot) error
}

func slotKey(s Slot) string { return s.Ref }

func binding(parts ...string) string { return strings.Join(parts, "\x1f") }
