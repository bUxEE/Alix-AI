package polly

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"

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

var routeSpeech = external.ExtRoute{Method: http.MethodPost, Path: "/v1/speech", Auth: true}

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
	hc.Transport = &external.ExtRouteGuard{
		Next: hc.Transport, Origin: origin,
		Routes: []external.ExtRoute{routeSpeech},
		Err:    external.ExtForbiddenRoute(ProviderName),
	}
	c.hc = hc
	return c, nil
}

// SynthesizeStream implements voice.TTS.
func (c *Client) SynthesizeStream(ctx context.Context, scope contracts.Scope, req voice.SynthesizeRequest) (voice.AudioStream, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := req.Validate(c.limits); err != nil {
		return nil, err
	}
	voiceID := c.cfg.voice()
	if req.Voice != "" {
		if !voiceRE.MatchString(req.Voice) {
			return nil, configError(errors.New("Voice is malformed"))
		}
		voiceID = req.Voice
	}
	pieces := textchunk.Split(req.Text, c.cfg.maxChars())
	if len(pieces) == 0 {
		return nil, contracts.New(contracts.ErrValidationFailed, ProviderName+": synthesize text is empty")
	}
	format := voice.PCM16Mono(c.cfg.sampleRateHz())
	started := c.clk.Now()
	lang := req.Language
	return pcmstream.New(ctx, pcmstream.Params{
		Pieces: pieces, Format: format, Model: c.cfg.engine() + "/" + voiceID, Clock: c.clk, Started: started,
		Synth: func(callCtx context.Context, text string) ([]byte, error) {
			return c.synthesizeOne(callCtx, scope, text, voiceID, lang)
		},
	})
}

func (c *Client) synthesizeOne(ctx context.Context, scope contracts.Scope, text, voiceID, language string) ([]byte, error) {
	rawCred, err := external.ExtAPIKey(ctx, ProviderName, c.keys, scope, c.cfg.CredentialRef)
	if err != nil {
		return nil, err
	}
	creds, err := parseAWSCreds(rawCred)
	if err != nil {
		return nil, external.ExtUnusableCredential(ProviderName, "malformed credential")
	}
	payload := map[string]string{
		"Engine":       c.cfg.engine(),
		"OutputFormat": "pcm",
		"SampleRate":   c.cfg.sampleRate(),
		"Text":         text,
		"TextType":     "text",
		"VoiceId":      voiceID,
	}
	if language != "" {
		payload["LanguageCode"] = language
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrInternal, err, ProviderName+": cannot encode the request").WithDetails("sent", false)
	}
	httpReq, err := http.NewRequestWithContext(ctx, routeSpeech.Method, routeSpeech.Target(c.cfg.BaseURL), bytes.NewReader(body))
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrInternal, err, ProviderName+": cannot build the request").WithDetails("sent", false)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "audio/pcm, application/octet-stream, */*")
	httpReq.Header.Set("User-Agent", "alixai-polly-tts-adapter")
	signV4(httpReq, body, creds, c.cfg.region(), c.clk.Now())
	started := c.clk.Now()
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, external.ExtMapTransportError(ctx, ProviderName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, external.ExtMaxErrorBody))
		mapped := mapHTTP(resp.StatusCode, b)
		external.ExtLogCall(ctx, c.log, c.clk, ProviderName, routeSpeech, resp.StatusCode, started, mapped, nil)
		return nil, mapped
	}
	raw, err := external.ExtReadBody(ctx, ProviderName, resp.Body, c.cfg.MaxResponseBytes)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, external.ExtMalformed(ProviderName, "the provider returned no audio")
	}
	external.ExtLogCall(ctx, c.log, c.clk, ProviderName, routeSpeech, resp.StatusCode, started, nil, nil)
	return raw, nil
}

func (c *Client) Name() string { return ProviderName + "-tts" }

func mapHTTP(status int, _ []byte) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return external.ExtAPIError(ProviderName, contracts.ErrPermissionDenied, pv.ProviderErrorKindAuthenticationFailed, true, "the provider rejected the credential")
	case status == http.StatusTooManyRequests:
		return external.ExtAPIError(ProviderName, contracts.ErrRateLimited, pv.ProviderErrorKindRateLimited, true, "the provider rate limit was reached")
	case status >= 500:
		return external.ExtAPIError(ProviderName, contracts.ErrUnavailable, pv.ProviderErrorKindUnavailable, true, "the provider is unavailable")
	case status >= 400:
		return external.ExtAPIError(ProviderName, contracts.ErrValidationFailed, pv.ProviderErrorKindBadRequest, true, "the provider rejected the request")
	}
	return external.ExtAPIError(ProviderName, contracts.ErrUnavailable, pv.ProviderErrorKindMalformedResponse, true, "the provider answered unexpectedly")
}
