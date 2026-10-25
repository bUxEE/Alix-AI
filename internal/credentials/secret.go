package credentials

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
)

// Secret is a provider key or token held in memory. It is a string kind so the
// adapters that need the value can convert it, but every way a program prints,
// logs or serialises a value shows a placeholder: %s %v %+v %#v %x, slog,
// encoding/json and encoding.TextMarshaler. The one way to the value is Reveal,
// which only the code that puts it on the wire calls.
//
// Every exported field of this package that can carry a key or a token has this
// type, never a plain string.
type Secret string

const secretPlaceholder = "[redacted]"

// Reveal returns the value.
func (s Secret) Reveal() string { return string(s) }

// String implements fmt.Stringer (and so %s, %q and %v).
func (Secret) String() string { return secretPlaceholder }

// GoString implements fmt.GoStringer (%#v).
func (Secret) GoString() string { return "credentials.Secret(" + secretPlaceholder + ")" }

// Format implements fmt.Formatter, so no verb prints the value.
func (Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(secretPlaceholder)) }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(secretPlaceholder) }

// MarshalJSON implements json.Marshaler.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + secretPlaceholder + `"`), nil }

// MarshalText implements encoding.TextMarshaler.
func (Secret) MarshalText() ([]byte, error) { return []byte(secretPlaceholder), nil }

// UnmarshalJSON refuses: a Secret is never read from a document, so a request
// body that carries one cannot fill it by accident.
func (*Secret) UnmarshalJSON([]byte) error {
	return fmt.Errorf("credentials: a Secret is not read from JSON")
}

// Empty reports whether there is no value.
func (s Secret) Empty() bool { return s == "" }

// fingerprintHexLen is the number of hex digits of the masked fingerprint
// (schemas/providers/credential-ref: the first 8 hex digits of the SHA-256).
const fingerprintHexLen = 8

// Fingerprint returns the masked fingerprint of a manual key: "sha256:" and the
// first 8 hex digits of its SHA-256. It is not a prefix, suffix or any other
// part of the key, and cannot be reversed into it.
func Fingerprint(s Secret) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])[:fingerprintHexLen]
}
