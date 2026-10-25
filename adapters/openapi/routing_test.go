package openapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// routingSpec is a document with one operation of every shape the adapter
// builds a request for.
const routingSpec = `openapi: 3.0.3
info: {title: routing, version: "1"}
paths:
  /things/{thing_id}/parts:
    post:
      operationId: listParts
      parameters:
        - {name: thing_id, in: path, required: true, schema: {type: string}}
        - {name: limit, in: query, schema: {type: integer, default: 10}}
        - {name: tags, in: query, schema: {type: array, items: {type: string}}}
        - {name: ids, in: query, explode: false, schema: {type: array, items: {type: integer}}}
        - {name: X-Region, in: header, schema: {type: string}}
        - {name: Authorization, in: header, schema: {type: string}}
      requestBody:
        content:
          application/json:
            schema:
              type: object
              additionalProperties: false
              properties:
                note: {type: string}
                entity_type: {type: string}
      responses: &responses
        "200": {$ref: "#/components/responses/Result"}
        "422": {$ref: "#/components/responses/Result"}
  /things/search:
    get:
      operationId: searchThings
      parameters:
        - {name: q, in: query, required: true, schema: {type: string, maxLength: 20}}
        - {name: limit, in: query, schema: {type: integer, maximum: 100}}
      responses: *responses
  /resolve/patient:
    get:
      operationId: resolvePatient
      parameters:
        - {name: mention, in: query, schema: {type: string}}
        - {name: candidate_id, in: query, schema: {type: string}}
      responses: *responses
  /resolve/any:
    post:
      operationId: resolveAny
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              properties:
                entity_type: {type: string}
                mention: {type: string}
      responses: *responses
  /things/{thing_id}/touch:
    post:
      operationId: touchThing
      parameters:
        - {name: thing_id, in: path, required: true, schema: {type: string}}
      requestBody:
        required: false
        content:
          application/json:
            schema: {type: object}
      responses: *responses
components:
  responses:
    Result:
      description: result
      content:
        application/json:
          schema:
            type: object
            required: [status]
            properties:
              status: {type: string}
`

func routingAdapter(t *testing.T) (*Adapter, *testAPI) {
	t.Helper()
	api := startAPI(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeBody(w, 200, "application/json", okResult)
	})
	cfg := miniConfig(t, api, []byte(routingSpec), []Capability{
		{ID: "things.list", Operations: map[adapters.Operation]Binding{
			adapters.OpRead:     {Endpoint: testEP, OperationID: "listParts", Arguments: map[string]string{"region": "X-Region"}},
			adapters.OpValidate: {Endpoint: testEP, OperationID: "touchThing"},
		}},
		{ID: "things.search", Operations: map[adapters.Operation]Binding{adapters.OpRead: {Endpoint: testEP, OperationID: "searchThings"}}},
		{ID: "patients.find", Operations: map[adapters.Operation]Binding{adapters.OpRead: {Endpoint: testEP, OperationID: "searchThings"}},
			Resolve: map[string]Binding{"patient": {Endpoint: testEP, OperationID: "resolvePatient"}, "doctor": {Endpoint: testEP, OperationID: "resolveAny"}}},
	})
	return newAdapter(t, cfg), api
}

func read(a *Adapter, capability string, fields map[string]any) (actions.DeterministicResult, error) {
	return a.Read(context.Background(), testScope(), adapters.ReadRequest{CapabilityID: capability, Fields: fields})
}

// Fields go where the operation declares them: path, query and header
// parameters by name (after the binding's renames), everything else as members of
// the JSON body. Lists are serialised as the parameter's style says, and a value
// that the place cannot carry is refused rather than bent.
func TestFieldRouting(t *testing.T) {
	t.Run("every kind of place", func(t *testing.T) {
		a, api := routingAdapter(t)
		_, err := read(a, "things.list", map[string]any{
			"thing_id": "t 1", "limit": 5, "tags": []any{"a", "b c"}, "ids": []any{1, 2, 3}, "region": "eu", "note": "hello",
		})
		if err != nil {
			t.Fatal(err)
		}
		r := api.Requests()[0]
		if r.Method != "POST" || r.Path != "/things/t%201/parts" {
			t.Errorf("request = %s %s", r.Method, r.Path)
		}
		if want := "ids=1%2C2%2C3&limit=5&tags=a&tags=b+c"; r.Query != want {
			t.Errorf("query = %q, want %q", r.Query, want)
		}
		if r.Header.Get("X-Region") != "eu" {
			t.Errorf("X-Region = %q", r.Header.Get("X-Region"))
		}
		if string(r.Body) != `{"note":"hello"}` || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("body = %s (%s)", r.Body, r.Header.Get("Content-Type"))
		}
		if r.Header.Get("Accept") != "application/json" || r.Header.Get("User-Agent") != "alixai" {
			t.Errorf("headers = %v", r.Header)
		}
	})

	t.Run("defaults of the document are not added", func(t *testing.T) {
		a, api := routingAdapter(t)
		if _, err := read(a, "things.list", map[string]any{"thing_id": "t1"}); err != nil {
			t.Fatal(err)
		}
		r := api.Requests()[0]
		if r.Query != "" || string(r.Body) != "" {
			t.Errorf("request = %s?%s %q: the validator filled in a default, so what was checked is not what was sent", r.Path, r.Query, r.Body)
		}
	})

	t.Run("a get has no body, and a leftover field has no place", func(t *testing.T) {
		a, api := routingAdapter(t)
		if _, err := read(a, "things.search", map[string]any{"q": "rossi", "limit": 3}); err != nil {
			t.Fatal(err)
		}
		if r := api.Requests()[0]; r.Method != "GET" || r.Query != "limit=3&q=rossi" || len(r.Body) != 0 || r.Header.Get("Content-Type") != "" {
			t.Errorf("request = %+v", r)
		}
		_, err := read(a, "things.search", map[string]any{"q": "rossi", "colour": "red"})
		if contracts.CodeOf(err) != contracts.ErrValidationFailed || reasonIn(err) != ReasonFieldNotAccepted {
			t.Errorf("a field with no place: %v, want validation_failed (%s)", err, ReasonFieldNotAccepted)
		}
		if len(api.Requests()) != 1 {
			t.Error("a request with a field that has no place was sent")
		}
	})

	t.Run("what the document refuses is not sent, and the error names no value", func(t *testing.T) {
		a, api := routingAdapter(t)
		for name, fields := range map[string]map[string]any{
			"a missing required parameter": {"limit": 3},
			"a value of the wrong type":    {"q": "x", "limit": "many-secret-words"},
			"a value past a maximum":       {"q": "x", "limit": 1000},
			"a value that is too long":     {"q": "this-query-is-much-longer-than-twenty"},
		} {
			_, err := read(a, "things.search", fields)
			if contracts.CodeOf(err) != contracts.ErrValidationFailed || reasonIn(err) != ReasonRequestViolatesSpec {
				t.Errorf("%s: %v, want validation_failed (%s)", name, err, ReasonRequestViolatesSpec)
			}
			for _, leak := range []string{"many-secret-words", "much-longer", "1000"} {
				if err != nil && strings.Contains(err.Error(), leak) {
					t.Errorf("%s: the error quotes the value: %v", name, err)
				}
			}
		}
		if n := len(api.Requests()); n != 0 {
			t.Errorf("%d requests were sent", n)
		}
		_, err := read(a, "things.search", map[string]any{"limit": 3})
		var ce *contracts.Error
		if !asContractsError(err, &ce) || ce.Details["location"] != "parameter q (query)" {
			t.Errorf("error = %v: want the location of the violation", err)
		}
	})

	t.Run("a body the document forbids extra members of", func(t *testing.T) {
		a, api := routingAdapter(t)
		_, err := read(a, "things.list", map[string]any{"thing_id": "t1", "colour": "red"})
		if contracts.CodeOf(err) != contracts.ErrValidationFailed || reasonIn(err) != ReasonRequestViolatesSpec {
			t.Errorf("error = %v, want validation_failed (%s)", err, ReasonRequestViolatesSpec)
		}
		if len(api.Requests()) != 0 {
			t.Error("a request the document forbids was sent")
		}
	})

	t.Run("no field fills a header the protocol owns", func(t *testing.T) {
		a, api := routingAdapter(t)
		// Authorization is declared as a header parameter of listParts. A field of
		// that name does not become the header: it is a body member, which the
		// document does not allow.
		_, err := read(a, "things.list", map[string]any{"thing_id": "t1", "Authorization": "Bearer forged"})
		if contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("error = %v, want validation_failed", err)
		}
		for _, r := range api.Requests() {
			if r.Header.Get("Authorization") != "" {
				t.Errorf("a field became the Authorization header: %+v", r.Header)
			}
		}
	})

	t.Run("values the place cannot carry", func(t *testing.T) {
		a, api := routingAdapter(t)
		for name, fields := range map[string]map[string]any{
			"a list in a path":                   {"thing_id": []any{"a", "b"}},
			"a comma in an item of a plain list": {"thing_id": "t", "ids": []any{"1,2"}},
			"a line break in a header":           {"thing_id": "t", "region": "eu\r\nX-Injected: 1"},
			"a separator in a path value":        {"thing_id": "a/b"},
		} {
			_, err := read(a, "things.list", fields)
			if contracts.CodeOf(err) != contracts.ErrValidationFailed || reasonIn(err) != ReasonValueNotEncodable {
				t.Errorf("%s: %v, want validation_failed (%s)", name, err, ReasonValueNotEncodable)
			}
		}
		if len(api.Requests()) != 0 {
			t.Error("a request with a value that cannot be carried was sent")
		}
	})

	t.Run("two fields cannot end up in one place", func(t *testing.T) {
		a, _ := routingAdapter(t)
		_, err := read(a, "things.list", map[string]any{"thing_id": "t", "region": "eu", "X-Region": "us"})
		if contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("error = %v, want validation_failed", err)
		}
	})

	t.Run("an optional body is sent only when there is something to put in it", func(t *testing.T) {
		a, api := routingAdapter(t)
		call := func(fields map[string]any) {
			if _, err := a.Validate(context.Background(), testScope(), adapters.ValidateRequest{CapabilityID: "things.list", Fields: fields}); err != nil {
				t.Fatal(err)
			}
		}
		call(map[string]any{"thing_id": "t1"})
		call(map[string]any{"thing_id": "t1", "anything": 1})
		reqs := api.Requests()
		if len(reqs[0].Body) != 0 || reqs[0].Header.Get("Content-Type") != "" {
			t.Errorf("an empty optional body was sent: %+v", reqs[0])
		}
		if string(reqs[1].Body) != `{"anything":1}` {
			t.Errorf("body = %s", reqs[1].Body)
		}
	})
}

func asContractsError(err error, target **contracts.Error) bool {
	for err != nil {
		if ce, ok := err.(*contracts.Error); ok {
			*target = ce
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// A Read whose fields carry the reserved entity_type is entity resolution: it goes
// to the resolver the capability declares for that type and to no other, and the
// reserved argument itself travels only where the operation declares a place for
// it (a resolver of one type needs no entity_type; a shared resolver does).
func TestEntityResolutionRouting(t *testing.T) {
	a, api := routingAdapter(t)
	if _, err := read(a, "patients.find", map[string]any{"entity_type": "patient", "mention": "Rossi", "candidate_id": "pat_7"}); err != nil {
		t.Fatal(err)
	}
	if _, err := read(a, "patients.find", map[string]any{"entity_type": "doctor", "mention": "Bianchi"}); err != nil {
		t.Fatal(err)
	}
	// Without the reserved argument the capability's own read is called.
	if _, err := read(a, "patients.find", map[string]any{"q": "Verdi"}); err != nil {
		t.Fatal(err)
	}
	reqs := api.Requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests: %+v", len(reqs), reqs)
	}
	if reqs[0].Method != "GET" || reqs[0].Path != "/resolve/patient" || reqs[0].Query != "candidate_id=pat_7&mention=Rossi" {
		t.Errorf("the patient resolver was called as %s %s?%s", reqs[0].Method, reqs[0].Path, reqs[0].Query)
	}
	var body map[string]any
	if reqs[1].Method != "POST" || reqs[1].Path != "/resolve/any" || json.Unmarshal(reqs[1].Body, &body) != nil ||
		cmp.Diff(map[string]any{"entity_type": "doctor", "mention": "Bianchi"}, body) != "" {
		t.Errorf("the shared resolver was called as %s %s %s", reqs[1].Method, reqs[1].Path, reqs[1].Body)
	}
	if reqs[2].Path != "/things/search" {
		t.Errorf("the capability's own read was called as %s", reqs[2].Path)
	}
	for _, typ := range []string{"nurse", "", "Patient"} {
		before := len(api.Requests())
		_, err := read(a, "patients.find", map[string]any{"entity_type": typ, "mention": "x"})
		if contracts.CodeOf(err) != contracts.ErrValidationFailed || reasonIn(err) != ReasonUnknownEntityType || len(api.Requests()) != before {
			t.Errorf("entity type %q: %v (%d requests sent)", typ, err, len(api.Requests())-before)
		}
	}
	// A capability that declares no resolver cannot be asked to resolve.
	if _, err := read(a, "things.search", map[string]any{"entity_type": "patient", "mention": "x"}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("a capability with no resolvers: %v", err)
	}
}

// A commit carries the prepared action as the body; a prepared action over the
// limit is not sent.
func TestCommitBody(t *testing.T) {
	api := startAPI(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeBody(w, 200, "application/json", okResult)
	})
	a := newAdapter(t, miniConfig(t, api, miniSpec("      type: object\n      required: [status]\n      properties:\n        status: {type: string}\n"), miniCapabilities()))
	p := preparedFor(fake.ActionCapability, newKey())
	if _, err := a.Commit(context.Background(), testScope(), adapters.CommitRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: p.IdempotencyKey, Prepared: p}); err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(p)
	if got := api.Requests()[0].Body; string(got) != string(want) {
		t.Errorf("body = %s\nwant   %s", got, want)
	}
	p.Summary = strings.Repeat("x", maxRequestBytes+1)
	_, err := a.Commit(context.Background(), testScope(), adapters.CommitRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: p.IdempotencyKey, Prepared: p})
	if contracts.CodeOf(err) != contracts.ErrValidationFailed || reasonIn(err) != ReasonRequestTooLarge || len(api.Requests()) != 1 {
		t.Errorf("an oversize prepared action: %v (%d requests)", err, len(api.Requests()))
	}
}

// A document that declares the credential's header as a parameter is satisfied
// by a placeholder while the request is checked, and the real credential is
// added only on the way out.
func TestDeclaredAuthorizationHeader(t *testing.T) {
	spec := `openapi: 3.0.3
info: {title: auth, version: "1"}
components:
  securitySchemes:
    bearer: {type: http, scheme: bearer}
security:
  - bearer: []
paths:
  /items/{id}:
    get:
      operationId: getItem
      parameters:
        - {name: id, in: path, required: true, schema: {type: string}}
        - {name: Authorization, in: header, required: true, schema: {type: string, pattern: "^Bearer .+"}}
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema: {type: object, required: [status], properties: {status: {type: string}}}
`
	api := startAPI(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeBody(w, 200, "application/json", okResult)
	})
	cfg := miniConfig(t, api, []byte(spec), miniCapabilities()[:1])
	cfg.Endpoints[0].CredentialRef = testRef
	cfg.Endpoints[0].Settings = mustSettings(t, "auth:\n  scheme: Bearer\n")
	cfg.Secrets = StaticSecrets{testRef: testToken}
	a := newAdapter(t, cfg)
	if _, err := read(a, fake.ReadCapability, map[string]any{"item_id": "x"}); err != nil {
		t.Fatal(err)
	}
	if got := api.Requests()[0].Header.Get("Authorization"); got != "Bearer "+testToken {
		t.Errorf("Authorization = %q", got)
	}
	// A credential in the other style: an API-key header, sent as it is. The
	// parameter's pattern would reject this key, but the validator only ever sees
	// the placeholder, which has the shape the scheme gives.
	keySpec := strings.Replace(spec, `{name: Authorization, in: header, required: true, schema: {type: string, pattern: "^Bearer .+"}}`,
		`{name: X-Api-Key, in: header, required: true, schema: {type: string, pattern: "^[a-z]+$"}}`, 1)
	cfg2 := miniConfig(t, api, []byte(keySpec), miniCapabilities()[:1])
	cfg2.Endpoints[0].CredentialRef = testRef
	cfg2.Endpoints[0].Settings = mustSettings(t, "auth:\n  header: X-Api-Key\n  scheme: \"\"\n")
	cfg2.Secrets = StaticSecrets{testRef: "plain-key-123"}
	a2 := newAdapter(t, cfg2)
	if _, err := read(a2, fake.ReadCapability, map[string]any{"item_id": "x"}); err != nil {
		t.Fatal(err)
	}
	reqs := api.Requests()
	if got := reqs[len(reqs)-1].Header.Get("X-Api-Key"); got != "plain-key-123" {
		t.Errorf("X-Api-Key = %q", got)
	}
	if got := reqs[len(reqs)-1].Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q: no credential belongs there", got)
	}
}
