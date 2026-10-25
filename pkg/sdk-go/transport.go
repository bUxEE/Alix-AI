package sdkgo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// The HTTP layer under the client. Its rules, all of them about the single-use context token and the
// fail-closed stance of the API:
//   - the token provider is called once per request or stream open, after the arguments have been validated
//     and before the request is sent, and the token lives only in a local variable for that call. It is never
//     cached, retried with, logged or put in an error;
//   - the token is sent only as `Authorization: Bearer`; redirects are an error (never followed with the
//     credential), cookies are never sent;
//   - nothing is retried here. A request that may have reached the server and whose outcome is unknown is
//     `uncertain_outcome`; the host decides what to do with `retryable`.

// TokenRequest tells the host's token provider what request it is minting a token for.
type TokenRequest struct {
	// Operation is the OpenAPI operationId of the request.
	Operation string
	// SessionID is the session the request concerns, when the path names one.
	SessionID string
	// PreparationID is the preparation the request concerns, when the path names one.
	PreparationID string
	// ApplicationID is the application the request concerns, when the path names one.
	ApplicationID string
}

// TokenProvider supplies a fresh, single-use context token for one request. It is called for every
// authenticated request and every event-stream open and is never cached by the SDK. Return the bare token
// (no "Bearer " prefix).
type TokenProvider func(ctx context.Context, request TokenRequest) (string, error)

// CallOption adjusts one call. An option that a method has no use for is ignored.
type CallOption func(*callOptions)

type callOptions struct {
	timeout    time.Duration
	hasTimeout bool
	cursor     *int64
	webSocket  bool
}

// WithTimeout overrides the client's timeout for this call: zero means the client default, a negative
// value disables the deadline. For a stream the deadline covers the whole stream.
func WithTimeout(d time.Duration) CallOption {
	return func(o *callOptions) {
		o.timeout = d
		o.hasTimeout = d != 0
	}
}

// WithCursor resumes an event stream after the event with this seq (the last one the caller received).
func WithCursor(seq int64) CallOption {
	return func(o *callOptions) { o.cursor = &seq }
}

// WithWebSocket reads the events of a turn over a WebSocket instead of server-sent events. It applies to
// StreamTurnEvents and to the automatic resume of a stream SendTurn opened.
func WithWebSocket() CallOption {
	return func(o *callOptions) { o.webSocket = true }
}

func applyOptions(options []CallOption) callOptions {
	var o callOptions
	for _, opt := range options {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// pick returns the deadline of a call: the option, else the default; negative disables it.
func (o callOptions) deadline(fallback time.Duration) time.Duration {
	d := fallback
	if o.hasTimeout {
		d = o.timeout
	}
	if d < 0 {
		return 0
	}
	return d
}

type transport struct {
	base  *url.URL
	hc    *http.Client
	token TokenProvider
}

// request is one HTTP request of an operation.
type request struct {
	op          Operation
	params      map[string]string
	query       url.Values
	header      http.Header
	accept      string
	contentType string
	body        []byte
	// allowStatus names statuses, besides 2xx, that are decoded as the success document instead of failing.
	allowStatus func(int) bool
}

// callScope is the cancellation and deadline of one call. For a stream it lives as long as the stream.
type callScope struct {
	parent context.Context
	ctx    context.Context
	cancel context.CancelFunc
	op     Operation
}

func newScope(parent context.Context, timeout time.Duration, op Operation) *callScope {
	s := &callScope{parent: parent, op: op}
	if timeout > 0 {
		s.ctx, s.cancel = context.WithTimeout(parent, timeout)
	} else {
		s.ctx, s.cancel = context.WithCancel(parent)
	}
	return s
}

func (s *callScope) release() { s.cancel() }

// as is the same scope (context, deadline, cancel) for a request of another operation, so its failures are classified by
// that operation. It is never released on its own: the scope it copies owns the context.
func (s *callScope) as(op Operation) *callScope {
	c := *s
	c.op = op
	return &c
}

// failure maps whatever a send or a read failed with to the error the host should see: the caller's own
// context error, a typed SDK error, or a transport failure. A read may be repeated safely; for anything else
// the request may have been processed, so the honest answer is that its outcome is unknown.
func (s *callScope) failure(err error) error {
	if perr := s.parent.Err(); perr != nil {
		return perr
	}
	var ce *contracts.Error
	var api *APIError
	if errors.As(err, &api) || errors.As(err, &ce) {
		return err
	}
	if errors.Is(s.ctx.Err(), context.DeadlineExceeded) {
		if s.op.Safe {
			return contracts.Wrap(contracts.ErrTimeout, err, "the request timed out").
				WithDetails("operation", s.op.ID)
		}
		return contracts.Wrap(contracts.ErrUncertainOutcome, err,
			"the request timed out: read the state before sending it again").
			WithDetails("operation", s.op.ID).WithDetails("phase", "timeout")
	}
	if s.op.Safe {
		return contracts.Wrap(contracts.ErrUnavailable, err, "the request could not be completed (network error)").
			WithDetails("operation", s.op.ID).WithDetails("phase", "transport")
	}
	return contracts.Wrap(contracts.ErrUncertainOutcome, err,
		"the request may not have completed (network error): read the state before sending it again").
		WithDetails("operation", s.op.ID).WithDetails("phase", "transport")
}

func fillPath(template string, params map[string]string) string {
	out := template
	for name, value := range params {
		out = strings.ReplaceAll(out, "{"+name+"}", url.PathEscape(value))
	}
	return out
}

func (t *transport) url(r request) string {
	u := t.base.String() + fillPath(r.op.Path, r.params)
	if len(r.query) > 0 {
		u += "?" + r.query.Encode()
	}
	return u
}

// authorize mints the token for an authenticated request, after the arguments were validated.
func (t *transport) authorize(scope *callScope, r request) (string, error) {
	tok, err := t.token(scope.ctx, TokenRequest{
		Operation:     r.op.ID,
		SessionID:     r.params["session_id"],
		PreparationID: r.params["preparation_id"],
		ApplicationID: r.params["application_id"],
	})
	if err != nil {
		if scope.ctx.Err() != nil {
			return "", scope.failure(scope.ctx.Err())
		}
		return "", contracts.Wrap(contracts.ErrUnauthenticated, err, "the token provider failed, so the request was not sent").
			WithDetails("operation", r.op.ID).WithDetails("reason", "token_provider_failed")
	}
	if err := checkContextToken(tok); err != nil {
		return "", err
	}
	// The call may have been cancelled while the host was minting the token: do not spend it.
	if scope.ctx.Err() != nil {
		return "", scope.failure(scope.ctx.Err())
	}
	return tok, nil
}

// send sends one request inside scope and returns the response head. A status that is neither 2xx nor
// allowed is read and returned as a typed error.
func (t *transport) send(scope *callScope, r request) (*http.Response, error) {
	if err := scope.ctx.Err(); err != nil {
		return nil, scope.failure(err)
	}
	h := http.Header{}
	for k, v := range r.header {
		h[k] = v
	}
	h.Set("Accept", r.accept)
	if len(r.body) > 0 || r.contentType != "" {
		h.Set("Content-Type", r.contentType)
	}
	if r.op.Authenticated {
		tok, err := t.authorize(scope, r)
		if err != nil {
			return nil, err
		}
		h.Set("Authorization", "Bearer "+tok)
	}
	var body io.Reader
	if len(r.body) > 0 {
		body = bytes.NewReader(r.body)
	}
	req, err := http.NewRequestWithContext(scope.ctx, r.op.Method, t.url(r), body)
	if err != nil {
		return nil, localError(contracts.ErrInternal, "the request could not be built", "operation", r.op.ID)
	}
	req.Header = h
	resp, err := t.hc.Do(req)
	if err != nil {
		return nil, scope.failure(err)
	}
	if resp.StatusCode/100 == 2 || (r.allowStatus != nil && r.allowStatus(resp.StatusCode)) {
		return resp, nil
	}
	defer resp.Body.Close()
	return nil, errorFromResponse(resp)
}

const maxResponseBody = 64 << 20

// doJSON sends a request and decodes its JSON answer into T, checking the members the document requires.
// A body that is not what the document says is never turned into a zero value.
func doJSON[T any](ctx context.Context, t *transport, r request, timeout time.Duration, check func(*T) error) (*T, error) {
	scope := newScope(ctx, timeout, r.op)
	defer scope.release()
	resp, err := t.send(scope, r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, scope.failure(err)
	}
	var v T
	err = json.Unmarshal(body, &v)
	if err == nil && check != nil {
		err = check(&v)
	}
	if err != nil {
		if resp.StatusCode/100 != 2 { // an allowed status whose body is not the report: an ordinary failure
			return nil, errorFromBytes(resp, body)
		}
		return nil, unexpectedShape(r.op, err)
	}
	return &v, nil
}

// doEmpty sends a request whose success answer carries no body (204) and ignores any body it has.
func doEmpty(ctx context.Context, t *transport, r request, timeout time.Duration) error {
	scope := newScope(ctx, timeout, r.op)
	defer scope.release()
	resp, err := t.send(scope, r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
	return nil
}
