package audit

import (
	"context"
	"errors"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// RecordSink adapts a log to the shape of a sink for records of type T: a
// single Emit(ctx, T) error. A package that produces self-describing records
// (the policy gateway's policy.Record) cannot import this package, and this
// package cannot import theirs; the composition root joins them:
//
//	sink := audit.NewRecordSink[policy.Record](log, audit.EventPolicyDecision,
//		func(r policy.Record) (audit.Locator, error) {
//			s := r.Subject
//			return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
//		})
//	gateway := policy.NewGateway(clk, policy.WithSink(sink))
//
// Emit returns an error unless the event is committed, which is what the
// gateway needs to turn a decision it could not record into a denial. A record
// whose Locator cannot be built because the subject is incomplete (the request
// carried no usable scope) is not dropped: it goes to the platform chain, so
// that a denied attempt without scope still leaves a record (SEC-087). Any
// other failure is returned.
type RecordSink[T any] struct {
	a      Appender
	name   EventType
	locate func(T) (Locator, error)
}

// NewRecordSink returns a sink that appends each record as an event of type
// name. locate says where a record belongs.
func NewRecordSink[T any](a Appender, name EventType, locate func(T) (Locator, error)) *RecordSink[T] {
	return &RecordSink[T]{a: a, name: name, locate: locate}
}

// Emit appends rec. It returns nil only after the event is committed.
func (s *RecordSink[T]) Emit(ctx context.Context, rec T) error {
	if s == nil || s.a == nil || s.locate == nil {
		return contracts.New(contracts.ErrInternal, "audit: the record sink is not wired")
	}
	loc, err := s.locate(rec)
	if err != nil {
		code := contracts.CodeOf(err)
		if !errors.As(err, new(*contracts.Error)) || (code != contracts.ErrScopeMissing && code != contracts.ErrScopeViolation) {
			return err
		}
		loc = Platform()
	}
	_, err = s.a.AppendAt(ctx, loc, NewEvent{Type: s.name, Data: rec})
	return err
}
