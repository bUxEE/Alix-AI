// Package fixture is a small, scriptable HTTP application for the tests of
// adapters/openapi. It puts internal/adapters/fake.App behind an HTTP API that
// spec.yaml describes with OpenAPI, the way a real host puts its service behind
// REST:
//
//	GET    /items/{id}                  items.get   read
//	POST   /item-drafts/validate        items.create validate   {"name": ...}
//	POST   /item-drafts/prepare         items.create prepare    {"name": ...}
//	POST   /items                       items.create commit     body: the PreparedAction, header Idempotency-Key
//	GET    /commits/{idempotency_key}   items.create status
//	DELETE /commits/{idempotency_key}   items.create cancel
//	GET    /describe, /health, /inspect the management operations
//	GET    /admin/export                in the document, mapped by nobody: it must never be called
//	POST   /tenants, DELETE /tenants/{tenant_id}   the provisioning operations
//
// Every call carries the identity as base64url JSON in X-AlixAI-Identity and,
// when the host requires one, a bearer credential. A DeterministicResult travels
// as the JSON body with the HTTP status a REST host would pick for it (200 for a
// success, 403 for a denial, 404 for the status of a key it never saw, 409 for a
// conflict or a stale preparation, 422 for a validation failure, 503 for a
// dependency that is down); the key a Commit, Status or Cancel carries is echoed
// in the Idempotency-Key header of the reply.
//
// The application behind it keeps the contract of task M1-12: Validate, on
// unchanged data, reports in freshness.version the version Prepare reported,
// which is the snapshot version of the records the action relies on (here, the
// version of the item collection of the tenant and workspace), not a
// per-response ETag and not a counter that moves with every response. A host
// that broke it would make every commit stale; the adapter passes the version
// through and the integration audit's probes check it.
//
// Nothing here is production code.
package fixture

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// Spec is the OpenAPI document of the fixture application.
//
//go:embed spec.yaml
var Spec []byte

// AdapterConfig is the adapter configuration file of the fixture endpoint.
//
//go:embed adapter.yaml
var AdapterConfig []byte

// The operationIds of the document.
const (
	OpGet       = "getItem"
	OpValidate  = "validateItem"
	OpPrepare   = "prepareItem"
	OpCommit    = "commitItem"
	OpStatus    = "statusItem"
	OpCancel    = "cancelItem"
	OpDescribe  = "describeApplication"
	OpHealth    = "getHealth"
	OpInspect   = "inspectApplication"
	OpExport    = "exportEverything"
	OpCreate    = "createTenant"
	OpCleanup   = "cleanupTenant"
	IdentityHdr = "X-AlixAI-Identity"
	KeyHdr      = "Idempotency-Key"
)

// Request is one HTTP request the application received.
type Request struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   []byte
	// Close is true when the client asked for the connection to be closed.
	Close bool
}

// Host is the application: fake.App behind HTTP on a loopback port. It
// implements the conformance suite's Fixture (SetAvailable closes the listener,
// so a connection is refused before anything is sent) and can be brought back on
// the same port.
type Host struct {
	*fake.App
	// Addr is host:port; URL is the base URL of the API.
	Addr string
	URL  string

	mu       sync.Mutex
	srv      *http.Server
	down     bool
	log      []Request
	override func(w http.ResponseWriter, r *http.Request) bool
	bearer   string
	exports  atomic.Int64
	tenants  map[string]bool
	t        testing.TB
}

// Start serves app on 127.0.0.1 and stops the server when the test ends.
func Start(t testing.TB, app *fake.App) *Host {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := &Host{App: app, Addr: ln.Addr().String(), tenants: map[string]bool{}, t: t}
	h.URL = "http://" + h.Addr
	h.serve(ln)
	t.Cleanup(h.stop)
	return h
}

func (h *Host) serve(ln net.Listener) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", h.wrap(func(w http.ResponseWriter, r *http.Request, body []byte) {
		h.call(w, r, adapters.OpRead, fake.ReadCapability, map[string]any{"item_id": r.PathValue("id")}, "", nil, nil)
	}))
	mux.HandleFunc("POST /item-drafts/validate", h.wrap(func(w http.ResponseWriter, r *http.Request, body []byte) {
		h.call(w, r, adapters.OpValidate, fake.ActionCapability, jsonObject(body), "", nil, nil)
	}))
	mux.HandleFunc("POST /item-drafts/prepare", h.wrap(func(w http.ResponseWriter, r *http.Request, body []byte) {
		h.call(w, r, adapters.OpPrepare, fake.ActionCapability, jsonObject(body), "", nil, nil)
	}))
	mux.HandleFunc("POST /items", h.wrap(func(w http.ResponseWriter, r *http.Request, body []byte) {
		var p actions.PreparedAction
		_ = json.Unmarshal(body, &p)
		h.call(w, r, adapters.OpCommit, fake.ActionCapability, nil, r.Header.Get(KeyHdr), &p, nil)
	}))
	mux.HandleFunc("GET /commits/{idempotency_key}", h.wrap(func(w http.ResponseWriter, r *http.Request, body []byte) {
		h.call(w, r, adapters.OpStatus, fake.ActionCapability, nil, r.PathValue("idempotency_key"), nil, nil)
	}))
	mux.HandleFunc("DELETE /commits/{idempotency_key}", h.wrap(func(w http.ResponseWriter, r *http.Request, body []byte) {
		h.call(w, r, adapters.OpCancel, fake.ActionCapability, nil, r.PathValue("idempotency_key"), nil, nil)
	}))
	mux.HandleFunc("GET /describe", h.wrap(func(w http.ResponseWriter, r *http.Request, body []byte) {
		h.call(w, r, adapters.OpDescribe, "", nil, "", nil, nil)
	}))
	mux.HandleFunc("GET /health", h.wrap(func(w http.ResponseWriter, r *http.Request, body []byte) {
		h.call(w, r, adapters.OpHealth, "", nil, "", nil, nil)
	}))
	mux.HandleFunc("GET /inspect", h.wrap(func(w http.ResponseWriter, r *http.Request, body []byte) {
		var aspects []adapters.Aspect
		for _, a := range r.URL.Query()["aspects"] {
			aspects = append(aspects, adapters.Aspect(a))
		}
		h.call(w, r, adapters.OpInspect, "", nil, "", nil, aspects)
	}))
	mux.HandleFunc("GET /admin/export", h.wrap(func(w http.ResponseWriter, r *http.Request, body []byte) {
		h.exports.Add(1)
		writeJSON(w, 200, []byte(`{"status":"ok","everything":true}`), "")
	}))
	mux.HandleFunc("POST /tenants", h.wrap(func(w http.ResponseWriter, r *http.Request, body []byte) {
		obj := jsonObject(body)
		id, _ := obj["tenant_id"].(string)
		h.mu.Lock()
		h.tenants[id] = true
		h.mu.Unlock()
		writeJSON(w, 201, []byte(`{"tenant_id":"`+id+`","created":true}`), "")
	}))
	mux.HandleFunc("DELETE /tenants/{tenant_id}", h.wrap(func(w http.ResponseWriter, r *http.Request, body []byte) {
		id := r.PathValue("tenant_id")
		h.mu.Lock()
		existed := h.tenants[id]
		delete(h.tenants, id)
		h.mu.Unlock()
		if !existed {
			writeJSON(w, 404, []byte(`{"existed":false,"remaining":{}}`), "")
			return
		}
		writeJSON(w, 200, []byte(`{"existed":true,"remaining":{}}`), "")
	}))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	h.mu.Lock()
	h.srv, h.down = srv, false
	h.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
}

func (h *Host) stop() {
	h.mu.Lock()
	srv := h.srv
	h.srv, h.down = nil, true
	h.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}

// SetAvailable takes the host down (the listener and every connection are
// closed, so a new connection is refused before anything is sent) or brings it
// back on the same port.
func (h *Host) SetAvailable(up bool) {
	h.App.SetAvailable(up)
	if !up {
		h.stop()
		return
	}
	h.mu.Lock()
	down := h.down
	h.mu.Unlock()
	if !down {
		return
	}
	var ln net.Listener
	var err error
	for i := 0; i < 100; i++ {
		if ln, err = net.Listen("tcp", h.Addr); err == nil {
			h.serve(ln)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("cannot listen on %s again: %v", h.Addr, err)
}

// RequireBearer makes the host answer 401 to any request without
// "Authorization: Bearer <token>". "" removes the requirement.
func (h *Host) RequireBearer(token string) {
	h.mu.Lock()
	h.bearer = token
	h.mu.Unlock()
}

// OnRequest installs a function that sees every request before the application
// does; when it returns true it has answered the request itself.
func (h *Host) OnRequest(f func(w http.ResponseWriter, r *http.Request) bool) {
	h.mu.Lock()
	h.override = f
	h.mu.Unlock()
}

// Requests returns every request received, oldest first.
func (h *Host) Requests() []Request {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Request(nil), h.log...)
}

// ExportHits counts the calls that reached /admin/export, which no capability
// maps.
func (h *Host) ExportHits() int { return int(h.exports.Load()) }

// wrap records the request, applies the credential requirement and the
// override, and hands the body to the handler.
func (h *Host) wrap(f func(w http.ResponseWriter, r *http.Request, body []byte)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		h.mu.Lock()
		h.log = append(h.log, Request{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: body, Close: r.Close})
		bearer, override := h.bearer, h.override
		h.mu.Unlock()
		if override != nil && override(w, r) {
			return
		}
		if bearer != "" && r.Header.Get("Authorization") != "Bearer "+bearer {
			writeJSON(w, 401, []byte(`{"error":"unauthorized"}`), "")
			return
		}
		f(w, r, body)
	}
}

// call runs one operation on the application and writes what it answers.
func (h *Host) call(w http.ResponseWriter, r *http.Request, op adapters.Operation, capability string, fields map[string]any,
	key string, prepared *actions.PreparedAction, aspects []adapters.Aspect) {
	var id adapters.Identity
	if raw := r.Header.Get(IdentityHdr); raw != "" {
		if b, err := base64.RawURLEncoding.DecodeString(raw); err == nil {
			_ = json.Unmarshal(b, &id)
		}
	}
	if op == adapters.OpStatus || op == adapters.OpCancel || op == adapters.OpCommit {
		if hk := r.Header.Get(KeyHdr); hk != key {
			writeJSON(w, 400, []byte(`{"error":"the idempotency key of the header and of the call differ"}`), "")
			return
		}
	}
	reply := h.App.Handle(r.Context(), fake.Request{
		Operation: op, Identity: id, CapabilityID: capability, Fields: fields, Prepared: prepared, IdempotencyKey: key, Aspects: aspects,
	})
	switch reply.Action {
	case fake.Hang:
		<-r.Context().Done()
		return
	case fake.Drop, fake.Refuse:
		drop(w)
		return
	}
	writeJSON(w, statusFor(op, reply.Body), reply.Body, reply.EchoedKey)
}

// drop closes the connection without a reply.
func drop(w http.ResponseWriter) {
	if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
		_ = conn.Close()
	}
}

// statusFor picks the HTTP status a REST host would give a DeterministicResult.
func statusFor(op adapters.Operation, body []byte) int {
	var res struct {
		Status string `json:"status"`
		Errors []struct {
			Code  string `json:"code"`
			Field string `json:"field"`
		} `json:"errors"`
	}
	if json.Unmarshal(bytes.TrimRight(body, " "), &res) != nil {
		return 200
	}
	switch res.Status {
	case "unavailable":
		return 503
	case "rejected":
		if len(res.Errors) == 0 {
			return 422
		}
		switch e := res.Errors[0]; {
		case e.Code == "permission_denied":
			return 403
		case e.Code == "conflict" || e.Code == "stale":
			return 409
		case e.Code == "validation_failed" && e.Field == "idempotency_key":
			return 404
		}
		return 422
	}
	return 200
}

func writeJSON(w http.ResponseWriter, status int, body []byte, echoedKey string) {
	w.Header().Set("Content-Type", "application/json")
	if echoedKey != "" {
		w.Header().Set(KeyHdr, echoedKey)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// jsonObject decodes a request body into its members ({} when it is not an
// object).
func jsonObject(body []byte) map[string]any {
	out := map[string]any{}
	if len(strings.TrimSpace(string(body))) > 0 {
		_ = json.Unmarshal(body, &out)
	}
	return out
}

// Port is the port the host listens on.
func (h *Host) Port() int {
	_, p, _ := net.SplitHostPort(h.Addr)
	n, _ := strconv.Atoi(p)
	return n
}
