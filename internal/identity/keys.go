package identity

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// IssuerKey is one Ed25519 public key registered for an application. The host
// application keeps the private half; AlixAI never holds a secret able to
// mint tokens (ADR 0013, option E rejected).
type IssuerKey struct {
	// KeyID is the "kid" a token names. It is only a selector into the
	// registry: nothing else in a token chooses or supplies key material.
	KeyID string
	// ApplicationID is the application this key speaks for. A token signed by
	// this key can only ever carry this application level.
	ApplicationID string
	// Issuer is the exact "iss" value tokens signed by this key must carry.
	Issuer string
	// Public is the 32-byte Ed25519 public key.
	Public ed25519.PublicKey
	// NotAfter, if set, retires the key: from that instant it verifies
	// nothing. It bounds the overlap while a key is rotated out.
	NotAfter time.Time
}

// ErrKeyNotFound is what a KeyResolver returns for a key id it does not hold,
// including a key that has been revoked.
var ErrKeyNotFound = errors.New("identity: issuer key not found")

// KeyResolver finds the issuer key for a key id. It is the seam to wherever
// keys are stored: KeyRegistry here, the trust store (internal/trust) later.
// An implementation returns ErrKeyNotFound for an unknown or revoked key and
// any other error when it cannot answer; the verifier refuses the token in
// both cases.
type KeyResolver interface {
	Lookup(ctx context.Context, keyID string) (IssuerKey, error)
}

// KeyRegistry is an in-memory KeyResolver holding issuer keys per application.
// It is safe for concurrent use. Revocation takes effect for the next lookup.
type KeyRegistry struct {
	mu   sync.RWMutex
	keys map[string]IssuerKey
	log  *slog.Logger
}

var _ KeyResolver = (*KeyRegistry)(nil)

// NewKeyRegistry returns an empty registry. Key registration and removal are
// logged to log (a nil log discards them): they are the events SEC-037 wants
// recorded, and the audit log (M1-15) takes them over from there.
func NewKeyRegistry(log *slog.Logger) *KeyRegistry {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &KeyRegistry{keys: make(map[string]IssuerKey), log: log}
}

// validateIssuerKey checks the parts of a key every resolver demands: the key
// id, the application, the issuer and a 32-byte Ed25519 public key.
func validateIssuerKey(k IssuerKey) error {
	switch {
	case !validKeyID(k.KeyID):
		return contracts.New(contracts.ErrValidationFailed, "identity: issuer key id is empty, too long or not [A-Za-z0-9._-]")
	case !validIdentifier(k.ApplicationID):
		return contracts.New(contracts.ErrValidationFailed, "identity: issuer key application id is not a valid scope identifier")
	case k.Issuer == "" || len(k.Issuer) > maxIssuerLen:
		return contracts.New(contracts.ErrValidationFailed, "identity: issuer key issuer is empty or too long")
	case len(k.Public) != ed25519.PublicKeySize:
		return contracts.New(contracts.ErrValidationFailed, "identity: issuer public key must be a 32-byte Ed25519 key")
	}
	return nil
}

// Register adds a key. The key id must be new, the application and issuer
// well-formed and the public key 32 bytes; the registry keeps its own copy of
// the key bytes.
func (r *KeyRegistry) Register(k IssuerKey) error {
	if err := validateIssuerKey(k); err != nil {
		return err
	}
	k.Public = append(ed25519.PublicKey(nil), k.Public...)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.keys[k.KeyID]; dup {
		return contracts.New(contracts.ErrConflict, "identity: issuer key id already registered")
	}
	r.keys[k.KeyID] = k
	r.log.Info("issuer key registered", "key_id", k.KeyID, "application_id", k.ApplicationID, "issuer", k.Issuer)
	return nil
}

// Revoke removes a key. Tokens it signed are refused from the next
// verification on, however long they have left to live. It reports whether the
// key existed.
func (r *KeyRegistry) Revoke(keyID string) bool {
	r.mu.Lock()
	k, ok := r.keys[keyID]
	delete(r.keys, keyID)
	r.mu.Unlock()
	if ok {
		r.log.Warn("issuer key revoked", "key_id", k.KeyID, "application_id", k.ApplicationID)
	}
	return ok
}

// RevokeApplication removes every key of an application and returns how many
// there were: the response to a compromised host application.
func (r *KeyRegistry) RevokeApplication(applicationID string) int {
	r.mu.Lock()
	var gone []string
	for id, k := range r.keys {
		if k.ApplicationID == applicationID {
			gone = append(gone, id)
			delete(r.keys, id)
		}
	}
	r.mu.Unlock()
	sort.Strings(gone)
	for _, id := range gone {
		r.log.Warn("issuer key revoked", "key_id", id, "application_id", applicationID)
	}
	return len(gone)
}

// Retire sets a key's NotAfter, so a rotated-out key stops verifying at a
// known instant instead of lingering. It fails for an unknown key.
func (r *KeyRegistry) Retire(keyID string, notAfter time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	k, ok := r.keys[keyID]
	if !ok {
		return contracts.New(contracts.ErrValidationFailed, "identity: issuer key not registered")
	}
	k.NotAfter = notAfter.UTC()
	r.keys[keyID] = k
	r.log.Info("issuer key retirement set", "key_id", keyID, "application_id", k.ApplicationID, "not_after", k.NotAfter)
	return nil
}

// Lookup implements KeyResolver.
func (r *KeyRegistry) Lookup(_ context.Context, keyID string) (IssuerKey, error) {
	r.mu.RLock()
	k, ok := r.keys[keyID]
	r.mu.RUnlock()
	if !ok {
		return IssuerKey{}, ErrKeyNotFound
	}
	k.Public = append(ed25519.PublicKey(nil), k.Public...)
	return k, nil
}

// validIdentifier reports whether s is acceptable as any single scope level
// value (non-empty, at most contracts.MaxScopeValueLen characters, no
// wildcard, padding or control characters). It asks contracts.Scope itself, so
// the rule can never drift from the one the whole system enforces.
func validIdentifier(s string) bool {
	probe := contracts.Scope{
		DeploymentID: "x", ApplicationID: s, TenantID: "x", WorkspaceID: "x",
		PrincipalID: "x", Roles: []string{"x"}, CapabilityScopes: []string{"x"}, SessionID: "x",
	}
	return probe.Validate() == nil
}
