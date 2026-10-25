package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	openapitransport "github.com/buxee/alix-ai/adapters/openapi"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/rag/ingest"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// ---- an application that follows the HTTP API contract, on a loopback port ----------

// ragServeAPI is the application behind an application_document_api source: a list endpoint, the
// bytes of an item and its metadata, over plain HTTP on loopback (the one kind of plain HTTP the
// egress gate allows). It records every request.
type ragServeAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu    sync.Mutex
	items map[string]string
	token string
	reqs  []ragServeAPIRequest
}

type ragServeAPIRequest struct {
	method, path string
	query        url.Values
	authorised   bool
}

const ragServeAPIRoot = "/documents"

func ragServeNewAPI(t *testing.T) *ragServeAPI {
	t.Helper()
	a := &ragServeAPI{t: t, items: map[string]string{}}
	a.srv = httptest.NewServer(a)
	t.Cleanup(a.srv.Close)
	return a
}

func (a *ragServeAPI) rootURL() string { return a.srv.URL + ragServeAPIRoot }

func (a *ragServeAPI) port() int {
	u, err := url.Parse(a.srv.URL)
	if err != nil {
		a.t.Fatal(err)
	}
	n, err := strconv.Atoi(u.Port())
	if err != nil {
		a.t.Fatal(err)
	}
	return n
}

func (a *ragServeAPI) put(key, body string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.items[key] = body
}

// clear empties the listing: the application answers 200 and has no document, which is also what
// an API that lost its data, or answers for the wrong scope, looks like.
func (a *ragServeAPI) clear() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.items = map[string]string{}
}

func (a *ragServeAPI) requests() []ragServeAPIRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]ragServeAPIRequest(nil), a.reqs...)
}

func (a *ragServeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	authorised := a.token == "" || r.Header.Get("Authorization") == "Bearer "+a.token
	a.reqs = append(a.reqs, ragServeAPIRequest{method: r.Method, path: r.URL.Path, query: r.URL.Query(), authorised: authorised})
	items := make(map[string]string, len(a.items))
	for k, v := range a.items {
		items[k] = v
	}
	a.mu.Unlock()
	if !authorised {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.Path == ragServeAPIRoot:
		type entry struct {
			Key        string `json:"key"`
			Version    string `json:"version"`
			Size       int    `json:"size"`
			ModifiedAt string `json:"modified_at"`
		}
		keys := make([]string, 0, len(items))
		for k := range items {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := struct {
			Items      []entry `json:"items"`
			NextCursor *string `json:"next_cursor"`
		}{Items: []entry{}}
		for _, k := range keys {
			out.Items = append(out.Items, entry{Key: k, Version: "v" + strconv.Itoa(len(items[k])), Size: len(items[k]), ModifiedAt: ragOldTime.Format(time.RFC3339)})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(r.URL.Path, ragServeAPIRoot+"/"):
		body, ok := items[strings.TrimPrefix(r.URL.Path, ragServeAPIRoot+"/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", `"v`+strconv.Itoa(len(body))+`"`)
		w.Header().Set("Last-Modified", ragOldTime.Format(http.TimeFormat))
		w.Header().Set("Content-Type", "text/markdown")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(body))
		}
	default:
		http.NotFound(w, r)
	}
}

// ---- the package that declares the source -----------------------------------------------

// ragServeAPIDecl turns the declaration of a folder source into the one of an API source over the
// application, naming the egress entry its manifest allows and, when asked, a credential.
func ragServeAPIDecl(a *ragServeAPI, credential string) func(*ap.RAGSource) {
	return func(d *ap.RAGSource) {
		u := ap.HTTPURL(a.rootURL())
		d.Type = ap.RAGSourceTypeApplicationDocumentAPI
		d.Root = ap.RAGSourceRoot{Value: ap.RAGSourceRootAPI{Kind: "api", URL: &u}}
		d.Formats = []ap.MimeType{source.MIMEMarkdown, source.MIMEText}
		d.Egress = ap.RAGSourceEgress{Entries: []ap.Slug{"app-api"}}
		if credential != "" {
			c := ap.CredentialRef(credential)
			d.CredentialRef = &c
		}
	}
}

// ragServePackage is the active package's own text, as far as the network is concerned: the
// manifest allows the application's loopback API, and the source declares it.
func ragServePackage(a *ragServeAPI, mutate ...func(*ap.RAGSource)) *apppkg.Package {
	d := ragTestDecl("")
	d.ID = "api-docs"
	for _, m := range append([]func(*ap.RAGSource){ragServeAPIDecl(a, "")}, mutate...) {
		m(&d)
	}
	return &apppkg.Package{
		Manifest: ap.Manifest{Network: ap.ManifestNetwork{Egress: map[string]ap.ManifestEgressEntry{
			"app-api": {Host: "127.0.0.1", Ports: []int64{int64(a.port())}, Scheme: ap.ManifestEgressEntrySchemeHTTP},
			// An entry no source names is not reachable through the sources' provider.
			"elsewhere": {Host: "127.0.0.2", Ports: []int64{9}, Scheme: ap.ManifestEgressEntrySchemeHTTP},
		}}},
		Sources: map[string]ap.RAGSource{"api-docs": d},
	}
}

// ragServeWithCredentials installs the credential provider of the build for the test.
func ragServeWithCredentials(t *testing.T, p openapitransport.SecretProvider) {
	t.Helper()
	prev := openapiTransportSecretProvider()
	RegisterOpenAPISecretProvider(p)
	t.Cleanup(func() { RegisterOpenAPISecretProvider(prev) })
}

// ragServeAPIRig is a stack with the providers serve registers, over a package that declares an API
// source named "api-docs", and the application it declares.
func ragServeAPIRig(t *testing.T, credential string) (*ragSourcesRig, *ragServeAPI, *ragServeNetwork) {
	t.Helper()
	api := ragServeNewAPI(t)
	rig := ragSourcesNewRig(t, nil, ragHash)
	rig.pkg.declare("api-docs", ragServeAPIDecl(api, credential))
	net := ragServeNewNetwork(slog.New(slog.DiscardHandler))
	provs, err := ragServeSourceProviders(rig.clk, nil, net, "")
	if err != nil {
		t.Fatal(err)
	}
	rig.stack.sources.providers = provs
	// What opening the active package does (ragInstallerSource.open): learn the network its
	// sources may use.
	pkg := ragServePackage(api, ragServeAPIDecl(api, credential))
	net.learn(ragApp, pkg)
	return rig, api, net
}

// A source of kind application_document_api or http_api is served by a provider of the
// composition, and its documents go through the whole pipeline: the verified scope is sent as query
// parameters, the credential is resolved and sent to the source's origin only, every connection
// goes through the egress allowlist the package's own declaration made, and an API that suddenly
// answers 200 with nothing is held by the mass-deletion guard instead of emptying the index.
func TestHTTPAPISourceProviderIsRegisteredAndSyncs(t *testing.T) {
	ctx := context.Background()
	ragServeWithCredentials(t, openapitransport.StaticSecrets{"cred:docs": "s3cret-token"})
	rig, api, _ := ragServeAPIRig(t, "cred:docs")
	api.token = "s3cret-token"
	api.put("policies/refund.md", "# Refunds\n\nThe harbour refund window lists the receipt rules.")
	api.put("policies/shipping.md", "# Shipping\n\nThe harbour dock schedule lists the tide tables.")
	op := ragSourcesOperator("t1", "w1")

	// The provider is there: the registration is accepted (before, "no provider serves sources of
	// type application_document_api") and the sync indexes what the API lists.
	rec := rig.register(t, op, "api-docs")
	id := string(rec.Status.SourceID)
	rig.drain()
	if got := ragTexts(rig.ask("harbour dock schedule")); len(got) == 0 || !strings.Contains(strings.Join(got, " "), "tide tables") {
		t.Fatalf("the documents of the API were not indexed: %v\n%s", got, rig.dumpJobs())
	}
	info, err := rig.stack.svc.Status(ctx, rig.scope, id)
	if err != nil || !info.Available || info.Counts.Indexed != 2 {
		t.Fatalf("status = %+v, %v", info, err)
	}

	// What the application was sent: the verified scope on every request, the credential, nothing
	// else of ours.
	reqs := api.requests()
	if len(reqs) == 0 {
		t.Fatal("the API saw no request")
	}
	for _, r := range reqs {
		q := r.query
		if q.Get("application_id") != ragApp || q.Get("tenant_id") != "t1" || q.Get("workspace_id") != "w1" {
			t.Errorf("%s %s carried the scope %v", r.method, r.path, q)
		}
		if !r.authorised {
			t.Errorf("%s %s was sent without the source's credential", r.method, r.path)
		}
	}

	// The API answers 200 with an empty listing, as an API that lost its data or answers for the
	// wrong scope does. Both providers believe a complete listing, so the guard is what holds the
	// scan: nothing is tombstoned, the source says why, and the documents are still served.
	api.clear()
	res, err := rig.stack.svc.RequestSync(ctx, rig.scope, id, ingest.SyncOptions{Key: "empty-listing", ServiceIdentity: ragServiceIdentity})
	if err != nil {
		t.Fatal(err)
	}
	rig.drain()
	if job := rig.jobState(res.Job.ID); job.State != jobs.StateFailed || job.ErrorCode != contracts.ErrUnavailable {
		t.Fatalf("the sync of an emptied listing ended %s (%s), want it held as a mass deletion\n%s", job.State, job.ErrorCode, rig.dumpJobs())
	}
	info, err = rig.stack.svc.Status(ctx, rig.scope, id)
	if err != nil || info.Hold == nil || info.Hold.Digest == "" || info.Hold.Confirmed {
		t.Fatalf("no hold in the status: %+v, %v", info, err)
	}
	if got := ragTexts(rig.ask("harbour dock schedule")); len(got) == 0 {
		t.Fatal("an empty listing emptied the index: the guard did not hold it")
	}
	// The operators of the deployment did not include anyone who may confirm it: a user cannot.
	if _, err := rig.stack.svc.ConfirmMassDeletion(ctx, ragScope("t1", "w1", "usr-1", "admin", "operator"), id, info.Hold.Digest); err == nil || contracts.CodeOf(err) == contracts.ErrConflict {
		t.Fatalf("a user confirmed a mass deletion: %v", err)
	}
	if got := ragTexts(rig.ask("harbour dock schedule")); len(got) == 0 {
		t.Fatal("a refused confirmation emptied the index")
	}
}

// A source that declares a credential the build cannot resolve sends nothing, and an application
// that the package's manifest does not allow reaches nothing: the default is deny.
func TestHTTPAPISourceProviderFailsClosed(t *testing.T) {
	ctx := context.Background()
	op := ragSourcesOperator("t1", "w1")

	t.Run("a credential that cannot be resolved sends nothing", func(t *testing.T) {
		ragServeWithCredentials(t, nil) // the build has no credential provider
		rig, api, _ := ragServeAPIRig(t, "cred:docs")
		api.put("a.md", "# A\n\nThe harbour dock schedule lists the tide tables.")
		id := string(rig.register(t, op, "api-docs").Status.SourceID)
		rig.drain()
		if n := len(api.requests()); n != 0 {
			t.Fatalf("%d requests reached the application without its credential", n)
		}
		if got := ragTexts(rig.ask("harbour dock schedule")); len(got) != 0 {
			t.Fatalf("an unauthenticated source was indexed: %v", got)
		}
		if info, err := rig.stack.svc.Status(ctx, rig.scope, id); err == nil && info.Available {
			t.Fatalf("a source that was never reached is available: %+v", info)
		}
	})

	t.Run("a package that allows no destination reaches nothing", func(t *testing.T) {
		rig, api, net := ragServeAPIRig(t, "")
		api.put("a.md", "# A\n\nThe harbour dock schedule lists the tide tables.")
		// The package is replaced by one whose sources name no entry of its manifest.
		bare := ragServePackage(api, func(d *ap.RAGSource) { d.Egress = ap.RAGSourceEgress{Entries: []ap.Slug{"not-in-the-manifest"}} })
		net.learn(ragApp, bare)
		rig.register(t, op, "api-docs")
		rig.drain()
		if n := len(api.requests()); n != 0 {
			t.Fatalf("%d requests went out with no egress entry", n)
		}
	})
}

// What a package allows is exactly the entries its API sources name; the endpoint resolver answers
// from the same package; and the egress of the application is the active package's alone.
func TestRAGServeNetworkLearnsFromThePackage(t *testing.T) {
	api := ragServeNewAPI(t)
	net := ragServeNewNetwork(nil)
	pkg := ragServePackage(api)
	base := ap.HTTPURL("http://127.0.0.1:4010/v1")
	pkg.Manifest.Endpoints = map[string]ap.ManifestEndpoint{
		"orders-api": {Value: ap.ManifestOpenAPIEndpoint{Transport: "openapi", BaseURL: base}},
	}
	net.learn(ragApp, pkg)

	d := net.egress.Dialer(ragApp)
	host := "127.0.0.1:" + strconv.Itoa(api.port())
	if _, err := d.DialContext(context.Background(), "tcp", host); err != nil {
		t.Errorf("the entry the source names is not reachable: %v", err)
	}
	if _, err := d.DialContext(context.Background(), "tcp", "127.0.0.2:9"); err == nil {
		t.Error("an entry no source names was reachable through the sources' provider")
	}
	if _, err := net.egress.Dialer("another-app").DialContext(context.Background(), "tcp", host); err == nil {
		t.Error("another application reached the destination")
	}

	if u, ok := net.endpoint(ragApp, "orders-api"); !ok || u != string(base) {
		t.Errorf("endpoint = %q, %v", u, ok)
	}
	if _, ok := net.endpoint(ragApp, "unknown"); ok {
		t.Error("an endpoint the package does not declare resolved")
	}
	if _, ok := net.endpoint("another-app", "orders-api"); ok {
		t.Error("another application's endpoint resolved")
	}

	// A replaced package takes what the old one allowed with it.
	net.learn(ragApp, &apppkg.Package{})
	if _, err := net.egress.Dialer(ragApp).DialContext(context.Background(), "tcp", host); err == nil {
		t.Error("the old package's destination is still reachable")
	}
	if _, ok := net.endpoint(ragApp, "orders-api"); ok {
		t.Error("the old package's endpoint still resolves")
	}
	var nilNet *ragServeNetwork
	nilNet.learn(ragApp, pkg) // a stack without a network (a test) is not a crash
}

// The credential provider of the build is adapted to the HTTP provider's own Secret type, and a
// reference it does not know is the provider's ErrNoSecret.
func TestRAGServeSecretsAdaptTheCredentialProvider(t *testing.T) {
	ctx := context.Background()
	s := ragServeSecrets{provider: func() openapitransport.SecretProvider {
		return openapitransport.StaticSecrets{"cred:docs": "value"}
	}}
	if got, err := s.Secret(ctx, "cred:docs"); err != nil || got.Reveal() != "value" || fmt.Sprint(got) == "value" {
		t.Fatalf("Secret = %q, %v (a secret prints as a placeholder)", fmt.Sprint(got), err)
	}
	if _, err := s.Secret(ctx, "cred:unknown"); err == nil || err.Error() != "httpapi: no such credential reference" {
		t.Fatalf("an unknown reference = %v", err)
	}
	if _, err := (ragServeSecrets{}).Secret(ctx, "cred:docs"); err == nil {
		t.Fatal("no credential provider resolved a reference")
	}
}

// serve registers the provider with the stack it composes, and with the integration audit.
func TestHTTPAPISourceProviderIsRegisteredWithServeAndTheAudit(t *testing.T) {
	f := newRagFixture(t, func(o *ragFixtureOpts) { o.nopool = true })
	oldParser, oldDisk, oldHook := ragNewParser, ragNewDiskPressure, ragOnCompose
	t.Cleanup(func() { ragNewParser, ragNewDiskPressure, ragOnCompose = oldParser, oldDisk, oldHook })
	ragNewParser = func(ingest.Config, *slog.Logger) ingest.Parser { return newRagStubParser() }
	ragNewDiskPressure = func(string, int64) ingest.DiskPressure { return &ragFakeDisk{} }
	var composed *ragStack
	ragOnCompose = func(s *ragStack) { composed = s }
	services := gateway.NewServices()
	if err := gateway.Provide[gateway.ApplicationService](services, ragFakeApps{}); err != nil {
		t.Fatal(err)
	}
	deps := serveDeps{Log: slog.New(slog.DiscardHandler), Clock: f.clk, DB: f.db, DataDir: "the-data-dir", Audit: f.aud, Services: services}
	if err := ragServeProvide(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []ap.RAGSourceType{
		ap.RAGSourceTypeLocalFolder, ap.RAGSourceTypeApplicationDocumentAPI, ap.RAGSourceTypeHTTPAPI,
	} {
		if composed.sources.providerFor(kind) == nil {
			t.Errorf("serve registers no provider for sources of type %s", kind)
		}
	}

	var kinds []ap.RAGSourceType
	for _, p := range auditRAGProvidersFor(clock.NewFake(ragStart)) {
		kinds = append(kinds, p.Kinds()...)
	}
	for _, want := range []ap.RAGSourceType{ap.RAGSourceTypeApplicationDocumentAPI, ap.RAGSourceTypeHTTPAPI} {
		found := false
		for _, k := range kinds {
			found = found || k == want
		}
		if !found {
			t.Errorf("the integration audit does not see a provider for %s (kinds: %v)", want, kinds)
		}
	}
}

// Opening the installed package for a registration or a re-bind is what teaches the network its
// destinations: the egress of the application exists once the package has been read, and not before.
func TestRAGInstallerSourceTeachesTheNetwork(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	db := storetest.Open(t, nil)
	inst, err := apppkg.NewInstaller(apppkg.InstallerOptions{DB: db, Clock: clk, DataDir: t.TempDir(), Verifier: trust.NewVerifier(devTrust{}, clk), CoreVersion: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inst.Install(ctx, apppkg.FromDir(filepath.Join(fixtures, "valid-minimal"))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE apppkg_packages SET status = 'active', audit_report_id = 'rep_cli_02' WHERE application_id = 'demo-library'`); err != nil {
		t.Fatal(err)
	}
	net := ragServeNewNetwork(nil)
	src := ragInstallerSource{inst: inst, net: net}
	if net.egress.Allowlist("demo-library") != nil {
		t.Fatal("the network knows an application before its package was read")
	}
	if _, _, err := src.Active(ctx, "demo-library"); err != nil {
		t.Fatal(err)
	}
	if net.egress.Allowlist("demo-library") == nil {
		t.Fatal("opening the active package did not teach the network: its API sources would reach nothing, or be refused at the first scan")
	}
}

// The integration audit builds an HTTP source the way serve does: a root that names a manifest
// endpoint plus a base_path (facility-maintenance's maintenance-history) is resolved from the
// package under audit, with the resolver serve uses, instead of being refused as validation_failed
// (a critical ragprobe.source_definition_refused). The audit still opens no connection of its own
// (no egress), and a root whose endpoint the package does not declare as an OpenAPI one is still
// refused, so the change loosens nothing it was not meant to.
func TestAuditRAGProviderResolvesEndpointsLikeServe(t *testing.T) {
	ctx := context.Background()
	scope := ragScope("t1", "w1", "usr-1")
	api := ragServeNewAPI(t)
	api.put("a.md", "# A\n\nThe harbour dock schedule lists the tide tables.")

	endpointRoot := func(endpoint string) func(*ap.RAGSource) {
		return func(d *ap.RAGSource) {
			ep, bp := ap.Slug(endpoint), ragServeAPIRoot
			d.Root = ap.RAGSourceRoot{Value: ap.RAGSourceRootAPI{Kind: "api", Endpoint: &ep, BasePath: &bp}}
		}
	}
	pkgWith := func(root string, endpoints map[string]ap.ManifestEndpoint) *apppkg.Package {
		pkg := ragServePackage(api, endpointRoot(root))
		pkg.Manifest.Application.ID = ragApp
		pkg.Manifest.Endpoints = endpoints
		return pkg
	}
	declared := map[string]ap.ManifestEndpoint{
		"docs-api": {Value: ap.ManifestOpenAPIEndpoint{Transport: "openapi", BaseURL: ap.HTTPURL(api.srv.URL)}},
	}
	definition := func(pkg *apppkg.Package) source.Definition {
		t.Helper()
		def, err := source.FromDeclaration(pkg.Sources["api-docs"], source.Binding{SourceID: ids.New(source.IDPrefix), TenantID: "t1", WorkspaceID: "w1"})
		if err != nil {
			t.Fatal(err)
		}
		return def
	}
	httpProvider := func(provs []source.Provider) source.Provider {
		t.Helper()
		for _, p := range provs {
			if providesKind(p, ap.RAGSourceTypeApplicationDocumentAPI) {
				return p
			}
		}
		t.Fatal("no provider serves application_document_api sources")
		return nil
	}

	// The reference: serve resolves the very same declaration from the same package and reaches the
	// application at the endpoint's base URL plus the base_path.
	good := pkgWith("docs-api", declared)
	net := ragServeNewNetwork(nil)
	net.learn(ragApp, good)
	serveProvs, err := ragServeSourceProviders(clock.NewFake(ragStart), nil, net, "")
	if err != nil {
		t.Fatal(err)
	}
	if h, err := httpProvider(serveProvs).Probe(ctx, scope, definition(good)); err != nil || !h.Reachable() {
		t.Fatalf("serve does not reach the source it resolves from the endpoint: %+v, %v", h, err)
	}

	// The audit builds it: not refused. It has no egress, so nothing reaches the application.
	before := len(api.requests())
	h, err := httpProvider(auditRAGProvidersForPackage(clock.NewFake(ragStart), good)).Probe(ctx, scope, definition(good))
	if err != nil {
		t.Fatalf("the audit's provider refuses a root the package declares through an endpoint (%v): %v", contracts.CodeOf(err), err)
	}
	if h.Reachable() {
		t.Errorf("the audit's provider reached the application with no egress: %+v", h)
	}
	if n := len(api.requests()) - before; n != 0 {
		t.Errorf("the audit's provider sent %d requests to the application", n)
	}

	// Negative controls: what serve refuses, the audit refuses.
	for _, tc := range []struct {
		name string
		pkg  *apppkg.Package // the package the provider is built from
		decl *apppkg.Package // the package that declares the source
	}{
		{"an endpoint the package does not declare", pkgWith("docs-api", map[string]ap.ManifestEndpoint{}), good},
		{"an endpoint declared as another kind", pkgWith("docs-api", map[string]ap.ManifestEndpoint{
			"docs-api": {Value: ap.ManifestMCPEndpoint{Transport: "mcp", URL: ap.HTTPURL(api.srv.URL)}},
		}), good},
		{"no package to read the endpoint from", nil, good},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := httpProvider(auditRAGProvidersForPackage(clock.NewFake(ragStart), tc.pkg)).Probe(ctx, scope, definition(tc.decl))
			if contracts.CodeOf(err) != contracts.ErrValidationFailed {
				t.Fatalf("the audit's provider answered %v, want the root refused as validation_failed", err)
			}
		})
	}
}

// ---- the documentation-package provider (M2-21) -------------------------------------------

const (
	docpkgClinicDir = "../../examples/reference-apps/clinic-booking/package"
	docpkgClinicApp = "clinic-booking"
	docpkgPolicies  = "documentation/policies"
)

// docpkgClinic is the clinic-booking reference package, installed in a throwaway data directory by the
// production installer (signature and content hash verified, as `alixai package install` does) and,
// when asked, activated the way the audit would.
type docpkgClinic struct {
	inst    *apppkg.Installer
	dataDir string
	rec     apppkg.Record
}

func docpkgInstallClinic(t *testing.T, db *store.DB, activate bool) *docpkgClinic {
	t.Helper()
	ctx := context.Background()
	src, err := filepath.Abs(docpkgClinicDir)
	if err != nil {
		t.Fatal(err)
	}
	dataDir, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	inst, err := apppkg.NewInstaller(apppkg.InstallerOptions{DB: db, Clock: clk, DataDir: dataDir, Verifier: trust.NewVerifier(devTrust{}, clk), CoreVersion: apppkg.DevCoreVersion})
	if err != nil {
		t.Fatal(err)
	}
	installed, err := inst.Install(ctx, apppkg.FromDir(src))
	if err != nil {
		t.Fatalf("the clinic package does not install: %v", err)
	}
	if activate {
		if _, err := db.ExecContext(ctx, `UPDATE apppkg_packages SET status = 'active', audit_report_id = 'rep_cli_21' WHERE application_id = ?`, docpkgClinicApp); err != nil {
			t.Fatal(err)
		}
	}
	rec, err := inst.Get(ctx, docpkgClinicApp, installed.Record.ManifestHash)
	if err != nil {
		t.Fatal(err)
	}
	return &docpkgClinic{inst: inst, dataDir: dataDir, rec: rec}
}

// file is a file of the installed package.
func (c *docpkgClinic) file(rel string) string {
	return filepath.Join(c.dataDir, filepath.FromSlash(c.rec.Path), filepath.FromSlash(rel))
}

// docpkgScope is a scope of the clinic for a principal.
func docpkgScope(principal string) contracts.Scope {
	sc := ragScope("t1", "w1", principal)
	sc.ApplicationID = docpkgClinicApp
	return sc
}

// docpkgRig is a RAG stack over the clinic package: the re-audit source is the installer's (as in
// serve), the providers are the ones serve registers, and the serve provider "rag_docpkg" has run.
func docpkgRig(t *testing.T, clinic *docpkgClinic, db *store.DB) *ragFixture {
	t.Helper()
	net := ragServeNewNetwork(slog.New(slog.DiscardHandler))
	over := &ragFixture{clk: clock.NewFake(ragStart), root: t.TempDir(), scope: docpkgScope("usr-1"), db: db}
	rig := newRagFixture(t, func(o *ragFixtureOpts) {
		o.over = over
		o.reaudit = ragInstallerSource{inst: clinic.inst, net: net}
		o.operators = []ragOperator{{ApplicationID: docpkgClinicApp, PrincipalID: "ops-lead", Actions: []string{string(ragSourcesActionManage)}}}
	})
	rig.pkgs.set(docpkgClinicApp, clinic.rec.ManifestHash)
	provs, err := ragServeSourceProviders(rig.clk, nil, net, "")
	if err != nil {
		t.Fatal(err)
	}
	rig.stack.sources.providers = provs

	services := gateway.NewServices()
	if err := gateway.Provide(services, rig.stack); err != nil {
		t.Fatal(err)
	}
	if err := docpkgServeProvide(context.Background(), serveDeps{Log: slog.New(slog.DiscardHandler), Clock: rig.clk, DB: db, DataDir: clinic.dataDir, Services: services}); err != nil {
		t.Fatalf("the serve provider of the documentation packages: %v", err)
	}
	return rig
}

// serve hosts the clinic's own declared source: clinic-policies, a documentation_package with a
// package root (the gate used to host a folder copy of it, because this build had no provider for
// package roots). Registered by the operator and synced from the files of the signed, active package
// the installer holds, it answers a question from the policies, cites them by the content hash of the
// very file the package ships, and reads nothing else of the package.
func TestServeServesTheClinicPoliciesDeclaredSource(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t, nil)
	clinic := docpkgInstallClinic(t, db, true)
	rig := docpkgRig(t, clinic, db)
	op := docpkgScope("ops-lead")

	rec, err := rig.stack.api.Register(ctx, op, gateway.RAGSourcesRegistration{PackageSourceID: "clinic-policies"})
	if err != nil {
		t.Fatalf("the clinic's own clinic-policies declaration was not registered: %v", err)
	}
	id := string(rec.Status.SourceID)
	rig.drain()

	info, err := rig.stack.svc.Status(ctx, rig.scope, id)
	if err != nil || !info.Available || info.Counts.Indexed != 4 {
		t.Fatalf("status = %+v, %v; want the four policy files indexed and nothing else of the package (its other markdown below documentation/ is not declared by this source)\n%s", info, err, rig.dumpJobs())
	}
	res := rig.ask("A booking waits minutes for the confirmation, then the slot is offered again")
	if len(res.Package.Items) == 0 || !strings.Contains(strings.Join(ragTexts(res), " "), "waits 10 minutes") {
		t.Fatalf("the policies were not answered from: %v\n%s", ragTexts(res), rig.dumpJobs())
	}
	shipped, err := os.ReadFile(clinic.file(docpkgPolicies + "/booking-rules.md"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(shipped)
	for _, it := range res.Package.Items {
		if string(it.SourceID) != id || it.DisplayName != "Clinic booking policies" || it.Availability != conversation.CitationAvailabilityAvailable {
			t.Errorf("evidence %+v does not name the clinic's source", it)
		}
		if strings.Contains(it.Text, "waits 10 minutes") && string(it.Version) != source.HashPrefix+hex.EncodeToString(sum[:]) {
			t.Errorf("the evidence cites version %q, want the content hash of the file the package ships", it.Version)
		}
	}

	// The package is signed content: a file changed under the data directory is not the package that
	// was activated, so the next look at the package fails its verification, the provider reads
	// nothing, nothing is tombstoned and the changed text is never indexed.
	if err := os.WriteFile(clinic.file(docpkgPolicies+"/booking-rules.md"), append(shipped, "\nThe harbour passphrase is TAMPERED-4471.\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	rig.clk.Advance(docpkgVerifyEvery + time.Second)
	sync, err := rig.stack.svc.RequestSync(ctx, rig.scope, id, ingest.SyncOptions{Key: "after-tamper", ServiceIdentity: ragServiceIdentity})
	if err != nil {
		t.Fatal(err)
	}
	rig.drain()
	if job := rig.jobState(sync.Job.ID); job.State != jobs.StateFailed || job.ErrorCode != contracts.ErrUnavailable {
		t.Fatalf("the sync of a package that no longer verifies ended %s (%s), want a failure as unavailable\n%s", job.State, job.ErrorCode, rig.dumpJobs())
	}
	if got := ragTexts(rig.ask("harbour passphrase TAMPERED")); len(got) != 0 {
		t.Fatalf("text of a package that no longer verifies was indexed: %v", got)
	}
	if got := ragTexts(rig.ask("A booking waits minutes for the confirmation")); len(got) == 0 {
		t.Fatal("an unreachable package emptied the index: it is an outage, not a deletion")
	}
}

// serve wires the provider itself: the stack serve composes has a provider for documentation_package
// sources, and the serve provider "rag_docpkg" (which runs after "rag") hands it the installer of the
// stack and the data directory, so the package it reads is the application's active, verified one.
func TestServeProvidesTheDocumentationPackageSource(t *testing.T) {
	ctx := context.Background()
	if names, _ := registeredServeProviders(); !slices.Contains(names, "rag") || !slices.Contains(names, docpkgServeProviderName) ||
		slices.Index(names, "rag") > slices.Index(names, docpkgServeProviderName) {
		t.Fatalf("serve providers = %v: want %q registered and run after rag", names, docpkgServeProviderName)
	}

	db := storetest.Open(t, nil)
	clinic := docpkgInstallClinic(t, db, false)
	aud, err := audit.New(db, audit.Options{Clock: clock.NewFake(ragStart)})
	if err != nil {
		t.Fatal(err)
	}
	oldParser, oldDisk, oldHook := ragNewParser, ragNewDiskPressure, ragOnCompose
	t.Cleanup(func() { ragNewParser, ragNewDiskPressure, ragOnCompose = oldParser, oldDisk, oldHook })
	ragNewParser = func(ingest.Config, *slog.Logger) ingest.Parser { return newRagStubParser() }
	ragNewDiskPressure = func(string, int64) ingest.DiskPressure { return &ragFakeDisk{} }
	var composed *ragStack
	ragOnCompose = func(s *ragStack) { composed = s }
	services := gateway.NewServices()
	if err := gateway.Provide[gateway.ApplicationService](services, ragFakeApps{}); err != nil {
		t.Fatal(err)
	}
	deps := serveDeps{Log: slog.New(slog.DiscardHandler), Clock: clock.NewFake(ragStart), DB: db, DataDir: clinic.dataDir, Audit: aud, Services: services}
	if err := ragServeProvide(ctx, deps); err != nil {
		t.Fatal(err)
	}
	if composed.sources.providerFor(ap.RAGSourceTypeDocumentationPackage) == nil {
		t.Fatal("serve registers no provider for sources of type documentation_package")
	}

	// Until the serve provider has run, no package is bound: the answer is an outage, not an empty package.
	net := composed.input.Reaudit.(ragInstallerSource).net
	if _, err := net.docpkgPackage(ctx, docpkgClinicApp); contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Fatalf("a package before the binding = %v, want unavailable", err)
	}
	if err := docpkgServeProvide(ctx, deps); err != nil {
		t.Fatal(err)
	}

	// The installed package is quarantined: nothing is served from it.
	if _, err := net.docpkgPackage(ctx, docpkgClinicApp); contracts.CodeOf(err) != contracts.ErrQuarantined {
		t.Fatalf("a quarantined package = %v, want quarantined", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE apppkg_packages SET status = 'active', audit_report_id = 'rep_cli_21' WHERE application_id = ?`, docpkgClinicApp); err != nil {
		t.Fatal(err)
	}
	// Active, but signed with a published TEST-ONLY key, which the trust store serve builds does not
	// accept: the installer refuses to open it, and a package that does not verify is read by nobody.
	// (TestServeServesTheClinicPoliciesDeclaredSource reads the same package with a trust store that
	// accepts the key.)
	var refused *apppkg.RefusedError
	if pkg, err := net.docpkgPackage(ctx, docpkgClinicApp); !errors.As(err, &refused) || pkg.Dir != "" {
		t.Fatalf("an active package that does not verify = %+v, %v; want it refused by the installer", pkg, err)
	}

	// No other application's package, and no application that has none.
	if _, err := net.docpkgPackage(ctx, "library-loans"); contracts.CodeOf(err) != contracts.ErrQuarantined {
		t.Errorf("an application with no package = %v, want quarantined", err)
	}

	// A serve that has no RAG stack has nothing to bind, and one whose stack reads packages through
	// something else is refused rather than left answering unavailable for ever.
	if err := docpkgServeProvide(ctx, serveDeps{Services: gateway.NewServices()}); err != nil {
		t.Errorf("a process without a RAG stack: %v", err)
	}
	odd := &ragStack{}
	odd.input.Reaudit = &ragFakeReaudit{}
	oddServices := gateway.NewServices()
	if err := gateway.Provide(oddServices, odd); err != nil {
		t.Fatal(err)
	}
	if err := docpkgServeProvide(ctx, serveDeps{Services: oddServices, DataDir: clinic.dataDir}); err == nil {
		t.Error("a stack that does not read packages through the installer was accepted")
	}
}

// What the installer records becomes a directory below the data directory and nothing else: a
// record whose path leaves it, or a data directory that is not absolute, gives no package.
func TestDocpkgPackageDirStaysBelowTheDataDirectory(t *testing.T) {
	abs, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("a", 64)
	rec := func(p string) apppkg.Record {
		return apppkg.Record{ApplicationID: docpkgClinicApp, ManifestHash: hash, Path: p}
	}
	ok, err := docpkgDir(abs, rec("packages/clinic-booking/"+hash))
	if err != nil || ok.Dir != filepath.Join(abs, "packages", "clinic-booking", hash) {
		t.Fatalf("a well-formed record = %+v, %v", ok, err)
	}
	for _, p := range []string{"", ".", "..", "../x", "packages/../../x", "/etc", "packages//x", `packages\x`, "./packages/x"} {
		if got, err := docpkgDir(abs, rec(p)); err == nil {
			t.Errorf("the record path %q gave the directory %q", p, got.Dir)
		}
	}
	if _, err := docpkgDir("relative/data", rec("packages/a/b")); err == nil {
		t.Error("a relative data directory gave a package directory")
	}
}

// The integration audit probes a documentation_package source live: the provider it runs reads the
// package under audit from its own record, while the package is still quarantined, and nothing else.
func TestAuditRAGDeploymentProbesTheClinicPoliciesSource(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t, nil)
	clinic := docpkgInstallClinic(t, db, false) // quarantined: this is what an audit sees
	pkg, rec, err := clinic.inst.Open(ctx, docpkgClinicApp, clinic.rec.ManifestHash)
	if err != nil {
		t.Fatal(err)
	}
	s := &stack{db: db, clk: clock.NewFake(ragStart), dataDir: clinic.dataDir}

	// The definition is the audit's own: straight from the declaration, with no package version.
	decl, ok := pkg.Sources["clinic-policies"]
	if !ok {
		t.Fatal("the clinic no longer declares clinic-policies")
	}
	def, err := source.FromDeclaration(decl, source.Binding{SourceID: ids.New(source.IDPrefix)})
	if err != nil {
		t.Fatal(err)
	}
	probe := func(rec apppkg.Record, pkg *apppkg.Package, scope contracts.Scope) (source.Health, error) {
		t.Helper()
		dep, err := auditRAGDeployment(s, ragConfig{}, pkg, rec)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range dep.Providers {
			if providesKind(p, ap.RAGSourceTypeDocumentationPackage) {
				return p.Probe(ctx, scope, def)
			}
		}
		t.Fatal("the audit's deployment has no provider for documentation_package sources")
		return source.Health{}, nil
	}

	scope := docpkgScope("usr-1")
	if h, err := probe(rec, pkg, scope); err != nil || !h.Reachable() {
		t.Fatalf("the package under audit = %+v, %v; want its policies folder reached", h, err)
	}
	// With nothing to read the package from, the audit reaches nothing, and says so as unavailable.
	if h, err := probe(apppkg.Record{}, pkg, scope); err != nil || h.Reachable() || h.Availability != source.Unavailable {
		t.Errorf("an audit with no record = %+v, %v; want unavailable", h, err)
	}
	if h, err := probe(rec, nil, scope); err != nil || h.Reachable() {
		t.Errorf("an audit with no package = %+v, %v; want nothing reached", h, err)
	}
	// The record of another package is not the package under audit.
	other := rec
	other.ManifestHash = strings.Repeat("b", 64)
	if h, err := probe(other, pkg, scope); err != nil || h.Reachable() {
		t.Errorf("a record of another package = %+v, %v; want nothing reached", h, err)
	}
	// Another application's scope is refused before anything is read.
	foreign := scope
	foreign.ApplicationID = "library-loans"
	if h, err := probe(rec, pkg, foreign); err == nil && h.Reachable() {
		t.Errorf("the source was reached for another application: %+v", h)
	}
}

// A real `alixai audit run` reaches a documentation_package source live: before M2-21 the audit said
// "no source provider in this deployment serves sources of type documentation_package" and recorded the
// demo library's loan-policies as unverified.
func TestAuditRunProbesADocumentationPackageSourceLive(t *testing.T) {
	e := auditRAGNewEnv(t, filepath.Join(fixtures, "valid-minimal"), nil)
	out := e.audit()
	if got := out.findings("ragprobe.source_unreachable", "warning"); len(got) != 0 {
		t.Errorf("the package's own documentation was reported unreachable: %v", got)
	}
	if srcs := e.sources(); !auditRAGLiveSource(srcs, "loan-policies") {
		t.Errorf("loan-policies, shipped in the package, was not verified live: %+v", srcs)
	}
}
