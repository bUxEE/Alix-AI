package voice

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/buxee/alix-ai/adapters/localai"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providers"
	pv "github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Compile-time proof that the client serves the three voice roles.
var (
	_ pv.STT = (*Client)(nil)
	_ pv.TTS = (*Client)(nil)
	_ pv.VAD = (*Client)(nil)
)

// Client talks to the audio routes of one LocalAI runtime. It is safe for
// concurrent use. It holds no audio between calls and starts no goroutine; a
// stream's state lives in the stream value the caller owns.
type Client struct {
	cfg       Config
	limits    pv.Limits
	timeout   time.Duration
	maxResp   int64
	endpoint  string
	reqScheme string
	host      string
	hc        *http.Client
	clk       clock.Clock
	log       *slog.Logger
}

// Option adjusts a Client under construction.
type Option func(*options)

type options struct {
	clk clock.Clock
	log *slog.Logger
	key *apiKey
}

// WithClock injects the time source used for timings.
func WithClock(c clock.Clock) Option { return func(o *options) { o.clk = c } }

// WithLogger sets the logger that receives debug records (method, path,
// status, duration). Audio, transcripts, spoken text and the key are never
// logged.
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.log = l } }

// WithAPIKey supplies the key directly, for composition roots that read it
// from an OS secret store; it takes precedence over the API key reference of
// the localai.Config. A plain string because localai.Secret keeps its value
// private to package localai; the client wraps it at once and never prints it.
func WithAPIKey(key string) Option {
	return func(o *options) { k := newAPIKey(key); o.key = &k }
}

// New validates both configurations, resolves the API key reference and builds
// a client. rt supplies the connection (the same value the text client was
// built from); rt.Validate applies the endpoint policy: plaintext only to
// loopback, https with verified certificates, unix sockets by absolute path.
// A refused endpoint, an unreadable key reference or an invalid TLS
// configuration fails here, not at the first request.
func New(rt localai.Config, cfg Config, opts ...Option) (*Client, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if err := rt.Validate(); err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "localai voice: invalid configuration")
	}
	if err := cfg.Validate(); err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "localai voice: invalid configuration")
	}
	endpoint := strings.TrimSpace(rt.Endpoint)
	if endpoint == "" {
		endpoint = localai.DefaultEndpoint
	}
	spec, err := parseEndpoint(endpoint)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "localai voice: invalid configuration")
	}
	var key apiKey
	if o.key != nil {
		key = *o.key
	} else if key, err = resolveKeyRef(rt.APIKeyRef); err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "localai voice: invalid configuration")
	}
	var tlsCfg *tls.Config
	if spec.scheme == "https" {
		if tlsCfg, err = buildTLS(rt.TLS); err != nil {
			return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "localai voice: invalid configuration")
		}
	}
	reqScheme := spec.scheme
	if reqScheme == "unix" {
		reqScheme = "http"
	}
	c := &Client{
		cfg:       cfg.withCopies(),
		limits:    cfg.limits(),
		timeout:   rt.RequestTimeout,
		maxResp:   rt.MaxResponseBytes,
		endpoint:  endpoint,
		reqScheme: reqScheme,
		host:      spec.host,
		hc: &http.Client{
			Transport:     &guardTransport{next: newTransport(spec, tlsCfg), scheme: reqScheme, host: spec.host, key: key},
			CheckRedirect: refuseRedirects,
		},
		clk: o.clk,
		log: o.log,
	}
	if c.timeout == 0 {
		c.timeout = localai.DefaultRequestTimeout
	}
	if c.maxResp == 0 {
		c.maxResp = localai.DefaultMaxResponseBytes
	}
	if c.clk == nil {
		c.clk = clock.Real{}
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	return c, nil
}

// String describes the client without any credential.
func (c *Client) String() string { return "localai/voice.Client{" + c.endpoint + "}" }

// post sends one POST to an allowlisted audio route and returns the response
// only when its status is 200; any other status becomes a mapped error and the
// body is released. The caller closes the body.
func (c *Client) post(ctx context.Context, path, contentType, accept string, body []byte) (*http.Response, error) {
	started := c.clk.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.reqScheme+"://"+c.host+path, bytes.NewReader(body))
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrInternal, err, "localai voice: cannot build the request")
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "alixai-localai-voice-adapter")
	resp, err := c.hc.Do(req)
	if err != nil {
		mapped := mapTransportError(ctx, err)
		c.logCall(ctx, path, 0, started, mapped)
		return nil, mapped
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		_ = resp.Body.Close()
		mapped := mapHTTPError(resp.StatusCode, resp.Header, b)
		c.logCall(ctx, path, resp.StatusCode, started, mapped)
		return nil, mapped
	}
	c.logCall(ctx, path, resp.StatusCode, started, nil)
	return resp, nil
}

func (c *Client) logCall(ctx context.Context, path string, status int, started time.Time, err error) {
	if !c.log.Enabled(ctx, slog.LevelDebug) {
		return
	}
	attrs := []slog.Attr{
		slog.String("method", http.MethodPost),
		slog.String("path", path),
		slog.Int("status", status),
		slog.Int64("dur_ms", c.clk.Now().Sub(started).Milliseconds()),
	}
	if err != nil {
		attrs = append(attrs, slog.String("code", string(contracts.CodeOf(err))))
	}
	c.log.LogAttrs(ctx, slog.LevelDebug, "localai voice request", attrs...)
}

// model resolves the model name for a voice role and an optional profile hint.
// An unknown profile or an unconfigured role is refused: callers never reach a
// model the operator did not configure.
func (c *Client) model(role providers.Role, profile string) (string, error) {
	set := c.cfg.Models
	if profile != "" {
		p, ok := c.cfg.Profiles[profile]
		if !ok {
			return "", contracts.New(contracts.ErrValidationFailed, "localai voice: unknown model profile")
		}
		if p.STT != "" {
			set.STT = p.STT
		}
		if p.TTS != "" {
			set.TTS = p.TTS
		}
		if p.VAD != "" {
			set.VAD = p.VAD
		}
	}
	var name string
	switch role {
	case providers.RoleSTT:
		name = set.STT
	case providers.RoleTTS:
		name = set.TTS
	case providers.RoleVAD:
		name = set.VAD
	}
	if name == "" {
		return "", contracts.Newf(contracts.ErrValidationFailed, "localai voice: no model is configured for role %q", string(role))
	}
	return name, nil
}

var traceIDRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// traceID keeps a server-provided identifier only if it has the shape of one.
func traceID(h http.Header) string {
	if s := h.Get("X-Request-Id"); traceIDRE.MatchString(s) {
		return s
	}
	return ""
}

// primaryLanguage reduces a BCP 47 tag to the ISO 639-1 style code LocalAI's
// audio routes take ("it-IT" becomes "it"). The empty tag stays empty.
func primaryLanguage(tag string) string {
	p, _, _ := strings.Cut(tag, "-")
	return strings.ToLower(p)
}

// reportedLanguage keeps a language the runtime reports only when it looks like
// a language tag; anything else is "unknown".
func reportedLanguage(s string) string {
	if pv.ValidLanguage(s) {
		return s
	}
	return ""
}
