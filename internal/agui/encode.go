package agui

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// MaxEventBytes is the largest event Encode sends. The stream hub caps a TurnEvent at 128 KiB; an AG-UI event wraps
// one (a block as activity content, a prepared action as tool arguments), so the bound has room for the wrapper.
const MaxEventBytes = 256 << 10

// Reasons of an encoding refusal (Error.Details["reason"]).
const (
	ReasonNotEmittable = "not_emittable"
	ReasonCredential   = "credential_member"
	ReasonWithheld     = "withheld_value"
	ReasonTooLarge     = "too_large"
	ReasonUnencodable  = "unencodable"
)

func encodeRefusal(reason, msg string) *contracts.Error {
	return contracts.New(contracts.ErrInternal, "agui: "+msg).WithDetails("reason", reason)
}

// Encode returns the JSON of an event, the bytes that go on the wire in a "data:" line or a WebSocket message. It is
// the one way an event leaves this package and it refuses, rather than sends:
//
//   - an event type that is not Emittable (a chunk form, Raw, a reasoning or sub-agent event, anything unknown);
//   - an event with a credential-shaped member name at any depth (token, secret, password, authorization, ...);
//   - an event that contains any of the withheld values, whatever member it sits in: the caller passes the secrets
//     of the run (the confirmation secret it holds) so that a leak through a data member is caught by value;
//   - an event larger than MaxEventBytes.
func Encode(ev Event, withhold ...string) ([]byte, error) {
	if ev == nil {
		return nil, encodeRefusal(ReasonNotEmittable, "there is no event")
	}
	if !Emittable(ev.Kind()) {
		return nil, encodeRefusal(ReasonNotEmittable, "this event type is never emitted").WithDetails("type", string(ev.Kind()))
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return nil, encodeRefusal(ReasonUnencodable, "the event cannot be encoded")
	}
	if len(data) > MaxEventBytes {
		return nil, encodeRefusal(ReasonTooLarge, "the event is larger than the stream allows")
	}
	if key := credentialMember(data); key != "" {
		return nil, encodeRefusal(ReasonCredential, "the event carries a member that may hold a credential")
	}
	for _, w := range withhold {
		if w != "" && bytes.Contains(data, []byte(w)) {
			return nil, encodeRefusal(ReasonWithheld, "the event carries a value that must never be sent")
		}
	}
	return data, nil
}

// credentialKeys are JSON member names that must never appear in an event, at any depth; the comparison ignores case,
// "_" and "-". It is the list of internal/stream (an exact-name list, not the log redactor's substring rule, so a
// member that merely mentions a token count is not refused). It is repeated here, not imported, because the stream
// package keeps it private; internal/agui's tests assert both lists refuse the same names.
var credentialKeys = map[string]bool{
	"token": true, "confirmationtoken": true, "confirmationsecret": true, "contexttoken": true,
	"accesstoken": true, "refreshtoken": true, "idtoken": true, "bearer": true, "secret": true,
	"clientsecret": true, "password": true, "passwd": true, "authorization": true, "apikey": true,
	"cookie": true, "setcookie": true, "privatekey": true, "credential": true, "credentials": true,
}

var keySeparators = strings.NewReplacer("_", "", "-", "")

func normalizeKey(k string) string {
	return keySeparators.Replace(strings.ToLower(k))
}

// credentialMember returns the first credential-shaped member name in a JSON document ("" when none).
func credentialMember(doc []byte) string {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return "unparseable"
	}
	var walk func(v any, depth int) string
	walk = func(v any, depth int) string {
		if depth > 64 {
			return "too_deep"
		}
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				if credentialKeys[normalizeKey(k)] {
					return k
				}
				if bad := walk(e, depth+1); bad != "" {
					return bad
				}
			}
		case []any:
			for _, e := range x {
				if bad := walk(e, depth+1); bad != "" {
					return bad
				}
			}
		}
		return ""
	}
	return walk(root, 0)
}

// Parsed is an AG-UI event read from the wire. It is the input side only: a chunk form or an event type this
// package does not know is accepted here, never produced.
type Parsed struct {
	Type EventType
	// Known is true for an event type of the pinned release.
	Known bool
	// Chunk is true for the compact chunk forms of a message or a tool call.
	Chunk bool
	// Raw is the event as received.
	Raw json.RawMessage
}

// ErrNotAnEvent is returned by ParseEvent for data that is not a JSON object with a string "type".
var ErrNotAnEvent = errors.New("agui: not an AG-UI event")

// ParseEvent reads the type of an AG-UI event. It accepts every event type of the release, the chunk forms and types
// it has never heard of (a newer protocol version): the caller decides what to do with them, and Emittable says
// which of them this package would ever send.
func ParseEvent(data []byte) (Parsed, error) {
	var head struct {
		Type *string `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil || head.Type == nil || *head.Type == "" {
		return Parsed{}, ErrNotAnEvent
	}
	t := EventType(*head.Type)
	return Parsed{
		Type:  t,
		Known: Known(t),
		Chunk: t == TextMessageChunk || t == ToolCallChunk || t == ReasoningMessageChunk,
		Raw:   append(json.RawMessage(nil), data...),
	}, nil
}
