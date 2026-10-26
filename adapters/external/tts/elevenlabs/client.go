package elevenlabs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/adapters/external/tts/internal/pcmstream"
	"github.com/buxee/alix-ai/adapters/external/tts/internal/textchunk"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

var _ voice.TTS = (*Client)(nil)

type KeySource = external.ExtKeySource

// Client talks to ElevenLabs TTS. Safe for concurrent use.
type Client struct {
	cfg    Config
	origin *url.URL
	hc     *http.Client
	keys   KeySource
	clk    clock.Clock
	log    *slog.Logger
	limits voice.Limits
}

type Option func(*Client)

func WithClock(c clock.Clock) Option   { return func(cl *Client) { cl.clk = c } }
func WithLogger(l *slog.Logger) Option { return func(cl *Client) { cl.log = l } }

// voiceGuard allows only POST /v1/text-to-speech/{voice_id}?output_format=pcm_*.
type voiceGuard struct {
	next   http.RoundTripper
	origin *url.URL
	format string
	err    error
}

func (g *voiceGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	u := req.URL
	ok := req.Method == http.MethodPost &&
		u != nil && !u.ForceQuery && u.Fragment == "" && u.Opaque == "" && u.RawPath == "" && u.User == nil &&
		strings.EqualFold(u.Scheme, g.origin.Scheme) && strings.EqualFold(u.Host, g.origin.Host) &&
		strings.HasPrefix(u.Path, "/v1/text-to-speech/") &&
		voiceIDRE.MatchString(strings.TrimPrefix(u.Path, "/v1/text-to-speech/")) &&
		u.RawQuery == "output_format="+g.format
	if !ok {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, g.err
	}
	return g.next.RoundTrip(req)
}

func (g *voiceGuard) CloseIdleConnections() {
	if ci, ok := g.next.(interface{ CloseIdleConnections() }); ok {
		ci.CloseIdleConnections()
	}
}

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
		limits: voice.Limits{MaxAudioBytes: int(cfg.MaxResponseBytes), MaxChunkBytes: 64 << 10, MaxTextRunes: 32 << 10},
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
	hc.Transport = &voiceGuard{
		next: hc.Transport, origin: origin, format: cfg.OutputFormat,
		err: external.ExtForbiddenRoute(ProviderName),
	}
	c.hc = hc
	return c, nil
}

func (c *Client) route(voiceID string) external.ExtRoute {
	return external.ExtRoute{
		Method: http.MethodPost,
		Path:   "/v1/text-to-speech/" + voiceID,
		Query:  "output_format=" + c.cfg.format(),
		Auth:   true,
	}
}

// SynthesizeStream implements voice.TTS.
func (c *Client) SynthesizeStream(ctx context.Context, scope contracts.Scope, req voice.SynthesizeRequest) (voice.AudioStream, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := req.Validate(c.limits); err != nil {
		return nil, err
	}
	voiceID := c.cfg.VoiceID
	if req.Voice != "" {
		if !voiceIDRE.MatchString(req.Voice) {
			return nil, configError(errors.New("Voice is malformed"))
		}
		voiceID = req.Voice
	}
	pieces := textchunk.Split(req.Text, c.cfg.maxChars())
	if len(pieces) == 0 {
		return nil, contracts.New(contracts.ErrValidationFailed, ProviderName+": synthesize text is empty")
	}
	format := voice.PCM16Mono(c.cfg.sampleRate())
	started := c.clk.Now()
	return pcmstream.New(ctx, pcmstream.Params{
		Pieces: pieces, Format: format, Model: c.cfg.model(), Clock: c.clk, Started: started,
		Synth: func(callCtx context.Context, text string) ([]byte, error) {
			return c.synthesizeOne(callCtx, scope, text, voiceID)
		},
	})
}

func (c *Client) synthesizeOne(ctx context.Context, scope contracts.Scope, text, voiceID string) ([]byte, error) {
	key, err := external.ExtAPIKey(ctx, ProviderName, c.keys, scope, c.cfg.CredentialRef)
	if err != nil {
		return nil, err
	}
	payload := map[string]string{"text": text, "model_id": c.cfg.model()}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrInternal, err, ProviderName+": cannot encode the request").WithDetails("sent", false)
	}
	r := c.route(voiceID)
	httpReq, err := http.NewRequestWithContext(ctx, r.Method, r.Target(c.cfg.BaseURL), bytes.NewReader(body))
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrInternal, err, ProviderName+": cannot build the request").WithDetails("sent", false)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("xi-api-key", key)
	httpReq.Header.Set("Accept", "audio/pcm, application/octet-stream, */*")
	httpReq.Header.Set("User-Agent", "alixai-elevenlabs-tts-adapter")
	started := c.clk.Now()
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, external.ExtMapTransportError(ctx, ProviderName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, external.ExtMaxErrorBody))
		mapped := mapHTTP(resp.StatusCode, b)
		external.ExtLogCall(ctx, c.log, c.clk, ProviderName, r, resp.StatusCode, started, mapped, nil)
		return nil, mapped
	}
	raw, err := external.ExtReadBody(ctx, ProviderName, resp.Body, c.cfg.MaxResponseBytes)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, external.ExtMalformed(ProviderName, "the provider returned no audio")
	}
	external.ExtLogCall(ctx, c.log, c.clk, ProviderName, r, resp.StatusCode, started, nil, nil)
	return raw, nil
}

func (c *Client) Name() string { return ProviderName + "-tts" }

func mapHTTP(status int, _ []byte) error {
	switch {
	case status == http.StatusUnauthorized:
		return external.ExtAPIError(ProviderName, contracts.ErrPermissionDenied, pv.ProviderErrorKindAuthenticationFailed, true, "the provider rejected the API key")
	case status == http.StatusForbidden:
		return external.ExtAPIError(ProviderName, contracts.ErrPermissionDenied, pv.ProviderErrorKindPermissionDenied, true, "the provider denied the request for this key")
	case status == http.StatusTooManyRequests:
		return external.ExtAPIError(ProviderName, contracts.ErrRateLimited, pv.ProviderErrorKindRateLimited, true, "the provider rate limit was reached")
	case status >= 500:
		return external.ExtAPIError(ProviderName, contracts.ErrUnavailable, pv.ProviderErrorKindUnavailable, true, "the provider is unavailable")
	case status >= 400:
		return external.ExtAPIError(ProviderName, contracts.ErrValidationFailed, pv.ProviderErrorKindBadRequest, true, "the provider rejected the request")
	}
	return external.ExtAPIError(ProviderName, contracts.ErrUnavailable, pv.ProviderErrorKindMalformedResponse, true, "the provider answered unexpectedly")
}
