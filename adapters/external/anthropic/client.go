package anthropic

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

// Compile-time proof that the client serves the roles it claims. There is no
// Embedder: the vendor offers no embeddings endpoint.
var (
	_ providers.MultimodalGenerator = (*Client)(nil)
	_ providers.CapabilityProvider  = (*Client)(nil)
	_ providers.AIRuntimeProvider   = (*Client)(nil)
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

// Client talks to the Anthropic API. It is safe for concurrent use.
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
	// modelRoute is GET /v1/models/<configured model>, the one parameterised
	// route; it is built once, from a validated model id.
	modelRoute route
}

// Option adjusts a Client under construction.
type Option func(*Client)

// WithClock injects the time source used for timings and rate-limit resets.
func WithClock(c clock.Clock) Option { return func(cl *Client) { cl.clk = c } }

// WithLogger sets the logger that receives debug records: method, path, status,
// duration and tok_in/tok_out. Prompts, completions and keys are never logged.
func WithLogger(l *slog.Logger) Option { return func(cl *Client) { cl.log = l } }

// WithSleeper replaces how retries wait (tests inject a recorder).
func WithSleeper(s func(ctx context.Context, d time.Duration) error) Option {
	return func(cl *Client) { cl.sleep = s }
}

// New builds a client. dialer is the provider's egress dialer
// (egress.ProviderRegistry.Dialer("anthropic")): the request goes only where
// its allowlist says, with certificates verified and no proxy. Nothing is sent
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
		model:      cfg.profileFor(cfg.Model),
		jitter:     rand.Float64,
		sleep:      external.Sleep,
		modelRoute: route{Method: http.MethodGet, Path: "/v1/models/" + cfg.Model, Auth: true},
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
	hc.Transport = &external.ExtRouteGuard{
		Next: hc.Transport, Origin: origin, Err: external.ExtForbiddenRoute(ProviderName),
		Routes: append(append([]route(nil), routes...), c.modelRoute),
	}
	c.hc = hc
	return c, nil
}

// Name implements providers.AIRuntimeProvider.
func (*Client) Name() string { return ProviderName }

// String describes the client without any credential.
func (c *Client) String() string {
	return "anthropic.Client{" + c.cfg.BaseURL + " " + c.cfg.Model + "}"
}

// Endpoint allowlist ------------------------------------------------------

// route is one call the client may make; Auth: the x-api-key header is attached.
type route = external.ExtRoute

// Routes: the static part of the set of calls the client can make. Messages is
// the only generation route and carries every parameter in the body (the model
// id never appears in a path). Batches, files, skills, agents, sessions, memory
// stores, vaults and the admin API are absent, so provider-native agents,
// hosted memory and file search are unreachable. The one parameterised route,
// GET /v1/models/<configured model>, is added in New.
var routes = []route{
	{Method: http.MethodPost, Path: "/v1/messages", Auth: true},
	{Method: http.MethodGet, Path: "/v1/models"},
}

// modelRouteTemplate names the parameterised route in AllowedEndpoints.
const modelRouteTemplate = "GET /v1/models/{configured model}"

// AllowedEndpoints lists the routes the client may call, as "METHOD /path".
func AllowedEndpoints() []string {
	out := make([]string, 0, len(routes)+1)
	for _, r := range routes {
		out = append(out, r.Method+" "+r.Path)
	}
	return append(out, modelRouteTemplate)
}

// Requests ----------------------------------------------------------------

// reply is a 200 response with what was learned from its headers.
type reply struct {
	resp    *http.Response
	rate    *providers.RateLimitInfo
	traceID string
}

// call sends one request, retrying a refusal the provider made before
// processing it (429 rate limit, 503 and 529 overload) within the bounded
// policy. It returns the response only when the status is 200; anything else is
// a mapped error with the body released. The caller closes reply.resp.Body.
func (c *Client) call(ctx context.Context, scope contracts.Scope, r route, payload any, accept string) (*reply, error) {
	rep, err := external.ExtCaller{
		Provider: ProviderName, BaseURL: c.cfg.BaseURL, HTTP: c.hc, Clock: c.clk, Log: c.log,
		Retry: c.cfg.Retry, Jitter: c.jitter, Sleep: c.sleep,
		Headers: func(ctx context.Context, scope contracts.Scope, r route, _ bool) (http.Header, error) {
			h := http.Header{}
			h.Set("anthropic-version", apiVersion) // never anthropic-beta: no beta feature is used
			if r.Auth {
				key, err := external.ExtAPIKey(ctx, ProviderName, c.keys, scope, c.cfg.CredentialRef)
				if err != nil {
					return nil, err
				}
				h.Set("x-api-key", key)
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

func traceFrom(h http.Header) string { return external.ExtTraceFrom(h, "Request-Id") }
