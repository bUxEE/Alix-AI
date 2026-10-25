package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	alog "github.com/buxee/alix-ai/internal/log"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Redacted replaces every value the audit log removes at write time.
const Redacted = alog.Redacted

// DefaultMaxDataBytes bounds the canonical data of one event. A policy
// decision record is a few KiB; an event is a reference to what happened, not
// a copy of it.
const DefaultMaxDataBytes = 32 << 10

// prepared is event data ready to be hashed: redacted, canonical, bounded.
type prepared struct {
	canonical []byte
	top       map[string]any
}

// prepareData turns a caller's payload into the canonical, redacted JSON
// object that is stored and hashed. The order is deliberate: the payload is
// canonicalised first (so duplicate keys, floats and integers beyond 2^53 are
// refused, never silently altered), then redacted, then canonicalised again
// because redaction changes values. Errors never echo the payload.
func prepareData(red *alog.Redactor, data any, maxBytes int) (prepared, error) {
	var raw []byte
	switch t := data.(type) {
	case nil:
		raw = []byte("{}")
	case json.RawMessage:
		raw = t
	case []byte:
		raw = t
	default:
		b, err := json.Marshal(data)
		if err != nil {
			return prepared{}, contracts.New(contracts.ErrSchemaInvalid, "audit: event data cannot be marshalled to JSON")
		}
		raw = b
	}
	if len(raw) > 4*maxBytes {
		return prepared{}, contracts.Newf(contracts.ErrValidationFailed, "audit: event data is larger than %d bytes", maxBytes)
	}
	first, err := trust.Canonicalize(raw)
	if err != nil {
		return prepared{}, contracts.Wrap(contracts.ErrSchemaInvalid, err,
			"audit: event data must be JSON made of strings, integers (at most 2^53-1), booleans, null, arrays and objects")
	}

	dec := json.NewDecoder(bytes.NewReader(first))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return prepared{}, contracts.New(contracts.ErrInternal, "audit: canonical event data does not decode")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return prepared{}, contracts.New(contracts.ErrInternal, "audit: canonical event data has trailing content")
	}
	top, ok := v.(map[string]any)
	if !ok {
		return prepared{}, contracts.New(contracts.ErrSchemaInvalid, "audit: event data must be a JSON object")
	}

	redacted := redactWalk(red, top)
	out, err := json.Marshal(redacted)
	if err != nil {
		return prepared{}, contracts.New(contracts.ErrInternal, "audit: redacted event data does not marshal")
	}
	canon, err := trust.Canonicalize(out)
	if err != nil {
		return prepared{}, contracts.Wrap(contracts.ErrInternal, err, "audit: redacted event data is not canonical")
	}
	if len(canon) > maxBytes {
		return prepared{}, contracts.Newf(contracts.ErrValidationFailed, "audit: event data is larger than %d bytes", maxBytes)
	}
	return prepared{canonical: canon, top: top}, nil
}

// redactWalk returns x with sensitive values replaced. A value under a
// sensitive key is replaced whole, whatever it holds; strings are scrubbed for
// embedded credentials.
func redactWalk(red *alog.Redactor, x any) any {
	switch t := x.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, v := range t {
			if red.SensitiveKey(k) {
				out[k] = Redacted
			} else {
				out[k] = redactWalk(red, v)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, v := range t {
			out[i] = redactWalk(red, v)
		}
		return out
	case string:
		return red.String(t)
	}
	return x
}
