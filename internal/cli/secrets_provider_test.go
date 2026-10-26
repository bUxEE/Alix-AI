//go:build !noexternal

package cli

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	openapitransport "github.com/buxee/alix-ai/adapters/openapi"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/store/storetest"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

const (
	appCredTestRefOpenAPI = "cred:outbound-openapi"
	appCredTestRefMCP     = "cred:outbound-mcp"
	appCredTestRefRAG     = "cred:outbound-rag"
	appCredTestApp        = "outbound-auth-demo"
	appCredTestTenant     = "ten_demo"
	appCredTestDeploy     = "dep_outbound"
)

func appCredTestProvider(t *testing.T, envName, secret string, refs ...string) *appCredProvider {
	t.Helper()
	t.Setenv(envName, secret)
	var logBuf syncBuf
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	t.Cleanup(func() {
		logs := logBuf.String()
		if strings.Contains(logs, secret) {
			t.Errorf("logs contain the secret %q:\n%s", secret, logs)
		}
	})
	clk := clock.NewFake(time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))
	db := storetest.Open(t, nil)
	aud, err := audit.New(db, audit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := openAppCredService(appCredServiceDeps{
		DB: db, Audit: aud, Clock: clk, Log: log, DeploymentID: appCredTestDeploy, Dir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	p := &appCredProvider{svc: svc, by: map[string]appCredBound{}, log: log}
	if len(refs) == 0 {
		refs = []string{appCredTestRefOpenAPI, appCredTestRefMCP, appCredTestRefRAG}
	}
	for _, ref := range refs {
		if err := p.registerBinding(context.Background(), appCredTestDeploy, appCredBinding{
			Ref: ref, Application: appCredTestApp, Tenant: appCredTestTenant, Env: envName,
		}); err != nil {
			t.Fatal(err)
		}
	}
	RegisterOpenAPISecretProvider(p)
	t.Cleanup(func() { RegisterOpenAPISecretProvider(nil) })
	return p
}

// TestApplicationCredentialsResolveRotateAndAudit: the composition-root provider
// resolves package credential_ref values for OpenAPI, MCP and RAG, picks up env
// rotation, refuses a missing secret with a credentials reason, and never puts
// the secret in logs.
func TestApplicationCredentialsResolveRotateAndAudit(t *testing.T) {
	secret := "test-outbound-secret-" + testKey(t)[len("TESTONLY-"):]
	envName := "ALIXAI_SECRET_OUTBOUND_AUTH_TEST"
	p := appCredTestProvider(t, envName, secret)

	for _, ref := range []string{appCredTestRefOpenAPI, appCredTestRefMCP, appCredTestRefRAG} {
		sec, err := p.Secret(context.Background(), ref)
		if err != nil || sec.Reveal() != secret {
			t.Fatalf("%s: %v %q", ref, err, sec)
		}
	}

	rag := ragServeSecrets{provider: openapiTransportSecretProvider}
	got, err := rag.Secret(context.Background(), appCredTestRefRAG)
	if err != nil || got.Reveal() != secret {
		t.Fatalf("ragServeSecrets: %v %q", err, got)
	}

	rotated := secret + "-rotated"
	t.Setenv(envName, rotated)
	sec, err := p.Secret(context.Background(), appCredTestRefMCP)
	if err != nil || sec.Reveal() != rotated {
		t.Fatalf("after rotation: %v %q", err, sec)
	}
	if strings.Contains(sec.String(), rotated) {
		t.Error("Secret.String leaked the value")
	}

	t.Setenv(envName, "")
	_ = os.Unsetenv(envName)
	if _, err := p.Secret(context.Background(), appCredTestRefMCP); err == nil {
		t.Fatal("missing secret: want an error")
	} else if credentials.ReasonOf(err) == "" {
		t.Fatalf("missing secret: want a credentials reason, got %v", err)
	}

	if _, err := p.Secret(context.Background(), "cred:unknown-ref"); err == nil {
		t.Fatal("unknown ref: want ErrNoSecret")
	}
}

// TestApplicationCredentialsServeProviderRegisters: start serve with
// application_credentials; the OpenAPI/RAG provider is registered and resolves
// without logging the secret.
func TestApplicationCredentialsServeProviderRegisters(t *testing.T) {
	f := newFixture(t)
	secret := "serve-provider-secret-" + testKey(t)[len("TESTONLY-"):]
	envName := "ALIXAI_SECRET_OUTBOUND_SERVE"
	t.Setenv(envName, secret)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))

	extra := "identity:\n  deployment_id: dep_cli\n  audience: alixai\n" +
		"application_credentials:\n  credentials:\n" +
		"    - ref: " + appCredTestRefOpenAPI + "\n" +
		"      application: " + appCredTestApp + "\n" +
		"      tenant: " + appCredTestTenant + "\n" +
		"      env: " + envName + "\n"
	cfg, _ := serveConfig(t, f, extra)
	srv := startServe(t, cfg)
	defer func() { _ = srv.stop() }()

	p := openapiTransportSecretProvider()
	if p == nil {
		t.Fatal("serve did not register a secret provider")
	}
	sec, err := p.Secret(context.Background(), appCredTestRefOpenAPI)
	if err != nil || sec.Reveal() != secret {
		t.Fatalf("resolve through serve: %v %q", err, sec)
	}
	if strings.Contains(srv.stderr.String(), secret) {
		t.Errorf("serve logs contain the secret:\n%s", srv.stderr.String())
	}
}

// TestMCPTransportWiresCredentialRef: an MCP package endpoint with credential_ref
// sends Authorization: Bearer <secret> on tool list/call through the transport.
func TestMCPTransportWiresCredentialRef(t *testing.T) {
	secret := "mcp-wire-" + testKey(t)[len("TESTONLY-"):]
	ref := "cred:clinic-booking-mcp"
	openapiTransportProvider(t, openapitransport.StaticSecrets{ref: secret})

	pkg, dir := mcpTransportLoadClinic(t)
	v := pkg.Manifest.Endpoints["clinic-mcp"].Value.(ap.ManifestMCPEndpoint)
	cred := ap.CredentialRef(ref)
	v.CredentialRef = &cred
	pkg.Manifest.Endpoints["clinic-mcp"] = ap.ManifestEndpoint{Value: v}

	h := mcpTransportClinicServer(t, pkg, dir)
	mcpTransportRepoint(t, pkg, "clinic-mcp", h.URL)

	ad, err := mcpTransportBuild(pkg, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { transportsClose(ad) })

	if _, err := ad.Describe(context.Background()); err != nil {
		t.Logf("describe: %v", err)
	}
	found := false
	for _, e := range h.Log() {
		if e.Authorization == "Bearer "+secret {
			found = true
		}
	}
	if !found {
		t.Fatalf("MCP requests missing Authorization Bearer; log=%#v", h.Log())
	}
}

func TestMCPTransportCredentialRefFailsClosedWhenSecretMissing(t *testing.T) {
	ref := "cred:clinic-booking-mcp"
	openapiTransportProvider(t, openapitransport.StaticSecrets{}) // provider present, ref unknown

	pkg, dir := mcpTransportLoadClinic(t)
	v := pkg.Manifest.Endpoints["clinic-mcp"].Value.(ap.ManifestMCPEndpoint)
	cred := ap.CredentialRef(ref)
	v.CredentialRef = &cred
	pkg.Manifest.Endpoints["clinic-mcp"] = ap.ManifestEndpoint{Value: v}

	h := mcpTransportClinicServer(t, pkg, dir)
	mcpTransportRepoint(t, pkg, "clinic-mcp", h.URL)
	ad, err := mcpTransportBuild(pkg, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { transportsClose(ad) })
	if _, err := ad.Describe(context.Background()); err == nil {
		t.Fatal("missing secret: want Describe to fail closed")
	}
}

func TestMCPTransportCredentialRefFailsClosedWhenNoProvider(t *testing.T) {
	RegisterOpenAPISecretProvider(nil)
	t.Cleanup(func() { RegisterOpenAPISecretProvider(nil) })

	pkg, dir := mcpTransportLoadClinic(t)
	v := pkg.Manifest.Endpoints["clinic-mcp"].Value.(ap.ManifestMCPEndpoint)
	cred := ap.CredentialRef("cred:clinic-booking-mcp")
	v.CredentialRef = &cred
	pkg.Manifest.Endpoints["clinic-mcp"] = ap.ManifestEndpoint{Value: v}

	if _, err := mcpTransportBuild(pkg, dir); err == nil {
		t.Fatal("no credential provider: want adapter build to fail closed")
	}
}

// TestOpenAPITransportSendsCredentialThroughProvider: OpenAPI through the
// transport composition sends Authorization: Bearer <secret> on every call.
func TestOpenAPITransportSendsCredentialThroughProvider(t *testing.T) {
	secret := "openapi-wire-" + testKey(t)[len("TESTONLY-"):]
	openapiTransportProvider(t, openapitransport.StaticSecrets{openapiTransportCredID: secret})

	api := openapiTransportStartAPI(t)
	pkg, dir := openapiTransportInstall(t, api.URL+"/api/v1", "", "")
	ad, err := openapiTransportBuild(pkg, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { transportsClose(ad) })

	scope := openapiTransportScope(pkg)
	if _, err := ad.Read(context.Background(), scope, adapters.ReadRequest{
		CapabilityID: "patients.find", Fields: map[string]any{"query": "a"},
	}); err != nil {
		t.Logf("read: %v", err)
	}
	found := false
	for _, r := range api.requests() {
		if r.Header.Get("Authorization") == "Bearer "+secret {
			found = true
		}
	}
	if !found {
		t.Fatalf("OpenAPI requests missing Authorization Bearer; reqs=%d", len(api.requests()))
	}
}
