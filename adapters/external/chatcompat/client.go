package chatcompat

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

var (
	_ providers.MultimodalGenerator = (*Client)(nil)
	_ providers.CapabilityProvider  = (*Client)(nil)
	_ providers.AIRuntimeProvider   = (*Client)(nil)
)

type KeySource = external.ExtKeySource

type ServiceKeys struct {
	Service  *credentials.Service
	Provider string
	Scopes   []string
}

func (k ServiceKeys) Key(ctx context.Context, scope contracts.Scope, ref string) (credentials.Secret, error) {
	return external.ExtResolveKey(ctx, k.Provider, k.Service, k.Scopes, scope, ref)
}

type Client struct {
	provider string
	cfg      Config
	origin   *url.URL
	hc       *http.Client
	keys     KeySource
	clk      clock.Clock
	log      *slog.Logger
	sleep    func(ctx context.Context, d time.Duration) error
	jitter   func() float64
	model    modelProfile
	routes   routeSet
}

type Option func(*Client)

func WithClock(c clock.Clock) Option { return func(cl *Client) { cl.clk = c } }

func WithLogger(l *slog.Logger) Option { return func(cl *Client) { cl.log = l } }

func WithSleeper(s func(ctx context.Context, d time.Duration) error) Option {
	return func(cl *Client) { cl.sleep = s }
}

// New builds a client for provider (egress registry id and error attribution).
func New(provider string, cfg Config, dialer *egress.Dialer, keys KeySource, opts ...Option) (*Client, error) {
	if provider == "" {
		return nil, configError(provider, errors.New("a provider id is required"))
	}
	if err := cfg.Validate(); err != nil {
		return nil, configError(provider, err)
	}
	cfg = cfg.withDefaults()
	if dialer == nil || keys == nil {
		return nil, configError(provider, errors.New("an egress dialer and a key source are required"))
	}
	if _, err := egress.DestinationFromURL(cfg.BaseURL); err != nil {
		return nil, configError(provider, errors.New("the base URL must be a bare http(s) origin"))
	}
	origin, _ := url.Parse(cfg.BaseURL)
	cfg.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/")
	c := &Client{
		provider: provider, cfg: cfg, origin: origin, keys: keys,
		model:  cfg.profileFor(cfg.WireModel),
		jitter: rand.Float64,
		sleep:  external.Sleep,
		routes: newRouteSet(cfg.APIPrefix),
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
	hc.Transport = &external.ExtRouteGuard{Next: hc.Transport, Origin: origin, Routes: c.routes.all(), Err: external.ExtForbiddenRoute(provider)}
	c.hc = hc
	return c, nil
}

func (c *Client) Name() string { return c.provider }

func (c *Client) String() string {
	return "chatcompat.Client{" + c.provider + " " + c.cfg.BaseURL + " " + c.cfg.WireModel + "}"
}

type route = external.ExtRoute

type routeSet struct {
	chat, health, models route
}

func newRouteSet(prefix string) routeSet {
	return routeSet{
		chat:   route{Method: http.MethodPost, Path: prefix + "/chat/completions", Auth: true},
		health: route{Method: http.MethodGet, Path: prefix + "/models"},
		models: route{Method: http.MethodGet, Path: prefix + "/models", Auth: true},
	}
}

func (r routeSet) all() []route { return []route{r.chat, r.health, r.models} }

func (c *Client) AllowedEndpoints() []string {
	return []string{
		c.routes.chat.Method + " " + c.routes.chat.Path,
		c.routes.models.Method + " " + c.routes.models.Path,
	}
}

type reply struct {
	resp    *http.Response
	rate    *providers.RateLimitInfo
	traceID string
}

func (c *Client) call(ctx context.Context, scope contracts.Scope, r route, payload any, accept string) (*reply, error) {
	rep, err := external.ExtCaller{
		Provider: c.provider, BaseURL: c.cfg.BaseURL, HTTP: c.hc, Clock: c.clk, Log: c.log,
		Retry: c.cfg.Retry, Jitter: c.jitter, Sleep: c.sleep,
		Headers: func(ctx context.Context, scope contracts.Scope, r route, _ bool) (http.Header, error) {
			h := http.Header{}
			if r.Auth {
				key, err := external.ExtAPIKey(ctx, c.provider, c.keys, scope, c.cfg.CredentialRef)
				if err != nil {
					return nil, err
				}
				h.Set("Authorization", "Bearer "+key)
			}
			return h, nil
		},
		MapHTTP: func(status int, h http.Header, body []byte, info *providers.RateLimitInfo, now time.Time) (error, bool) {
			return mapHTTPError(c.provider, status, h, body, info, now)
		},
		RateInfo: rateInfo,
		TraceID:  traceFrom,
	}.Do(ctx, scope, r, payload, accept)
	if err != nil {
		return nil, err
	}
	return &reply{resp: rep.Resp, rate: rep.Rate, traceID: rep.TraceID}, nil
}

func (c *Client) logCall(ctx context.Context, r route, status int, started time.Time, err error, u *providers.CostUsage) {
	external.ExtLogCall(ctx, c.log, c.clk, c.provider, r, status, started, err, u)
}

func (c *Client) readBody(ctx context.Context, r io.Reader) ([]byte, error) {
	return external.ExtReadBody(ctx, c.provider, r, c.cfg.MaxResponseBytes)
}

func traceFrom(h http.Header) string { return external.ExtTraceFrom(h, "X-Request-Id", "Request-Id") }
