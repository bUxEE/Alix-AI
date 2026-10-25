package openapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// reasonIn finds Details["reason"] on any *contracts.Error in the chain.
func reasonIn(err error) string {
	for err != nil {
		var ce *contracts.Error
		if !errors.As(err, &ce) {
			return ""
		}
		if r, ok := ce.Details["reason"].(string); ok {
			return r
		}
		err = ce.Unwrap()
	}
	return ""
}

func readItem(ctx context.Context, ad adapters.AppAdapter, id string) (actions.DeterministicResult, error) {
	return ad.Read(ctx, testScope(), adapters.ReadRequest{CapabilityID: fake.ReadCapability, Fields: map[string]any{"item_id": id}})
}

// An operation the package does not map cannot be called, however it is asked
// for: the document may define it, a capability ID, a field or a model may name
// it, and none of that makes it reachable. Nothing reaches the application.
//
// SEC-053
func TestUnapprovedOperationRejected(t *testing.T) {
	app := fake.NewApp(fake.WithUnmapped("host_claims_this", fixture.OpGet, fixture.OpExport))
	h := fixture.Start(t, app)
	h.RequireBearer(testToken)
	a := newAdapter(t, fixtureConfig(t, h))
	ctx := context.Background()
	sc := testScope()

	t.Run("an operation of a capability that is not mapped", func(t *testing.T) {
		for name, call := range map[string]func() (actions.DeterministicResult, error){
			"cancel of a read capability": func() (actions.DeterministicResult, error) {
				return a.Cancel(ctx, sc, adapters.CancelRequest{CapabilityID: fake.ReadCapability, IdempotencyKey: newKey()})
			},
			"validate of a read capability": func() (actions.DeterministicResult, error) {
				return a.Validate(ctx, sc, adapters.ValidateRequest{CapabilityID: fake.ReadCapability, Fields: map[string]any{"name": "x"}})
			},
			"read of an action capability": func() (actions.DeterministicResult, error) {
				return a.Read(ctx, sc, adapters.ReadRequest{CapabilityID: fake.ActionCapability, Fields: map[string]any{"item_id": "x"}})
			},
			"an unknown capability": func() (actions.DeterministicResult, error) {
				return a.Read(ctx, sc, adapters.ReadRequest{CapabilityID: "admin.export", Fields: map[string]any{}})
			},
			"a capability named like an operationId": func() (actions.DeterministicResult, error) {
				return a.Read(ctx, sc, adapters.ReadRequest{CapabilityID: fixture.OpExport, Fields: map[string]any{}})
			},
			"a capability named like a path": func() (actions.DeterministicResult, error) {
				return a.Read(ctx, sc, adapters.ReadRequest{CapabilityID: "/admin/export", Fields: map[string]any{}})
			},
		} {
			res, err := call()
			if contracts.CodeOf(err) != contracts.ErrUnknownCapability || reasonIn(err) != ReasonOperationNotMapped {
				t.Errorf("%s: %v (%+v), want unknown_capability (%s)", name, err, res, ReasonOperationNotMapped)
			}
		}
	})

	t.Run("an entity type the capability declares no resolver for", func(t *testing.T) {
		_, err := a.Read(ctx, sc, adapters.ReadRequest{CapabilityID: fake.ReadCapability,
			Fields: map[string]any{"entity_type": "patient", "mention": "Rossi"}})
		if contracts.CodeOf(err) != contracts.ErrValidationFailed || reasonIn(err) != ReasonUnknownEntityType {
			t.Errorf("error = %v, want validation_failed (%s)", err, ReasonUnknownEntityType)
		}
	})

	t.Run("values cannot change which operation is addressed", func(t *testing.T) {
		for _, id := range []string{"../admin/export", "..", ".", "", "a/b", `a\b`, "a\nb", "a\x00b", "../../admin/export"} {
			_, err := readItem(ctx, wrapped(t, a), id)
			if contracts.CodeOf(err) != contracts.ErrValidationFailed {
				t.Errorf("item_id %q: %v, want validation_failed", id, err)
			}
		}
		// Characters that would end the path or start a query are data, escaped.
		for id, wantPath := range map[string]string{
			"x?y=1": "/items/x%3Fy=1", "a#b": "/items/a%23b", "100%": "/items/100%25", "a b": "/items/a%20b",
			"%2e%2e": "/items/%252e%252e", "x;y": "/items/x%3By", "é": "/items/%C3%A9",
		} {
			before := len(h.Requests())
			if _, err := readItem(ctx, wrapped(t, a), id); err != nil {
				t.Errorf("item_id %q: %v", id, err)
				continue
			}
			reqs := h.Requests()[before:]
			if len(reqs) != 1 || reqs[0].Path != wantPath || reqs[0].Query != "" {
				t.Errorf("item_id %q reached the host as %+v, want path %s and no query", id, reqs, wantPath)
			}
		}
	})

	t.Run("only the mapped operations are called, and the rest is reported", func(t *testing.T) {
		d, err := a.Describe(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Validate(); err != nil {
			t.Errorf("the description is invalid: %v", err)
		}
		// exportEverything is in the document and mapped by nobody; the management and
		// provisioning operations are approved by the adapter configuration and are
		// not unmapped. What the host says it has that the adapter does not know is
		// reported too, once, unless it is an operation the adapter knows (getItem is mapped).
		if diff := cmp.Diff([]string{fixture.OpExport, "host_claims_this"}, d.Unmapped); diff != "" {
			t.Errorf("unmapped (-want +got):\n%s", diff)
		}
		for _, r := range h.Requests() {
			if strings.Contains(r.Path, "/admin") {
				t.Errorf("the unmapped operation was reached: %+v", r)
			}
		}
		if h.ExportHits() != 0 {
			t.Errorf("exportEverything was called %d times", h.ExportHits())
		}
	})

	t.Run("a package cannot map what the document does not define, or a method the role forbids", func(t *testing.T) {
		for name, mutate := range map[string]func(c *Config){
			"an operationId the document does not define": func(c *Config) {
				c.Capabilities[0].Operations[adapters.OpRead] = Binding{Endpoint: testEP, OperationID: "dropEverything"}
			},
			"an operation of another endpoint": func(c *Config) {
				c.Capabilities[0].Operations[adapters.OpRead] = Binding{Endpoint: "other-api", OperationID: fixture.OpGet}
			},
			"a read on DELETE": func(c *Config) {
				c.Capabilities[0].Operations[adapters.OpRead] = Binding{Endpoint: testEP, OperationID: fixture.OpCancel}
			},
			"a commit on GET": func(c *Config) {
				c.Capabilities[1].Operations[adapters.OpCommit] = Binding{Endpoint: testEP, OperationID: fixture.OpGet}
			},
			"a prepare on GET": func(c *Config) {
				c.Capabilities[1].Operations[adapters.OpPrepare] = Binding{Endpoint: testEP, OperationID: fixture.OpStatus}
			},
			"a management operation that is not in the document": func(c *Config) {
				c.Endpoints[0].Settings.Management.Describe = "noSuchOperation"
			},
		} {
			cfg := fixtureConfig(t, h)
			mutate(&cfg)
			if ad, err := New(cfg); err == nil {
				_ = ad.Close()
				t.Errorf("%s: an adapter was built", name)
			} else if contracts.CodeOf(err) != contracts.ErrValidationFailed {
				t.Errorf("%s: %v, want validation_failed", name, err)
			}
		}
	})

	for _, r := range h.Requests() {
		if r.Header.Get(fixture.IdentityHdr) == "" {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(r.Header.Get(fixture.IdentityHdr))
		var id adapters.Identity
		if err != nil || json.Unmarshal(raw, &id) != nil {
			t.Errorf("the identity a request carried does not decode: %v", err)
			continue
		}
		if diff := cmp.Diff(adapters.IdentityFrom(sc), id); diff != "" {
			t.Errorf("the identity a request carried is not the call's:\n%s", diff)
		}
	}
}

// A reply that does not satisfy the response the document describes is an
// error and none of it is used. The reply below is a perfectly good
// DeterministicResult, so adapters.Normalise accepts it: what refuses it is the
// document, which asks for two values. The same reply under a document that asks
// for one is accepted, which shows that the document was the cause.
//
// SEC-049
// SEC-050
func TestResponseSchemaViolationIsError(t *testing.T) {
	ctx := context.Background()
	strict := "      type: object\n      required: [status, values]\n      properties:\n        status: {type: string}\n        values: {type: array, minItems: 2, items: {type: object}}\n"
	loose := strings.Replace(strict, "minItems: 2", "minItems: 1", 1)
	reply := func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeBody(w, 200, "application/json", okResult)
	}

	t.Run("the document asks for more than the reply has", func(t *testing.T) {
		api := startAPI(t, reply)
		a := newAdapter(t, miniConfig(t, api, miniSpec(strict), miniCapabilities()))
		for name, ad := range map[string]adapters.AppAdapter{"raw": a, "wrapped": wrapped(t, a)} {
			res, err := readItem(ctx, ad, "item_1")
			if err == nil {
				t.Fatalf("%s: a reply that violates the document was accepted: %+v", name, res)
			}
			if contracts.CodeOf(err) != contracts.ErrSchemaInvalid || reasonIn(err) != ReasonResponseViolatesSpec {
				t.Errorf("%s: %v, want schema_invalid (%s)", name, err, ReasonResponseViolatesSpec)
			}
			if res.Status != "" || len(res.Values) != 0 {
				t.Errorf("%s: a result came back with the error: %+v", name, res)
			}
			if strings.Contains(err.Error(), "Widget") {
				t.Errorf("%s: the error quotes the reply: %v", name, err)
			}
		}
		if n := len(api.Requests()); n != 2 {
			t.Errorf("the application received %d requests, want 2 (one per call, no retry)", n)
		}
	})

	t.Run("the same reply under a document that allows it", func(t *testing.T) {
		api := startAPI(t, reply)
		a := newAdapter(t, miniConfig(t, api, miniSpec(loose), miniCapabilities()))
		res, err := readItem(ctx, wrapped(t, a), "item_1")
		if err != nil || res.Status != actions.DeterministicResultStatusOk || len(res.Values) != 1 {
			t.Fatalf("the control reply: %+v, %v", res, err)
		}
	})

	t.Run("a Commit whose reply violates the document is uncertain_outcome", func(t *testing.T) {
		api := startAPI(t, reply)
		a := newAdapter(t, miniConfig(t, api, miniSpec(strict), miniCapabilities()))
		p := preparedFor(fake.ActionCapability, newKey())
		commit := func(ad adapters.AppAdapter) error {
			_, err := ad.Commit(ctx, testScope(), adapters.CommitRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: p.IdempotencyKey, Prepared: p})
			return err
		}
		for name, ad := range map[string]adapters.AppAdapter{"raw": a, "wrapped": wrapped(t, a)} {
			err := commit(ad)
			if contracts.CodeOf(err) != contracts.ErrUncertainOutcome || !contracts.HasCode(err, contracts.ErrSchemaInvalid) {
				t.Errorf("%s: %v, want uncertain_outcome caused by schema_invalid: the host may have applied the commit", name, err)
			}
		}
	})

	t.Run("a reply the document does not describe", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			status int
			ct     string
			body   string
			reason string
		}{
			{"a status that is not described", 418, "application/json", okResult, ReasonStatusNotDeclared},
			{"a body that is not JSON", 200, "text/plain", okResult, ReasonNotJSON},
			{"no content type", 200, "", okResult, ReasonNotJSON},
			{"JSON that is not an object", 200, "application/json", `[1,2]`, ReasonResponseViolatesSpec},
			{"truncated JSON", 200, "application/json", okResult[:len(okResult)/2], ReasonResponseViolatesSpec},
			{"a status that is described without a body", 202, "application/json", okResult, ReasonResponseNotDescribed},
			{"a redirect", 302, "application/json", "", ReasonRedirect},
			{"a 404 the document does not describe", 404, "text/html", "<h1>not found</h1>", ReasonStatusNotDeclared},
		} {
			t.Run(tc.name, func(t *testing.T) {
				api := startAPI(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
					if tc.status == 302 {
						w.Header().Set("Location", "http://127.0.0.1:1/elsewhere")
					}
					writeBody(w, tc.status, tc.ct, tc.body)
				})
				a := newAdapter(t, miniConfig(t, api, miniSpec(loose), miniCapabilities()))
				_, err := readItem(ctx, a, "item_1")
				if contracts.CodeOf(err) != contracts.ErrSchemaInvalid || reasonIn(err) != tc.reason {
					t.Errorf("%v, want schema_invalid (%s)", err, tc.reason)
				}
			})
		}
	})

	t.Run("a described status carries the host's refusal as a result", func(t *testing.T) {
		const rejected = `{"status":"rejected","identifiers":[],"values":[],"errors":[{"code":"validation_failed","message":"no such item","field":"item_id"}],"allowed_next_actions":[]}`
		api := startAPI(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
			writeBody(w, 422, "application/json; charset=utf-8", rejected)
		})
		a := newAdapter(t, miniConfig(t, api, miniSpec("      type: object\n      required: [status]\n      properties:\n        status: {type: string}\n"), miniCapabilities()))
		res, err := readItem(ctx, wrapped(t, a), "item_1")
		if err != nil || res.Status != actions.DeterministicResultStatusRejected || len(res.Errors) != 1 {
			t.Fatalf("a 422 with a result: %+v, %v", res, err)
		}
	})

	t.Run("server errors and rate limits are failures of the call", func(t *testing.T) {
		for _, tc := range []struct {
			status int
			body   string
			code   contracts.ErrCode
		}{
			{500, "<html>boom</html>", contracts.ErrUnavailable},
			{503, "{}", contracts.ErrUnavailable},
			{429, "slow down", contracts.ErrRateLimited},
		} {
			api := startAPI(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) { writeBody(w, tc.status, "text/html", tc.body) })
			a := newAdapter(t, miniConfig(t, api, miniSpec(loose), miniCapabilities()))
			if _, err := readItem(ctx, a, "item_1"); contracts.CodeOf(err) != tc.code {
				t.Errorf("a read answered %d: %v, want %s", tc.status, err, tc.code)
			}
			// A Commit that was answered this way may still have been applied: never a
			// code that says nothing happened.
			p := preparedFor(fake.ActionCapability, newKey())
			_, err := a.Commit(ctx, testScope(), adapters.CommitRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: p.IdempotencyKey, Prepared: p})
			if contracts.CodeOf(err) != contracts.ErrUncertainOutcome {
				t.Errorf("a commit answered %d: %v, want uncertain_outcome", tc.status, err)
			}
		}
	})
}

// Outbound access is denied unless the destination is listed, and a listed
// destination is the exact host, port and scheme. A request for anything else is
// refused before a connection is made, whatever the document, the package or a
// redirect says, and a Commit that was refused is not uncertain: nothing was sent.
//
// SEC-055
func TestEgressAllowlistEnforced(t *testing.T) {
	ctx := context.Background()
	app := fake.NewApp()
	h := fixture.Start(t, app)

	build := func(t *testing.T, mutate func(*Config)) *Adapter {
		cfg := fixtureConfig(t, h)
		mutate(&cfg)
		return newAdapter(t, cfg)
	}
	denied := func(t *testing.T, err error, reason egress.Reason) {
		t.Helper()
		if !egress.IsDenied(err) || egress.ReasonOf(err) != reason {
			t.Fatalf("error = %v, want an egress refusal (%s)", err, reason)
		}
		if contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Errorf("code = %s, want unavailable: nothing was sent", contracts.CodeOf(err))
		}
	}
	untouched := func(t *testing.T) {
		t.Helper()
		if n := len(h.Requests()); n != 0 {
			t.Errorf("the application received %d requests", n)
		}
	}

	t.Run("a destination that is not listed", func(t *testing.T) {
		var hook []egress.Denial
		a := build(t, func(c *Config) {
			allow, err := egress.NewAllowlist(egress.Destination{Host: "127.0.0.1", Ports: []int{h.Port() + 1}, Scheme: "http"})
			if err != nil {
				t.Fatal(err)
			}
			c.Dialer = egress.NewDialer(allow, egress.WithDenyHook(func(d egress.Denial) { hook = append(hook, d) }))
			c.Secrets = StaticSecrets{testRef: testToken}
		})
		_, err := readItem(ctx, wrapped(t, a), "item_1")
		denied(t, err, egress.ReasonNotAllowlisted)
		untouched(t)
		if len(hook) != 1 || hook[0].Host != "127.0.0.1" || hook[0].Port != h.Port() {
			t.Errorf("deny hook saw %+v", hook)
		}
	})

	t.Run("default deny: no dialer", func(t *testing.T) {
		a := build(t, func(c *Config) { c.Dialer = nil })
		_, err := readItem(ctx, wrapped(t, a), "item_1")
		denied(t, err, egress.ReasonNoAllowlist)
		untouched(t)
	})

	t.Run("the same server under another name", func(t *testing.T) {
		a := build(t, func(c *Config) { c.Endpoints[0].BaseURL = "http://localhost:" + fmt.Sprint(h.Port()) })
		_, err := readItem(ctx, wrapped(t, a), "item_1")
		denied(t, err, egress.ReasonNotAllowlisted)
		untouched(t)
	})

	t.Run("another scheme", func(t *testing.T) {
		a := build(t, func(c *Config) { c.Endpoints[0].BaseURL = "https://127.0.0.1:" + fmt.Sprint(h.Port()) })
		_, err := readItem(ctx, wrapped(t, a), "item_1")
		denied(t, err, egress.ReasonScheme)
		untouched(t)
	})

	t.Run("a Commit that was refused is not uncertain", func(t *testing.T) {
		a := build(t, func(c *Config) { c.Dialer = nil })
		p := preparedFor(fake.ActionCapability, newKey())
		_, err := wrapped(t, a).Commit(ctx, testScope(), adapters.CommitRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: p.IdempotencyKey, Prepared: p})
		denied(t, err, egress.ReasonNoAllowlist)
		untouched(t)
	})

	t.Run("a probe of reachability goes through the gate too", func(t *testing.T) {
		cfg := fixtureConfig(t, h)
		cfg.Endpoints[0].Settings.Management.Health = ""
		cfg.Dialer = nil
		a := newAdapter(t, cfg)
		rep, err := a.Health(ctx)
		if err != nil || rep.Status != adapters.HealthUnavailable {
			t.Errorf("health = %+v, %v; want unavailable", rep, err)
		}
		untouched(t)
	})

	t.Run("a redirect is not followed, even to a listed destination", func(t *testing.T) {
		other := startAPI(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
			writeBody(w, 200, "application/json", okResult)
		})
		api := startAPI(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
			w.Header().Set("Location", other.URL+"/stolen")
			w.WriteHeader(http.StatusTemporaryRedirect)
		})
		cfg := miniConfig(t, api, miniSpec("      type: object\n      required: [status]\n      properties:\n        status: {type: string}\n"), miniCapabilities())
		cfg.Dialer = api.Dialer(t, other.Port())
		cfg.Endpoints[0].CredentialRef = testRef
		cfg.Endpoints[0].Settings = mustSettings(t, "auth: {}\n")
		cfg.Secrets = StaticSecrets{testRef: testToken}
		a := newAdapter(t, cfg)

		_, err := readItem(ctx, a, "item_1")
		if contracts.CodeOf(err) != contracts.ErrSchemaInvalid || reasonIn(err) != ReasonRedirect {
			t.Errorf("a redirected read: %v, want schema_invalid (%s)", err, ReasonRedirect)
		}
		p := preparedFor(fake.ActionCapability, newKey())
		_, err = a.Commit(ctx, testScope(), adapters.CommitRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: p.IdempotencyKey, Prepared: p})
		if contracts.CodeOf(err) != contracts.ErrUncertainOutcome {
			t.Errorf("a redirected commit: %v, want uncertain_outcome (a 307 may be a replay of the POST elsewhere)", err)
		}
		if n := len(other.Requests()); n != 0 {
			t.Errorf("the redirect target received %d requests: %+v", n, other.Requests())
		}
		if n := len(api.Requests()); n != 2 {
			t.Errorf("the endpoint received %d requests, want 2", n)
		}
	})

	t.Run("the document's servers are not where requests go", func(t *testing.T) {
		// fixture/spec.yaml lists a link-local metadata address as its server.
		a := build(t, func(c *Config) { c.Secrets = StaticSecrets{testRef: testToken} })
		if _, err := readItem(ctx, wrapped(t, a), "item_1"); err != nil {
			t.Fatal(err)
		}
		if reqs := h.Requests(); len(reqs) != 1 || reqs[0].Path != "/items/item_1" {
			t.Errorf("requests = %+v, want one to the endpoint's own base URL", reqs)
		}
	})
}

// A credential is resolved from its opaque reference for each request, goes to
// the application in a header and nowhere else: not into an error, not into a
// log record, not into the output of any way of printing an error or the secret.
// Every scenario below reaches the application with the credential (checked),
// then fails in some way that could drag it along, with a hostile application
// that reflects it, a provider that puts it in its own error text and so on.
//
// SEC-083
func TestSecretNotInErrorsOrLogs(t *testing.T) {
	ctx := context.Background()
	reflect := func(status int, ctype string) func(w http.ResponseWriter, r *http.Request) bool {
		return func(w http.ResponseWriter, r *http.Request) bool {
			auth := r.Header.Get("Authorization")
			w.Header().Set("X-Debug", auth)
			writeBody(w, status, ctype, `{"error":"rejected `+auth+`","status":"`+auth+`"}`)
			return true
		}
	}
	type scenario struct {
		name    string
		reaches bool // the application receives the credential
		setup   func(h *fixture.Host, cfg *Config)
		call    func(t *testing.T, ad adapters.AppAdapter, h *fixture.Host) []error
	}
	read := func(t *testing.T, ad adapters.AppAdapter, h *fixture.Host) []error {
		_, err := readItem(ctx, ad, "item_1")
		return []error{err}
	}
	scenarios := []scenario{
		{"a successful read", true, func(h *fixture.Host, cfg *Config) {}, func(t *testing.T, ad adapters.AppAdapter, h *fixture.Host) []error {
			if _, err := readItem(ctx, ad, "item_1"); err != nil {
				t.Errorf("the control read failed: %v", err)
			}
			rep, err := ad.Health(ctx)
			return []error{err, errors.New(rep.Reason)}
		}},
		{"401 reflecting the credential", true, func(h *fixture.Host, cfg *Config) { h.OnRequest(reflect(401, "application/json")) }, read},
		{"500 reflecting the credential", true, func(h *fixture.Host, cfg *Config) { h.OnRequest(reflect(500, "text/html")) }, read},
		{"a violation reflecting the credential", true, func(h *fixture.Host, cfg *Config) { h.OnRequest(reflect(200, "application/json")) }, read},
		{"a commit answered 500", true, func(h *fixture.Host, cfg *Config) { h.OnRequest(reflect(500, "text/plain")) },
			func(t *testing.T, ad adapters.AppAdapter, h *fixture.Host) []error {
				p := preparedFor(fake.ActionCapability, newKey())
				_, err := ad.Commit(ctx, testScope(), adapters.CommitRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: p.IdempotencyKey, Prepared: p})
				return []error{err}
			}},
		{"a timeout", true, func(h *fixture.Host, cfg *Config) { h.Inject(adapters.OpRead, fake.FaultHang) },
			func(t *testing.T, ad adapters.AppAdapter, h *fixture.Host) []error {
				cctx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
				defer cancel()
				_, err := readItem(cctx, ad, "item_1")
				return []error{err}
			}},
		{"a connection that is dropped", true, func(h *fixture.Host, cfg *Config) { h.Inject(adapters.OpRead, fake.FaultDropRequest) }, read},
		{"an egress refusal", false, func(h *fixture.Host, cfg *Config) { cfg.Dialer = nil }, read},
		{"a connection refused", false, func(h *fixture.Host, cfg *Config) { h.SetAvailable(false) }, read},
		{"a provider whose error holds the credential", false, func(h *fixture.Host, cfg *Config) {
			cfg.Secrets = secretFunc(func(context.Context, string) (Secret, error) {
				return "", fmt.Errorf("vault unavailable for %s (token %s)", testRef, testToken)
			})
		}, read},
		{"a credential that cannot be sent", false, func(h *fixture.Host, cfg *Config) {
			cfg.Secrets = StaticSecrets{testRef: testToken + "\r\nX-Injected: 1"}
		}, read},
		{"an empty credential", false, func(h *fixture.Host, cfg *Config) { cfg.Secrets = StaticSecrets{testRef: ""} }, read},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			h := fixture.Start(t, fake.NewApp())
			h.RequireBearer(testToken)
			cfg := fixtureConfig(t, h)
			log, buf := debugLogger()
			cfg.Logger = log
			sc.setup(h, &cfg)
			a := newAdapter(t, cfg)
			errs := sc.call(t, wrapped(t, a), h)

			var transcript strings.Builder
			for _, err := range errs {
				if err == nil {
					continue
				}
				fmt.Fprintf(&transcript, "%v|%+v|%#v|%s|", err, err, err, err.Error())
				var ce *contracts.Error
				if errors.As(err, &ce) {
					b, _ := json.Marshal(ce)
					transcript.Write(b)
				}
				for u := err; u != nil; u = errors.Unwrap(u) {
					transcript.WriteString(u.Error() + "|")
				}
			}
			transcript.WriteString(buf.String())
			for _, secret := range []string{testToken, testRef, "X-Injected"} {
				if strings.Contains(transcript.String(), secret) {
					t.Errorf("%q is in what the adapter returned or logged:\n%s", secret, transcript.String())
				}
			}
			delivered := false
			for _, r := range h.Requests() {
				delivered = delivered || r.Header.Get("Authorization") == "Bearer "+testToken
				if strings.Contains(r.Path+r.Query, testToken) {
					t.Errorf("the credential is in a URL: %s?%s", r.Path, r.Query)
				}
			}
			if delivered != sc.reaches {
				t.Errorf("the application received the credential: %v, want %v (%d requests)", delivered, sc.reaches, len(h.Requests()))
			}
		})
	}

	t.Run("the Secret type prints nothing", func(t *testing.T) {
		s := Secret(testToken)
		out := fmt.Sprint(s, fmt.Sprintf("|%s|%v|%+v|%#v|%q|%x|%d", s, s, s, s, s, s, s))
		b, _ := json.Marshal(map[string]any{"s": s})
		txt, _ := s.MarshalText()
		log, buf := debugLogger()
		log.Info("x", "secret", s)
		all := out + string(b) + string(txt) + buf.String()
		if strings.Contains(all, testToken) || !strings.Contains(all, "[redacted]") {
			t.Errorf("a Secret printed as %s", all)
		}
		if s.Reveal() != testToken {
			t.Error("Reveal does not return the value")
		}
	})
}

// secretFunc adapts a function to a SecretProvider.
type secretFunc func(ctx context.Context, ref string) (Secret, error)

func (f secretFunc) Secret(ctx context.Context, ref string) (Secret, error) { return f(ctx, ref) }

// The idempotency key of a Commit, Status or Cancel is sent in the idempotency
// header (and in the path parameter the document declares for it), the reply is
// held to the key it echoes, and nothing but the protocol can fill the header.
func TestIdempotencyHeaderSent(t *testing.T) {
	ctx := context.Background()
	sc := testScope()

	t.Run("the fixture application, through the adapter as production wires it", func(t *testing.T) {
		app := fake.NewApp()
		h := fixture.Start(t, app)
		h.RequireBearer(testToken)
		ad := wrapped(t, newAdapter(t, fixtureConfig(t, h)))

		po, err := ad.Prepare(ctx, sc, adapters.PrepareRequest{CapabilityID: fake.ActionCapability, Fields: map[string]any{"name": "Widget"}})
		if err != nil || po.Preparation == nil {
			t.Fatalf("prepare: %+v, %v", po, err)
		}
		key := newKey()
		p := preparedFor(fake.ActionCapability, key)
		p.Effects, p.DataVersions = po.Preparation.Effects, po.Preparation.DataVersions
		commit := func() actions.DeterministicResult {
			res, err := ad.Commit(ctx, sc, adapters.CommitRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: key, Prepared: p})
			if err != nil || res.Status != actions.DeterministicResultStatusOk {
				t.Fatalf("commit: %+v, %v", res, err)
			}
			return res
		}
		first, again := commit(), commit()
		if diff := cmp.Diff(first, again); diff != "" || app.Effects() != 1 {
			t.Errorf("two commits with one key: %d effects, diff:\n%s", app.Effects(), diff)
		}
		if _, err := ad.Status(ctx, sc, adapters.StatusRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: key}); err != nil {
			t.Fatal(err)
		}
		if _, err := ad.Cancel(ctx, sc, adapters.CancelRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: key}); err != nil {
			t.Fatal(err)
		}
		if _, err := readItem(ctx, ad, "item_1"); err != nil {
			t.Fatal(err)
		}

		type want struct {
			method, path string
			keyed        bool
		}
		reqs := h.Requests()
		wants := []want{
			{"POST", "/item-drafts/prepare", false},
			{"POST", "/items", true}, {"POST", "/items", true},
			{"GET", "/commits/" + string(key), true},
			{"DELETE", "/commits/" + string(key), true},
			{"GET", "/items/item_1", false},
		}
		if len(reqs) != len(wants) {
			t.Fatalf("%d requests, want %d: %+v", len(reqs), len(wants), reqs)
		}
		for i, w := range wants {
			r := reqs[i]
			got := r.Header.Get(fixture.KeyHdr)
			if r.Method != w.method || r.Path != w.path {
				t.Errorf("request %d is %s %s, want %s %s", i, r.Method, r.Path, w.method, w.path)
			}
			switch {
			case w.keyed && got != string(key):
				t.Errorf("request %d (%s %s) carries the idempotency header %q, want %q", i, r.Method, r.Path, got, key)
			case !w.keyed && got != "":
				t.Errorf("request %d (%s %s) carries an idempotency header: %q", i, r.Method, r.Path, got)
			}
			if r.Header.Get("Authorization") != "Bearer "+testToken {
				t.Errorf("request %d lacks the credential", i)
			}
			if !r.Close {
				t.Errorf("request %d did not ask for the connection to be closed", i)
			}
		}
		var body actions.PreparedAction
		if err := json.Unmarshal(reqs[1].Body, &body); err != nil || body.IdempotencyKey != key || body.PreparationID != p.PreparationID {
			t.Errorf("the commit body is not the prepared action: %v %s", err, reqs[1].Body)
		}
		if ct := reqs[1].Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("commit content type = %q", ct)
		}
	})

	t.Run("the header name is the package's to configure", func(t *testing.T) {
		api := startAPI(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
			w.Header().Set("X-Request-Key", r.Header.Get("X-Request-Key"))
			writeBody(w, 200, "application/json", okResult)
		})
		cfg := miniConfig(t, api, miniSpec("      type: object\n      required: [status]\n      properties:\n        status: {type: string}\n"), miniCapabilities())
		cfg.Endpoints[0].Settings = mustSettings(t, "idempotency:\n  header: X-Request-Key\n")
		a := newAdapter(t, cfg)
		p := preparedFor(fake.ActionCapability, newKey())
		if _, err := a.Commit(ctx, sc, adapters.CommitRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: p.IdempotencyKey, Prepared: p}); err != nil {
			t.Fatal(err)
		}
		r := api.Requests()[0]
		if r.Header.Get("X-Request-Key") != string(p.IdempotencyKey) || r.Header.Get("Idempotency-Key") != "" {
			t.Errorf("headers = %v", r.Header)
		}
	})

	t.Run("a reply that echoes another key answers another request", func(t *testing.T) {
		for name, echo := range map[string][]string{
			"another key":         {"idem_somebody_elses_key_0001"},
			"two different keys":  {"first", "second"},
			"the right key twice": nil,
		} {
			api := startAPI(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				for _, v := range echo {
					w.Header().Add("Idempotency-Key", v)
				}
				if echo == nil {
					w.Header().Add("Idempotency-Key", r.Header.Get("Idempotency-Key"))
					w.Header().Add("Idempotency-Key", r.Header.Get("Idempotency-Key"))
				}
				writeBody(w, 200, "application/json", okResult)
			})
			a := newAdapter(t, miniConfig(t, api, miniSpec("      type: object\n      required: [status]\n      properties:\n        status: {type: string}\n"), miniCapabilities()))
			p := preparedFor(fake.ActionCapability, newKey())
			_, err := a.Commit(ctx, sc, adapters.CommitRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: p.IdempotencyKey, Prepared: p})
			if name == "the right key twice" {
				if err != nil {
					t.Errorf("%s: %v", name, err)
				}
				continue
			}
			if contracts.CodeOf(err) != contracts.ErrUncertainOutcome || !contracts.HasCode(err, contracts.ErrSchemaInvalid) {
				t.Errorf("%s: %v, want uncertain_outcome caused by schema_invalid", name, err)
			}
		}
	})

	t.Run("a field cannot take the place of the key", func(t *testing.T) {
		app := fake.NewApp()
		h := fixture.Start(t, app)
		a := newAdapter(t, fixtureConfig(t, h))
		// A Read with a field of that name has no such parameter, and is refused
		// rather than forwarded.
		_, err := a.Read(ctx, sc, adapters.ReadRequest{CapabilityID: fake.ReadCapability, Fields: map[string]any{"item_id": "x", "idempotency_key": "idem_forged_forged_forged"}})
		if contracts.CodeOf(err) != contracts.ErrValidationFailed || reasonIn(err) != ReasonFieldNotAccepted {
			t.Errorf("a field named idempotency_key: %v, want validation_failed (%s)", err, ReasonFieldNotAccepted)
		}
		for _, r := range h.Requests() {
			if r.Method == "GET" && strings.HasPrefix(r.Path, "/items/") {
				t.Errorf("the forged field reached the application: %+v", r)
			}
		}
	})
}

// A host whose description holds a name that is not short printable text is not
// describable: the whole description is refused, and nothing of it is used.
func TestHostileUnmappedNamesRefuseTheDescription(t *testing.T) {
	for name, claim := range map[string]string{
		"a control character": "evil\nname",
		"an escape sequence":  "evil\x1b[31mname",
		"a very long name":    strings.Repeat("x", 400),
	} {
		h := fixture.Start(t, fake.NewApp(fake.WithUnmapped(claim)))
		h.RequireBearer(testToken)
		a := newAdapter(t, fixtureConfig(t, h))
		d, err := a.Describe(context.Background())
		if contracts.CodeOf(err) != contracts.ErrSchemaInvalid || len(d.Capabilities) != 0 {
			t.Errorf("%s: %+v, %v; want schema_invalid and nothing else", name, d, err)
		}
	}
}
