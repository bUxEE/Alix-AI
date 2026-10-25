package events

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const (
	// Format is the constant every signed event carries. It is part of what is
	// signed, so a signature made for another payload cannot be replayed as an
	// event, and a signature made for an event cannot be replayed as another
	// payload.
	Format = "alixai.application-event.v1"

	// MCPNotificationMethod is the method of the MCP notification that carries a
	// delivery in its params. The MCP adapter does not interpret it.
	MCPNotificationMethod = "notifications/alixai/event"

	// ServicePrincipal, ServiceRole and ServiceSession are the fixed service
	// identity an event is applied under. The session ID is never a session.
	ServicePrincipal = "alixai-events"
	ServiceRole      = "event-source"
	ServiceSession   = "events-intake"
)

// Event is the typed application event: what the host signs and what the intake
// verifies. Every member that decides what the event does is in it.
type Event struct {
	Format        string   `json:"format"`
	ID            string   `json:"id"`
	Type          string   `json:"type"`
	Kind          string   `json:"kind"`
	ApplicationID string   `json:"application_id"`
	TenantID      string   `json:"tenant_id"`
	WorkspaceID   string   `json:"workspace_id"`
	Entities      []Entity `json:"entities,omitempty"`
	Capabilities  []string `json:"capabilities,omitempty"`
	Everything    bool     `json:"everything,omitempty"`
	PrincipalID   string   `json:"principal_id,omitempty"`
}

// Entity is a record an event concerns. Version is the version the record is now
// at, for a record revision; empty means the host did not say.
type Entity struct {
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
	Version    string `json:"version,omitempty"`
}

// Envelope is a delivery: the event as it was signed, and its signatures. The
// event stays raw until its signature is verified, so what is verified is
// exactly what the sender signed.
type Envelope struct {
	Event      json.RawMessage   `json:"event"`
	Signatures []trust.Signature `json:"signatures"`
}

// session is the event the session layer applies. It is validated there too.
func (e Event) session() sessions.ApplicationEvent {
	out := sessions.ApplicationEvent{
		ID:           e.ID,
		Type:         e.Type,
		Kind:         sessions.InvalidationKind(e.Kind),
		Capabilities: append([]string(nil), e.Capabilities...),
		Everything:   e.Everything,
		PrincipalID:  e.PrincipalID,
	}
	for _, en := range e.Entities {
		out.Entities = append(out.Entities, sessions.EntityRef{EntityType: en.EntityType, EntityID: en.EntityID, Version: en.Version})
	}
	return out
}

func invalid(code contracts.ErrCode, msg string) error {
	return contracts.New(code, "events: "+msg)
}

// decodeEvent reads a verified event strictly: one JSON object, no unknown
// members (a signed member this version does not understand is a reason to
// refuse, never to ignore), no trailing data.
func decodeEvent(raw []byte) (Event, error) {
	var ev Event
	if err := decodeStrict(raw, &ev); err != nil {
		return Event{}, invalid(contracts.ErrSchemaInvalid, "the signed event is malformed: "+err.Error())
	}
	if ev.Format != Format {
		return Event{}, invalid(contracts.ErrSchemaInvalid, "the signed payload is not an application event")
	}
	return ev, nil
}

// decodeEnvelope reads a delivery strictly.
func decodeEnvelope(raw []byte) (Envelope, error) {
	var env Envelope
	if err := decodeStrict(raw, &env); err != nil {
		return Envelope{}, invalid(contracts.ErrSchemaInvalid, "the delivery is malformed: "+err.Error())
	}
	if len(env.Event) == 0 || bytes.Equal(bytes.TrimSpace(env.Event), []byte("null")) {
		return Envelope{}, invalid(contracts.ErrSchemaInvalid, "the delivery carries no event")
	}
	return env, nil
}

func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.New("not a JSON object of the expected shape")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("data after the end of the JSON value")
	}
	return nil
}

// Sign builds the delivery body for ev: it sets the format, signs the canonical
// JSON of the event with key (registered for the application in the receiving
// AlixAI's trust store under keyID) and returns the envelope as JSON, with the one
// signature a delivery carries. The
// signature is valid from signedAt until expiresAt; keep that short (the intake
// refuses more than Config.MaxValidity, default DefaultMaxValidity).
func Sign(ev Event, keyID string, key ed25519.PrivateKey, signedAt, expiresAt time.Time) ([]byte, error) {
	ev.Format = Format
	sig, err := trust.SignPayload(ev, keyID, key, signedAt, expiresAt)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrSchemaInvalid, err, "events: the event cannot be encoded")
	}
	return json.Marshal(Envelope{Event: body, Signatures: []trust.Signature{sig}})
}
