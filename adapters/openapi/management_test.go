package openapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/adapters/openapi/testdata/fixture"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestHealth(t *testing.T) {
	ctx := context.Background()
	newHost := func(t *testing.T) (*fixture.Host, *Adapter) {
		h := fixture.Start(t, fake.NewApp())
		h.RequireBearer(testToken)
		return h, newAdapter(t, fixtureConfig(t, h))
	}

	t.Run("the health operation says", func(t *testing.T) {
		h, a := newHost(t)
		rep, err := a.Health(ctx)
		if err != nil || rep.Status != adapters.HealthOK || rep.ApplicationVersion != "1.0.0" {
			t.Fatalf("health = %+v, %v", rep, err)
		}
		h.OnRequest(func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path != "/health" {
				return false
			}
			writeBody(w, 200, "application/json", `{"status":"degraded","reason":"the queue is slow"}`)
			return true
		})
		rep, err = a.Health(ctx)
		if err != nil || rep.Status != adapters.HealthDegraded || rep.Reason != "the queue is slow" {
			t.Errorf("health = %+v, %v", rep, err)
		}
		h.OnRequest(func(w http.ResponseWriter, r *http.Request) bool {
			writeBody(w, 200, "application/json", `{"status":"unavailable"}`) // not ok, and no reason
			return true
		})
		if _, err = a.Health(ctx); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
			t.Errorf("a report that is not ok and says nothing: %v, want schema_invalid", err)
		}
	})

	t.Run("no health operation: only reachability is known", func(t *testing.T) {
		h := fixture.Start(t, fake.NewApp())
		cfg := fixtureConfig(t, h)
		cfg.Endpoints[0].Settings.Management.Health = ""
		a := newAdapter(t, cfg)
		rep, err := a.Health(ctx)
		if err != nil || rep.Status != adapters.HealthDegraded || !strings.Contains(rep.Reason, "no health operation") {
			t.Errorf("health = %+v, %v; want degraded, never ok: nothing but a connection was checked", rep, err)
		}
		if n := len(h.Requests()); n != 0 {
			t.Errorf("the probe sent %d requests; it must not call an operation nobody mapped", n)
		}
		h.SetAvailable(false)
		rep, err = a.Health(ctx)
		if err != nil || rep.Status != adapters.HealthUnavailable {
			t.Errorf("health of a host that is down = %+v, %v", rep, err)
		}
		if err := rep.Validate(); err != nil {
			t.Errorf("the report is invalid: %v", err)
		}
	})
}

func TestInspect(t *testing.T) {
	ctx := context.Background()
	h := fixture.Start(t, fake.NewApp())
	h.RequireBearer(testToken)
	a := newAdapter(t, fixtureConfig(t, h))
	in, err := a.Inspect(ctx, testScope(), adapters.InspectRequest{Aspects: []adapters.Aspect{adapters.AspectNetwork, adapters.AspectVersions}})
	if err != nil || len(in.Reports) != 2 {
		t.Fatalf("inspect = %+v, %v", in, err)
	}
	r := h.Requests()[0]
	if r.Method != "GET" || r.Path != "/inspect" || r.Query != "aspects=network&aspects=versions" || r.Header.Get(fixture.IdentityHdr) == "" {
		t.Errorf("request = %+v", r)
	}
	if _, err := a.Inspect(ctx, testScope(), adapters.InspectRequest{Aspects: []adapters.Aspect{"nonsense"}}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("an unknown aspect: %v", err)
	}

	t.Run("a report for an aspect nobody asked for", func(t *testing.T) {
		h.OnRequest(func(w http.ResponseWriter, r *http.Request) bool {
			writeBody(w, 200, "application/json", `{"reports":[{"aspect":"storage","state":"unsupported","facts":[]}]}`)
			return true
		})
		_, err := a.Inspect(ctx, testScope(), adapters.InspectRequest{Aspects: []adapters.Aspect{adapters.AspectVersions}})
		if contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
			t.Errorf("error = %v, want schema_invalid", err)
		}
	})

	t.Run("no inspect operation: every aspect is unsupported", func(t *testing.T) {
		cfg := fixtureConfig(t, h)
		cfg.Endpoints[0].Settings.Management.Inspect = ""
		bare := newAdapter(t, cfg)
		before := len(h.Requests())
		in, err := bare.Inspect(ctx, testScope(), adapters.InspectRequest{})
		if err != nil || len(in.Reports) != len(adapters.AllAspects()) || len(in.Unverified()) != len(adapters.AllAspects()) {
			t.Errorf("inspect = %+v, %v", in, err)
		}
		if len(h.Requests()) != before {
			t.Error("the adapter looked for the facts by itself")
		}
	})
}

// The audit's provisioning operations are approved by the adapter configuration,
// are not capabilities and cannot be reached through the adapter contract; they
// can be called through Manage and nothing else can.
func TestManage(t *testing.T) {
	ctx := context.Background()
	h := fixture.Start(t, fake.NewApp())
	h.RequireBearer(testToken)
	a := newAdapter(t, fixtureConfig(t, h))

	created, err := a.Manage(ctx, testEP, fixture.OpCreate, map[string]any{"tenant_id": "ten_audit_1", "workspaces": []any{"wks_1", "wks_2"}})
	if err != nil || created.Status != 201 || created.Body["created"] != true {
		t.Fatalf("create = %+v, %v", created, err)
	}
	gone, err := a.Manage(ctx, testEP, fixture.OpCleanup, map[string]any{"tenant_id": "ten_audit_1"})
	if err != nil || gone.Status != 200 || gone.Body["existed"] != true {
		t.Fatalf("cleanup = %+v, %v", gone, err)
	}
	again, err := a.Manage(ctx, testEP, fixture.OpCleanup, map[string]any{"tenant_id": "ten_audit_1"})
	if err != nil || again.Status != 404 || again.Body["existed"] != false {
		t.Fatalf("a second cleanup = %+v, %v; a status the document describes is the caller's to read", again, err)
	}

	reqs := h.Requests()
	if len(reqs) != 3 || reqs[0].Method != "POST" || reqs[0].Path != "/tenants" || reqs[1].Method != "DELETE" || reqs[1].Path != "/tenants/ten_audit_1" {
		t.Fatalf("requests = %+v", reqs)
	}
	var body map[string]any
	if err := json.Unmarshal(reqs[0].Body, &body); err != nil || cmp.Diff(map[string]any{"tenant_id": "ten_audit_1", "workspaces": []any{"wks_1", "wks_2"}}, body) != "" {
		t.Errorf("body = %s", reqs[0].Body)
	}
	for _, r := range reqs {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Errorf("a provisioning request lacks the credential: %+v", r.Header)
		}
		if r.Header.Get(fixture.IdentityHdr) != "" || r.Header.Get(fixture.KeyHdr) != "" {
			t.Errorf("a provisioning request carries an identity or a key: %+v", r.Header)
		}
	}

	for name, tc := range map[string]struct{ ep, op string }{
		"an operation of the document that is not provisioning": {testEP, fixture.OpExport},
		"a management operation":                                {testEP, fixture.OpHealth},
		"a capability's operation":                              {testEP, fixture.OpGet},
		"an unknown operation":                                  {testEP, "dropEverything"},
		"an unknown endpoint":                                   {"other-api", fixture.OpCreate},
	} {
		before := len(h.Requests())
		if _, err := a.Manage(ctx, tc.ep, tc.op, nil); contracts.CodeOf(err) != contracts.ErrUnknownCapability || len(h.Requests()) != before {
			t.Errorf("%s: %v (%d requests sent)", name, err, len(h.Requests())-before)
		}
	}
	// A status the document does not describe is an error, not a reply.
	h.OnRequest(func(w http.ResponseWriter, r *http.Request) bool {
		writeBody(w, 500, "text/plain", "boom")
		return true
	})
	if _, err := a.Manage(ctx, testEP, fixture.OpCleanup, map[string]any{"tenant_id": "x"}); contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Errorf("a server error: %v", err)
	}
	h.OnRequest(nil)
	// Not through the adapter contract: no capability can be bound to one by a package
	// that does not also map it, and the description does not list it as callable.
	d, err := a.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range d.Unmapped {
		if u == fixture.OpCreate || u == fixture.OpCleanup {
			t.Errorf("an approved provisioning operation is reported as unmapped: %s", u)
		}
	}
}

// Several endpoints of one application: each operation goes to the endpoint it
// is mapped on, with that endpoint's credential and no other, and the
// application's description and health combine them.
func TestSeveralEndpoints(t *testing.T) {
	ctx := context.Background()
	spec := []byte(`openapi: 3.0.3
info: {title: part, version: "1"}
paths:
  /items/{id}:
    get:
      operationId: getItem
      parameters:
        - {name: id, in: path, required: true, schema: {type: string}}
      responses: &responses
        "200": {$ref: "#/components/responses/Json"}
  /items:
    post:
      operationId: commitItem
      requestBody: {required: true, content: {application/json: {schema: {type: object}}}}
      responses: *responses
  /describe:
    get: {operationId: describeApplication, responses: *responses}
  /health:
    get: {operationId: getHealth, responses: *responses}
components:
  responses:
    Json:
      description: json
      content: {application/json: {schema: {type: object}}}
`)
	var alpha, beta *testAPI
	answer := func(versions, health string) func(w http.ResponseWriter, r *http.Request, body []byte) {
		return func(w http.ResponseWriter, r *http.Request, body []byte) {
			switch r.URL.Path {
			case "/describe":
				writeBody(w, 200, "application/json", `{"contract_versions":`+versions+`,"application_version":"`+r.Host+`"}`)
			case "/health":
				writeBody(w, 200, "application/json", health)
			default:
				writeBody(w, 200, "application/json", okResult)
			}
		}
	}
	alpha = startAPI(t, answer("[1,2]", `{"status":"ok"}`))
	beta = startAPI(t, answer("[1,3]", `{"status":"ok"}`))
	mkEndpoint := func(id string, api *testAPI, ref string) Endpoint {
		return Endpoint{
			ID: id, BaseURL: api.URL, Spec: spec, CredentialRef: ref,
			Settings: mustSettings(t, "management:\n  describe: describeApplication\n  health: getHealth\n"),
		}
	}
	allow := func(ports ...int) *egress.Dialer { return newLoopbackDialer(t, ports...) }
	cfg := Config{
		ApplicationID: testApp,
		Endpoints:     []Endpoint{mkEndpoint("beta-api", beta, "cred:beta"), mkEndpoint("alpha-api", alpha, "cred:alpha")},
		Capabilities: []Capability{
			{ID: fake.ReadCapability, Operations: map[adapters.Operation]Binding{adapters.OpRead: {Endpoint: "alpha-api", OperationID: "getItem", Arguments: map[string]string{"item_id": "id"}}}},
			{ID: fake.ActionCapability, Operations: map[adapters.Operation]Binding{adapters.OpCommit: {Endpoint: "beta-api", OperationID: "commitItem"}}},
		},
		Dialer:           allow(alpha.Port(), beta.Port()),
		Secrets:          StaticSecrets{"cred:alpha": "alpha-secret-1", "cred:beta": "beta-secret-2"},
		MaxResponseBytes: testSize,
	}
	a := newAdapter(t, cfg)

	if _, err := read(a, fake.ReadCapability, map[string]any{"item_id": "x"}); err != nil {
		t.Fatal(err)
	}
	p := preparedFor(fake.ActionCapability, newKey())
	if _, err := a.Commit(ctx, testScope(), adapters.CommitRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: p.IdempotencyKey, Prepared: p}); err != nil {
		t.Fatal(err)
	}
	ar, br := alpha.Requests(), beta.Requests()
	if len(ar) != 1 || ar[0].Path != "/items/x" || ar[0].Header.Get("Authorization") != "Bearer alpha-secret-1" {
		t.Errorf("alpha received %+v", ar)
	}
	if len(br) != 1 || br[0].Path != "/items" || br[0].Header.Get("Authorization") != "Bearer beta-secret-2" {
		t.Errorf("beta received %+v", br)
	}

	d, err := a.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]int{1}, d.ContractVersions); diff != "" {
		t.Errorf("versions are those both endpoints speak (-want +got):\n%s", diff)
	}
	if len(d.Capabilities) != 2 {
		t.Errorf("capabilities = %+v", d.Capabilities)
	}
	for _, r := range append(alpha.Requests(), beta.Requests()...) {
		if r.Path == "/describe" && !strings.Contains(r.Header.Get("Authorization"), "secret") {
			t.Errorf("a management call lacks its endpoint's credential: %+v", r.Header)
		}
	}

	t.Run("the worst health is the application's", func(t *testing.T) {
		beta.Set(answer("[1,3]", `{"status":"degraded","reason":"beta is slow"}`))
		rep, err := a.Health(ctx)
		if err != nil || rep.Status != adapters.HealthDegraded || rep.Reason != "beta is slow" {
			t.Errorf("health = %+v, %v", rep, err)
		}
	})

	t.Run("endpoints that share no version", func(t *testing.T) {
		alpha.Set(answer("[2]", `{"status":"ok"}`))
		beta.Set(answer("[3]", `{"status":"ok"}`))
		if _, err := a.Describe(ctx); contracts.CodeOf(err) != contracts.ErrValidationFailed || reasonIn(err) != "version_incompatible" {
			t.Errorf("error = %v, want validation_failed (version_incompatible)", err)
		}
	})
}

// Close ends a call in flight and refuses the ones after it, and a call that was
// refused because the adapter is closed sent nothing.
func TestCloseEndsCallsInFlight(t *testing.T) {
	ctx := context.Background()
	released := make(chan struct{})
	api := startAPI(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		select {
		case <-r.Context().Done():
		case <-released:
		}
	})
	t.Cleanup(func() { close(released) })
	a := newAdapter(t, miniConfig(t, api, miniSpec("      type: object\n"), miniCapabilities()))

	type outcome struct{ err error }
	run := func(call func() error) chan outcome {
		ch := make(chan outcome, 1)
		go func() { ch <- outcome{call()} }()
		return ch
	}
	p := preparedFor(fake.ActionCapability, newKey())
	rd := run(func() error { _, err := read(a, fake.ReadCapability, map[string]any{"item_id": "x"}); return err })
	cm := run(func() error {
		_, err := a.Commit(ctx, testScope(), adapters.CommitRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: p.IdempotencyKey, Prepared: p})
		return err
	})
	deadline := time.Now().Add(5 * time.Second)
	for len(api.Requests()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(api.Requests()) < 2 {
		t.Fatal("the calls did not reach the application")
	}
	start := time.Now()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	for name, ch := range map[string]chan outcome{"read": rd, "commit": cm} {
		select {
		case o := <-ch:
			want := contracts.ErrUnavailable
			if name == "commit" {
				want = contracts.ErrUncertainOutcome // it was sent: it may have been applied
			}
			if contracts.CodeOf(o.err) != want {
				t.Errorf("%s in flight when the adapter closed: %v, want %s", name, o.err, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s is still running %s after Close", name, time.Since(start))
		}
	}

	before := len(api.Requests())
	if _, err := read(a, fake.ReadCapability, map[string]any{"item_id": "x"}); contracts.CodeOf(err) != contracts.ErrUnavailable || reasonIn(err) != ReasonAdapterClosed {
		t.Errorf("a read after Close: %v", err)
	}
	_, err := a.Commit(ctx, testScope(), adapters.CommitRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: p.IdempotencyKey, Prepared: p})
	if contracts.CodeOf(err) != contracts.ErrUnavailable || reasonIn(err) != ReasonAdapterClosed {
		t.Errorf("a commit after Close: %v, want unavailable: nothing was sent", err)
	}
	if len(api.Requests()) != before {
		t.Error("a call after Close reached the application")
	}
	if err := a.Close(); err != nil {
		t.Errorf("a second Close: %v", err)
	}
}
