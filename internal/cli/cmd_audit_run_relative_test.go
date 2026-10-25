package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/apppkg"
)

// relDataRoot makes a data directory below the working directory of the test and
// returns it as a relative path, the way `data_dir: ./data` reads.
func relDataRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp(".", "relData-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return "./" + filepath.ToSlash(root)
}

// The audit builds the adapter of an installed package from
// <data_dir>/<record path>, and reads the package's adapter configuration from
// there. With a relative data_dir the directory was refused as "not known"; it
// is the working directory's, as the database's is.
func TestAuditRunFindsThePackageDirectoryWithARelativeDataDir(t *testing.T) {
	t.Setenv(nativeTransportKeyEnv, nativeTransportSecret)
	pkg, _ := nativeTransportInstall(t, "loopback_tcp", "127.0.0.1:7001", "")
	data := relDataRoot(t)
	rel := filepath.Join(data, "packages", nativeTransportApp, "h1")
	cfgPath := filepath.Join(rel, filepath.FromSlash(nativeTransportCfgRel))
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(nativeTransportCfgDoc), 0o600); err != nil {
		t.Fatal(err)
	}

	src := transportsSource{dataDir: data}
	rec := apppkg.Record{ApplicationID: nativeTransportApp, ManifestHash: "h1", State: apppkg.StateQuarantined, Path: "packages/" + nativeTransportApp + "/h1"}
	ad, _, err := src.build(context.Background(), nativeTransportApp, pkg, rec)
	if err != nil {
		t.Fatalf("a relative data_dir must find the package directory: %v", err)
	}
	transportsClose(ad)

	// the three transports share the one resolution
	if b, err := nativeTransportReadFile(rel, "adapter_config", nativeTransportCfgRel, 1<<20); err != nil || string(b) != nativeTransportCfgDoc {
		t.Errorf("native: %q, %v", b, err)
	}
	if b, err := openapiTransportReadFile(rel, "adapter_config", nativeTransportCfgRel, 1<<20); err != nil || string(b) != nativeTransportCfgDoc {
		t.Errorf("openapi: %q, %v", b, err)
	}
	if b, err := mcpTransportReadConfig(rel, nativeTransportCfgRel); err != nil || string(b) != nativeTransportCfgDoc {
		t.Errorf("mcp: %q, %v", b, err)
	}

	// an empty directory is still unknown, and a path out of the package is still refused
	if _, err := nativeTransportReadFile("", "adapter_config", nativeTransportCfgRel, 1<<20); err == nil || !strings.Contains(err.Error(), "not known") {
		t.Errorf("an empty package directory: %v", err)
	}
	if _, err := mcpTransportReadConfig(rel, "../outside.yaml"); err == nil {
		t.Error("a path out of the package is read")
	}
}
