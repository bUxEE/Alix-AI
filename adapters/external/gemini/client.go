package gemini

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"time"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Compile-time proof that the client serves the roles it claims. There is no
// Embedder: embeddings are not part of this adapter.
var (
	_ providers.MultimodalGenerator = (*Client)(nil)
	_ providers.CapabilityProvider  = (*Client)(nil)
	_ providers.AIRuntimeProvider   = (*Client)(nil)
)

// Client talks to the Gemini API. It is safe for concurrent use.
type Client struct {
	cfg    Config
	origin *url.URL
	hc     *http.Client
	keys   KeySource
	tokens TokenSource
	clk    clock.Clock
	log    *slog.Logger
	sleep  func(ctx context.Context, d time.Duration) error
	jitter func() float64
	model  modelProfile
	sigs   *sigCache
	ids    idSource
	// modelRoute is GET /v1beta/models/<configured model>.
	modelRoute route
}

// Option adjusts a Client under construction.
type Option func(*Client)

// WithClock injects the time source used for timings and rate-limit resets.
func WithClock(c clock.Clock) Option { return func(cl *Client) { cl.clk = c } }

// WithLogger sets the logger that receives debug records: method, path, status,
// duration and tok_in/tok_out. Prompts, completions, keys and tokens are never
// logged.
func WithLogger(l *slog.Logger) Option { return func(cl *Client) { cl.log = l } }

// WithSleeper replaces how retries wait (tests inject a recorder).
func WithSleeper(s func(ctx context.Context, d time.Duration) error) Option {
	return func(cl *Client) { cl.sleep = s }
}

// WithTokenSource supplies the OAuth access tokens (Config.Auth is AuthOAuth).
func WithTokenSource(t TokenSource) Option { return func(cl *Client) { cl.tokens = t } }

// New builds a client. dialer is the provider's egress dialer
// (egress.ProviderRegistry.Dialer("gemini")): the request goes only where its
// allowlist says, with certificates verified and no proxy. keys may be nil
// for an OAuth connection; an OAuth connection needs WithTokenSource. Nothing
// is sent during construction.
func New(cfg Config, dialer *egress.Dialer, keys KeySource, opts ...Option) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, configError(err)
	}
	cfg = cfg.withDefaults()
	if dialer == nil {
		return nil, configError(errors.New("an egress dialer is required"))
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
		sigs:       newSigCache(sigCacheSize),
		modelRoute: route{Method: http.MethodGet, Path: apiVersionPath + "/models/" + cfg.Model, Auth: true},
	}
	for _, o := range opts {
		o(c)
	}
	switch cfg.Auth {
	case AuthOAuth:
		if c.tokens == nil {
			return nil, configError(errors.New("an OAuth connection needs a token source"))
		}
	default:
		if c.keys == nil {
			return nil, configError(errors.New("an API key connection needs a key source"))
		}
	}
	if c.clk == nil {
		c.clk = clock.Real{}
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	hc := dialer.HTTPClient(0)
	hc.CheckRedirect = external.ExtRefuseRedirects
	hc.Transport = &external.ExtRouteGuard{Next: hc.Transport, Origin: origin, Routes: c.allRoutes(), Err: external.ExtForbiddenRoute(ProviderName)}
	c.hc = hc
	return c, nil
}

// Name implements providers.AIRuntimeProvider.
func (*Client) Name() string { return ProviderName }

// Auth reports how this connection authenticates.
func (c *Client) Auth() AuthKind { return c.cfg.Auth }

// String describes the client without any credential.
func (c *Client) String() string {
	return "gemini.Client{" + c.cfg.BaseURL + " " + c.cfg.Model + " " + string(c.cfg.Auth) + "}"
}

// Endpoint allowlist ------------------------------------------------------

// route is one call the client may make. Query is the exact raw query of the
// route ("" for none): only the stream route has one, and it is a constant, so
// no credential ever travels in it. Auth: the credential is attached.
type route = external.ExtRoute

const streamQuery = "alt=sse"

// Static routes: the model list, used by the anonymous health probe.
var modelsRoute = route{Method: http.MethodGet, Path: apiVersionPath + "/models"}

// generateRoute is the generation route of one model. The model id is in the
// path, so the table holds the configured model and the Config.Profiles models
// and nothing else. Cached contents, files, tuned models, batches, the live
// API, embeddings and every other method are absent, so provider-native
// agents, hosted memory and file search are unreachable.
func generateRoute(model string, stream bool) route {
	if stream {
		return route{Method: http.MethodPost, Path: apiVersionPath + "/models/" + model + ":streamGenerateContent", Query: streamQuery, Auth: true}
	}
	return route{Method: http.MethodPost, Path: apiVersionPath + "/models/" + model + ":generateContent", Auth: true}
}

func (c *Client) modelIDs() []string {
	set := map[string]bool{c.cfg.Model: true}
	for _, v := range c.cfg.Profiles {
		set[v] = true
	}
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

func (c *Client) allRoutes() []route {
	rs := []route{modelsRoute, c.modelRoute}
	for _, m := range c.modelIDs() {
		rs = append(rs, generateRoute(m, false), generateRoute(m, true))
	}
	return rs
}

// AllowedEndpoints lists the routes the client may call, as "METHOD /path".
func AllowedEndpoints() []string {
	p := apiVersionPath + "/models/"
	return []string{
		"POST " + p + "{configured model}:generateContent",
		"POST " + p + "{configured model}:streamGenerateContent?" + streamQuery,
		"GET " + p[:len(p)-1],
		"GET " + p + "{configured model}",
	}
}

// Requests ----------------------------------------------------------------

var credCharsRE = regexp.MustCompile(`^[\x21-\x7e]+$`)

// credential resolves the secret for one attempt and returns the header that
// carries it. The value goes into a header of an allowlisted route and nowhere
// else: never a URL, a body, a log or an error.
func (c *Client) credential(ctx context.Context, scope contracts.Scope, forceRefresh bool) (name, value string, err error) {
	var (
		secret interface{ Reveal() string }
		rerr   error
	)
	if c.cfg.Auth == AuthOAuth {
		s, e := c.tokens.Bearer(ctx, scope, c.cfg.CredentialRef, c.cfg.OAuthScopes, forceRefresh)
		secret, rerr = s, e
		name = "Authorization"
	} else {
		s, e := c.keys.Key(ctx, scope, c.cfg.CredentialRef)
		secret, rerr = s, e
		name = "x-goog-api-key"
	}
	if rerr != nil {
		return "", "", external.ExtCredentialFailure(ProviderName, rerr)
	}
	v := secret.Reveal()
	if len(v) > 4096 || !credCharsRE.MatchString(v) {
		return "", "", external.ExtUnusableCredential(ProviderName, "the credential is not usable")
	}
	if c.cfg.Auth == AuthOAuth {
		v = "Bearer " + v
	}
	return name, v, nil
}

// reply is a 200 response.
type reply struct {
	resp *http.Response
}

// call sends one request, retrying a refusal the provider made before
// processing it (429 rate limit, 503 unavailable) within the bounded policy,
// and, for an OAuth connection, refreshing the access token once after a 401.
// It returns the response only when the status is 200; anything else is a
// mapped error with the body released. The caller closes reply.resp.Body.
func (c *Client) call(ctx context.Context, scope contracts.Scope, r route, payload any, accept string) (*reply, error) {
	rep, err := external.ExtCaller{
		Provider: ProviderName, BaseURL: c.cfg.BaseURL, HTTP: c.hc, Clock: c.clk, Log: c.log,
		Retry: c.cfg.Retry, Jitter: c.jitter, Sleep: c.sleep,
		Headers: func(ctx context.Context, scope contracts.Scope, r route, force bool) (http.Header, error) {
			h := http.Header{}
			if r.Auth {
				name, value, err := c.credential(ctx, scope, force)
				if err != nil {
					return nil, err
				}
				h.Set(name, value)
				if c.cfg.Auth == AuthOAuth && c.cfg.UserProject != "" {
					h.Set("x-goog-user-project", c.cfg.UserProject)
				}
			}
			return h, nil
		},
		MapHTTP: func(status int, h http.Header, body []byte, _ *providers.RateLimitInfo, now time.Time) (error, bool) {
			return mapHTTPError(status, h, body, now)
		},
		// The access token was revoked or rotated provider-side: the engine is
		// asked for a fresh one, once.
		RefreshOn: func(status int) bool { return status == http.StatusUnauthorized && c.cfg.Auth == AuthOAuth },
	}.Do(ctx, scope, r, payload, accept)
	if err != nil {
		return nil, err
	}
	return &reply{resp: rep.Resp}, nil
}

func (c *Client) logCall(ctx context.Context, r route, status int, started time.Time, err error, u *providers.CostUsage) {
	external.ExtLogCall(ctx, c.log, c.clk, ProviderName, r, status, started, err, u)
}

// readBody reads a capped response body.
func (c *Client) readBody(ctx context.Context, r io.Reader) ([]byte, error) {
	return external.ExtReadBody(ctx, ProviderName, r, c.cfg.MaxResponseBytes)
}
