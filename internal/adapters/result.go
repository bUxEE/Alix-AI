package adapters

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

// DefaultMaxResponseBytes is the reply size limit used when a caller names
// none: 1 MiB. A DeterministicResult is bounded by the schema (256 identifiers
// and values, 64 errors), so a legitimate reply is a few KiB; anything near the
// limit is hostile or broken.
const DefaultMaxResponseBytes = 1 << 20

// ReadLimited reads a reply of at most limit bytes (DefaultMaxResponseBytes when
// limit is zero or negative). It stops reading one byte past the limit, so an
// unbounded or enormous reply costs at most limit+1 bytes of memory, and returns
// a schema_invalid error with Details["reason"] == ReasonResponseTooLarge
// instead of a truncated body. A read error is returned unchanged for the
// transport to classify.
func ReadLimited(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = DefaultMaxResponseBytes
	}
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, responseTooLarge(limit)
	}
	return b, nil
}

func responseTooLarge(limit int64) error {
	return schemaInvalid("reply is larger than the size limit").
		WithDetails("reason", ReasonResponseTooLarge).
		WithDetails("limit_bytes", limit)
}

var idempotencyKeyRE = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

// validIdempotencyKey mirrors schemas/actions/shared idempotencyKey.
func validIdempotencyKey(k string) bool { return idempotencyKeyRE.MatchString(k) }

// Expect is what the core knows about the call a reply answers. Whatever the
// reply claims is checked against it; nothing the reply says about the call is
// believed.
type Expect struct {
	// Operation is the operation that was called; it must be one of the six
	// that return a DeterministicResult.
	Operation Operation
	// CapabilityID is the capability the call was made for.
	CapabilityID string
	// IdempotencyKey is the key sent with Commit, Status or Cancel, and empty
	// for the other operations.
	IdempotencyKey string
	// EchoedIdempotencyKey is the key the host's reply carried, when the
	// protocol echoes one (a response header or an envelope member). Empty means
	// the protocol does not echo it. A key that differs from IdempotencyKey is a
	// reply to another request.
	EchoedIdempotencyKey string
	// MaxBytes bounds the reply; zero means DefaultMaxResponseBytes.
	MaxBytes int
}

func (e Expect) validate() (actions.DeterministicResultOperation, error) {
	op, ok := e.Operation.Result()
	switch {
	case !ok:
		return "", contracts.New(contracts.ErrInternal, "adapters: operation does not return a DeterministicResult")
	case !validIdentifier(e.CapabilityID):
		return "", contracts.New(contracts.ErrInternal, "adapters: capability id is malformed")
	case e.IdempotencyKey != "" && !validIdempotencyKey(e.IdempotencyKey):
		return "", contracts.New(contracts.ErrInternal, "adapters: idempotency key is malformed")
	case needsKey(e.Operation) && e.IdempotencyKey == "":
		return "", contracts.New(contracts.ErrInternal, "adapters: operation needs an idempotency key")
	}
	return op, nil
}

// needsKey reports whether an operation is addressed by idempotency key.
func needsKey(op Operation) bool {
	return op == OpCommit || op == OpStatus || op == OpCancel
}

// Normalise turns a host's raw reply into a DeterministicResult, or fails with
// schema_invalid. It is the one place a reply becomes a fact (master plan 4.5
// check 2: a result that does not normalise is never partially trusted):
//
//   - the reply must be one JSON object of at most MaxBytes;
//   - operation and capability_id belong to the core. The reply need not carry
//     them; if it does they must equal the call, otherwise it answers a
//     different request. The core's values are what the result holds;
//   - the object must satisfy schemas/actions/deterministic-result, including
//     its cross-field rules (errors empty exactly for ok and queued, freshness
//     for ok, queued and partial, follow-up for queued, a preparation exactly
//     for a successful prepare) and every shared pattern, range and enum;
//   - it must then decode strictly into the generated type;
//   - an echoed idempotency key must match the one sent.
//
// Prose, a bare status string, a result with extra members and a reply for
// another capability are all schema_invalid. The error names the locations that
// failed and never quotes the reply.
func Normalise(raw []byte, exp Expect) (actions.DeterministicResult, error) {
	var zero actions.DeterministicResult
	op, err := exp.validate()
	if err != nil {
		return zero, err
	}
	limit := exp.MaxBytes
	if limit <= 0 {
		limit = DefaultMaxResponseBytes
	}
	if len(raw) > limit {
		return zero, responseTooLarge(int64(limit))
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return zero, schemaInvalid("reply is not exactly one JSON document")
	}
	obj, ok := instance.(map[string]any)
	if !ok {
		return zero, schemaInvalid("reply is not a JSON object")
	}
	if v, present := obj["operation"]; present {
		if s, _ := v.(string); s != string(op) {
			return zero, schemaInvalid("reply names a different operation than the one that was called")
		}
	}
	if v, present := obj["capability_id"]; present {
		if s, _ := v.(string); s != exp.CapabilityID {
			return zero, schemaInvalid("reply names a different capability than the one that was called")
		}
	}
	if exp.EchoedIdempotencyKey != "" && exp.EchoedIdempotencyKey != exp.IdempotencyKey {
		return zero, schemaInvalid("reply answers a different idempotency key")
	}
	obj["operation"] = string(op)
	obj["capability_id"] = exp.CapabilityID

	schema, _, err := compiledSchemas()
	if err != nil {
		return zero, contracts.Wrap(contracts.ErrInternal, err, "adapters: result schema unavailable")
	}
	if err := schema.Validate(obj); err != nil {
		return zero, schemaInvalid("reply is not a DeterministicResult: " + violations(err))
	}
	canonical, err := json.Marshal(obj)
	if err != nil {
		return zero, schemaInvalid("reply cannot be re-encoded")
	}
	var res actions.DeterministicResult
	if err := decodeStrict(canonical, &res); err != nil {
		return zero, schemaInvalid("reply does not decode into a DeterministicResult")
	}
	return res, nil
}

// CheckResult re-validates a result that is already a typed value, for example
// one an adapter built by hand. It encodes the result and runs it through
// Normalise, so the same schema, authority and mismatch rules apply and a
// hand-built "ok with errors" or "commit for another capability" is refused.
// The returned value is the validated copy.
func CheckResult(res actions.DeterministicResult, exp Expect) (actions.DeterministicResult, error) {
	b, err := json.Marshal(res)
	if err != nil {
		return actions.DeterministicResult{}, schemaInvalid("result cannot be encoded: it holds a value outside the schema's enums")
	}
	return Normalise(b, exp)
}

// StatusShowsNoRecord reports whether a Status result is the no-record marker:
// the host answered that it has no record of the idempotency key. It is
// rejected with exactly one validation_failed error on field idempotency_key.
//
// It is the only Status result that proves a Commit was not applied, and so the
// only one after which the Commit may be sent again. A Status call can be
// rejected for other reasons (a reauthorisation denial, a scope error) while the
// commit it asks about was applied, so every other result, rejected or not, has
// to be treated as an unknown outcome.
func StatusShowsNoRecord(res actions.DeterministicResult) bool {
	if res.Operation != actions.DeterministicResultOperationStatus ||
		res.Status != actions.DeterministicResultStatusRejected ||
		len(res.Errors) != 1 || len(res.Identifiers) != 0 {
		return false
	}
	e := res.Errors[0]
	return e.Code == common.ErrorCodeValidationFailed && e.Field != nil && *e.Field == "idempotency_key"
}
