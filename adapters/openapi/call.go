package openapi

import (
	"context"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3filter"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

type origin struct{ scheme, host, port string }

func originOf(u *url.URL) origin {
	o := origin{scheme: strings.ToLower(u.Scheme), host: strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")), port: u.Port()}
	if o.port == "" {
		o.port = map[string]string{"http": "80", "https": "443"}[o.scheme]
	}
	return o
}

// credTripper adds the endpoint's credential to a request on its way out, and
// only to a request for the endpoint's own origin. The credential exists only
// here, for the duration of the call: it is not in the request the adapter
// built, not in what is validated against the document, not in an error and not
// in a log record.
type credTripper struct {
	next    http.RoundTripper
	origin  origin
	ref     string
	secrets SecretProvider
	header  string
	scheme  string
	log     *slog.Logger
	app, ep string
}

func (t *credTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.ref == "" || req.URL == nil || originOf(req.URL) != t.origin {
		return t.next.RoundTrip(req)
	}
	fail := func(reason string) (*http.Response, error) {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		if reason == ReasonCredentialUnavailable && req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		t.log.Warn("the credential of an endpoint cannot be used, so the request was not sent",
			slog.String("application_id", t.app), slog.String("endpoint", t.ep), slog.String("reason", reason))
		return nil, &credentialError{reason: reason}
	}
	sec, err := t.secrets.Secret(req.Context(), t.ref)
	if err != nil {
		// The provider's own error text is not passed on: it is not the adapter's to
		// vouch for.
		return fail(ReasonCredentialUnavailable)
	}
	value := sec.Reveal()
	if value == "" || !validHeaderValue(value) || strings.ContainsRune(value, '\t') {
		return fail(ReasonCredentialMalformed)
	}
	if t.scheme != "" {
		value = t.scheme + " " + value
	}
	req = req.Clone(req.Context())
	req.Header.Set(t.header, value)
	return t.next.RoundTrip(req)
}

// CloseIdleConnections lets http.Client.CloseIdleConnections reach the transport.
func (t *credTripper) CloseIdleConnections() {
	if c, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// newClient builds the HTTP client of an endpoint: the egress client (every
// connection held to the allowlist, certificates verified, no proxy) behind the
// credential round tripper. It never follows a redirect: an API that redirects
// is misconfigured, a redirect of a POST can send a Commit twice, and a
// credential must not follow one.
func (a *Adapter) newClient(ep *endpoint) *http.Client {
	base := a.cfg.Dialer.HTTPClient(0)
	t := &credTripper{next: base.Transport, origin: ep.origin, log: a.cfg.Logger, app: a.cfg.ApplicationID, ep: ep.cfg.ID}
	if ep.cfg.CredentialRef != "" {
		t.ref, t.secrets = ep.cfg.CredentialRef, a.cfg.Secrets
		t.header, t.scheme = ep.cfg.Settings.Auth.Header, *ep.cfg.Settings.Auth.Scheme
	}
	return &http.Client{
		Transport:     t,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// reply is a complete answer: the status, the headers and the body, read within
// the size limit.
type reply struct {
	status int
	header http.Header
	body   []byte
}

// exchange builds the request of a call, sends it once and reads the reply. It
// never retries. commit says that a failure after the request may have been
// delivered must be reported as uncertain_outcome.
func (a *Adapter) exchange(ctx context.Context, rt *route, args callArgs, commit bool) (*wire, *reply, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err // nothing was sent
	}
	if a.closeCtx.Err() != nil {
		return nil, nil, typed(contracts.ErrUnavailable, "the adapter was closed", ReasonAdapterClosed)
	}
	w, err := a.build(ctx, rt, args)
	if err != nil {
		return nil, nil, err
	}

	// The call runs under the caller's context, the operation's timeout and the
	// adapter's own lifetime: Close ends a call in flight.
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(a.closeCtx, cancel)
	defer stop()
	if rt.timeout > 0 {
		var cancelT context.CancelFunc
		cctx, cancelT = context.WithTimeout(cctx, rt.timeout)
		defer cancelT()
	}
	req := w.req.WithContext(cctx)

	start := time.Now()
	resp, err := rt.op.ep.client.Do(req)
	if err != nil {
		return nil, nil, sendFailure(ctx, cctx, err, commit, a.closeCtx.Err() != nil)
	}
	defer resp.Body.Close()
	limit := a.cfg.MaxResponseBytes
	if resp.ContentLength > limit {
		return nil, nil, afterReply(commit, responseTooLarge(limit))
	}
	body, err := adapters.ReadLimited(resp.Body, limit)
	if err != nil {
		if contracts.HasCode(err, contracts.ErrSchemaInvalid) {
			return nil, nil, afterReply(commit, err)
		}
		return nil, nil, sendFailure(ctx, cctx, err, commit, a.closeCtx.Err() != nil)
	}
	a.cfg.Logger.Debug("openapi call", slog.String("application_id", a.cfg.ApplicationID), slog.String("endpoint", rt.op.ep.cfg.ID),
		slog.String("operation", unmappedName(rt.op.id)), slog.Int("status", resp.StatusCode), slog.Duration("took", time.Since(start)))
	return w, &reply{status: resp.StatusCode, header: resp.Header, body: body}, nil
}

// statusFailure is the error for an answer that is not a usable result and
// whose status says what happened.
func statusFailure(status int, reason string) error {
	switch {
	case status == http.StatusTooManyRequests:
		return typed(contracts.ErrRateLimited, "the application is rate limiting the adapter", ReasonHTTPStatus).WithDetails("http_status", status)
	case status >= 500:
		return typed(contracts.ErrUnavailable, "the application answered with a server error", ReasonHTTPStatus).WithDetails("http_status", status)
	}
	return typed(contracts.ErrSchemaInvalid, "the application answered with a status the OpenAPI document does not describe for this operation", reason).
		WithDetails("http_status", status)
}

// checkedBody applies the document to a reply: the status must be one the
// document describes for the operation, the body must be JSON, and it must
// satisfy the response schema. A reply that does not is an error and none of it
// is used; nothing is checked best-effort. The returned bytes are the body.
func (a *Adapter) checkedBody(ctx context.Context, rt *route, w *wire, rp *reply) ([]byte, error) {
	status := rp.status
	if status >= 300 && status < 400 {
		return nil, typed(contracts.ErrSchemaInvalid, "the application answered with a redirect, which the adapter does not follow", ReasonRedirect).
			WithDetails("http_status", status)
	}
	resp := rt.op.responseFor(status)
	if resp == nil {
		return nil, statusFailure(status, ReasonStatusNotDeclared)
	}
	// From here a reply that cannot be used is the application's malfunction
	// when it is a server error, and a contract violation otherwise.
	fail := func(e *contracts.Error) error {
		if status >= 500 {
			return statusFailure(status, "")
		}
		return e.WithDetails("http_status", status)
	}
	mt := resp.Content["application/json"]
	if mt == nil || mt.Schema == nil {
		return nil, fail(typed(contracts.ErrSchemaInvalid, "the OpenAPI document describes no JSON body for this status, so the reply cannot be checked", ReasonResponseNotDescribed))
	}
	media, _, err := mime.ParseMediaType(rp.header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return nil, fail(typed(contracts.ErrSchemaInvalid, "the reply is not application/json", ReasonNotJSON))
	}
	header := rp.header.Clone()
	header.Set("Content-Type", "application/json")
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: &http.Request{Method: w.method}, Route: rt.op.route},
		Status:                 status,
		Header:                 header,
		Options:                &openapi3filter.Options{IncludeResponseStatus: true},
	}
	in.SetBodyBytes(rp.body)
	if err := openapi3filter.ValidateResponse(context.WithoutCancel(ctx), in); err != nil {
		e := typed(contracts.ErrSchemaInvalid, "the reply does not satisfy the response the OpenAPI document describes", ReasonResponseViolatesSpec)
		if loc := responseLocation(err); loc != "" {
			e.WithDetails("location", loc)
		}
		return nil, fail(e)
	}
	return rp.body, nil
}

// responseLocation says where in a reply the document was violated, in names
// that are safe to print.
func responseLocation(err error) string {
	var re *openapi3filter.ResponseError
	if errors.As(err, &re) && re.Reason != "" && !strings.Contains(re.Reason, "\"") {
		if p := pointerOf(err); p != "" {
			return "response body" + p
		}
		return "response"
	}
	if p := pointerOf(err); p != "" {
		return "response body" + p
	}
	return ""
}

// echoedKey is the idempotency key a reply echoes in its idempotency header, ""
// when it does not echo one. Two different values count as a mismatch.
func (a *Adapter) echoedKey(rt *route, rp *reply, key string) string {
	if key == "" {
		return ""
	}
	vals := rp.header.Values(rt.op.ep.cfg.Settings.Idempotency.Header)
	switch len(vals) {
	case 0:
		return ""
	case 1:
		return vals[0]
	}
	for _, v := range vals[1:] {
		if v != vals[0] {
			return "mismatch-" + vals[0]
		}
	}
	return vals[0]
}

// result sends one operation that answers with a DeterministicResult and turns
// the reply into one: the document check and adapters.Normalise, the one place a
// reply becomes a fact.
func (a *Adapter) result(ctx context.Context, op adapters.Operation, capID string, rt *route, args callArgs) (actions.DeterministicResult, error) {
	commit := op == adapters.OpCommit
	w, rp, err := a.exchange(ctx, rt, args, commit)
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	body, err := a.checkedBody(ctx, rt, w, rp)
	if err != nil {
		return actions.DeterministicResult{}, afterReply(commit, err)
	}
	res, err := adapters.Normalise(body, adapters.Expect{
		Operation:            op,
		CapabilityID:         capID,
		IdempotencyKey:       args.key,
		EchoedIdempotencyKey: a.echoedKey(rt, rp, args.key),
		MaxBytes:             int(a.cfg.MaxResponseBytes),
	})
	if err != nil {
		if rp.status >= 500 {
			return actions.DeterministicResult{}, afterReply(commit, statusFailure(rp.status, ""))
		}
		return actions.DeterministicResult{}, afterReply(commit, err)
	}
	return res, nil
}

// document sends a management operation and returns the body of its answer,
// which must be a successful, declared and valid JSON reply.
func (a *Adapter) document(ctx context.Context, rt *route, args callArgs) ([]byte, error) {
	w, rp, err := a.exchange(ctx, rt, args, false)
	if err != nil {
		return nil, err
	}
	if rp.status < 200 || rp.status > 299 {
		return nil, statusFailure(rp.status, ReasonStatusNotDeclared)
	}
	return a.checkedBody(ctx, rt, w, rp)
}
