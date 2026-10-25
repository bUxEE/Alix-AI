package credentials

import "fmt"

// None of the service types prints its fields: a Service holds a validator and backends, a Backend may hold
// values, and fmt cannot call a Secret's own Format on a value it reaches through an unexported field. Every
// verb prints the type's name only.

func opaque(f fmt.State, name string) { _, _ = f.Write([]byte(name)) }

// Format implements fmt.Formatter.
func (*Service) Format(f fmt.State, _ rune) { opaque(f, "credentials.Service") }

// Format implements fmt.Formatter.
func (*Resolver) Format(f fmt.State, _ rune) { opaque(f, "credentials.Resolver") }

// Format implements fmt.Formatter.
func (SecretProvider[S]) Format(f fmt.State, _ rune) { opaque(f, "credentials.SecretProvider") }

// Format implements fmt.Formatter.
func (*OSBackend) Format(f fmt.State, _ rune) { opaque(f, "credentials.OSBackend") }

// Format implements fmt.Formatter.
func (*EncryptedFile) Format(f fmt.State, _ rune) { opaque(f, "credentials.EncryptedFile") }

// Format implements fmt.Formatter.
func (*EnvBackend) Format(f fmt.State, _ rune) { opaque(f, "credentials.EnvBackend") }

// Format implements fmt.Formatter.
func (*FileBackend) Format(f fmt.State, _ rune) { opaque(f, "credentials.FileBackend") }

// Format implements fmt.Formatter.
func (OSStoreKeySource) Format(f fmt.State, _ rune) { opaque(f, "credentials.OSStoreKeySource") }

// Format implements fmt.Formatter.
func (KeyFileSource) Format(f fmt.State, _ rune) { opaque(f, "credentials.KeyFileSource") }
