package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/tools/devkeys"
)

// A publisher key revoked from the CLI must stop a RUNNING server from serving what it signed at
// once, not when the server's short package cache expires (found by M1-24: the registry keeps a
// same-hash entry and the package source caches for 30 s).
func TestTrustRevokeKeyTellsARunningServer(t *testing.T) {
	auditChecksProbesBuiltinChecksOnly(t) // this test is about what follows an audit, not the runtime probes (M1-18)
	transportsSwapDefault(t)
	e, key := transportsOpsEnv(t, transportsAcceptConfig)
	ctx := context.Background()

	transportsActivate(t, e, key)
	w, _ := transportsWire(t, e)
	admin := w.admin.(serveAdminControl)
	if err := admin.registry.Refresh(ctx, "demo-library"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.apps.Package(ctx, "demo-library"); err != nil {
		t.Fatalf("control: the server cannot serve the active package before the revoke: %v", err)
	}
	transportsServing(t, e, w)

	res := e.mustRun("trust", "revoke-key", "-app", "demo-library", "-key-id", devkeys.KeyID,
		"-reason", "key compromised", "-operator", "root")
	if !strings.Contains(res.stderr, "reloaded the capability registry") {
		t.Fatalf("the running server was not told about the revocation:\n%s", res.stderr)
	}
	if _, err := admin.apps.Package(ctx, "demo-library"); err == nil {
		t.Fatal("the running server still serves a package whose publisher key was revoked")
	}
}
