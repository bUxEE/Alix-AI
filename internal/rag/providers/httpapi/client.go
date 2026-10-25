package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Reasons carried in Details["reason"] of the typed errors of this package.
const (
	ReasonCredentialUnavailable = "credential_unavailable"
	ReasonCredentialMalformed   = "credential_malformed"
	ReasonRedirect              = "redirect_not_followed"
	ReasonHTTPStatus            = "http_status"
	ReasonEncoded               = "content_encoded"
	ReasonResponseTooLarge      = "response_too_large"
	ReasonMalformedPage         = "malformed_page"
	ReasonPagination            = "pagination"
	ReasonVersionChanged        = "version_changed"
)

type origin struct{ scheme, host, port string }

func originOf(u *url.URL) origin {
	o := origin{scheme: strings.ToLower(u.Scheme), host: strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")), port: u.Port()}
	if o.port == "" {
		o.port = map[string]string{"http": "80", "https": "443"}[o.scheme]
	}
	return o
}

// credentialError is what the credential round tripper fails with. Its text is
// a fixed reason and never anything the SecretProvider said.
type credentialError struct{ reason string }

func (e *credentialError) Error() string {
	return "the source's credential cannot be used: " + e.reason
}

// credTripper adds the source's credential to a request on its way out, and
// only to a request for the origin of the source's root. The credential exists
// only here, for the duration of the request: it is not in the URL, not in the
// request the provider built, not in an error and not in a log record.
type credTripper struct {
	next    http.RoundTripper
	origin  origin
	ref     string // "" when the source declares no credential
	secrets SecretProvider
	header  string
	scheme  string
	bare    bool
	log     *slog.Logger
	source  string
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
		t.log.Warn("the credential of a source cannot be used, so the request was not sent",
			slog.String("source_id", t.source), slog.String("reason", reason))
		return nil, &credentialError{reason: reason}
	}
	if t.secrets == nil {
		return fail(ReasonCredentialUnavailable)
	}
	sec, err := t.secrets.Secret(req.Context(), t.ref)
	if err != nil {
		// The provider's own error text is not passed on: it is not ours to
		// vouch for.
		return fail(ReasonCredentialUnavailable)
	}
	value := sec.Reveal()
	if value == "" || !validHeaderValue(value) {
		return fail(ReasonCredentialMalformed)
	}
	if !t.bare {
		value = t.scheme + " " + value
	}
	req = req.Clone(req.Context())
	req.Header.Set(t.header, value)
	return t.next.RoundTrip(req)
}

// CloseIdleConnections lets http.Client.CloseIdleConnections reach the
// transport.
func (t *credTripper) CloseIdleConnections() {
	if c, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// validHeaderValue reports whether v can be a header value: visible ASCII and
// spaces only, so a credential cannot inject a second header.
func validHeaderValue(v string) bool {
	for i := 0; i < len(v); i++ {
		if c := v[i]; c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// newClient builds the HTTP client of a call: the egress client of the source's
// application (every connection and request held to the allowlist, certificates
// verified, no proxy) behind the credential round tripper. It never follows a
// redirect: a redirect could lead outside the declared root, and a credential
// must not follow one.
func (p *Provider) newClient(s *session) *http.Client {
	var d *egress.Dialer
	if p.cfg.Egress != nil {
		d = p.cfg.Egress.Dialer(s.def.ApplicationID)
	}
	if d == nil {
		d = egress.NewDialer(nil) // default deny
	}
	base := d.HTTPClient(0)
	t := &credTripper{
		next: base.Transport, origin: s.root.origin(), secrets: p.cfg.Secrets,
		header: p.cfg.AuthHeader, scheme: p.cfg.AuthScheme, bare: p.cfg.AuthBare,
		log: p.log, source: s.def.SourceID,
	}
	if ref := s.def.Decl.CredentialRef; ref != nil {
		t.ref = string(*ref)
	}
	return &http.Client{
		Transport:     t,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// goneError is the answer "this item does not exist" (404 or 410). It is only a
// claim: callers settle it by checking that the list endpoint answers (see
// session.confirmGone) before they call anything deleted.
type goneError struct{ status int }

func (e *goneError) Error() string { return "the API answered that the item does not exist" }

// exchange is one request in flight: the response and the cancel function of
// its deadline. Close it when done.
type exchange struct {
	resp   *http.Response
	cancel context.CancelFunc
}

func (x *exchange) close() {
	if x == nil {
		return
	}
	if x.resp != nil && x.resp.Body != nil {
		_ = x.resp.Body.Close()
	}
	if x.cancel != nil {
		x.cancel()
	}
}

// send makes one request, bounded by timeout, and returns the response whatever
// its status; the caller decides what the status means. The request carries the
// credential (by the round tripper) and no body. A failure to send is a typed
// error that holds no URL.
func (s *session) send(ctx context.Context, method, rawURL string, timeout time.Duration) (*exchange, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	req, err := http.NewRequestWithContext(rctx, method, rawURL, nil)
	if err != nil {
		cancel()
		return nil, contracts.New(contracts.ErrInternal, "the request could not be built")
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", s.p.cfg.UserAgent)
	start := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		cancel()
		return nil, s.transportError(ctx, rctx, err)
	}
	s.p.log.Debug("httpapi request",
		slog.String("source_id", s.def.SourceID), slog.String("method", method),
		slog.Int("status", resp.StatusCode), slog.Duration("took", time.Since(start)))
	return &exchange{resp: resp, cancel: cancel}, nil
}

// transportError turns a failure of client.Do into a typed error. net/http puts
// the request URL, query string included, in its errors; StripURL removes it
// before the error is wrapped, so nothing downstream can log or return it.
func (s *session) transportError(parent, request context.Context, err error) error {
	err = egress.StripURL(err)
	var ce *contracts.Error
	var cred *credentialError
	switch {
	case egress.IsDenied(err):
		return err
	case errors.As(err, &cred):
		return contracts.New(contracts.ErrUnavailable, "the source's credential cannot be used, so no request was sent").
			WithDetails("reason", cred.reason)
	case parent.Err() != nil:
		return interrupted(parent.Err())
	case errors.Is(request.Err(), context.DeadlineExceeded):
		return contracts.New(contracts.ErrTimeout, "the API did not answer in time")
	case errors.As(err, &ce) && ce != nil:
		return ce
	}
	return unavailable("the API could not be reached", err)
}

// statusError maps a non-success status to a typed error. None of them is a
// deletion: only the item endpoints' 404 and 410 say "gone", and callers handle
// them before this.
func statusError(status int) *contracts.Error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return contracts.New(contracts.ErrPermissionDenied, "the application refused the source's credential or scope").
			WithDetails("reason", ReasonHTTPStatus).WithDetails("http_status", status)
	case status == http.StatusTooManyRequests:
		return contracts.New(contracts.ErrRateLimited, "the application is rate limiting the source").
			WithDetails("reason", ReasonHTTPStatus).WithDetails("http_status", status)
	case status >= 300 && status < 400:
		return contracts.New(contracts.ErrSchemaInvalid, "the application answered with a redirect, which the provider does not follow").
			WithDetails("reason", ReasonRedirect).WithDetails("http_status", status)
	case status >= 500:
		return contracts.New(contracts.ErrUnavailable, "the application answered with a server error").
			WithDetails("reason", ReasonHTTPStatus).WithDetails("http_status", status)
	case status == http.StatusNotFound || status == http.StatusGone:
		return contracts.New(contracts.ErrUnavailable, "the application does not serve the source's root").
			WithDetails("reason", ReasonHTTPStatus).WithDetails("http_status", status)
	}
	return contracts.New(contracts.ErrSchemaInvalid, "the application answered with a status the API contract does not describe").
		WithDetails("reason", ReasonHTTPStatus).WithDetails("http_status", status)
}

// checkEncoding refuses an encoded response: the provider asks for identity, so
// the length and the bytes it hashes are the item's own.
func checkEncoding(resp *http.Response) error {
	if enc := strings.TrimSpace(resp.Header.Get("Content-Encoding")); enc != "" && !strings.EqualFold(enc, "identity") {
		return contracts.New(contracts.ErrSchemaInvalid, "the application answered with an encoded body, which the provider did not ask for").
			WithDetails("reason", ReasonEncoded)
	}
	return nil
}

// fatalForScan reports whether a failure on one item says that the rest of the
// scan would fail the same way, so the scan stops instead of repeating it: a
// denied destination, a credential that cannot be used, a rate limit.
func fatalForScan(err error) bool {
	if egress.IsDenied(err) {
		return true
	}
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce == nil {
		return false
	}
	if ce.Code == contracts.ErrRateLimited {
		return true
	}
	r := ce.Details["reason"]
	return r == ReasonCredentialUnavailable || r == ReasonCredentialMalformed
}
