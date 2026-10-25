package wire

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// ErrUnsupportedRuntime is wrapped by an adapter error when the selected
// runtime or backend cannot host the scorer. The adapters turn it into the
// unsupported_runtime fallback reason; nothing here assumes a runtime supports
// anything.
var ErrUnsupportedRuntime = errors.New("decision adapter: the selected runtime does not support the scorer")

// ErrForbiddenEndpoint is the sentinel behind a request the allowlist refused.
var ErrForbiddenEndpoint = errors.New("decision wire: request refused by the endpoint allowlist")

// Route is one callable route of an adapter.
type Route struct {
	Method string
	Path   string
	// Auth attaches the API key. A route without it is called anonymously.
	Auth bool
}

// Options configures a Client.
type Options struct {
	// Who prefixes error messages ("localai decisions", "clm").
	Who      string
	Endpoint Endpoint
	TLS      *tls.Config
	Key      Key
	// Routes is the adapter's complete allowlist.
	Routes []Route
	// Timeout bounds one call on top of the caller's deadline.
	Timeout time.Duration
	// MaxResponse caps a response body, in bytes.
	MaxResponse int64
	Logger      *slog.Logger
}

// Client is the allowlisted HTTP client of one adapter. It is safe for
// concurrent use.
type Client struct {
	who     string
	scheme  string
	host    string
	timeout time.Duration
	maxResp int64
	routes  []Route
	hc      *http.Client
	log     *slog.Logger
}

// NewClient builds the client. Every request it sends goes through the guard.
func NewClient(o Options) *Client {
	scheme := o.Endpoint.RequestScheme()
	c := &Client{
		who: o.Who, scheme: scheme, host: o.Endpoint.Host, timeout: o.Timeout, maxResp: o.MaxResponse,
		routes: append([]Route(nil), o.Routes...), log: o.Logger,
	}
	c.hc = &http.Client{
		Transport:     &guard{next: newTransport(o.Endpoint, o.TLS), scheme: scheme, host: o.Endpoint.Host, routes: c.routes, key: o.Key},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	return c
}

// AllowedEndpoints lists the allowlist as "METHOD /path".
func (c *Client) AllowedEndpoints() []string { return Describe(c.routes) }

// Describe formats routes as "METHOD /path".
func Describe(routes []Route) []string {
	out := make([]string, len(routes))
	for i, r := range routes {
		out[i] = r.Method + " " + r.Path
	}
	return out
}

// Response is a complete, size-capped answer. Body is for decoding only and must
// never be copied into an error or a log record.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Do sends one request and returns the response whatever its status. A transport
// failure, a refused route or an oversized body is a mapped *contracts.Error. The
// body never appears in an error or a log.
func (c *Client) Do(ctx context.Context, method, path string, body []byte) (*Response, error) {
	started := time.Now()
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.scheme+"://"+c.host+path, rd)
	if err != nil {
		return nil, contracts.New(contracts.ErrInternal, c.who+": cannot build the request")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "alixai-decision-adapter")
	resp, err := c.hc.Do(req)
	if err != nil {
		mapped := c.mapTransportError(ctx, err)
		c.logCall(ctx, method, path, 0, started, mapped)
		return nil, mapped
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResp+1))
	if err != nil {
		mapped := c.mapTransportError(ctx, err)
		c.logCall(ctx, method, path, resp.StatusCode, started, mapped)
		return nil, mapped
	}
	if int64(len(data)) > c.maxResp {
		mapped := contracts.New(contracts.ErrSchemaInvalid, c.who+": response exceeds the size limit")
		c.logCall(ctx, method, path, resp.StatusCode, started, mapped)
		return nil, mapped
	}
	c.logCall(ctx, method, path, resp.StatusCode, started, nil)
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: data}, nil
}

// logCall records method, path, status and duration; never a body, a header or the key.
func (c *Client) logCall(ctx context.Context, method, path string, status int, started time.Time, err error) {
	if !c.log.Enabled(ctx, slog.LevelDebug) {
		return
	}
	attrs := []slog.Attr{
		slog.String("method", method), slog.String("path", path), slog.Int("status", status),
		slog.Int64("dur_ms", time.Since(started).Milliseconds()),
	}
	if err != nil {
		attrs = append(attrs, slog.String("code", string(contracts.CodeOf(err))))
	}
	c.log.LogAttrs(ctx, slog.LevelDebug, c.who+" request", attrs...)
}

func (c *Client) mapTransportError(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		if errors.Is(cerr, context.DeadlineExceeded) {
			return contracts.Wrap(contracts.ErrTimeout, cerr, c.who+": deadline exceeded")
		}
		return contracts.Wrap(contracts.ErrUnavailable, cerr, c.who+": request canceled")
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return contracts.Wrap(contracts.ErrTimeout, err, c.who+": deadline exceeded")
	case errors.Is(err, context.Canceled):
		return contracts.Wrap(contracts.ErrUnavailable, err, c.who+": request canceled")
	case errors.Is(err, ErrForbiddenEndpoint):
		return contracts.Wrap(contracts.ErrInternal, ErrForbiddenEndpoint, c.who+": request refused by the endpoint allowlist")
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return contracts.New(contracts.ErrUnavailable, c.who+": the runtime's TLS certificate was not accepted")
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return contracts.New(contracts.ErrTimeout, c.who+": network timeout")
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return contracts.New(contracts.ErrUnavailable, c.who+": the runtime refused the connection")
	}
	// err is dropped, not wrapped: a net/http error carries the URL, never a body, but
	// the rule is that nothing from the exchange is echoed.
	return contracts.New(contracts.ErrUnavailable, c.who+": the runtime is unreachable")
}

// guard is the single choke point every request passes through. It refuses
// anything outside the allowlist or aimed at another scheme or host before any
// I/O and attaches the key itself, so the key can only travel to the configured
// host on an authenticated allowlisted route.
type guard struct {
	next   http.RoundTripper
	scheme string
	host   string
	routes []Route
	key    Key
}

func (g *guard) lookup(method string, u *url.URL) (Route, bool) {
	if u == nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" || u.User != nil {
		return Route{}, false
	}
	for _, r := range g.routes {
		if r.Method == method && r.Path == u.Path {
			return r, true
		}
	}
	return Route{}, false
}

func (g *guard) RoundTrip(req *http.Request) (*http.Response, error) {
	r, ok := g.lookup(req.Method, req.URL)
	if !ok || !strings.EqualFold(req.URL.Scheme, g.scheme) || !strings.EqualFold(req.URL.Host, g.host) {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, ErrForbiddenEndpoint
	}
	out := req.Clone(req.Context())
	out.Header.Del("Authorization")
	if r.Auth && g.key.IsSet() {
		out.Header.Set("Authorization", "Bearer "+g.key.reveal())
	}
	return g.next.RoundTrip(out)
}
