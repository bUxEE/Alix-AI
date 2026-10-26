//go:build noexternal

package cli

import (
	"context"
)

// The noexternal build has no credential service. application_credentials in
// config is refused; with no bindings the provider stays unset so adapters
// without credential_ref are unchanged and a declared ref fails closed at build.
// Use the default build for outbound host auth with application_credentials.

func init() {
	registerServeProvider(appCredServeName, appCredServeProvideNoExt)
}

func appCredServeProvideNoExt(_ context.Context, _ serveDeps) error {
	if len(appCredSection.Credentials) > 0 {
		return errAppCredNeedsCredentialsBuild
	}
	return nil
}

func stackWireApplicationCredentials(_ context.Context, _ *stack) error {
	if len(appCredSection.Credentials) > 0 {
		return errAppCredNeedsCredentialsBuild
	}
	return nil
}

var errAppCredNeedsCredentialsBuild = errString("application_credentials: this binary was built with -tags noexternal; rebuild without that tag to provision outbound host credentials")

type errString string

func (e errString) Error() string { return string(e) }
