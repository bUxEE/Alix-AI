package external

// The HTTP client scaffolding every hosted-provider adapter shares (M9-04):
// the route guard, the refusal of redirects, the bounded call loop with its
// retry policy, the capped body readers, the transport error mapping, the
// provider_error_kind / sent details and the credential checks. Each adapter
// keeps its own wire format, auth header, route table and HTTP error mapping and
// plugs them in through ExtCaller's hooks.
//
// Every error built here carries two details besides its code:
//
//	provider_error_kind  the schema's ProviderErrorKind, so the router's sender
//	                     can classify the failure without guessing;
//	sent                 whether bytes of the request may have reached the
//	                     provider: false only for a refused dial, a refused
//	                     route, a missing credential and local validation.
//
// A response body or a credential is NEVER copied into a message, a detail or a
// log record: messages are fixed phrases.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// ExtMaxErrorBody bounds how much of a failure body is read for classification.
const ExtMaxErrorBody = 64 << 10

// ExtRoute is one call an adapter may make.
type ExtRoute struct {
	Method string
	Path   string
	// Query is the exact raw query of the route ("" for none). It is a constant:
	// no credential ever travels in it.
	Query string
	// Auth: the credential header is attached.
	Auth bool
}

// Target is the request URL of the route below base (no trailing slash).
func (r ExtRoute) Target(base string) string {
	t := base + r.Path
	if r.Query != "" {
		t += "?" + r.Query
	}
	return t
}

// ExtRefuseRedirects returns a 3xx to the caller (an unexpected status):
// following one could carry the credential and the prompt elsewhere. Set it as
// http.Client.CheckRedirect.
func ExtRefuseRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// ExtForbiddenRoute is the error a route guard returns for a refused request.
func ExtForbiddenRoute(provider string) error {
	return contracts.New(contracts.ErrPermissionDenied, provider+": request refused by the route allowlist").
		WithDetails("component", provider).WithDetails("sent", false)
}

// ExtRouteGuard refuses any request outside the route table or aimed at another
// origin before any I/O. The egress transport underneath (Next) enforces the
// destination allowlist, TLS verification and the no-proxy rule.
type ExtRouteGuard struct {
	Next   http.RoundTripper
	Origin *url.URL
	Routes []ExtRoute
	// Err is what a refused request returns (ExtForbiddenRoute(provider)).
	Err error
}

// Allowed reports whether the route table lists the request line exactly: same
// method, same path and same raw query, with no fragment, opaque form, escaped
// path or userinfo.
func (g *ExtRouteGuard) Allowed(method string, u *url.URL) bool {
	if u == nil || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" || u.User != nil {
		return false
	}
	for _, r := range g.Routes {
		if r.Method == method && r.Path == u.Path && r.Query == u.RawQuery {
			return true
		}
	}
	return false
}

// RoundTrip implements http.RoundTripper.
func (g *ExtRouteGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	if !g.Allowed(req.Method, req.URL) || !strings.EqualFold(req.URL.Scheme, g.Origin.Scheme) || !strings.EqualFold(req.URL.Host, g.Origin.Host) {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, g.Err
	}
	return g.Next.RoundTrip(req)
}

// CloseIdleConnections forwards to the transport underneath.
func (g *ExtRouteGuard) CloseIdleConnections() {
	if ci, ok := g.Next.(interface{ CloseIdleConnections() }); ok {
		ci.CloseIdleConnections()
	}
}

// Errors ------------------------------------------------------------------

// ExtAPIError builds a typed provider error with the two standard details. The
// message is a fixed phrase prefixed by the provider name.
func ExtAPIError(provider string, code contracts.ErrCode, kind pv.ProviderErrorKind, sent bool, msg string) *contracts.Error {
	return contracts.New(code, provider+": "+msg).
		WithDetails("provider_error_kind", string(kind)).WithDetails("sent", sent)
}

// ExtMalformed is a response that is not what the provider's API documents.
func ExtMalformed(provider, msg string) *contracts.Error {
	return ExtAPIError(provider, contracts.ErrSchemaInvalid, pv.ProviderErrorKindMalformedResponse, true, msg)
}

// ExtRateLimited builds the rate-limited error from the limit information: the
// shared providers.RateLimitError plus the two standard details.
func ExtRateLimited(rl providers.RateLimitInfo, kind pv.ProviderErrorKind) *contracts.Error {
	var ce *contracts.Error
	_ = errors.As(providers.RateLimitError(rl), &ce)
	return ce.WithDetails("provider_error_kind", string(kind)).WithDetails("sent", true)
}

// ExtMapTransportError converts a failure of the connection or of the context.
// Any URL in the error is stripped first, so a key that travelled in a URL
// could not reach a message either.
func ExtMapTransportError(ctx context.Context, provider string, err error) error {
	err = egress.StripURL(err)
	var ce *contracts.Error
	if errors.As(err, &ce) && ce != nil {
		cp := *ce
		cp.Details = map[string]any{}
		for k, v := range ce.Details {
			cp.Details[k] = v
		}
		cp.Details["sent"] = false
		if _, ok := cp.Details["provider_error_kind"]; !ok {
			cp.Details["provider_error_kind"] = string(pv.ProviderErrorKindPermissionDenied)
		}
		return &cp // an egress denial or a refused route: nothing left the machine
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		e := providers.ContextError(context.DeadlineExceeded)
		if ce, ok := e.(*contracts.Error); ok {
			return ce.WithDetails("provider_error_kind", string(pv.ProviderErrorKindTimeout)).WithDetails("sent", true)
		}
		return e
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		return providers.ContextError(context.Canceled)
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return ExtAPIError(provider, contracts.ErrUnavailable, pv.ProviderErrorKindUnavailable, false, "cannot connect to the provider")
	}
	return ExtAPIError(provider, contracts.ErrUnavailable, pv.ProviderErrorKindConnectionLost, true, "the connection to the provider failed")
}

// Credentials -------------------------------------------------------------

// ExtKeySource resolves the credential reference to the API key for ONE call,
// for the scope the call serves. *credentials.Service satisfies it through
// ExtResolveKey; tests use a fake. Nothing is cached, so a rotation or a
// revocation takes effect on the next call.
type ExtKeySource interface {
	Key(ctx context.Context, scope contracts.Scope, ref string) (credentials.Secret, error)
}

// ExtResolveKey resolves ref through the credential service, which audits every
// use and refuses a reference that is not the scope's, not for this provider,
// revoked, expired or quarantined.
func ExtResolveKey(ctx context.Context, provider string, svc *credentials.Service, scopes []string, scope contracts.Scope, ref string) (credentials.Secret, error) {
	if svc == nil {
		return "", contracts.New(contracts.ErrInternal, provider+": the credential service is not wired")
	}
	return svc.Resolve(ctx, scope, ref, credentials.Use{
		Provider: pv.ProviderID(provider), Scopes: scopes, Purpose: "inference",
	})
}

var extKeyCharsRE = regexp.MustCompile(`^[\x21-\x7e]{1,512}$`)

// ExtCredentialFailure wraps the error of a credential source: nothing is sent
// and no other credential or provider is tried. The source's error carries a
// fixed message and a reason code, never a value; it is kept as the cause so the
// reason survives.
func ExtCredentialFailure(provider string, err error) error {
	code := contracts.CodeOf(err)
	if code == contracts.ErrInternal && !contracts.HasCode(err, contracts.ErrInternal) {
		code = contracts.ErrPermissionDenied
	}
	return contracts.Wrap(code, err, provider+": the credential could not be used").
		WithDetails("provider_error_kind", string(pv.ProviderErrorKindAuthenticationFailed)).WithDetails("sent", false)
}

// ExtUnusableCredential is the refusal of a credential that cannot be put in a
// header. msg is a fixed phrase.
func ExtUnusableCredential(provider, msg string) error {
	return contracts.New(contracts.ErrPermissionDenied, provider+": "+msg).
		WithDetails("provider_error_kind", string(pv.ProviderErrorKindAuthenticationFailed)).WithDetails("sent", false)
}

// ExtAPIKey resolves the API key for one call and checks that it can travel in
// a header (printable ASCII, at most 512 bytes).
func ExtAPIKey(ctx context.Context, provider string, keys ExtKeySource, scope contracts.Scope, ref string) (string, error) {
	key, err := keys.Key(ctx, scope, ref)
	if err != nil {
		return "", ExtCredentialFailure(provider, err)
	}
	v := key.Reveal()
	if !extKeyCharsRE.MatchString(v) {
		return "", ExtUnusableCredential(provider, "the credential is not a usable API key")
	}
	return v, nil
}

// Bodies ------------------------------------------------------------------

// ExtReadBody reads a response body capped at max bytes. A longer body is a
// malformed response, never a truncated success.
func ExtReadBody(ctx context.Context, provider string, r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, ExtMapTransportError(ctx, provider, err)
	}
	if int64(len(b)) > max {
		return nil, ExtMalformed(provider, "the response exceeds the size limit")
	}
	return b, nil
}

// ExtDrain discards at most ExtMaxErrorBody bytes of a body.
func ExtDrain(r io.Reader) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r, ExtMaxErrorBody))
}

var extTraceIDRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// ExtTraceID reports whether v is shaped like a request id fit for a record.
func ExtTraceID(v string) bool { return extTraceIDRE.MatchString(v) }

// ExtTraceFrom returns the first header of names whose value is a request id.
func ExtTraceFrom(h http.Header, names ...string) string {
	for _, k := range names {
		if v := h.Get(k); extTraceIDRE.MatchString(v) {
			return v
		}
	}
	return ""
}

// Logging -----------------------------------------------------------------

// ExtLogCall writes one debug record: method, path, status, duration, the error
// code and tok_in/tok_out. Prompts, completions, keys and bodies are never
// logged.
func ExtLogCall(ctx context.Context, log *slog.Logger, clk clock.Clock, provider string, r ExtRoute, status int, started time.Time, err error, u *providers.CostUsage) {
	if log == nil || !log.Enabled(ctx, slog.LevelDebug) {
		return
	}
	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("path", r.Path),
		slog.Int("status", status),
		slog.Int64("dur_ms", clk.Now().Sub(started).Milliseconds()),
	}
	if err != nil {
		attrs = append(attrs, slog.String("code", string(contracts.CodeOf(err))))
	}
	if u != nil {
		attrs = append(attrs, slog.Int("tok_in", u.ToksIn), slog.Int("tok_out", u.ToksOut))
	}
	log.LogAttrs(ctx, slog.LevelDebug, provider+" request", attrs...)
}

// The call loop -----------------------------------------------------------

// ExtReply is a 200 response with what was learned from its headers.
type ExtReply struct {
	Resp    *http.Response
	Rate    *providers.RateLimitInfo
	TraceID string
}

// ExtCaller sends one request through an adapter's HTTP client, retrying a
// refusal the provider made before processing it within the bounded policy. The
// adapter supplies what differs: the headers (credential included), the HTTP
// error mapping and the optional rate-limit and trace readers.
type ExtCaller struct {
	Provider string
	BaseURL  string
	HTTP     *http.Client
	Clock    clock.Clock
	Log      *slog.Logger
	Retry    RetryPolicy
	Jitter   func() float64
	Sleep    func(ctx context.Context, d time.Duration) error

	// Headers returns the provider-specific headers of one attempt, credential
	// included for an authenticated route. It runs before the request is built,
	// so a missing credential sends nothing. forceRefresh is true only on the
	// attempt right after RefreshOn matched.
	Headers func(ctx context.Context, scope contracts.Scope, r ExtRoute, forceRefresh bool) (http.Header, error)
	// MapHTTP builds the typed error for a non-200 answer and says whether a
	// bounded retry may follow. info is what RateInfo read (nil without it).
	MapHTTP func(status int, h http.Header, body []byte, info *providers.RateLimitInfo, now time.Time) (error, bool)
	// RateInfo reads the provider's rate-limit headers (optional).
	RateInfo func(h http.Header, now time.Time) *providers.RateLimitInfo
	// TraceID reads the request id from the headers of a 200 (optional).
	TraceID func(h http.Header) string
	// RefreshOn (optional) names the status after which the credential is
	// refreshed once: the attempt is repeated with forceRefresh and consumes no
	// retry slot.
	RefreshOn func(status int) bool
}

// Do sends one request. payload is JSON-encoded when non-nil. It returns the
// response only when the status is 200; anything else is a mapped error with the
// body released. The caller closes Resp.Body.
func (c ExtCaller) Do(ctx context.Context, scope contracts.Scope, r ExtRoute, payload any, accept string) (*ExtReply, error) {
	var body []byte
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, contracts.Wrap(contracts.ErrInternal, err, c.Provider+": cannot encode the request").WithDetails("sent", false)
		}
		body = b
	}
	refreshed, force := false, false
	for attempt := 0; ; attempt++ {
		var extra http.Header
		if c.Headers != nil {
			h, err := c.Headers(ctx, scope, r, force)
			force = false // only the attempt right after a refreshable status forces a refresh
			if err != nil {
				return nil, err
			}
			extra = h
		}
		req, err := http.NewRequestWithContext(ctx, r.Method, r.Target(c.BaseURL), bytes.NewReader(body))
		if err != nil {
			return nil, contracts.Wrap(contracts.ErrInternal, err, c.Provider+": cannot build the request").WithDetails("sent", false)
		}
		req.Header.Set("Accept", accept)
		req.Header.Set("User-Agent", "alixai-"+c.Provider+"-adapter")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, vs := range extra {
			for _, v := range vs {
				req.Header.Set(k, v)
			}
		}
		started := c.Clock.Now()
		resp, err := c.HTTP.Do(req)
		if err != nil {
			mapped := ExtMapTransportError(ctx, c.Provider, err)
			ExtLogCall(ctx, c.Log, c.Clock, c.Provider, r, 0, started, mapped, nil)
			return nil, mapped
		}
		now := c.Clock.Now()
		var info *providers.RateLimitInfo
		if c.RateInfo != nil {
			info = c.RateInfo(resp.Header, now)
		}
		if resp.StatusCode == http.StatusOK {
			ExtLogCall(ctx, c.Log, c.Clock, c.Provider, r, resp.StatusCode, started, nil, nil)
			rep := &ExtReply{Resp: resp, Rate: info}
			if c.TraceID != nil {
				rep.TraceID = c.TraceID(resp.Header)
			}
			return rep, nil
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, ExtMaxErrorBody))
		_ = resp.Body.Close()
		mapped, retryable := c.MapHTTP(resp.StatusCode, resp.Header, b, info, now)
		ExtLogCall(ctx, c.Log, c.Clock, c.Provider, r, resp.StatusCode, started, mapped, nil)
		if c.RefreshOn != nil && c.RefreshOn(resp.StatusCode) && !refreshed {
			// The access token was revoked or rotated provider-side: ask for a
			// fresh one, once. This is not a retry of a refused request in the
			// sense of the policy, so it does not consume one.
			refreshed, force = true, true
			attempt--
			continue
		}
		if !retryable {
			return nil, mapped
		}
		var after time.Duration
		if rl, ok := providers.RateLimitFrom(mapped); ok {
			after = rl.RetryAfter
		} else if ra, ok := providers.ParseRetryAfter(resp.Header.Get("Retry-After"), now); ok {
			after = ra
		}
		d, ok := c.Retry.Delay(attempt, after, c.Jitter)
		if !ok {
			return nil, mapped
		}
		if err := c.Sleep(ctx, d); err != nil {
			// The wait was cut short: the answer is the refusal, which nothing processed.
			return nil, mapped
		}
	}
}
