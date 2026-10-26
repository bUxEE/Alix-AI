package mistral

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

var _ voice.STT = (*Client)(nil)

// KeySource resolves the credential reference for one call.
type KeySource = external.ExtKeySource

// Client talks to OpenAI transcriptions. Safe for concurrent use.
type Client struct {
	cfg    Config
	origin *url.URL
	hc     *http.Client
	keys   KeySource
	clk    clock.Clock
	log    *slog.Logger
	limits voice.Limits
}

// Option adjusts a Client under construction.
type Option func(*Client)

// WithClock injects the time source.
func WithClock(c clock.Clock) Option { return func(cl *Client) { cl.clk = c } }

// WithLogger sets the debug logger (no audio or transcript).
func WithLogger(l *slog.Logger) Option { return func(cl *Client) { cl.log = l } }

var routeTranscribe = external.ExtRoute{Method: http.MethodPost, Path: "/v1/audio/transcriptions", Auth: true}

// New builds a client. dialer is the provider egress dialer.
func New(cfg Config, dialer *egress.Dialer, keys KeySource, opts ...Option) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, configError(err)
	}
	cfg = cfg.withDefaults()
	if dialer == nil || keys == nil {
		return nil, configError(errors.New("an egress dialer and a key source are required"))
	}
	origin, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, configError(errors.New("the base URL must be a bare http(s) origin"))
	}
	c := &Client{
		cfg: cfg, origin: origin, keys: keys,
		limits: voice.Limits{MaxAudioBytes: cfg.MaxAudioBytes, MaxChunkBytes: 64 << 10, MaxTextRunes: 2000},
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
	hc := dialer.HTTPClient(cfg.RequestTimeout)
	hc.CheckRedirect = external.ExtRefuseRedirects
	hc.Transport = &external.ExtRouteGuard{
		Next: hc.Transport, Origin: origin,
		Routes: []external.ExtRoute{routeTranscribe},
		Err:    external.ExtForbiddenRoute(ProviderName),
	}
	c.hc = hc
	return c, nil
}

// Transcribe implements voice.STT.
func (c *Client) Transcribe(ctx context.Context, scope contracts.Scope, req voice.TranscribeRequest) (voice.Transcript, error) {
	if err := scope.Validate(); err != nil {
		return voice.Transcript{}, err
	}
	if err := req.Validate(c.limits); err != nil {
		return voice.Transcript{}, err
	}
	body, ct, err := c.buildBody(req)
	if err != nil {
		return voice.Transcript{}, err
	}
	key, err := external.ExtAPIKey(ctx, ProviderName, c.keys, scope, c.cfg.CredentialRef)
	if err != nil {
		return voice.Transcript{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, routeTranscribe.Method, routeTranscribe.Target(c.cfg.BaseURL), bytes.NewReader(body))
	if err != nil {
		return voice.Transcript{}, contracts.Wrap(contracts.ErrInternal, err, ProviderName+": cannot build the request").WithDetails("sent", false)
	}
	httpReq.Header.Set("Content-Type", ct)
	httpReq.Header.Set("Authorization", "Bearer "+key)
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", "alixai-"+ProviderName+"-stt-adapter")
	started := c.clk.Now()
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return voice.Transcript{}, external.ExtMapTransportError(ctx, ProviderName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, external.ExtMaxErrorBody))
		mapped, _ := mapHTTP(resp.StatusCode, resp.Header, b)
		external.ExtLogCall(ctx, c.log, c.clk, ProviderName, routeTranscribe, resp.StatusCode, started, mapped, nil)
		return voice.Transcript{}, mapped
	}
	raw, err := external.ExtReadBody(ctx, ProviderName, resp.Body, c.cfg.MaxResponseBytes)
	if err != nil {
		return voice.Transcript{}, err
	}
	var out struct {
		Text     string `json:"text"`
		Language string `json:"language"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return voice.Transcript{}, external.ExtMalformed(ProviderName, "response is not the expected JSON document")
	}
	external.ExtLogCall(ctx, c.log, c.clk, ProviderName, routeTranscribe, resp.StatusCode, started, nil, nil)
	return voice.Transcript{
		Text:     strings.TrimSpace(out.Text),
		Language: out.Language,
		Duration: req.Format.DurationOf(len(req.Audio)),
		Model:    c.cfg.model(),
		Timings:  providers.Timings{Total: c.clk.Now().Sub(started)},
	}, nil
}

func (c *Client) buildBody(req voice.TranscribeRequest) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("model", c.cfg.model())
	_ = w.WriteField("response_format", "json")
	if req.Language != "" {
		_ = w.WriteField("language", req.Language)
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="audio.wav"`)
	h.Set("Content-Type", "audio/wav")
	part, err := w.CreatePart(h)
	if err != nil {
		return nil, "", contracts.Wrap(contracts.ErrInternal, err, ProviderName+": cannot encode the request")
	}
	_, _ = part.Write(wavHeader(req.Format, len(req.Audio)))
	_, _ = part.Write(req.Audio)
	if err := w.Close(); err != nil {
		return nil, "", contracts.Wrap(contracts.ErrInternal, err, ProviderName+": cannot encode the request")
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

// TranscribeStream is unsupported: raw audio is never streamed out.
func (c *Client) TranscribeStream(context.Context, contracts.Scope, voice.STTStreamRequest) (voice.STTStream, error) {
	return nil, apiErr(contracts.ErrUnavailable, pv.ProviderErrorKindUnavailable, false, "streaming recognition is not supported")
}

// Name reports the adapter name for logs.
func (c *Client) Name() string { return ProviderName + "-stt" }
