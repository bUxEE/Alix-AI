package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// errorSchema compiles the published schemas/common/error from the repository
// (read at test time, so the envelope is checked against the real contract).
func errorSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	path := filepath.Join("..", "..", "schemas", "common", "error.schema.json")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	doc, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	if err := c.AddResource("urn:alixai:schema:common:error:v1", doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("urn:alixai:schema:common:error:v1")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// errorServer is a gateway with one route per scenario, each failing with the
// error the test names.
func errorServer(t *testing.T, scenarios map[string]func(w http.ResponseWriter, r *http.Request)) *harness {
	t.Helper()
	return newHarness(t, func(o *Options) {
		o.ExtraAreas = map[string]AreaFunc{"errtest": func(rt *Router, _ *Deps) {
			for name, f := range scenarios {
				rt.HandleFunc("GET /err/"+name, f)
			}
		}}
	})
}

// SEC-083: error envelopes carry no credential, whatever the failure.
func TestErrorEnvelope(t *testing.T) {
	schema := errorSchema(t)
	const secret = "hunter2-CANARY"
	leaky := errors.New("dial https://svc:" + secret + "@runtime.local failed: Authorization: Bearer " + secret + " token=" + secret)

	type scenario struct {
		err        error
		status     int
		code       contracts.ErrCode
		retryable  bool
		message    string // exact, when set
		mustNotSay []string
	}
	cases := map[string]scenario{
		"typed validation":     {err: contracts.New(contracts.ErrValidationFailed, "count must be positive"), status: 400, code: "validation_failed", message: "count must be positive"},
		"typed unauth":         {err: contracts.New(contracts.ErrUnauthenticated, "no credential"), status: 401, code: "unauthenticated"},
		"typed forbidden":      {err: contracts.New(contracts.ErrPermissionDenied, "no"), status: 403, code: "permission_denied"},
		"typed scope":          {err: contracts.New(contracts.ErrScopeViolation, "no"), status: 403, code: "scope_violation"},
		"typed schema invalid": {err: contracts.New(contracts.ErrSchemaInvalid, "bad"), status: 422, code: "schema_invalid"},
		"typed conflict":       {err: contracts.New(contracts.ErrConflict, "dup"), status: 409, code: "conflict"},
		"typed confirmation":   {err: contracts.New(contracts.ErrConfirmationRequired, "confirm"), status: 428, code: "confirmation_required"},
		"typed rate limited":   {err: contracts.New(contracts.ErrRateLimited, "slow down"), status: 429, code: "rate_limited", retryable: true},
		"typed unavailable":    {err: contracts.New(contracts.ErrUnavailable, "down"), status: 503, code: "unavailable", retryable: true},
		"typed exhausted":      {err: contracts.New(contracts.ErrResourceExhausted, "full"), status: 503, code: "resource_exhausted", retryable: true},
		"typed timeout":        {err: contracts.New(contracts.ErrTimeout, "slow"), status: 504, code: "timeout", retryable: true},
		"typed uncertain":      {err: contracts.New(contracts.ErrUncertainOutcome, "unknown"), status: 500, code: "uncertain_outcome"},
		"wrapped typed":        {err: fmt.Errorf("lookup: %w", contracts.New(contracts.ErrUnavailable, "store busy")), status: 503, code: "unavailable", retryable: true, message: "store busy"},

		// A caller cancel is a plain error that CodeOf reports as internal. The
		// envelope must not call it a fault of ours.
		"caller cancel":         {err: fmt.Errorf("providers: canceled: %w", context.Canceled), status: 503, code: "unavailable", retryable: true, message: "request canceled"},
		"bare cancel":           {err: context.Canceled, status: 503, code: "unavailable", retryable: true, message: "request canceled"},
		"plain deadline":        {err: fmt.Errorf("read: %w", context.DeadlineExceeded), status: 504, code: "timeout", retryable: true, message: "request timed out"},
		"oversize body":         {err: &http.MaxBytesError{Limit: 1024}, status: 413, code: "validation_failed", message: "request body too large"},
		"typed deadline":        {err: contracts.Wrap(contracts.ErrTimeout, context.DeadlineExceeded, "deadline"), status: 504, code: "timeout", retryable: true},
		"nil error":             {err: nil, status: 500, code: "internal", message: "internal error"},
		"untyped fault":         {err: errors.New("sqlite: disk I/O error at /var/lib/alixai/alixai.db"), status: 500, code: "internal", message: "internal error", mustNotSay: []string{"sqlite", "/var/lib"}},
		"typed internal":        {err: contracts.New(contracts.ErrInternal, "panic at gateway.go:42 in handler").WithDetails("stack", "frames"), status: 500, code: "internal", message: "internal error", mustNotSay: []string{"gateway.go", "frames"}},
		"secret in cause":       {err: contracts.Wrap(contracts.ErrUnavailable, leaky, "runtime unreachable"), status: 503, code: "unavailable", retryable: true, message: "runtime unreachable", mustNotSay: []string{secret}},
		"secret in message":     {err: contracts.New(contracts.ErrValidationFailed, "bad header Authorization: Bearer "+secret), status: 400, code: "validation_failed", mustNotSay: []string{secret}},
		"secret in untyped":     {err: leaky, status: 500, code: "internal", message: "internal error", mustNotSay: []string{secret, "runtime.local"}},
		"secret in details":     {err: contracts.New(contracts.ErrUnavailable, "x").WithDetails("api_key", secret).WithDetails("note", "token="+secret).WithDetails("nested", map[string]any{"k": secret}).WithDetails("upstream_status", 503), status: 503, code: "unavailable", retryable: true, mustNotSay: []string{secret}},
		"joined with a typed":   {err: errors.Join(errors.New("noise"), contracts.New(contracts.ErrQuarantined, "package is quarantined")), status: 403, code: "quarantined", message: "package is quarantined"},
		"typed wrapping cancel": {err: contracts.Wrap(contracts.ErrUnavailable, context.Canceled, "upstream gone"), status: 503, code: "unavailable", retryable: true, message: "upstream gone"},
	}

	scenarios := map[string]func(http.ResponseWriter, *http.Request){}
	names := map[string]string{}
	i := 0
	for name, sc := range cases {
		route := fmt.Sprintf("c%d", i)
		i++
		names[name] = route
		sc := sc
		scenarios[route] = func(w http.ResponseWriter, r *http.Request) { WriteError(w, r, sc.err) }
	}
	h := errorServer(t, scenarios)

	for name, sc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := h.do(http.MethodGet, "/err/"+names[name], nil)
			if rec.Code != sc.status {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, sc.status, rec.Body.String())
			}
			env := mustEnvelope(t, rec)
			if string(sc.code) != env.Code || env.Retryable != sc.retryable {
				t.Errorf("code = %q retryable = %v, want %q %v", env.Code, env.Retryable, sc.code, sc.retryable)
			}
			if sc.message != "" && env.Message != sc.message {
				t.Errorf("message = %q, want %q", env.Message, sc.message)
			}
			if got := contracts.ErrCode(env.Code).Retryable(); got != env.Retryable {
				t.Errorf("retryable = %v contradicts the code table (%v)", env.Retryable, got)
			}
			if sc.retryable && rec.Header().Get("Retry-After") == "" {
				t.Error("a retryable error has no Retry-After")
			}
			if sc.status == 401 && rec.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Errorf("WWW-Authenticate = %q on a 401", rec.Header().Get("WWW-Authenticate"))
			}
			for _, bad := range append(sc.mustNotSay, secret) {
				if strings.Contains(rec.Body.String(), bad) {
					t.Errorf("the body contains %q: %s", bad, rec.Body.String())
				}
			}
			// The body is exactly the published error schema.
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(rec.Body.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(doc); err != nil {
				t.Errorf("body does not satisfy schemas/common/error: %v\n%s", err, rec.Body.String())
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q", cc)
			}
		})
	}

	// The server log keeps the detail the client was denied, redacted.
	logs := h.logs.String()
	if !strings.Contains(logs, "disk I/O error") {
		t.Errorf("the log lost the real cause of an internal fault:\n%s", logs)
	}
	if strings.Contains(logs, secret) {
		t.Errorf("a secret reached the log:\n%s", logs)
	}
}

func TestWriteErrorStatusOverridesTheDefault(t *testing.T) {
	h := errorServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"x": func(w http.ResponseWriter, r *http.Request) {
			WriteErrorStatus(w, r, http.StatusBadGateway, contracts.New(contracts.ErrSchemaInvalid, "upstream sent junk"))
		},
	})
	rec := h.do(http.MethodGet, "/err/x", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if env := mustEnvelope(t, rec); env.Code != "schema_invalid" {
		t.Errorf("code = %q, want schema_invalid", env.Code)
	}
}

// A canceled caller is logged as a cancel, not as a server failure.
func TestCallerCancelIsNotLoggedAsFailure(t *testing.T) {
	h := errorServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"x": func(w http.ResponseWriter, r *http.Request) {
			WriteError(w, r, fmt.Errorf("providers: canceled: %w", context.Canceled))
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/err/x", nil).WithContext(ctx)
	h.srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
	logs := h.logs.String()
	if !strings.Contains(logs, "request canceled by the caller") {
		t.Errorf("the cancel was not recognised in the log:\n%s", logs)
	}
	if strings.Contains(logs, `"level":"ERROR"`) {
		t.Errorf("a caller cancel was logged as an error:\n%s", logs)
	}
}

func TestPanicIsAGenericEnvelope(t *testing.T) {
	h := errorServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"x": func(http.ResponseWriter, *http.Request) { panic("boom: password=PANICSECRET") },
	})
	rec := h.do(http.MethodGet, "/err/x", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	env := mustEnvelope(t, rec)
	if env.Code != "internal" || env.Message != "internal error" {
		t.Errorf("envelope = %+v", env)
	}
	if strings.Contains(rec.Body.String(), "boom") || strings.Contains(h.logs.String(), "PANICSECRET") {
		t.Error("the panic value leaked into the response or the log unredacted")
	}
	if !strings.Contains(h.logs.String(), "handler panic") {
		t.Error("the panic was not logged")
	}
	// The server keeps serving.
	if rec := h.do(http.MethodGet, "/healthz", nil); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz after a panic = %d", rec.Code)
	}
}

func TestUnknownRoutesAnswerWithTheEnvelope(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/", "/nope", "/v1/sessions", "/v0/dev/propose", "/admin", "/api/models/apply", "/healthz/extra"} {
		rec := h.do(http.MethodGet, path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
			continue
		}
		if env := mustEnvelope(t, rec); env.Code != "validation_failed" || env.Message != "no such route" {
			t.Errorf("GET %s envelope = %+v", path, env)
		}
	}
}

// SEC-092: no response ever carries a CORS header, preflight included.
func TestNoCORSHeaders(t *testing.T) {
	h := newHarness(t, withSpike())
	for _, tc := range []struct{ method, path string }{
		{"GET", "/healthz"}, {"GET", "/readyz"}, {"GET", "/nope"}, {"OPTIONS", "/healthz"},
		{"OPTIONS", "/v0/dev/propose"}, {"POST", "/v0/dev/propose"},
	} {
		rec := h.doRaw(tc.method, tc.path, "{}", "application/json", "Origin", "https://evil.example", "Access-Control-Request-Method", "POST")
		for k := range rec.Header() {
			if strings.HasPrefix(strings.ToLower(k), "access-control-") {
				t.Errorf("%s %s sent %s", tc.method, tc.path, k)
			}
		}
	}
}

func TestRequestIDFlowsIntoEnvelope(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodGet, "/nope", nil, HeaderRequestID, "caller-supplied")
	env := mustEnvelope(t, rec)
	if got := env.Details["request_id"]; got == "caller-supplied" {
		t.Error("the caller's request ID was echoed")
	}
	var raw map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	if _, ok := raw["request_id"]; ok {
		t.Error("request_id leaked outside details")
	}
}
