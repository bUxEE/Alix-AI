// Package oskeyring is the credentials.OSStore over the operating system's own secret store, through
// github.com/zalando/go-keyring: Windows Credential Manager, macOS Keychain and the Linux Secret Service
// (D-Bus). A composition root imports it; internal/credentials itself does not, so a build that wants
// no OS store (a headless server, the noexternal build) links none of it.
//
// Research (X-07, 2026-10-07): go-keyring v0.2.8 is MIT licensed; it pulls in github.com/danieljoos/wincred
// (MIT, Windows) and github.com/godbus/dbus/v5 (BSD-2-Clause, Linux), all on the ADR-0008 allowlist. It is
// pure Go: `CGO_ENABLED=0 go build` succeeds for windows, darwin and linux (checked for amd64 and arm64).
// On macOS it drives the system `security` tool, with the value passed on standard input rather than on the
// command line. A host with no usable store (no D-Bus session, a locked keychain) gets an error from every
// call, and the composition root then uses the encrypted file with a key file.
package oskeyring

import (
	"errors"

	"github.com/zalando/go-keyring"

	"github.com/buxee/alix-ai/internal/credentials"
)

// Store is the operating system's secret store.
type Store struct{}

var _ credentials.OSStore = Store{}

// New returns the store.
func New() Store { return Store{} }

// Get implements credentials.OSStore.
func (Store) Get(service, user string) (string, error) {
	v, err := keyring.Get(service, user)
	return v, mapErr(err)
}

// Set implements credentials.OSStore.
func (Store) Set(service, user, password string) error {
	return mapErr(keyring.Set(service, user, password))
}

// Delete implements credentials.OSStore.
func (Store) Delete(service, user string) error {
	return mapErr(keyring.Delete(service, user))
}

// mapErr maps go-keyring's "not found" to the interface's and drops every other error's text: it comes
// from the operating system and can quote what was being stored.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, keyring.ErrNotFound):
		return credentials.ErrOSStoreNotFound
	default:
		return errUnavailable
	}
}

var errUnavailable = errors.New("oskeyring: the OS secret store failed")
