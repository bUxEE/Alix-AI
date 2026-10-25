package adapters

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// Bounds on the request fields an adapter forwards.
const (
	maxRequestFields    = 64
	maxFieldStringRunes = 4096
	maxFieldListItems   = 256
	maxFieldsJSONBytes  = 64 << 10
)

var fieldNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func invalidRequest(format string, args ...any) error {
	return contracts.New(contracts.ErrValidationFailed, "adapter request: "+fmt.Sprintf(format, args...))
}

// validateFields checks request fields: at most 64, keyed by lower snake_case
// names, each value a non-null JSON scalar or a flat list of scalars, strings of
// at most 4096 characters, numbers finite, 64 KiB of JSON in all. The core has
// already typed and normalised them; this is the last check before they leave
// the process, and the host validates them again.
func validateFields(fields map[string]any) error {
	if len(fields) > maxRequestFields {
		return invalidRequest("more than %d fields", maxRequestFields)
	}
	for name, v := range fields {
		if !fieldNameRE.MatchString(name) {
			return invalidRequest("a field name is not lower snake_case")
		}
		b, err := json.Marshal(v)
		if err != nil {
			return invalidRequest("field %s cannot be encoded as JSON", name)
		}
		var generic any
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.UseNumber()
		if err := dec.Decode(&generic); err != nil {
			return invalidRequest("field %s cannot be decoded", name)
		}
		switch x := generic.(type) {
		case []any:
			if len(x) > maxFieldListItems {
				return invalidRequest("field %s lists more than %d items", name, maxFieldListItems)
			}
			for _, item := range x {
				if !validScalar(item) {
					return invalidRequest("field %s holds a list item that is not a scalar", name)
				}
			}
		default:
			if !validScalar(x) {
				return invalidRequest("field %s is not a scalar or a flat list of scalars", name)
			}
		}
	}
	if len(fields) > 0 {
		b, err := json.Marshal(fields)
		if err != nil || len(b) > maxFieldsJSONBytes {
			return invalidRequest("fields are larger than %d bytes", maxFieldsJSONBytes)
		}
	}
	return nil
}

// validScalar accepts a non-null string, boolean or finite number.
func validScalar(v any) bool {
	switch x := v.(type) {
	case string:
		return utf8.ValidString(x) && utf8.RuneCountInString(x) <= maxFieldStringRunes
	case bool:
		return true
	case json.Number:
		f, err := x.Float64()
		return err == nil && !math.IsInf(f, 0) && !math.IsNaN(f)
	}
	return false
}

func validateCapability(id string) error {
	if !validIdentifier(id) {
		return invalidRequest("capability id is empty or malformed")
	}
	return nil
}

func validateKey(k actions.IdempotencyKey) error {
	if !validIdempotencyKey(string(k)) {
		return invalidRequest("idempotency key must be 16 to 128 URL-safe characters")
	}
	return nil
}

// Validate checks the request before it leaves the process.
func (r ReadRequest) Validate() error {
	if err := validateCapability(r.CapabilityID); err != nil {
		return err
	}
	return validateFields(r.Fields)
}

// Validate checks the request before it leaves the process.
func (r ValidateRequest) Validate() error {
	if err := validateCapability(r.CapabilityID); err != nil {
		return err
	}
	return validateFields(r.Fields)
}

// Validate checks the request before it leaves the process.
func (r PrepareRequest) Validate() error {
	if err := validateCapability(r.CapabilityID); err != nil {
		return err
	}
	return validateFields(r.Fields)
}

// Validate checks the request before it leaves the process: the key is well
// formed and is the one the prepared action carries, and the prepared action is
// for this capability. Nothing is committed without a prepared action.
func (r CommitRequest) Validate() error {
	if err := validateCapability(r.CapabilityID); err != nil {
		return err
	}
	if err := validateKey(r.IdempotencyKey); err != nil {
		return err
	}
	p := r.Prepared
	switch {
	case p.PreparationID == "":
		return invalidRequest("commit needs a prepared action")
	case string(p.CapabilityID) != r.CapabilityID:
		return invalidRequest("the prepared action is for another capability")
	case p.IdempotencyKey != r.IdempotencyKey:
		return invalidRequest("the prepared action carries another idempotency key")
	}
	return nil
}

// Validate checks the request before it leaves the process.
func (r StatusRequest) Validate() error {
	if err := validateCapability(r.CapabilityID); err != nil {
		return err
	}
	return validateKey(r.IdempotencyKey)
}

// Validate checks the request before it leaves the process.
func (r CancelRequest) Validate() error {
	if err := validateCapability(r.CapabilityID); err != nil {
		return err
	}
	return validateKey(r.IdempotencyKey)
}
