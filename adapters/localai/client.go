package localai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// ProviderName is what Name reports and what the registry labels this runtime.
const ProviderName = "localai"

const (
	pathReady  = "/readyz"
	pathVer    = "/version"
	pathModels = "/v1/models"
	pathChat   = "/v1/chat/completions"
	pathEmbed  = "/v1/embeddings"
)

// Compile-time proof that the client serves the roles it claims.
var (
	_ providers.TextGenerator     = (*Client)(nil)
	_ providers.Embedder          = (*Client)(nil)
	_ providers.AIRuntimeProvider = (*Client)(nil)
)

// Client talks to one LocalAI runtime. It is safe for concurrent use.
type Client struct {
	cfg       Config
	spec      endpointSpec
	reqScheme string
	hc        *http.Client
	clk       clock.Clock
	log       *slog.Logger
}

// Option adjusts a Client under construction.
type Option func(*options)

type options struct {
	clk  clock.Clock
	log  *slog.Logger
	key  *Secret
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// WithClock injects the time source used for timings.
func WithClock(c clock.Clock) Option { return func(o *options) { o.clk = c } }

// WithLogger sets the logger that receives debug records (method, path,
// status, duration, tok_in and tok_out). Prompts, completions and the key are
// never logged.
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.log = l } }

// WithAPIKey supplies the key directly, for composition roots that read it
// from an OS secret store. It takes precedence over Config.APIKeyRef.
func WithAPIKey(s Secret) Option { return func(o *options) { o.key = &s } }

// withDialContext replaces how connections are made. It is unexported and
// exists for tests only: they reach a TLS server on a unix socket while the
// endpoint stays an https URL, so certificate handling is exercised without
// depending on the machine's TCP loopback.
func withDialContext(d func(ctx context.Context, network, addr string) (net.Conn, error)) Option {
	return func(o *options) { o.dial = d }
}

// New validates the configuration, resolves the API key reference and builds
// a client. A non-loopback endpoint without TLS, an unreadable key reference
// or an invalid TLS configuration is refused here, not at the first request.
func New(cfg Config, opts ...Option) (*Client, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if err := cfg.Validate(); err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "localai: invalid configuration")
	}
	cfg = cfg.withDefaults()
	spec, err := parseEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "localai: invalid configuration")
	}
	var key Secret
	if o.key != nil {
		key = *o.key
	} else if key, err = ResolveSecret(cfg.APIKeyRef); err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "localai: invalid configuration")
	}
	var tr *http.Transport
	if spec.scheme == "https" {
		tlsCfg, err := buildTLS(cfg.TLS)
		if err != nil {
			return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "localai: invalid configuration")
		}
		tr = newTransport(spec, tlsCfg)
	} else {
		tr = newTransport(spec, nil)
	}
	if o.dial != nil {
		tr.DialContext = o.dial
	}
	reqScheme := spec.scheme
	if reqScheme == "unix" {
		reqScheme = "http"
	}
	c := &Client{
		cfg:       cfg,
		spec:      spec,
		reqScheme: reqScheme,
		hc: &http.Client{
			Transport:     &guardTransport{next: tr, scheme: reqScheme, host: spec.host, key: key},
			CheckRedirect: refuseRedirects,
		},
		clk: o.clk,
		log: o.log,
	}
	if c.clk == nil {
		c.clk = clock.Real{}
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	return c, nil
}

// Name implements providers.AIRuntimeProvider.
func (*Client) Name() string { return ProviderName }

// String describes the client without any credential.
func (c *Client) String() string { return "localai.Client{" + c.cfg.Endpoint + "}" }

// exchange sends one request and returns the response whatever its status.
// The caller closes the body. A transport failure is already mapped.
func (c *Client) exchange(ctx context.Context, method, path string, payload any, accept string) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, contracts.Wrap(contracts.ErrInternal, err, "localai: cannot encode the request")
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.reqScheme+"://"+c.spec.host+path, body)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrInternal, err, "localai: cannot build the request")
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "alixai-localai-adapter")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	started := c.clk.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		mapped := mapTransportError(ctx, err)
		c.logCall(ctx, method, path, 0, started, mapped)
		return nil, mapped
	}
	return resp, nil
}

// send is exchange that returns the response only when the status is 200;
// any other status becomes a mapped error and the body is released.
func (c *Client) send(ctx context.Context, method, path string, payload any, accept string) (*http.Response, error) {
	started := c.clk.Now()
	resp, err := c.exchange(ctx, method, path, payload, accept)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		_ = resp.Body.Close()
		mapped := mapHTTPError(resp.StatusCode, resp.Header, b)
		c.logCall(ctx, method, path, resp.StatusCode, started, mapped)
		return nil, mapped
	}
	c.logCall(ctx, method, path, resp.StatusCode, started, nil)
	return resp, nil
}

// getJSON performs a bounded GET and decodes a JSON object into out.
func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	resp, err := c.send(ctx, http.MethodGet, path, nil, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return c.decodeBody(ctx, resp.Body, out)
}

// decodeBody reads a capped body and decodes it as one JSON document.
func (c *Client) decodeBody(ctx context.Context, body io.Reader, out any) error {
	raw, err := io.ReadAll(newCapReader(body, c.cfg.MaxResponseBytes))
	if err != nil {
		return mapTransportError(ctx, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return schemaInvalid("response is not the expected JSON document")
	}
	return nil
}

func (c *Client) logCall(ctx context.Context, method, path string, status int, started time.Time, err error) {
	if !c.log.Enabled(ctx, slog.LevelDebug) {
		return
	}
	attrs := []slog.Attr{
		slog.String("method", method),
		slog.String("path", path),
		slog.Int("status", status),
		slog.Int64("dur_ms", c.clk.Now().Sub(started).Milliseconds()),
	}
	if err != nil {
		attrs = append(attrs, slog.String("code", string(contracts.CodeOf(err))))
	}
	c.log.LogAttrs(ctx, slog.LevelDebug, "localai request", attrs...)
}

// model resolves the model name for a role and an optional profile hint. An
// unknown profile or an unconfigured role is refused: callers never reach a
// model the operator did not configure.
func (c *Client) model(role providers.Role, profile string) (string, error) {
	set := c.cfg.Models
	if profile != "" {
		p, ok := c.cfg.Profiles[profile]
		if !ok {
			return "", contracts.New(contracts.ErrValidationFailed, "localai: unknown model profile")
		}
		if p.Controller != "" {
			set.Controller = p.Controller
		}
		if p.Router != "" {
			set.Router = p.Router
		}
		if p.Embedder != "" {
			set.Embedder = p.Embedder
		}
	}
	var name string
	switch role {
	case providers.RoleController:
		name = set.Controller
	case providers.RoleRouter:
		name = set.Router
	case providers.RoleEmbedder:
		name = set.Embedder
	}
	if name == "" {
		return "", contracts.Newf(contracts.ErrValidationFailed, "localai: no model is configured for role %q", string(role))
	}
	return name, nil
}

var (
	traceIDRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
)

// traceID keeps a server-provided identifier only if it has the shape of one.
func traceID(s string) string {
	if traceIDRE.MatchString(s) {
		return s
	}
	return ""
}

// reportedModel prefers the model the runtime says it used, falling back to
// the one requested when the answer is missing or malformed.
func reportedModel(reported, requested string) string {
	if modelNameRE.MatchString(reported) {
		return reported
	}
	return requested
}

// logDone records a finished call with its counters. The counter keys are
// tok_in and tok_out: internal/log redacts any key containing "token".
func (c *Client) logDone(ctx context.Context, op, model string, u providers.Usage, started time.Time) {
	if !c.log.Enabled(ctx, slog.LevelDebug) {
		return
	}
	c.log.LogAttrs(ctx, slog.LevelDebug, "localai "+op,
		slog.String("model", model),
		slog.Int("tok_in", u.InputTokens),
		slog.Int("tok_out", u.OutputTokens),
		slog.Int64("dur_ms", c.clk.Now().Sub(started).Milliseconds()),
	)
}
