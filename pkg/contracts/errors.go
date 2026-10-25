// Package contracts holds the public, dependency-free types shared by every
// AlixAI package, SDK and adapter: the request Scope, the stable ErrCode
// taxonomy and Error type, and the RiskLevel scale.
//
// It imports nothing from internal/ and nothing outside the standard library
// (tests excepted). The JSON shapes here are mirrored by schemas/common/*.
package contracts

import (
	"errors"
	"fmt"
)

// ErrCode is a stable, machine-readable failure classification. The string
// values are part of the public API (HTTP error envelopes, SDK error types,
// audit records). The full list and each code's Retryable() classification are
// pinned by testdata/errcodes.golden; changing either breaks
// TestErrCodesMatchGolden on purpose.
type ErrCode string

const (
	// ErrScopeMissing: a level of the verified context (deployment,
	// application, tenant, workspace, principal, roles/scopes, session) is
	// absent. There are no defaults and no wildcards.
	ErrScopeMissing ErrCode = "scope_missing"
	// ErrUnauthenticated: the caller's identity or context token could not be
	// verified (bad signature, wrong audience, expired token, replayed nonce).
	ErrUnauthenticated ErrCode = "unauthenticated"
	// ErrPermissionDenied: the verified principal is not allowed to do this.
	ErrPermissionDenied ErrCode = "permission_denied"
	// ErrScopeViolation: the request, or a value inside it, reaches outside
	// the verified scope (another tenant or workspace, a capability the
	// context token does not permit, a wildcard or malformed scope value).
	ErrScopeViolation ErrCode = "scope_violation"
	// ErrSchemaInvalid: a payload (request, adapter result or model output)
	// does not conform to its schema. Never partially trusted.
	ErrSchemaInvalid ErrCode = "schema_invalid"
	// ErrUnknownCapability: the capability ID is not registered for this
	// application, or is not available to this caller.
	ErrUnknownCapability ErrCode = "unknown_capability"
	// ErrAmbiguous: the request or an entity reference matches several
	// candidates; the user must clarify.
	ErrAmbiguous ErrCode = "ambiguous"
	// ErrMissingFields: required typed fields are absent.
	ErrMissingFields ErrCode = "missing_fields"
	// ErrValidationFailed: supplied values failed deterministic validation.
	ErrValidationFailed ErrCode = "validation_failed"
	// ErrStale: the data versions a prepared action was validated against no
	// longer match; it must be prepared again.
	ErrStale ErrCode = "stale"
	// ErrExpired: a prepared action, token or session passed its expiry.
	ErrExpired ErrCode = "expired"
	// ErrReplayed: a one-time artefact (nonce, confirmation, preparation) was
	// presented more than once.
	ErrReplayed ErrCode = "replayed"
	// ErrConfirmationRequired: the action needs an explicit user
	// confirmation before it can commit.
	ErrConfirmationRequired ErrCode = "confirmation_required"
	// ErrStepUpRequired: the action needs stronger authentication or the
	// host application's own UI; the AI cannot complete it alone.
	ErrStepUpRequired ErrCode = "step_up_required"
	// ErrConflict: the operation conflicts with the current state of the
	// target (concurrent modification, duplicate, business-rule clash).
	ErrConflict ErrCode = "conflict"
	// ErrUnavailable: a required dependency (adapter, model runtime, store,
	// source) is not reachable right now. Fails closed.
	ErrUnavailable ErrCode = "unavailable"
	// ErrDegraded: the service is running in a reduced mode that cannot serve
	// this request at the required quality or safety level.
	ErrDegraded ErrCode = "degraded"
	// ErrTimeout: a deadline passed before the operation completed.
	ErrTimeout ErrCode = "timeout"
	// ErrRateLimited: the caller exceeded a request-rate limit.
	ErrRateLimited ErrCode = "rate_limited"
	// ErrResourceExhausted: a bounded resource (queue depth, memory budget,
	// concurrency slots, quota) is full. Never a silent drop.
	ErrResourceExhausted ErrCode = "resource_exhausted"
	// ErrNoSupportedEvidence: no authorized source supports an answer, so
	// none is given (§3 rule 9).
	ErrNoSupportedEvidence ErrCode = "no_supported_evidence"
	// ErrUncertainOutcome: a commit was attempted but its result is unknown.
	// The caller must reconcile (idempotency key, audit) and must NOT retry
	// blindly.
	ErrUncertainOutcome ErrCode = "uncertain_outcome"
	// ErrPartialFailure: some effects of a multi-step operation succeeded and
	// some did not. Reported in full; never presented as success.
	ErrPartialFailure ErrCode = "partial_failure"
	// ErrQuarantined: the application, package, source or capability is
	// quarantined and may not execute.
	ErrQuarantined ErrCode = "quarantined"
	// ErrInternal: an unexpected AlixAI fault. Fails closed.
	ErrInternal ErrCode = "internal"
)

// codeInfo is the single source of truth for the taxonomy: its order is the
// canonical order of AllErrCodes and of the golden file.
var codeInfo = [...]struct {
	code      ErrCode
	retryable bool
}{
	{ErrScopeMissing, false},
	{ErrUnauthenticated, false},
	{ErrPermissionDenied, false},
	{ErrScopeViolation, false},
	{ErrSchemaInvalid, false},
	{ErrUnknownCapability, false},
	{ErrAmbiguous, false},
	{ErrMissingFields, false},
	{ErrValidationFailed, false},
	{ErrStale, false},
	{ErrExpired, false},
	{ErrReplayed, false},
	{ErrConfirmationRequired, false},
	{ErrStepUpRequired, false},
	{ErrConflict, false},
	{ErrUnavailable, true},
	{ErrDegraded, true},
	{ErrTimeout, true},
	{ErrRateLimited, true},
	{ErrResourceExhausted, true},
	{ErrNoSupportedEvidence, false},
	{ErrUncertainOutcome, false},
	{ErrPartialFailure, false},
	{ErrQuarantined, false},
	{ErrInternal, false},
}

// AllErrCodes returns every defined code in canonical order. The slice is a
// copy; callers may modify it.
func AllErrCodes() []ErrCode {
	out := make([]ErrCode, len(codeInfo))
	for i, ci := range codeInfo {
		out[i] = ci.code
	}
	return out
}

// Valid reports whether c is one of the defined codes.
func (c ErrCode) Valid() bool {
	for _, ci := range codeInfo {
		if ci.code == c {
			return true
		}
	}
	return false
}

// String returns the stable wire string.
func (c ErrCode) String() string { return string(c) }

// Retryable reports whether re-sending the identical request unchanged, after
// a delay, can reasonably produce a different outcome. It is true only for
// transient capacity or availability failures.
//
// Everything that needs the user, the host or a new preparation to change
// something first (stale, conflict, missing_fields, confirmation_required...)
// is false, as are uncertain_outcome and partial_failure: blindly repeating a
// request whose effects are unknown or partially applied can double-commit.
// An undefined code is not retryable (fail closed).
func (c ErrCode) Retryable() bool {
	for _, ci := range codeInfo {
		if ci.code == c {
			return ci.retryable
		}
	}
	return false
}

// Error is the typed error carried across package, API and SDK boundaries.
// Its JSON form is schemas/common/error.schema.json.
type Error struct {
	// Code is the stable classification.
	Code ErrCode `json:"code"`
	// Message is a human-readable, non-sensitive description. It never carries
	// bearer credentials, secrets or other tenants' data.
	Message string `json:"message"`
	// Retryable mirrors Code.Retryable() at construction time so clients need
	// not embed the table.
	Retryable bool `json:"retryable"`
	// Details holds optional structured, non-sensitive context (for example
	// {"levels": ["tenant"]} for scope_missing).
	Details map[string]any `json:"details,omitempty"`

	cause error
}

// New returns an Error with the given code and message. Retryable is derived
// from the code.
func New(code ErrCode, message string) *Error {
	return &Error{Code: code, Message: message, Retryable: code.Retryable()}
}

// Newf is New with a formatted message.
func Newf(code ErrCode, format string, args ...any) *Error {
	return New(code, fmt.Sprintf(format, args...))
}

// Wrap returns an Error with the given code and message whose Unwrap is cause.
// If cause is nil it is equivalent to New.
func Wrap(code ErrCode, cause error, message string) *Error {
	e := New(code, message)
	e.cause = cause
	return e
}

// WithDetails returns e after setting one detail key. It is meant for fluent
// construction: New(...).WithDetails("field", "tenant_id").
func (e *Error) WithDetails(key string, value any) *Error {
	if e.Details == nil {
		e.Details = make(map[string]any, 1)
	}
	e.Details[key] = value
	return e
}

// Error implements the error interface: "code: message" (plus the cause).
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	s := string(e.Code)
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.cause != nil {
		s += ": " + e.cause.Error()
	}
	return s
}

// Unwrap returns the wrapped cause, if any.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Is makes errors.Is(err, contracts.New(code, "")) match any *Error with the
// same code. A target with a non-empty Message must match the message too.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok || e == nil || t == nil {
		return false
	}
	if e.Code != t.Code {
		return false
	}
	return t.Message == "" || t.Message == e.Message
}

// CodeOf returns the ErrCode of the first *Error in err's chain. It returns
// ErrInternal for any other non-nil error (fail closed: an untyped failure is
// never mapped to something that looks benign) and "" for nil.
func CodeOf(err error) ErrCode {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) && e != nil {
		return e.Code
	}
	return ErrInternal
}

// HasCode reports whether any *Error in err's chain has the given code.
func HasCode(err error, code ErrCode) bool {
	for err != nil {
		if e, ok := err.(*Error); ok && e != nil && e.Code == code {
			return true
		}
		switch u := err.(type) {
		case interface{ Unwrap() error }:
			err = u.Unwrap()
		case interface{ Unwrap() []error }:
			for _, sub := range u.Unwrap() {
				if HasCode(sub, code) {
					return true
				}
			}
			return false
		default:
			return false
		}
	}
	return false
}
