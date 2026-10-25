package ledger_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/examples/reference-apps/ledger"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/rag/providers/httpapi"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

const docsToken = "ledger-docs-credential-for-tests-only"

// TestLedgerContractsDocumentAPI: AlixAI's application document API source provider, built
// from the source declaration the package ships, reads the ledger's contracts: only the
// caller's organisation and team, only with the credential, with content-addressed
// versions the provider verifies, and a change shows up as a modified item.
func TestLedgerContractsDocumentAPI(t *testing.T) {
	w := newWorld(t)
	fx := w.provision()
	handler, err := w.app.DocumentAPI(docsToken)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	decl, ok := w.pkg.Sources["ledger-contracts"]
	if !ok {
		t.Fatal("the package declares no ledger-contracts source")
	}
	root, ok := decl.Root.Value.(ap.RAGSourceRootAPI)
	if !ok || root.URL == nil || string(*root.URL) != "http://127.0.0.1:7422" {
		t.Fatalf("the source root = %+v", decl.Root.Value)
	}
	// The package names the port the application serves on; the test serves on a free one.
	local := ap.HTTPURL(srv.URL)
	root.URL = &local
	decl.Root = ap.RAGSourceRoot{Value: root}

	reg := egress.NewRegistry()
	allow, err := egress.NewAllowlist(egress.Destination{Host: "127.0.0.1", Ports: []int{port}, Scheme: egress.SchemeHTTP})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Set(ledger.ApplicationID, allow); err != nil {
		t.Fatal(err)
	}
	provider := func(token string) *httpapi.Provider {
		p, err := httpapi.New(httpapi.Config{
			Clock: clock.NewFake(epoch), Egress: reg,
			Secrets: httpapi.StaticSecrets{string(*decl.CredentialRef): token},
		})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := provider(docsToken)
	scopeOf := func(ref string) contracts.Scope {
		sc := w.as(ref)
		sc.CapabilityScopes = []string{"rag.sync"}
		return sc
	}
	defFor := func(ref string) source.Definition {
		def, err := source.FromDeclaration(decl, source.Binding{SourceID: ids.New(source.IDPrefix), TenantID: fx.tenantOf[ref], WorkspaceID: fx.teamOf[ref]})
		if err != nil {
			t.Fatal(err)
		}
		return def
	}
	ctx := context.Background()

	// Each team has its own standard contract, so the source has something to serve.
	cs, err := good.Discover(ctx, scopeOf("acct-a-north"), defFor("acct-a-north"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Added) != 1 || cs.Added[0].Key != "contracts/master-services.md" || len(cs.Failed) != 0 || len(cs.Skipped) != 0 {
		t.Fatalf("north team's contracts = %+v", cs)
	}
	body := readItem(t, good, scopeOf("acct-a-north"), defFor("acct-a-north"), "contracts/master-services.md")
	sum := sha256.Sum256([]byte(body))
	if strings.TrimPrefix(cs.Added[0].Version.Hash, "sha256:") != hex.EncodeToString(sum[:]) || !strings.Contains(body, "Payment is due 30 days") {
		t.Fatalf("the provider's hash is not the content address of what it read: %s", body)
	}

	// Another organisation reads its own contract, not this one.
	other, err := good.Discover(ctx, scopeOf("acct-b"), defFor("acct-b"), nil)
	if err != nil || len(other.Added) != 1 {
		t.Fatalf("the other organisation's contracts = %+v, %v", other, err)
	}
	// A scope the ledger does not know is refused, not answered with an empty source: the
	// ledger reauthorises the scope itself, so a verified scope is not enough.
	for _, c := range []struct{ name, tenant, team string }{
		{"an unknown organisation", "tnt_nobody", "ws_x"},
		{"a team of another organisation", fx.tenantOf["acct-b"], fx.teamOf["acct-a-north"]},
	} {
		sc := w.scope(c.tenant, c.team, "usr_sync", "viewer")
		sc.CapabilityScopes = []string{"rag.sync"}
		if cs, err := good.Discover(ctx, sc, mustDef(t, decl, c.tenant, c.team), nil); err == nil || len(cs.Added) != 0 {
			t.Fatalf("%s was served: %+v, %v", c.name, cs, err)
		}
	}
	// No credential, no contracts.
	if cs, err := provider("not-the-credential-0000000000").Discover(ctx, scopeOf("acct-a-north"), defFor("acct-a-north"), nil); err == nil && len(cs.Added) != 0 {
		t.Fatalf("a wrong credential read the contracts: %+v", cs)
	}

	// A contract that changes is a modified item, and the event the ledger emits says so.
	if err := w.app.PutContract(fx.tenantOf["acct-a-north"], fx.teamOf["acct-a-north"], "master-services", "Master services agreement", "Payment is due 45 days after the invoice date."); err != nil {
		t.Fatal(err)
	}
	if got := eventTypes(w); len(got) != 1 || got[0] != ledger.EventContractUpdated {
		t.Fatalf("events = %v", got)
	}
	next, err := good.Discover(ctx, scopeOf("acct-a-north"), defFor("acct-a-north"), cs.Next)
	if err != nil || len(next.Modified) != 1 || len(next.Added) != 0 {
		t.Fatalf("a changed contract = %+v, %v", next, err)
	}

	// The wire contract itself: HEAD carries no body, a missing or foreign key is the same 404.
	get := func(method, path, token string, q url.Values) *http.Response {
		req, _ := http.NewRequest(method, srv.URL+path+"?"+q.Encode(), nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	scope := func(tenantRef, teamRef string) url.Values {
		return url.Values{"application_id": {"ledger"}, "tenant_id": {tenantRef}, "workspace_id": {teamRef}}
	}
	nTenant, nTeam := fx.tenantOf["acct-a-north"], fx.teamOf["acct-a-north"]
	if r := get(http.MethodHead, "/contracts/master-services.md", docsToken, scope(nTenant, nTeam)); r.StatusCode != 200 || r.ContentLength <= 0 || r.Header.Get("ETag") == "" {
		t.Fatalf("HEAD = %d %v", r.StatusCode, r.Header)
	}
	if r := get(http.MethodGet, "/contracts/master-services.md", docsToken, scope(fx.tenantOf["acct-b"], fx.teamOf["acct-b"])); r.StatusCode != 200 {
		t.Fatalf("the other organisation's own contract = %d", r.StatusCode)
	}
	if r := get(http.MethodGet, "/contracts/nothing.md", docsToken, scope(nTenant, nTeam)); r.StatusCode != 404 {
		t.Fatalf("a missing contract = %d", r.StatusCode)
	}
	if r := get(http.MethodGet, "/contracts/master-services.md", "", scope(nTenant, nTeam)); r.StatusCode != 401 {
		t.Fatalf("no credential = %d", r.StatusCode)
	}
	if r := get(http.MethodGet, "/", docsToken, url.Values{"tenant_id": {nTenant}}); r.StatusCode != 400 {
		t.Fatalf("a request that names no full scope = %d", r.StatusCode)
	}
	if r := get(http.MethodPost, "/", docsToken, scope(nTenant, nTeam)); r.StatusCode != 405 {
		t.Fatalf("POST = %d", r.StatusCode)
	}
	if _, err := w.app.DocumentAPI("short"); err == nil {
		t.Fatal("a document API with a guessable credential was built")
	}
}

func mustDef(t *testing.T, decl ap.RAGSource, tenant, team string) source.Definition {
	t.Helper()
	def, err := source.FromDeclaration(decl, source.Binding{SourceID: ids.New(source.IDPrefix), TenantID: tenant, WorkspaceID: team})
	if err != nil {
		t.Fatal(err)
	}
	return def
}

func readItem(t *testing.T, p *httpapi.Provider, sc contracts.Scope, def source.Definition, key string) string {
	t.Helper()
	c, err := p.Open(context.Background(), sc, def, key)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
