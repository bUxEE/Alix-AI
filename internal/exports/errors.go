package exports

import (
	"errors"

	"github.com/buxee/alix-ai/pkg/contracts"
)

func notFound(what, msg string) error {
	return contracts.New(contracts.ErrValidationFailed, "exports: "+msg).
		WithDetails("reason", "not_found").WithDetails("what", what)
}

func invalid(reason, msg string) error {
	return contracts.New(contracts.ErrValidationFailed, "exports: "+msg).WithDetails("reason", reason)
}

func exhausted(reason, msg string) error {
	return contracts.New(contracts.ErrResourceExhausted, "exports: "+msg).WithDetails("reason", reason)
}

// IsNotFound reports whether err is the turn/block not-found refusal.
func IsNotFound(err error) bool {
	var ce *contracts.Error
	return errors.As(err, &ce) && ce.Code == contracts.ErrValidationFailed && ce.Details["reason"] == "not_found"
}
