package openapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// Secret is a credential value. Every way of printing it, logging it or
// encoding it prints a fixed placeholder, so a Secret that ends up in an error,
// a log record or a JSON document by mistake does not leak. The one way to the
// value is Reveal, which only the code that puts it in a header calls.
type Secret string

const secretPlaceholder = "[redacted]"

// Reveal returns the credential value.
func (s Secret) Reveal() string { return string(s) }

// String implements fmt.Stringer. It also covers %s, %q and %v.
func (Secret) String() string { return secretPlaceholder }

// GoString implements fmt.GoStringer (%#v).
func (Secret) GoString() string { return "openapi.Secret(" + secretPlaceholder + ")" }

// Format implements fmt.Formatter, so no verb (%x, %d, %+v ...) prints the value.
func (Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(secretPlaceholder)) }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(secretPlaceholder) }

// MarshalJSON implements json.Marshaler.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + secretPlaceholder + `"`), nil }

// MarshalText implements encoding.TextMarshaler.
func (Secret) MarshalText() ([]byte, error) { return []byte(secretPlaceholder), nil }

// SecretProvider resolves the opaque credential reference a package declares
// (credential_ref of an endpoint) to the credential. The adapter never sees
// anything but the reference, and the reference never leaves the process: it is
// not sent to the application, not logged and not put in an error.
//
// Implementations (the environment, a file, the OS secret store) come with the
// credential service; a composition root supplies one. The adapter asks for the
// credential once per request, so a rotated credential is used by the next
// request and nothing here caches it. Implementations must be safe for
// concurrent use, must honour ctx, and must not put the credential in the
// errors they return (the adapter does not pass those on either).
type SecretProvider interface {
	Secret(ctx context.Context, ref string) (Secret, error)
}

// ErrNoSecret is what a provider returns for a reference it does not know.
var ErrNoSecret = errors.New("openapi: no such credential reference")

// StaticSecrets is a SecretProvider over a fixed map of reference to credential.
// It is for tests and for hosts that wire the adapter in code.
type StaticSecrets map[string]string

// Secret implements SecretProvider.
func (s StaticSecrets) Secret(_ context.Context, ref string) (Secret, error) {
	v, ok := s[ref]
	if !ok {
		return "", ErrNoSecret
	}
	return Secret(v), nil
}
