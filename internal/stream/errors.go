package stream

import (
	"errors"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Reason says why a stream operation was refused, finer than the error code. A
// client that resumes a stream branches on it (a slow consumer reconnects at
// once, an expired cursor starts the turn's account again from the turn
// resource), so the reasons are stable strings.
type Reason string

const (
	// ReasonSlowConsumer: the subscriber fell further behind than its queue
	// allows, or out of the replay window. It may resume from the last event it
	// received, if that event is still retained.
	ReasonSlowConsumer Reason = "slow_consumer"
	// ReasonCursorExpired: the events after the cursor are no longer retained.
	// The stream is never resumed with a gap.
	ReasonCursorExpired Reason = "cursor_expired"
	// ReasonCursorAhead: the cursor names an event the stream has not produced.
	ReasonCursorAhead Reason = "cursor_ahead"
	// ReasonCursorMalformed: the cursor is not a sequence number.
	ReasonCursorMalformed Reason = "cursor_malformed"
	// ReasonNotVisible: no such turn is visible in the caller's scope. An
	// unknown turn, a turn of another principal and a turn past its retention
	// all answer the same way, so a turn id is no oracle.
	ReasonNotVisible Reason = "turn_not_visible"
	// ReasonTurnClosed: the producer has finished or abandoned the turn.
	ReasonTurnClosed Reason = "turn_closed"
	// ReasonOutOfOrder: an event does not continue the turn's sequence.
	ReasonOutOfOrder Reason = "out_of_order"
	// ReasonEventRefused: an event was refused before it was buffered
	// (validation, size, a credential-shaped member).
	ReasonEventRefused Reason = "event_refused"
	// ReasonCapacity: a bound of the hub was reached.
	ReasonCapacity Reason = "capacity"
	// ReasonSubscribers: the turn already has its most subscribers.
	ReasonSubscribers Reason = "subscribers"
	// ReasonAborted: the turn was abandoned before it finished.
	ReasonAborted Reason = "aborted"
)

// Error is the error of this package. It wraps a contracts.Error, so
// contracts.CodeOf and the gateway's error writer see the typed code, and it
// carries a Reason that errors.Is can match against the sentinels below.
type Error struct {
	Reason Reason
	err    *contracts.Error
}

func newError(reason Reason, code contracts.ErrCode, message string) *Error {
	return &Error{Reason: reason, err: contracts.New(code, message).WithDetails("reason", string(reason))}
}

// Error implements error.
func (e *Error) Error() string { return e.err.Error() }

// Unwrap returns the typed contracts.Error.
func (e *Error) Unwrap() error { return e.err }

// Is matches another *Error with the same Reason, so
// errors.Is(err, stream.ErrSlowConsumer) works on any error built from it.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Reason == e.Reason
}

// The sentinels. Compare with errors.Is.
var (
	ErrSlowConsumer    = newError(ReasonSlowConsumer, contracts.ErrResourceExhausted, "the stream is read too slowly; reconnect and resume from the last event received")
	ErrCursorExpired   = newError(ReasonCursorExpired, contracts.ErrExpired, "the events after the cursor are no longer retained")
	ErrCursorAhead     = newError(ReasonCursorAhead, contracts.ErrValidationFailed, "the cursor is ahead of the stream")
	ErrCursorMalformed = newError(ReasonCursorMalformed, contracts.ErrValidationFailed, "the cursor is not a sequence number")
	ErrNotVisible      = newError(ReasonNotVisible, contracts.ErrScopeViolation, "the turn is not visible in this scope")
	ErrTurnClosed      = newError(ReasonTurnClosed, contracts.ErrConflict, "the turn is already finished")
	ErrOutOfOrder      = newError(ReasonOutOfOrder, contracts.ErrConflict, "the event does not continue the turn's sequence")
	ErrEventRefused    = newError(ReasonEventRefused, contracts.ErrSchemaInvalid, "the event was refused and was not sent")
	ErrCapacity        = newError(ReasonCapacity, contracts.ErrResourceExhausted, "the stream capacity of this deployment is reached")
	ErrSubscribers     = newError(ReasonSubscribers, contracts.ErrResourceExhausted, "the turn has the most subscribers it allows")
	ErrAborted         = newError(ReasonAborted, contracts.ErrUnavailable, "the turn was abandoned before it finished")
)

// ReasonOf returns the Reason of err, or "" when err is not a stream error.
func ReasonOf(err error) Reason {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ""
}

// refuse wraps the cause of a refused event: the code and the violations of the
// block validator are kept (they never quote the block), the reason is added.
func refuse(cause error, message string) error {
	code := contracts.ErrSchemaInvalid
	if c := contracts.CodeOf(cause); c != "" && c != contracts.ErrInternal {
		code = c
	}
	e := &Error{Reason: ReasonEventRefused, err: contracts.Wrap(code, cause, message).WithDetails("reason", string(ReasonEventRefused))}
	return e
}
