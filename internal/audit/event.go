package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"time"

	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Formats of the hashed documents. A change to what is hashed is a new
// version, never an edit.
const (
	// EventFormat is the value of the "format" member of the canonical event
	// body that is hashed.
	EventFormat = "alixai.audit-event/v1"
	// GenesisFormat is the format of the document whose hash is the prev_hash
	// of the first event of a chain.
	GenesisFormat = "alixai.audit-genesis/v1"

	// IDPrefix is the prefix of event ids ("aud_" plus a ULID).
	IDPrefix = "aud"
)

// Event is one audit record, as stored and as exported.
type Event struct {
	ID            string `json:"id"`
	ApplicationID string `json:"application_id"`
	// TenantID is empty on an application chain.
	TenantID    string `json:"tenant_id"`
	WorkspaceID string `json:"workspace_id"`
	// ChainSeq is the 1-based position in the chain.
	ChainSeq int64     `json:"chain_seq"`
	Type     EventType `json:"type"`
	// OccurredAt is UTC with millisecond precision.
	OccurredAt  time.Time `json:"occurred_at"`
	PrincipalID string    `json:"principal_id"`
	SessionID   string    `json:"session_id"`
	// Data is the redacted payload as canonical JSON (an object).
	Data     json.RawMessage `json:"data"`
	PrevHash string          `json:"prev_hash"`
	Hash     string          `json:"hash"`
	// Tombstone is set only on an event read by Export whose payload was
	// erased (ErasedMarker is then its Data): the signed record of who erased
	// it, why and when. Hash is still the hash the event had; it cannot be
	// recomputed, so a recipient verifies the tombstone instead (ADR-0036).
	Tombstone *Tombstone `json:"tombstone,omitempty"`
}

// Chain returns the chain the event belongs to.
func (e Event) Chain() ChainKey {
	return ChainKey{ApplicationID: e.ApplicationID, TenantID: e.TenantID}
}

// NewEvent is what a caller hands to an append.
type NewEvent struct {
	// Type must be registered in the Log's registry.
	Type EventType
	// Data is marshalled to JSON, which must be an object (nil is an empty
	// object), and then redacted and canonicalised. See the package comment
	// for what it may contain.
	Data any
}

// eventBody is the document that is canonicalised and hashed. It holds every
// stored column of an event except the global rowid, prev_hash and hash (the
// last two enter the digest on their own).
type eventBody struct {
	Format        string          `json:"format"`
	ID            string          `json:"id"`
	ApplicationID string          `json:"application_id"`
	TenantID      string          `json:"tenant_id"`
	WorkspaceID   string          `json:"workspace_id"`
	ChainSeq      int64           `json:"chain_seq"`
	Type          string          `json:"type"`
	OccurredAtMs  int64           `json:"occurred_at_ms"`
	PrincipalID   string          `json:"principal_id"`
	SessionID     string          `json:"session_id"`
	Data          json.RawMessage `json:"data"`
}

func (e Event) body() eventBody {
	return eventBody{
		Format:        EventFormat,
		ID:            e.ID,
		ApplicationID: e.ApplicationID,
		TenantID:      e.TenantID,
		WorkspaceID:   e.WorkspaceID,
		ChainSeq:      e.ChainSeq,
		Type:          string(e.Type),
		OccurredAtMs:  e.OccurredAt.UnixMilli(),
		PrincipalID:   e.PrincipalID,
		SessionID:     e.SessionID,
		Data:          e.Data,
	}
}

var hexHashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidHash reports whether s is a lower-case hex SHA-256.
func ValidHash(s string) bool { return hexHashRE.MatchString(s) }

// ComputeHash returns sha256(canonical(event without hash and prev_hash) ||
// prev_hash) for the event's own columns and PrevHash, as lower-case hex. It
// fails when the data is not canonicalisable JSON.
func (e Event) ComputeHash() (string, error) {
	if !ValidHash(e.PrevHash) {
		return "", contracts.New(contracts.ErrSchemaInvalid, "audit: prev_hash is not a lower-case hex SHA-256")
	}
	canon, err := trust.CanonicalJSON(e.body())
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(canon)
	h.Write([]byte(e.PrevHash))
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashValid reports whether the event's hash is the one its columns and
// prev_hash produce. It checks the event on its own; whether it sits in an
// intact chain is VerifyChain's question. A nil error means the hash matches.
func (e Event) HashValid() error {
	if e.Tombstone != nil || string(e.Data) == ErasedMarker {
		return contracts.Newf(contracts.ErrConflict, "audit: event %s is erased: its hash cannot be recomputed, verify its tombstone", e.ID)
	}
	want, err := e.ComputeHash()
	if err != nil {
		return err
	}
	if want != e.Hash {
		return contracts.Newf(contracts.ErrInternal, "audit: event %s: hash does not match its content", e.ID).
			WithDetails("chain_seq", e.ChainSeq)
	}
	return nil
}

// GenesisHash is the prev_hash of the first event of a chain: the SHA-256 of
// the canonical JSON of {format, application_id, tenant_id}. Binding the first
// link to its owner means a chain cannot be moved to another tenant intact.
func GenesisHash(k ChainKey) string {
	canon, err := trust.CanonicalJSON(struct {
		Format        string `json:"format"`
		ApplicationID string `json:"application_id"`
		TenantID      string `json:"tenant_id"`
	}{GenesisFormat, k.ApplicationID, k.TenantID})
	if err != nil {
		// Strings of valid UTF-8 always canonicalise; an invalid key is
		// refused before it gets here, and a hash of nothing never matches.
		canon = nil
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}
