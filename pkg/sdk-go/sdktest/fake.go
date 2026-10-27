// Package sdktest is a fake AlixAI server for the tests of host applications that use the Go SDK, so they run
// without a running AlixAI.
//
// The fake speaks the public API as schemas/api/openapi.yaml defines it, for the operations the SDK speaks
// (sdkgo.Operations): it routes by that table, answers with documents that validate against the schemas, and
// enforces the rules a host cares about: every context token is single use (a second presentation is
// `replayed`), a path that names another session, preparation or application than the token is
// `scope_violation`, a confirmation secret is used up by the first presentation, an application that is not
// active serves no traffic, and a turn's events can be read again from a cursor. A test describes what the
// assistant does in a turn with OnTurn, and what goes wrong with FailNext, DropNext and the other switches.
//
// The fake keeps no secret and no key: tokens and confirmation secrets are random strings of its own.
package sdktest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	sdkgo "github.com/buxee/alix-ai/pkg/sdk-go"
)

// Claims is what a context token carries: the verified scope of the caller. Zero members take a default.
type Claims struct {
	ApplicationID    string
	TenantID         string
	WorkspaceID      string
	PrincipalID      string
	SessionID        string
	CapabilityScopes []string
	Roles            []string
}

func (c Claims) withDefaults() Claims {
	def := func(v *string, d string) {
		if *v == "" {
			*v = d
		}
	}
	def(&c.ApplicationID, "fake-app")
	def(&c.TenantID, "tenant-1")
	def(&c.WorkspaceID, "workspace-1")
	def(&c.PrincipalID, "principal-1")
	def(&c.SessionID, "ses-1")
	if c.CapabilityScopes == nil {
		c.CapabilityScopes = []string{"demo.lookup", "demo.book", "demo.close_account"}
	}
	return c
}

// Request is one request the fake received, for assertions.
type Request struct {
	// Operation is the operationId, "" when no operation matched.
	Operation string
	Method    string
	Path      string
	RawQuery  string
	// Status is the status the fake answered with (101 for an accepted WebSocket).
	Status int
	// Authenticated is true when the request carried a bearer token.
	Authenticated bool
	// LastEventID is the Last-Event-ID header of the request.
	LastEventID string
	// WebSocket is true for a WebSocket upgrade.
	WebSocket bool
}

type failure struct {
	status int
	err    contracts.Error
}

// Fake is a fake AlixAI server. Create one with New and close it with Close.
type Fake struct {
	srv *httptest.Server

	mu        sync.Mutex
	logMu     sync.Mutex // guards requests only, so the log is readable while a handler holds mu
	tokens    map[string]Claims
	used      map[string]bool
	sessions  map[string]*session
	turns     map[string]*turn
	preps     map[string]*prep
	packages  map[string]*pkg
	pkgOrder  []string
	requests  []Request
	failNext  map[string][]failure
	drops     []int
	retain    int
	ready     bool
	appState  string
	onTurn    func(Turn) TurnPlan
	rag       ragState
	decision  decisionState
	providers providersState
	files     map[string]*fileEntry
	now       func() time.Time

	dictationUnavailable bool
	dictationDisabled    bool
	dictationSilent      bool
	dictationText        string
}

// New starts a fake AlixAI server on a loopback port.
func New() *Fake {
	f := &Fake{
		tokens:   map[string]Claims{},
		used:     map[string]bool{},
		sessions: map[string]*session{},
		turns:    map[string]*turn{},
		preps:    map[string]*prep{},
		packages: map[string]*pkg{},
		failNext: map[string][]failure{},
		ready:    true,
		appState: "active",
		now:      time.Now,
		rag:      newRagState(),
		files:    map[string]*fileEntry{},
	}
	f.srv = httptest.NewServer(f.routes())
	return f
}

// URL is the base URL of the fake, for sdkgo.ClientOptions.BaseURL (loopback http).
func (f *Fake) URL() string { return f.srv.URL }

// Close shuts the fake down.
func (f *Fake) Close() { f.srv.Close() }

// Client returns an SDK client of the fake whose token provider issues a fresh single-use token with these
// claims for every request.
func (f *Fake) Client(claims Claims, options ...func(*sdkgo.ClientOptions)) (*sdkgo.Client, error) {
	o := sdkgo.ClientOptions{
		BaseURL:         f.URL(),
		GetContextToken: f.TokenProvider(claims),
		HTTPClient:      f.srv.Client(),
		ResumeBackoff:   time.Millisecond,
	}
	for _, opt := range options {
		opt(&o)
	}
	return sdkgo.NewClient(o)
}

// IssueToken mints a single-use context token for these claims.
func (f *Fake) IssueToken(claims Claims) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	tok := "fake." + randomSegment(18)
	f.tokens[tok] = claims.withDefaults()
	return tok
}

// TokenProvider is an sdkgo.TokenProvider that issues a fresh token with these claims on every call.
func (f *Fake) TokenProvider(claims Claims) sdkgo.TokenProvider {
	return func(_ context.Context, _ sdkgo.TokenRequest) (string, error) { return f.IssueToken(claims), nil }
}

// Requests returns every request received so far, oldest first.
func (f *Fake) Requests() []Request {
	f.logMu.Lock()
	defer f.logMu.Unlock()
	return append([]Request(nil), f.requests...)
}

// RequestsOf returns the requests of one operation.
func (f *Fake) RequestsOf(operationID string) []Request {
	var out []Request
	for _, r := range f.Requests() {
		if r.Operation == operationID {
			out = append(out, r)
		}
	}
	return out
}

// FailNext makes the next request of an operation answer with this HTTP status and error instead of its
// normal answer. The request's token is still used up.
func (f *Fake) FailNext(operationID string, status int, code contracts.ErrCode, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext[operationID] = append(f.failNext[operationID], failure{status: status, err: *contracts.New(code, message)})
}

// DropNext cuts the next event-stream connection (a turn's server-sent events, or a resume over SSE or
// WebSocket) after n events, without the `done` event: the client has to resume. Each call queues one drop.
func (f *Fake) DropNext(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drops = append(f.drops, n)
}

// RetainEvents bounds the replay window to the last n events of a turn (0 keeps all): a cursor behind it is
// `expired` (details.reason cursor_expired).
func (f *Fake) RetainEvents(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retain = n
}

// SetReady switches the readiness probes between ready (200) and not ready (503, still a report).
func (f *Fake) SetReady(ready bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ready = ready
}

// SetApplicationStatus sets the status of the application's package: "active" serves traffic; any other
// ("quarantined", "rejected", "superseded", "not_installed") makes sessions, turns, actions and capabilities
// answer `quarantined`.
func (f *Fake) SetApplicationStatus(status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appState = status
}

// SetClock replaces the clock the fake stamps documents and expiries with.
func (f *Fake) SetClock(now func() time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = now
}

// OnTurn sets what the assistant does in a turn. Without it every text turn is answered with a short reply.
func (f *Fake) OnTurn(fn func(Turn) TurnPlan) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onTurn = fn
}

// ---- plumbing ----

func randomSegment(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ulid is a 26-character ULID (Crockford base32), time first, as the core mints them.
func ulid(now time.Time) string {
	var out [26]byte
	t := uint64(now.UnixMilli())
	for i := 9; i >= 0; i-- {
		out[i] = crockford[t%32]
		t /= 32
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	for i := 0; i < 16; i++ {
		out[10+i] = crockford[b[i]&31]
	}
	return string(out[:])
}

func (f *Fake) typedID(prefix string) string { return prefix + "_" + ulid(f.now()) }

func (f *Fake) stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// recorder remembers the status of a response and passes streaming and upgrades through.
type recorder struct {
	http.ResponseWriter
	status   int
	onStatus func(int)
}

func (r *recorder) setStatus(code int) {
	if r.status == 0 {
		r.status = code
		r.onStatus(code)
	}
}

func (r *recorder) WriteHeader(code int) {
	r.setStatus(code)
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.setStatus(http.StatusOK)
	return r.ResponseWriter.Write(b)
}

func (r *recorder) Flush() {
	r.setStatus(http.StatusOK)
	http.NewResponseController(r.ResponseWriter).Flush()
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// call is one request in flight, with the verified claims of its token.
type call struct {
	f      *Fake
	w      http.ResponseWriter
	r      *http.Request
	rec    *recorder
	op     sdkgo.Operation
	claims Claims
	locked bool
}

func (c *call) unlock() {
	if c.locked {
		c.locked = false
		c.f.mu.Unlock()
	}
}

func (c *call) param(name string) string { return c.r.PathValue(name) }

// json writes a JSON document.
func (c *call) json(status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		c.fail(http.StatusInternalServerError, contracts.ErrInternal, "the fake could not encode its answer: "+err.Error())
		return
	}
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(status)
	_, _ = c.w.Write(body)
}

// errorBody is the bare Error document.
func errorBody(code contracts.ErrCode, message string, details map[string]any) map[string]any {
	doc := map[string]any{"code": code, "message": message, "retryable": code.Retryable()}
	if details != nil {
		doc["details"] = details
	}
	return doc
}

func (c *call) fail(status int, code contracts.ErrCode, message string, details ...any) {
	d := map[string]any{}
	for i := 0; i+1 < len(details); i += 2 {
		d[details[i].(string)] = details[i+1]
	}
	if len(d) == 0 {
		d = nil
	}
	if status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests {
		c.w.Header().Set("Retry-After", "1")
	}
	c.json(status, errorBody(code, message, d))
}

func (f *Fake) routes() http.Handler {
	mux := http.NewServeMux()
	handlers := map[string]func(*call){
		"getLiveness":                       (*call).liveness,
		"getReadiness":                      (*call).readiness,
		"getHealth":                         (*call).health,
		"createSession":                     (*call).createSession,
		"getSession":                        (*call).getSession,
		"endSession":                        (*call).endSession,
		"getPendingState":                   (*call).getPending,
		"sendTurn":                          (*call).sendTurn,
		"streamTurnEvents":                  (*call).streamTurnEvents,
		"getAction":                         (*call).getAction,
		"confirmAction":                     (*call).confirmAction,
		"rejectAction":                      (*call).rejectAction,
		"getActionReceipt":                  (*call).getReceipt,
		"reportStepUpOutcome":               (*call).reportStepUpOutcome,
		"registerApplication":               (*call).registerApplication,
		"getApplication":                    (*call).getApplication,
		"listCapabilities":                  (*call).listCapabilities,
		"registerRagSource":                 (*call).ragRegister,
		"listRagSources":                    (*call).ragList,
		"reauditRagSources":                 (*call).ragReaudit,
		"getRagSource":                      (*call).ragGet,
		"removeRagSource":                   (*call).ragRemove,
		"syncRagSource":                     (*call).ragSync,
		"rebuildRagSource":                  (*call).ragRebuild,
		"listRagGenerations":                (*call).ragGenerations,
		"invalidateRagGeneration":           (*call).ragInvalidate,
		"searchRag":                         (*call).ragSearch,
		"answerRag":                         (*call).ragAnswer,
		"resolveRagCitation":                (*call).ragResolve,
		"getDecisionStatus":                 (*call).decisionStatus,
		"getDictationCapabilities":          (*call).dictationCapabilities,
		"createSessionDictation":            (*call).dictationCreate,
		"listProviders":                     (*call).providersList,
		"putProviderCredential":             (*call).providersCredential,
		"deleteProviderConnection":          (*call).providersDisconnect,
		"ackProviderBillingNotice":          (*call).providersAck,
		"getProviderDisclosure":             (*call).providersDisclosure,
		"getProviderMode":                   (*call).providersGetMode,
		"putProviderMode":                   (*call).providersPutMode,
		"getProviderEgressPolicy":           (*call).providersGetPolicy,
		"putProviderEgressPolicy":           (*call).providersPutPolicy,
		"getProviderBudgets":                (*call).providersGetBudgets,
		"putProviderBudget":                 (*call).providersPutBudget,
		"exportProviderUsage":               (*call).providersUsage,
		"getProviderKillSwitch":             (*call).providersGetKill,
		"putProviderKillSwitch":             (*call).providersPutKill,
		"startProviderOAuth":                (*call).oauthStart,
		"completeProviderOAuth":             (*call).oauthCallback,
		"getProviderOAuthFlow":              (*call).oauthFlowStatus,
		"cancelProviderOAuthFlow":           (*call).oauthFlowCancel,
		"getProviderCatalog":                (*call).meCatalog,
		"getPrincipalLlm":                   (*call).meGetLLM,
		"putPrincipalLlmRole":               (*call).mePutRole,
		"deletePrincipalLlmRole":            (*call).meDeleteRole,
		"putPrincipalProviderCredential":    (*call).mePutCredential,
		"deletePrincipalProviderCredential": (*call).meDeleteCredential,
		"ackPrincipalProviderBillingNotice": (*call).meAckBilling,
		"startPrincipalOAuth":               (*call).meOAuthStart,
		"completePrincipalOAuth":            (*call).meOAuthCallback,
		"getPrincipalOAuthFlow":             (*call).meOAuthFlowStatus,
		"cancelPrincipalOAuthFlow":          (*call).meOAuthFlowCancel,
		"startErasure":                      (*call).opsStartErasure,
		"getErasureJob":                     (*call).opsGetErasure,
		"putPrivateLlmEndpoint":             (*call).opsPutPrivateLlmEndpoint,
		"getPrivateLlmEndpoint":             (*call).opsGetPrivateLlmEndpoint,
		"deletePrivateLlmEndpoint":          (*call).opsDeletePrivateLlmEndpoint,
		"putTenantPrivateLlm":               (*call).opsPutTenantPrivateLlm,
		"getTenantPrivateLlm":               (*call).opsGetTenantPrivateLlm,
		"deleteTenantPrivateLlm":            (*call).opsDeleteTenantPrivateLlm,
		"putPrincipalPrivateLlm":            (*call).opsPutPrincipalPrivateLlm,
		"getPrincipalPrivateLlm":            (*call).opsGetPrincipalPrivateLlm,
		"deletePrincipalPrivateLlm":         (*call).opsDeletePrincipalPrivateLlm,
		"getMeUsage":                        (*call).meUsage,
		"getMeBudget":                       (*call).meBudget,
		"getPrincipalAnalyst":               (*call).meAnalyst,
		"getTenantAnalyst":                  (*call).opsGetTenantAnalyst,
		"putTenantAnalyst":                  (*call).opsPutTenantAnalyst,
		"listMemory":                        (*call).memoryList,
		"createMemory":                      (*call).memoryCreate,
		"patchMemory":                       (*call).memoryPatch,
		"deleteMemory":                      (*call).memoryDelete,
		"confirmMemory":                     (*call).memoryConfirm,
		"rejectMemory":                      (*call).memoryReject,
		"createExport":                      (*call).exportCreate,
	}
	for _, op := range sdkgo.Operations() {
		handle := handlers[op.ID]
		if handle == nil {
			panic("sdktest: no handler for operation " + op.ID)
		}
		mux.HandleFunc(op.Method+" "+op.Path, func(w http.ResponseWriter, r *http.Request) { f.serve(op, handle, w, r) })
	}
	return mux
}

// serve authenticates a request (using its token up), applies a queued failure, runs the handler and logs.
func (f *Fake) serve(op sdkgo.Operation, handle func(*call), w http.ResponseWriter, r *http.Request) {
	rec := &recorder{ResponseWriter: w}
	w.Header().Set("X-Request-ID", "req_"+randomSegment(9))
	c := &call{f: f, w: rec, r: r, rec: rec, op: op}
	// The request is logged as it arrives and its status as soon as it is known, so a test that reads the log
	// right after a call sees every request, even one whose handler is still closing a connection.
	f.logMu.Lock()
	idx := len(f.requests)
	f.requests = append(f.requests, Request{
		Operation: op.ID, Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery,
		LastEventID: r.Header.Get("Last-Event-ID"), WebSocket: strings.EqualFold(r.Header.Get("Upgrade"), "websocket"),
	})
	f.logMu.Unlock()
	rec.onStatus = func(code int) {
		f.logMu.Lock()
		f.requests[idx].Status = code
		f.logMu.Unlock()
	}
	f.mu.Lock()
	c.locked = true
	defer c.unlock()
	if op.Authenticated {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		claims, known := f.tokens[tok]
		switch {
		case !ok || !known:
			rec.Header().Set("WWW-Authenticate", "Bearer")
			c.fail(http.StatusUnauthorized, contracts.ErrUnauthenticated, "the context token is missing or unknown")
			return
		case f.used[tok]:
			rec.Header().Set("WWW-Authenticate", "Bearer")
			c.fail(http.StatusUnauthorized, contracts.ErrReplayed, "the context token was already used: a token is single use")
			return
		}
		f.used[tok] = true
		f.logMu.Lock()
		f.requests[idx].Authenticated = true
		f.logMu.Unlock()
		c.claims = claims
	}
	if queue := f.failNext[op.ID]; len(queue) > 0 {
		next := queue[0]
		f.failNext[op.ID] = queue[1:]
		c.fail(next.status, next.err.Code, next.err.Message)
		return
	}
	if f.appState != "active" && quarantines(op.ID) {
		c.fail(http.StatusForbidden, contracts.ErrQuarantined, "the application's package is not active")
		return
	}
	handle(c)
}

// quarantines lists the operations an application whose package is not active does not serve.
func quarantines(id string) bool {
	switch id {
	case "createSession", "getSession", "endSession", "getPendingState", "sendTurn", "streamTurnEvents",
		"reportStepUpOutcome",
		"getAction", "confirmAction", "rejectAction", "getActionReceipt", "listCapabilities", "getDecisionStatus":
		return true
	}
	return false
}

// readBody reads a request body, at most 1 MiB.
func (c *call) readBody() ([]byte, bool) {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(http.MaxBytesReader(c.w, c.r.Body, 1<<20)); err != nil {
		c.fail(http.StatusRequestEntityTooLarge, contracts.ErrValidationFailed, "the body is too large")
		return nil, false
	}
	return buf.Bytes(), true
}

// closedObject decodes a request body into v, refusing unknown members: every request body is a closed object.
func (c *call) closedObject(body []byte, v any) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "the request body is not valid: "+err.Error())
		return false
	}
	return true
}
