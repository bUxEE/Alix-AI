//go:build !noexternal

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpfixture "github.com/buxee/alix-ai/adapters/mcp/testdata/fixture"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/rag/ingest"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// TestServeSendsCredentialRefToHostOnEveryTransport is the serve-level proof the
// integrator asked for: real `alixai serve` registers application_credentials;
// MCP (tools/list + tools/call), OpenAPI and RAG application_document_api each
// send Authorization: Bearer <their own secret> to recording hosts; rotation is
// picked up on the next call; a missing secret fails that transport alone; and
// neither serve logs nor the audit export contain any of the secrets.
//
// One package cannot declare both MCP and OpenAPI (mixed kinds are refused), so
// the three transports use the clinic MCP package, the clinic OpenAPI rewrite and
// a RAG api source — each with its own credential_ref — under one serve.
func TestServeSendsCredentialRefToHostOnEveryTransport(t *testing.T) {
	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))

	suffix := testKey(t)[len("TESTONLY-"):]
	secretMCP := "serve-e2e-mcp-" + suffix
	secretOpenAPI := "serve-e2e-openapi-" + suffix
	secretRAG := "serve-e2e-rag-" + suffix
	envMCP := "ALIXAI_SECRET_SERVE_E2E_MCP"
	envOpenAPI := "ALIXAI_SECRET_SERVE_E2E_OPENAPI"
	envRAG := "ALIXAI_SECRET_SERVE_E2E_RAG"
	refMCP := "cred:outbound-mcp"
	refOpenAPI := "cred:outbound-openapi"
	refRAG := "cred:outbound-rag"
	appID := "outbound-auth-e2e"
	tenant := "ten_e2e"
	t.Setenv(envMCP, secretMCP)
	t.Setenv(envOpenAPI, secretOpenAPI)
	t.Setenv(envRAG, secretRAG)

	mcpPkg, mcpDir := mcpTransportLoadClinic(t)
	mcpHost := mcpTransportClinicServer(t, mcpPkg, mcpDir)
	mcpTransportRepoint(t, mcpPkg, "clinic-mcp", mcpHost.URL)
	credMCP := ap.CredentialRef(refMCP)
	epMCP := mcpPkg.Manifest.Endpoints["clinic-mcp"].Value.(ap.ManifestMCPEndpoint)
	epMCP.CredentialRef = &credMCP
	mcpPkg.Manifest.Endpoints["clinic-mcp"] = ap.ManifestEndpoint{Value: epMCP}

	apiHost := openapiTransportStartAPI(t)
	openapiPkg, openapiDir := openapiTransportInstall(t, apiHost.URL+"/api/v1", "", "")
	credOA := ap.CredentialRef(refOpenAPI)
	epOA := openapiPkg.Manifest.Endpoints[openapiTransportEP].Value.(ap.ManifestOpenAPIEndpoint)
	epOA.CredentialRef = &credOA
	openapiPkg.Manifest.Endpoints[openapiTransportEP] = ap.ManifestEndpoint{Value: epOA}

	extra := identityYAML +
		"application_credentials:\n  credentials:\n" +
		"    - ref: " + refMCP + "\n      application: " + appID + "\n      tenant: " + tenant + "\n      env: " + envMCP + "\n" +
		"    - ref: " + refOpenAPI + "\n      application: " + appID + "\n      tenant: " + tenant + "\n      env: " + envOpenAPI + "\n" +
		"    - ref: " + refRAG + "\n      application: " + appID + "\n      tenant: " + tenant + "\n      env: " + envRAG + "\n"
	cfg, dataDir := serveConfig(t, f, extra)
	srv := startServe(t, cfg)
	defer func() { _ = srv.stop() }()

	if openapiTransportSecretProvider() == nil {
		t.Fatal("serve did not register the application_credentials provider")
	}

	// RAG sync uses serve's registered provider (ragServeSecrets → openapiTransportSecretProvider).
	rig, ragAPI, _ := ragServeAPIRig(t, refRAG)
	ragAPI.token = secretRAG
	ragAPI.put("policies/refund.md", "# Refunds\n\nThe harbour refund window lists the receipt rules.")

	mcpAd, err := mcpTransportBuild(mcpPkg, mcpDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { transportsClose(mcpAd) })
	openapiAd, err := openapiTransportBuild(openapiPkg, openapiDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { transportsClose(openapiAd) })

	ctx := context.Background()
	mcpScope := contracts.Scope{
		DeploymentID: "dep_t", ApplicationID: mcpTransportClinicApp, TenantID: "tnt_demo", WorkspaceID: "site_north",
		PrincipalID: "usr_reception_site_north", Roles: []string{"receptionist"},
		CapabilityScopes: []string{"patients.find"}, SessionID: "ses_e2e_mcp",
	}
	openapiScope := openapiTransportScope(openapiPkg)

	if _, err := mcpAd.Describe(ctx); err != nil {
		t.Fatalf("MCP tools/list: %v", err)
	}
	if _, err := mcpAd.Read(ctx, mcpScope, adapters.ReadRequest{
		CapabilityID: "patients.find", Fields: map[string]any{"query": "Rossi"},
	}); err != nil {
		t.Logf("MCP tools/call: %v", err)
	}
	if _, err := openapiAd.Read(ctx, openapiScope, adapters.ReadRequest{
		CapabilityID: "patients.find", Fields: map[string]any{"query": "a"},
	}); err != nil {
		t.Logf("OpenAPI read: %v", err)
	}
	op := ragSourcesOperator("t1", "w1")
	rec := rig.register(t, op, "api-docs")
	rig.drain()
	sourceID := string(rec.Status.SourceID)
	if info, err := rig.stack.svc.Status(ctx, rig.scope, sourceID); err != nil || !info.Available {
		t.Fatalf("RAG sync/list+fetch status = %+v, %v", info, err)
	}

	mcpAuths := serveE2EMCPAuths(mcpHost)
	openapiAuths := serveE2EOpenAPIAuths(apiHost)
	ragAuths := serveE2ERAGAuths(ragAPI)
	serveE2EAssertBearer(t, "MCP", secretMCP, mcpAuths)
	serveE2EAssertBearer(t, "OpenAPI", secretOpenAPI, openapiAuths)
	serveE2EAssertBearer(t, "RAG", secretRAG, ragAuths)
	serveE2ENoCrossWire(t, mcpAuths, openapiAuths, ragAuths, secretMCP, secretOpenAPI, secretRAG)

	rotated := secretMCP + "-rotated"
	t.Setenv(envMCP, rotated)
	beforeRotate := len(mcpHost.Log())
	if _, err := mcpAd.Describe(ctx); err != nil {
		t.Fatalf("MCP after rotation: %v", err)
	}
	foundRotated := false
	for _, e := range mcpHost.Log()[beforeRotate:] {
		switch e.Authorization {
		case "Bearer " + rotated:
			foundRotated = true
		case "Bearer " + secretMCP:
			t.Fatal("MCP still sent the old secret after rotation")
		}
	}
	if !foundRotated {
		t.Fatalf("MCP after rotation missing new Bearer; log=%#v", mcpHost.Log()[beforeRotate:])
	}

	t.Setenv(envOpenAPI, "")
	_ = os.Unsetenv(envOpenAPI)
	openapiBefore := len(apiHost.requests())
	_, openapiErr := openapiAd.Read(ctx, openapiScope, adapters.ReadRequest{
		CapabilityID: "patients.find", Fields: map[string]any{"query": "b"},
	})
	if openapiErr == nil {
		t.Fatal("missing OpenAPI secret: want the call to fail closed")
	}
	if reason := credentials.ReasonOf(openapiErr); reason == "" &&
		!strings.Contains(strings.ToLower(openapiErr.Error()), "credential") {
		t.Fatalf("missing OpenAPI secret: want a credentials/audited reason, got %v", openapiErr)
	}
	for _, r := range apiHost.requests()[openapiBefore:] {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("missing secret still sent Authorization: %q", got)
		}
	}

	mcpBeforeMiss := len(mcpHost.Log())
	ragBeforeMiss := len(ragAPI.requests())
	if _, err := mcpAd.Describe(ctx); err != nil {
		t.Fatalf("MCP must stay up when another secret is missing: %v", err)
	}
	ragAPI.put("policies/shipping.md", "# Shipping\n\nThe harbour dock schedule lists the tide tables.")
	if _, err := rig.stack.svc.RequestSync(ctx, rig.scope, sourceID, ingest.SyncOptions{
		Key: "after-openapi-missing", ServiceIdentity: ragServiceIdentity,
	}); err != nil {
		t.Fatalf("RAG resync: %v", err)
	}
	rig.drain()
	for _, e := range mcpHost.Log()[mcpBeforeMiss:] {
		if e.Authorization != "Bearer "+rotated {
			t.Fatalf("MCP after OpenAPI secret missing: %q", e.Authorization)
		}
	}
	if len(ragAPI.requests()) <= ragBeforeMiss {
		t.Fatal("RAG made no further requests after OpenAPI secret was cleared")
	}
	for _, r := range ragAPI.requests()[ragBeforeMiss:] {
		if r.authorization != "Bearer "+secretRAG {
			t.Fatalf("RAG Authorization after OpenAPI missing: %q", r.authorization)
		}
	}

	logs := srv.stdout.String() + srv.stderr.String()
	auditBlob := serveE2EAuditExport(t, dataDir, appID, tenant)
	for _, secret := range []string{secretMCP, secretOpenAPI, secretRAG, rotated} {
		if strings.Contains(logs, secret) {
			t.Errorf("serve logs contain secret %q", secret)
		}
		if strings.Contains(auditBlob, secret) {
			t.Errorf("audit export contains secret %q", secret)
		}
	}
}

func serveE2EAssertBearer(t *testing.T, label, secret string, got []string) {
	t.Helper()
	want := "Bearer " + secret
	if len(got) == 0 {
		t.Fatalf("%s: host saw no Authorization", label)
	}
	for _, a := range got {
		if a != want {
			t.Fatalf("%s: Authorization %q, want %q", label, a, want)
		}
	}
}

func serveE2ENoCrossWire(t *testing.T, mcp, openapi, rag []string, secretMCP, secretOpenAPI, secretRAG string) {
	t.Helper()
	forbid := func(label string, got []string, others ...string) {
		t.Helper()
		for _, a := range got {
			for _, s := range others {
				if strings.Contains(a, s) {
					t.Fatalf("%s Authorization %q carried another transport's secret", label, a)
				}
			}
		}
	}
	forbid("MCP", mcp, secretOpenAPI, secretRAG)
	forbid("OpenAPI", openapi, secretMCP, secretRAG)
	forbid("RAG", rag, secretMCP, secretOpenAPI)
}

func serveE2EMCPAuths(h *mcpfixture.HTTPServer) []string {
	out := make([]string, 0, len(h.Log()))
	for _, e := range h.Log() {
		if e.Authorization != "" {
			out = append(out, e.Authorization)
		}
	}
	return out
}

func serveE2EOpenAPIAuths(a *openapiTransportAPI) []string {
	out := make([]string, 0, len(a.requests()))
	for _, r := range a.requests() {
		if v := r.Header.Get("Authorization"); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func serveE2ERAGAuths(a *ragServeAPI) []string {
	out := make([]string, 0, len(a.requests()))
	for _, r := range a.requests() {
		if r.authorization != "" {
			out = append(out, r.authorization)
		}
	}
	return out
}

func serveE2EAuditExport(t *testing.T, dataDir, app, tenant string) string {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(dataDir, store.DefaultFileName)})
	if err != nil {
		t.Fatalf("open serve store for audit export: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	alog, err := audit.New(db, audit.Options{})
	if err != nil {
		t.Fatalf("open audit log: %v", err)
	}
	scope := contracts.Scope{
		DeploymentID: "dep_cli", ApplicationID: app, TenantID: tenant, WorkspaceID: tenant,
		PrincipalID: "auditor", Roles: []string{"auditor"},
		CapabilityScopes: []string{audit.CapabilityExport}, SessionID: "serve-e2e-export",
	}
	var buf bytes.Buffer
	if _, err := alog.Export(ctx, scope, &buf, audit.ExportOptions{}); err != nil {
		t.Fatalf("audit export: %v", err)
	}
	return buf.String()
}
