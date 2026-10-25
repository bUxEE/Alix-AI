package realtime

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/buxee/alix-ai/adapters/localai"
	"github.com/buxee/alix-ai/internal/clock"
	pv "github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Compile-time proof that the client serves the STT role. It deliberately has
// no TTS method: see the package documentation.
var _ pv.STT = (*Client)(nil)

// maxEventBytes bounds one server event. Events in a transcription-only
// session are small JSON documents (no audio comes back), so the limit is far
// below the runtime's own 10 MB.
const maxEventBytes = 1 << 20

// Client talks to the Realtime WebSocket of one LocalAI runtime. It is safe for
// concurrent use; each stream owns its connection and one reader goroutine,
// both released by Close.
type Client struct {
	cfg       Config
	limits    pv.Limits
	timeout   time.Duration
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

// WithLogger sets the logger that receives debug records (path, outcome,
// duration). Audio, transcripts and the key are never logged.
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.log = l } }

// WithAPIKey supplies the key directly, for composition roots that read it from
// an OS secret store; it takes precedence over the API key reference.
func WithAPIKey(key string) Option {
	return func(o *options) { k := newAPIKey(key); o.key = &k }
}

// New validates both configurations, resolves the API key reference and builds
// a client. rt is the connection the text client was built from; rt.Validate
// applies the endpoint policy (plaintext only to loopback, https with verified
// certificates, unix sockets by absolute path). Nothing is dialled here.
func New(rt localai.Config, cfg Config, opts ...Option) (*Client, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	bad := func(err error) (*Client, error) {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "localai realtime: invalid configuration")
	}
	if err := rt.Validate(); err != nil {
		return bad(err)
	}
	if err := cfg.Validate(); err != nil {
		return bad(err)
	}
	endpoint := strings.TrimSpace(rt.Endpoint)
	if endpoint == "" {
		endpoint = localai.DefaultEndpoint
	}
	spec, err := parseEndpoint(endpoint)
	if err != nil {
		return bad(err)
	}
	var key apiKey
	if o.key != nil {
		key = *o.key
	} else if key, err = resolveKeyRef(rt.APIKeyRef); err != nil {
		return bad(err)
	}
	var tlsCfg *tls.Config
	if spec.scheme == "https" {
		if tlsCfg, err = buildTLS(rt.TLS); err != nil {
			return bad(err)
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
	if c.clk == nil {
		c.clk = clock.Real{}
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	return c, nil
}

// String describes the client without any credential.
func (c *Client) String() string { return "localai/realtime.Client{" + c.endpoint + "}" }

// model resolves the pipeline model for an optional profile hint. An unknown
// profile or an unconfigured model is refused.
func (c *Client) model(profile string) (string, error) {
	name := c.cfg.Model
	if profile != "" {
		m, ok := c.cfg.Profiles[profile]
		if !ok {
			return "", contracts.New(contracts.ErrValidationFailed, "localai realtime: unknown model profile")
		}
		if m != "" {
			name = m
		}
	}
	if name == "" {
		return "", contracts.New(contracts.ErrValidationFailed, "localai realtime: no pipeline model is configured")
	}
	return name, nil
}

// dial opens the WebSocket. The query holds the model and nothing else (the
// guard transport refuses any other parameter). The context bounds the
// handshake only; the connection outlives it.
func (c *Client) dial(ctx context.Context, model string) (*websocket.Conn, error) {
	started := c.clk.Now()
	dctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	target := c.reqScheme + "://" + c.host + pathRealtime + "?" + url.Values{"model": {model}}.Encode()
	conn, resp, err := websocket.Dial(dctx, target, &websocket.DialOptions{
		HTTPClient:      c.hc,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		mapped := mapDialError(ctx, resp, err)
		c.logOpen(ctx, started, mapped)
		return nil, mapped
	}
	conn.SetReadLimit(maxEventBytes)
	c.logOpen(ctx, started, nil)
	return conn, nil
}

func (c *Client) logOpen(ctx context.Context, started time.Time, err error) {
	if !c.log.Enabled(ctx, slog.LevelDebug) {
		return
	}
	attrs := []slog.Attr{
		slog.String("method", http.MethodGet),
		slog.String("path", pathRealtime),
		slog.Int64("dur_ms", c.clk.Now().Sub(started).Milliseconds()),
	}
	if err != nil {
		attrs = append(attrs, slog.String("code", string(contracts.CodeOf(err))))
	}
	c.log.LogAttrs(ctx, slog.LevelDebug, "localai realtime open", attrs...)
}

// primaryLanguage reduces a BCP 47 tag to the ISO 639-1 style code LocalAI
// takes ("it-IT" becomes "it"). The empty tag stays empty.
func primaryLanguage(tag string) string {
	p, _, _ := strings.Cut(tag, "-")
	return strings.ToLower(p)
}
