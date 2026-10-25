package main

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/examples/reference-apps/ledger"
	"github.com/buxee/alix-ai/pkg/bridge"
	"github.com/buxee/alix-ai/tools/devkeys"
)

// ledgerPackage is the ledger reference application's signed package: a Go application behind a native
// bridge on loopback.
const ledgerPackage = "../../examples/reference-apps/ledger/package"

// The ledger (Go, native bridge) in-process, in test support mode, on a loopback port it picks, with a
// throwaway application key where the package's adapter configuration reads it. It returns the address
// a run points the package's native endpoint at.
func startLedger(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	key := []byte(hex.EncodeToString(b))
	t.Setenv("LEDGER_BRIDGE_KEY", string(key))
	app := ledger.New(ledger.Config{TestSupport: true})
	srv, err := app.NewBridge(ledger.BridgeConfig{Keys: map[string][]byte{"ledger-bridge-1": key}})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := bridge.ListenLoopback(0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); <-done })
	return ln.Addr().String()
}

// The conformance run over a package served by a native bridge: the endpoint is pointed at the running
// application by its host:port, the audit reaches it through the native transport and the conversation
// stage replays every example of the package over the same adapter, with the tenants of the package's
// own test suite made by the transport's provisioning bridge. The stage used to answer not_run for
// anything but MCP.
//
// The run as a whole is not asserted to pass: the stage rightly finds an example of the ledger package
// that omits a required field (the package is signed content outside this task); what is asserted is
// that the stage ran.
func TestConformanceDrivesAPackageOverItsNativeBridge(t *testing.T) {
	addr := startLedger(t)
	res := runConformance(t,
		"-endpoint", "ledger-bridge="+addr,
		"-trust-key", devkeys.KeyID+"="+devkeys.PublicKeyBase64(), "-allow-test-keys",
		ledgerPackage)

	if p := res.report.Package; p.ApplicationID != "ledger" {
		t.Fatalf("package = %+v\n%s", p, describe(res))
	}
	audit := res.stage(t, "audit")
	if it, ok := audit.item("audit.outcome"); !ok || it.Details["transport"] != "in_use" {
		t.Errorf("audit.outcome = %+v: the audit must have inspected the native adapter", it)
	}

	conv := res.stage(t, "conversation")
	if it, ok := conv.item("conversation.transport"); ok {
		t.Fatalf("the conversation stage does not drive a native package: %+v", it)
	}
	if conv.count(StatusNotRun) != 0 {
		t.Errorf("a conversation item was not run:\n%s", describe(res))
	}
	if s := res.report.Summary; s.ExamplesDeclared == 0 || s.Examples != s.ExamplesDeclared {
		t.Errorf("%d example(s) replayed of %d declared", s.Examples, s.ExamplesDeclared)
	}
	for _, id := range []string{"conversation.demonstrated", "conversation.cleanup"} {
		if it, ok := conv.item(id); !ok || it.Status != StatusPass {
			t.Errorf("%s = %+v", id, it)
		}
	}
}

// retarget reaches the endpoints of the other two transports too: the base_url of an OpenAPI one, with
// the egress entry that allowed it, and the host:port of a native one over loopback_tcp (which is local
// IPC and has no egress entry), and refuses a native address that is not host:port or an endpoint with
// nothing to retarget. (The copy is not signed again here, so it is read as text.)
func TestRetargetPointsOpenAPIAndNativeEndpointsToo(t *testing.T) {
	copied := func(t *testing.T, from string) string {
		t.Helper()
		dir := t.TempDir()
		if err := copyPackage(from, dir); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(dir, "alixai.yaml")
	}
	read := func(t *testing.T, path string) string {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	t.Run("openapi", func(t *testing.T) {
		path := copied(t, "../../examples/reference-apps/facility-maintenance/package")
		if err := retarget(path, map[string]string{"facility-api": "http://127.0.0.1:45691"}); err != nil {
			t.Fatal(err)
		}
		if text := read(t, path); !strings.Contains(text, "base_url: http://127.0.0.1:45691") || !strings.Contains(text, "45691") || strings.Contains(text, "8788") {
			t.Fatalf("the base_url and the egress entry must move together: %s", text)
		}
	})

	t.Run("native over loopback_tcp", func(t *testing.T) {
		path := copied(t, ledgerPackage)
		before := read(t, path)
		if err := retarget(path, map[string]string{"ledger-bridge": "127.0.0.1:45692"}); err != nil {
			t.Fatal(err)
		}
		text := read(t, path)
		if !strings.Contains(text, "127.0.0.1:45692") || !strings.Contains(text, "loopback_tcp") || strings.Contains(text, "7421") {
			t.Fatalf("only the bridge's address may change: %s", text)
		}
		if strings.Count(text, "7422") != strings.Count(before, "7422") {
			t.Error("the egress entry of the ledger's document API moved with a native address that is not an egress destination")
		}
		if err := retarget(path, map[string]string{"ledger-bridge": "not an address"}); err == nil {
			t.Error("a native target that is not host:port must be refused")
		}
		if err := retarget(path, map[string]string{"ledger-events": "http://127.0.0.1:1/"}); err == nil {
			t.Error("an endpoint with no url to retarget (events) must be refused")
		}
	})
}
