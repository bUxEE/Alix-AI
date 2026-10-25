package openai

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"time"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Compile-time proof that the client serves the roles it claims.
var (
	_ providers.MultimodalGenerator = (*Client)(nil)
	_ providers.CapabilityProvider  = (*Client)(nil)
	_ providers.AIRuntimeProvider   = (*Client)(nil)
	_ providers.Embedder            = (*Client)(nil)
)

// KeySource resolves the credential reference to the API key for ONE call, for
// the scope the call serves. *credentials.Service satisfies it through
// ServiceKeys; tests use a fake. Nothing is cached here, so a rotation or a
// revocation takes effect on the next call.
type KeySource = external.ExtKeySource

// ServiceKeys adapts the credential service to KeySource. Every use is audited
// by the service, which also refuses a reference that is not the scope's, not
// for this provider, revoked, expired or quarantined.
type ServiceKeys struct {
	Service *credentials.Service
	// Scopes are the provider scopes the calls need (usually none).
	Scopes []string
}

// Key implements KeySource.
func (k ServiceKeys) Key(ctx context.Context, scope contracts.Scope, ref string) (credentials.Secret, error) {
	return external.ExtResolveKey(ctx, ProviderName, k.Service, k.Scopes, scope, ref)
}

// Client talks to the OpenAI API. It is safe for concurrent use.
type Client struct {
	cfg    Config
	origin *url.URL
	hc     *http.Client
	keys   KeySource
	clk    clock.Clock
	log    *slog.Logger
	sleep  func(ctx context.Context, d time.Duration) error
	jitter func() float64
	model  modelProfile
}

// Option adjusts a Client under construction.
type Option func(*Client)

// WithClock injects the time source used for timings and retry-after dates.
func WithClock(c clock.Clock) Option { return func(cl *Client) { cl.clk = c } }

// WithLogger sets the logger that receives debug records: method, path, status,
// duration and tok_in/tok_out. Prompts, completions and keys are never logged.
func WithLogger(l *slog.Logger) Option { return func(cl *Client) { cl.log = l } }

// WithSleeper replaces how retries wait (tests inject a recorder).
func WithSleeper(s func(ctx context.Context, d time.Duration) error) Option {
	return func(cl *Client) { cl.sleep = s }
}

// New builds a client. dialer is the provider's egress dialer
// (egress.ProviderRegistry.Dialer("openai")): the request goes only where its
// allowlist says, with certificates verified and no proxy. Nothing is sent
// during construction.
func New(cfg Config, dialer *egress.Dialer, keys KeySource, opts ...Option) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, configError(err)
	}
	cfg = cfg.withDefaults()
	if dialer == nil || keys == nil {
		return nil, configError(errors.New("an egress dialer and a key source are required"))
	}
	if _, err := egress.DestinationFromURL(cfg.BaseURL); err != nil {
		return nil, configError(errors.New("the base URL must be a bare http(s) origin"))
	}
	origin, _ := url.Parse(cfg.BaseURL)
	c := &Client{
		cfg: cfg, origin: origin, keys: keys,
		model:  cfg.profileFor(cfg.Model),
		jitter: rand.Float64,
		sleep:  external.Sleep,
	}
	for _, o := range opts {
		o(c)
	}
	if c.clk == nil {
		c.clk = clock.Real{}
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	hc := dialer.HTTPClient(0)
	hc.CheckRedirect = external.ExtRefuseRedirects
	hc.Transport = &external.ExtRouteGuard{Next: hc.Transport, Origin: origin, Routes: routes, Err: external.ExtForbiddenRoute(ProviderName)}
	c.hc = hc
	return c, nil
}

// Name implements providers.AIRuntimeProvider.
func (*Client) Name() string { return ProviderName }

// String describes the client without any credential.
func (c *Client) String() string { return "openai.Client{" + c.cfg.BaseURL + " " + c.cfg.Model + "}" }

// Endpoint allowlist ------------------------------------------------------

// route is one call the client may make; Auth: the Authorization header is
// attached.
type route = external.ExtRoute

// Routes: the complete set of calls the client can make. Responses is the only
// generation route and carries every parameter in the body (the model id never
// appears in a path). Assistants, threads, conversations, vector stores, files,
// batches, background responses, realtime and fine-tuning are absent, so
// provider-native agents, hosted memory and file search are unreachable.
var routes = []route{
	{Method: http.MethodPost, Path: "/v1/responses", Auth: true},
	{Method: http.MethodPost, Path: "/v1/embeddings", Auth: true},
	{Method: http.MethodGet, Path: "/v1/models", Auth: true},
}

// healthRoute is the unauthenticated reachability probe: the same path as
// model listing, sent without a key (the answer is 401, which proves the API is
// reachable and the destination allowed).
var healthRoute = route{Method: http.MethodGet, Path: "/v1/models"}

// AllowedEndpoints lists the routes the client may call, as "METHOD /path".
func AllowedEndpoints() []string {
	out := make([]string, len(routes))
	for i, r := range routes {
		out[i] = r.Method + " " + r.Path
	}
	return out
}

// Requests ----------------------------------------------------------------

// reply is a 200 response with what was learned from its headers.
type reply struct {
	resp    *http.Response
	rate    *providers.RateLimitInfo
	traceID string
}

// call sends one request, retrying a refusal the provider made before
// processing it (429 rate limit, 503 overload) within the bounded policy. It
// returns the response only when the status is 200; anything else is a mapped
// error with the body released. The caller closes reply.resp.Body.
func (c *Client) call(ctx context.Context, scope contracts.Scope, r route, payload any, accept string) (*reply, error) {
	rep, err := external.ExtCaller{
		Provider: ProviderName, BaseURL: c.cfg.BaseURL, HTTP: c.hc, Clock: c.clk, Log: c.log,
		Retry: c.cfg.Retry, Jitter: c.jitter, Sleep: c.sleep,
		Headers: func(ctx context.Context, scope contracts.Scope, r route, _ bool) (http.Header, error) {
			h := http.Header{}
			if r.Auth {
				key, err := external.ExtAPIKey(ctx, ProviderName, c.keys, scope, c.cfg.CredentialRef)
				if err != nil {
					return nil, err
				}
				h.Set("Authorization", "Bearer "+key)
			}
			return h, nil
		},
		MapHTTP:  mapHTTPError,
		RateInfo: rateInfo,
		TraceID:  traceFrom,
	}.Do(ctx, scope, r, payload, accept)
	if err != nil {
		return nil, err
	}
	return &reply{resp: rep.Resp, rate: rep.Rate, traceID: rep.TraceID}, nil
}

func (c *Client) logCall(ctx context.Context, r route, status int, started time.Time, err error, u *providers.CostUsage) {
	external.ExtLogCall(ctx, c.log, c.clk, ProviderName, r, status, started, err, u)
}

// readBody reads a capped response body.
func (c *Client) readBody(ctx context.Context, r io.Reader) ([]byte, error) {
	return external.ExtReadBody(ctx, ProviderName, r, c.cfg.MaxResponseBytes)
}

func traceFrom(h http.Header) string { return external.ExtTraceFrom(h, "X-Request-Id") }
