package agui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	sdkgo "github.com/buxee/alix-ai/pkg/sdk-go"
)

// HeaderConfirmationToken is the header that carries the confirmation secret: in the gateway's response to the run that
// prepared an action, and in the request of the resume that approves it. It is never part of an event or a body, and the
// proxy never sends it to, or accepts it from, the browser.
const HeaderConfirmationToken = "X-AlixAI-Confirmation-Token"

// MaxRunRequestBytes bounds the body of a run the proxy reads: the gateway's own limit (1 MiB of input plus its framing).
const MaxRunRequestBytes = 1<<20 + 4<<10

// DefaultSecretTTL is how long a stored confirmation secret is kept when the interrupt names no earlier expiry.
const DefaultSecretTTL = 15 * time.Minute

const (
	maxSecretTTL      = time.Hour
	maxErrorBodyBytes = 64 << 10
	maxJSONBodyBytes  = 1 << 20
	confirmCallPrefix = "alixai_confirm_"
)

// The operationIds of schemas/api/paths/agui.yaml, which is what the host's token callback is told it is minting a token for.
const (
	OpRunAgui          = "runAgui"
	OpStreamRunEvents  = "streamAguiRunEvents"
	OpCancelRun        = "cancelAguiRun"
	OpGetCapabilities  = "getAguiCapabilities"
	upstreamRunPath    = "/v1/agui"
	upstreamCapsPath   = "/v1/agui/capabilities"
	upstreamThreadBase = "/v1/agui/threads/"
)

var (
	threadIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	cursorPattern   = regexp.MustCompile(`^[0-9]{1,10}$`)
	token68Pattern  = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]+$`)
)

// Options configures a Proxy.
type Options struct {
	// BaseURL is the AlixAI service, e.g. "http://127.0.0.1:8080". The same rules as sdkgo.ClientOptions: plain http only for
	// a loopback host, no credentials, query or fragment in it.
	BaseURL string
	// GetContextToken is the host's callback. It is called once for every request the proxy forwards, after the request was
	// validated and before it is sent, with the context of the incoming request (put the authenticated user there with the
	// host's own middleware), and with the thread as SessionID. It must authenticate the user, check that they own the
	// thread, and return a fresh single-use token whose session is that thread. Returning a *contracts.Error with code
	// permission_denied or scope_violation answers 403, unavailable 503, rate_limited 429; any other error is 401. The proxy
	// never caches, logs or retries with a token.
	GetContextToken sdkgo.TokenProvider
	// HTTPClient carries the upstream requests; its Transport is used as it is and redirects are never followed. Defaults to
	// a client with the default transport and no overall timeout (a run is a stream).
	HTTPClient *http.Client
	// AllowInsecureHTTP allows plain http to a host that is not loopback. The context token is a bearer credential: leave it off.
	AllowInsecureHTTP bool
	// Secrets keeps the confirmation secrets server-side. Defaults to an in-memory store.
	Secrets SecretStore
	// SecretTTL is how long a secret is kept at most (it is also dropped at the expiry the interrupt names). Default 15 minutes, at most one hour.
	SecretTTL time.Duration
	// Logger receives one line per forwarded request: the operation, the status and the error code. Never a body, an event, a
	// token or a secret. Defaults to discarding.
	Logger *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Proxy forwards AG-UI runs from a host's frontend to the AlixAI gateway. It is safe for concurrent use.
type Proxy struct {
	base    string
	token   sdkgo.TokenProvider
	hc      *http.Client
	secrets SecretStore
	ttl     time.Duration
	now     func() time.Time
	log     *slog.Logger
}

// NewProxy checks the options and returns a proxy. It sends nothing.
func NewProxy(o Options) (*Proxy, error) {
	// The SDK's own client validates the base URL and the token callback; its rules are this package's rules.
	if _, err := sdkgo.NewClient(sdkgo.ClientOptions{BaseURL: o.BaseURL, GetContextToken: o.GetContextToken, AllowInsecureHTTP: o.AllowInsecureHTTP}); err != nil {
		return nil, err
	}
	u, err := url.Parse(strings.TrimSpace(o.BaseURL))
	if err != nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "BaseURL: it is not an absolute URL")
	}
	p := &Proxy{
		base:    strings.TrimRight(u.String(), "/"),
		token:   o.GetContextToken,
		secrets: o.Secrets,
		ttl:     o.SecretTTL,
		now:     o.Now,
		log:     o.Logger,
	}
	hc := http.Client{}
	if o.HTTPClient != nil {
		hc = *o.HTTPClient
	}
	// A redirect would carry the bearer credential and the secret to another place: it is an error, never followed.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	p.hc = &hc
	if p.now == nil {
		p.now = time.Now
	}
	if p.secrets == nil {
		p.secrets = NewMemorySecretStore(p.now, 0)
	}
	if p.ttl <= 0 || p.ttl > maxSecretTTL {
		p.ttl = DefaultSecretTTL
	}
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
	}
	return p, nil
}

// Handler serves the four routes of the AG-UI endpoint relative to where it is mounted (use http.StripPrefix):
//
//	POST /                                          a run: RunAgentInput in, AG-UI events out as server-sent events
//	GET  /capabilities                              what the endpoint claims
//	GET  /threads/{thread_id}/runs/{run_id}/events  the events of a run again, from the cursor in Last-Event-ID or ?cursor=
//	POST /threads/{thread_id}/runs/{run_id}/cancel  stop a run that is still in flight
//
// A host that wants one route only (CopilotKit's runtime or an AG-UI HttpAgent pointed at a single URL) registers Run.
func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /{$}", p.Run)
	mux.HandleFunc("GET /capabilities", p.Capabilities)
	mux.HandleFunc("GET /threads/{thread_id}/runs/{run_id}/events", func(w http.ResponseWriter, r *http.Request) {
		p.Events(w, r, r.PathValue("thread_id"), r.PathValue("run_id"))
	})
	mux.HandleFunc("POST /threads/{thread_id}/runs/{run_id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		p.Cancel(w, r, r.PathValue("thread_id"), r.PathValue("run_id"))
	})
	return mux
}

// ---- errors of the proxy itself ----

func statusOf(code contracts.ErrCode) int {
	switch code {
	case contracts.ErrValidationFailed:
		return http.StatusBadRequest
	case contracts.ErrUnauthenticated:
		return http.StatusUnauthorized
	case contracts.ErrPermissionDenied, contracts.ErrScopeViolation:
		return http.StatusForbidden
	case contracts.ErrRateLimited:
		return http.StatusTooManyRequests
	case contracts.ErrTimeout:
		return http.StatusGatewayTimeout
	case contracts.ErrUnavailable:
		return http.StatusBadGateway
	}
	return http.StatusInternalServerError
}

// fail answers with an AlixAI Error document. The messages are fixed texts: nothing the request carried is echoed.
func (p *Proxy) fail(w http.ResponseWriter, op string, status int, code contracts.ErrCode, message string, reason string) {
	e := contracts.New(code, message)
	if reason != "" {
		e.WithDetails("reason", reason)
	}
	body, _ := json.Marshal(e)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
	p.log.Debug("agui proxy", "op", op, "status", status, "code", string(code), "reason", reason)
}

func (p *Proxy) failCode(w http.ResponseWriter, op string, code contracts.ErrCode, message, reason string) {
	p.fail(w, op, statusOf(code), code, message, reason)
}

// mint asks the host for the token of one request. It reports false when the request was answered (or the client is gone).
func (p *Proxy) mint(w http.ResponseWriter, r *http.Request, op, thread string) (string, bool) {
	ctx := r.Context()
	tok, err := p.token(ctx, sdkgo.TokenRequest{Operation: op, SessionID: thread})
	if ctx.Err() != nil {
		return "", false // the client went away while the host was minting: do not spend the token
	}
	if err != nil {
		code := contracts.ErrUnauthenticated
		var ce *contracts.Error
		if errors.As(err, &ce) {
			switch ce.Code {
			case contracts.ErrPermissionDenied, contracts.ErrScopeViolation, contracts.ErrUnavailable, contracts.ErrRateLimited:
				code = ce.Code
			}
		}
		p.failCode(w, op, code, "the token provider refused the request, so it was not sent", "token_provider_failed")
		return "", false
	}
	if tok == "" || len(tok) > 16*1024 || !token68Pattern.MatchString(tok) {
		p.failCode(w, op, contracts.ErrUnauthenticated, "the token provider must return the bare context token", "bad_token_shape")
		return "", false
	}
	return tok, true
}

// ---- the request of a run ----

// runInput is what the proxy reads of a RunAgentInput: the ids, and whether it answers an interrupt. The body itself is
// forwarded as it came; nothing else of it is looked at.
type runInput struct {
	threadID string
	runID    string
	// approve is set when the run explicitly approves one preparation: the only case in which a secret is attached.
	approve *SecretKey
	// discard is the preparation a resume declines (rejected or cancelled): its stored secret is of no further use.
	discard *SecretKey
}

func readRunInput(body []byte) (runInput, bool) {
	var in struct {
		ThreadID string `json:"threadId"`
		RunID    string `json:"runId"`
		Resume   []struct {
			InterruptID string          `json:"interruptId"`
			Status      string          `json:"status"`
			Payload     json.RawMessage `json:"payload"`
		} `json:"resume"`
		Messages []struct {
			Role       string          `json:"role"`
			ToolCallID string          `json:"toolCallId"`
			Content    json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return runInput{}, false
	}
	if !validThread(in.ThreadID) || in.RunID == "" || len(in.RunID) > 128 || strings.ContainsAny(in.RunID, "\x00\r\n") {
		return runInput{}, false
	}
	out := runInput{threadID: in.ThreadID, runID: in.RunID}
	var prep, status string
	var payload json.RawMessage
	switch {
	case len(in.Resume) == 1:
		prep, status, payload = in.Resume[0].InterruptID, in.Resume[0].Status, in.Resume[0].Payload
	case len(in.Resume) == 0 && len(in.Messages) > 0:
		// The form of clients that answer the confirmation call as a tool message: its content is the same answer.
		last := in.Messages[len(in.Messages)-1]
		var content string
		if last.Role == "tool" && strings.HasPrefix(last.ToolCallID, confirmCallPrefix) && json.Unmarshal(last.Content, &content) == nil {
			prep, status, payload = strings.TrimPrefix(last.ToolCallID, confirmCallPrefix), "resolved", json.RawMessage(content)
		}
	}
	if prep == "" || len(prep) > 128 {
		return out, true
	}
	key := &SecretKey{ThreadID: in.ThreadID, PreparationID: prep}
	switch approved, explicit := decodeApproval(payload); {
	case status == "cancelled":
		out.discard = key
	case status == "resolved" && explicit && approved:
		out.approve = key
	case status == "resolved" && explicit && !approved:
		out.discard = key
	}
	return out, true
}

func validThread(id string) bool { return threadIDPattern.MatchString(id) && id != "." && id != ".." }

// validRun accepts the run ids the run request accepts (the OpenAPI's 1 to 128 characters, no NUL or line break), except a
// dot segment, which no URL path can carry. The id is path-escaped into the gateway's URL.
func validRun(id string) bool {
	return id != "" && len(id) <= 128 && !strings.ContainsAny(id, "\x00\r\n") && id != "." && id != ".."
}

// decodeApproval reads exactly {"approved": true|false}. Anything else (no payload, a string, extra members, a token) is
// not an answer: a confirmation is never inferred.
func decodeApproval(payload json.RawMessage) (approved, explicit bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(payload, &m) != nil || len(m) != 1 {
		return false, false
	}
	switch strings.TrimSpace(string(m["approved"])) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// ---- routes ----

// Run forwards a run (POST) and streams its events back.
func (p *Proxy) Run(w http.ResponseWriter, r *http.Request) {
	const op = OpRunAgui
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		p.fail(w, op, http.StatusUnsupportedMediaType, contracts.ErrValidationFailed, "Content-Type must be application/json", "")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRunRequestBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			p.fail(w, op, http.StatusRequestEntityTooLarge, contracts.ErrValidationFailed, "the request body is too large", "")
		} else {
			p.fail(w, op, http.StatusBadRequest, contracts.ErrValidationFailed, "the request body could not be read", "")
		}
		return
	}
	in, ok := readRunInput(body)
	if !ok {
		p.fail(w, op, http.StatusBadRequest, contracts.ErrValidationFailed, "the body is not a RunAgentInput with a valid threadId and runId", "")
		return
	}
	tok, ok := p.mint(w, r, op, in.threadID)
	if !ok {
		return
	}
	// The secret is looked up only now: a request that was refused earlier leaves it untouched.
	var secret string
	if in.approve != nil {
		s, found, err := p.secrets.Get(r.Context(), *in.approve)
		if err != nil {
			p.failCode(w, op, contracts.ErrUnavailable, "the confirmation secret store is unavailable", "secret_store_unavailable")
			return
		}
		if found {
			secret = s
		}
	}
	resp, ok := p.send(w, r, op, http.MethodPost, p.base+upstreamRunPath, body, tok, secret, "")
	if !ok {
		return
	}
	defer resp.Body.Close()
	p.settleSecret(r.Context(), in, resp.StatusCode)
	p.relay(w, r, op, resp, streamState{thread: in.threadID, sent: secret})
}

// Events forwards the reconnect of a run: its events again, from the cursor in Last-Event-ID or ?cursor=.
func (p *Proxy) Events(w http.ResponseWriter, r *http.Request, threadID, runID string) {
	const op = OpStreamRunEvents
	if !validThread(threadID) || !validRun(runID) {
		p.fail(w, op, http.StatusBadRequest, contracts.ErrValidationFailed, "the thread and run ids are not valid", "")
		return
	}
	q := url.Values{}
	lastID := ""
	if c := r.URL.Query().Get("cursor"); c != "" {
		if !cursorPattern.MatchString(c) {
			p.fail(w, op, http.StatusBadRequest, contracts.ErrValidationFailed, "the cursor is not valid", "")
			return
		}
		q.Set("cursor", c)
	}
	if l := r.Header.Get("Last-Event-ID"); l != "" {
		if !cursorPattern.MatchString(l) {
			p.fail(w, op, http.StatusBadRequest, contracts.ErrValidationFailed, "Last-Event-ID is not valid", "")
			return
		}
		lastID = l
	}
	target := p.base + upstreamThreadBase + url.PathEscape(threadID) + "/runs/" + url.PathEscape(runID) + "/events"
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	tok, ok := p.mint(w, r, op, threadID)
	if !ok {
		return
	}
	resp, ok := p.send(w, r, op, http.MethodGet, target, nil, tok, "", lastID)
	if !ok {
		return
	}
	defer resp.Body.Close()
	p.relay(w, r, op, resp, streamState{thread: threadID})
}

// Cancel forwards the cancel of a run in flight (POST, no body) and relays the JSON answer.
func (p *Proxy) Cancel(w http.ResponseWriter, r *http.Request, threadID, runID string) {
	const op = OpCancelRun
	if !validThread(threadID) || !validRun(runID) {
		p.fail(w, op, http.StatusBadRequest, contracts.ErrValidationFailed, "the thread and run ids are not valid", "")
		return
	}
	target := p.base + upstreamThreadBase + url.PathEscape(threadID) + "/runs/" + url.PathEscape(runID) + "/cancel"
	p.forwardJSON(w, r, op, http.MethodPost, target, threadID)
}

// Capabilities forwards the capabilities document. It needs a token like every route; the host's callback is told no thread.
func (p *Proxy) Capabilities(w http.ResponseWriter, r *http.Request) {
	p.forwardJSON(w, r, OpGetCapabilities, http.MethodGet, p.base+upstreamCapsPath, "")
}

func (p *Proxy) forwardJSON(w http.ResponseWriter, r *http.Request, op, method, target, thread string) {
	tok, ok := p.mint(w, r, op, thread)
	if !ok {
		return
	}
	resp, ok := p.send(w, r, op, method, target, nil, tok, "", "")
	if !ok {
		return
	}
	defer resp.Body.Close()
	p.relay(w, r, op, resp, streamState{thread: thread})
}

// ---- the upstream ----

// send builds the upstream request from scratch: nothing of the browser's request is copied but the body (and the cursor, which was
// validated). It reports false when the request was answered with an error.
func (p *Proxy) send(w http.ResponseWriter, r *http.Request, op, method, target string, body []byte, tok, secret, lastEventID string) (*http.Response, bool) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(r.Context(), method, target, rd)
	if err != nil {
		p.failCode(w, op, contracts.ErrInternal, "the request could not be built", "")
		return nil, false
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream, application/json")
	} else if op == OpStreamRunEvents {
		req.Header.Set("Accept", "text/event-stream, application/json")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if secret != "" {
		req.Header.Set(HeaderConfirmationToken, secret)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		if r.Context().Err() != nil {
			return nil, false // the client went away
		}
		// A run may have reached the gateway: say so rather than that it failed. Nothing is retried here.
		code, status := contracts.ErrUnavailable, http.StatusBadGateway
		reason := "upstream_unreachable"
		if method == http.MethodPost {
			code, reason = contracts.ErrUncertainOutcome, "upstream_uncertain"
		}
		p.fail(w, op, status, code, "the AlixAI service could not be reached", reason)
		return nil, false
	}
	return resp, true
}

// spent reports whether a secret that was presented got an answer that used it up: accepted (2xx), already decided (409) or
// expired (410), as the CopilotKit adapter does. Any other refusal (400, 401, 403, 415, a rate limit, a server error) and a
// transport failure leave it: the gateway did not decide, the same approval can be sent again, and it never confirms twice.
func spent(status int) bool {
	return status/100 == 2 || status == http.StatusConflict || status == http.StatusGone
}

// settleSecret forgets the stored secret of a preparation once it was presented (an approval whose answer settled it) or
// declined (a resume that rejected or cancelled and was accepted). A run without an answer, and a replay that came back without
// the header, touch nothing.
func (p *Proxy) settleSecret(ctx context.Context, in runInput, status int) {
	var key *SecretKey
	switch {
	case in.approve != nil && spent(status):
		key = in.approve
	case in.discard != nil && status/100 == 2:
		key = in.discard
	}
	if key == nil {
		return
	}
	if err := p.secrets.Delete(ctx, *key); err != nil {
		p.log.Warn("agui proxy: a confirmation secret could not be deleted", "op", OpRunAgui)
	}
}

// ---- the answer ----

type streamState struct {
	thread string
	// sent is the secret this request attached, kept only to be sure no frame carries it back.
	sent string
}

var relayedHeaders = []string{"Retry-After", "X-Request-ID"}

func jsonContentType(h http.Header) bool {
	mt, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	return err == nil && mt == "application/json"
}

// relay answers the browser from the upstream response. Headers are copied by allowlist: the upstream's
// X-AlixAI-Confirmation-Token, cookies and everything else stay here.
func (p *Proxy) relay(w http.ResponseWriter, r *http.Request, op string, resp *http.Response, st streamState) {
	status := resp.StatusCode
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	for _, name := range relayedHeaders {
		if v := resp.Header.Get(name); v != "" {
			h.Set(name, v)
		}
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	isStream := op == OpRunAgui || op == OpStreamRunEvents
	switch {
	case status == http.StatusNoContent:
		w.WriteHeader(status)
		p.log.Debug("agui proxy", "op", op, "status", status)
	case status/100 == 2 && isStream && mt == "text/event-stream":
		p.stream(w, r, op, resp, st)
	case status/100 == 2 && !isStream:
		p.copyJSON(w, op, resp, maxJSONBodyBytes)
	case status/100 == 2:
		p.fail(w, op, http.StatusBadGateway, contracts.ErrInternal, "the AlixAI service answered with something other than an event stream", "upstream_malformed")
	default:
		p.copyJSON(w, op, resp, maxErrorBodyBytes)
	}
}

// copyJSON relays a JSON answer, an error document included, with its status. A body that is not JSON (a proxy's HTML page) or
// is too large becomes a typed error of the proxy's own.
func (p *Proxy) copyJSON(w http.ResponseWriter, op string, resp *http.Response, limit int64) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(body)) > limit || !jsonContentType(resp.Header) || !json.Valid(body) {
		p.fail(w, op, http.StatusBadGateway, contracts.ErrUnavailable, "the AlixAI service answered without a JSON document", "upstream_malformed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
	p.log.Debug("agui proxy", "op", op, "status", resp.StatusCode)
}

// stream copies an event stream frame by frame. One frame is held at a time. The secret of the response (when there is one) is
// stored before the RUN_FINISHED frame that asks for the confirmation is written, and stays until it is used, declined or
// expired: it never leaves this server, and a client that lost the connection and reads the run again sees the same card.
// Nothing follows the end of a run, and a stream that ends without one gets a RUN_ERROR.
func (p *Proxy) stream(w http.ResponseWriter, r *http.Request, op string, resp *http.Response, st streamState) {
	upstreamSecret := resp.Header.Get(HeaderConfirmationToken)
	guard := make([][]byte, 0, 2)
	for _, s := range []string{upstreamSecret, st.sent} {
		if s != "" {
			guard = append(guard, []byte(s))
		}
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // a run outlives the server's write timeout
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()

	enc := NewEncoder(w)
	fr := NewFrameReader(resp.Body)
	ended := false
	for !ended {
		f, err := fr.Next()
		if err != nil {
			break
		}
		if len(f.Data) == 0 {
			continue
		}
		typ, err := eventType(f.Data)
		if err != nil || containsAny(f.Data, guard) {
			// Not an event, or a frame that carries a secret: it is not forwarded and the run ends.
			_ = enc.WriteEvent("", NewRunError("the event stream could not be relayed", "upstream_malformed"))
			p.log.Warn("agui proxy: a frame was refused", "op", op)
			return
		}
		if typ == RunFinished && upstreamSecret != "" {
			p.rememberSecret(r.Context(), f.Data, upstreamSecret, st.thread)
		}
		if err := enc.WriteFrame(f); err != nil {
			return
		}
		ended = Terminal(typ)
	}
	if !ended {
		_ = enc.WriteEvent("", NewRunError("the event stream ended before the run did", "upstream_ended"))
	}
	p.log.Debug("agui proxy", "op", op, "status", http.StatusOK)
}

func containsAny(data []byte, needles [][]byte) bool {
	for _, n := range needles {
		if bytes.Contains(data, n) {
			return true
		}
	}
	return false
}

// rememberSecret stores the secret of the response under (thread, preparation) for each confirmation interrupt of a
// RUN_FINISHED frame. It stores nothing for a secret of the wrong shape, an outcome that is not an interrupt, another thread, or
// an interrupt that has already expired.
func (p *Proxy) rememberSecret(ctx context.Context, data []byte, secret, thread string) {
	if !confirmationSecretPattern.MatchString(secret) {
		return
	}
	ev, err := ParseEvent(data)
	fin, ok := ev.(RunFinishedEvent)
	if err != nil || !ok || fin.Outcome == nil || fin.Outcome.Type != "interrupt" || fin.ThreadID != thread {
		return
	}
	now := p.now()
	for _, it := range fin.Outcome.Interrupts {
		if it.ID == "" || len(it.ID) > 128 || it.Reason != "confirmation_required" {
			continue
		}
		expires := now.Add(p.ttl)
		if at, err := time.Parse(time.RFC3339Nano, it.ExpiresAt); err == nil && at.Before(expires) {
			expires = at
		}
		if !expires.After(now) {
			continue
		}
		key := SecretKey{ThreadID: thread, PreparationID: it.ID}
		if err := p.secrets.Put(ctx, key, secret, expires); err != nil {
			p.log.Warn("agui proxy: a confirmation secret could not be stored", "op", OpRunAgui)
		}
	}
}
