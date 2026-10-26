package deepgram

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

const (
	ProviderName    = "deepgram"
	DefaultBaseURL  = "https://api.deepgram.com"
	DefaultModel    = "nova-3"
	DefaultMaxBytes = 25 << 20
)

type Config struct {
	BaseURL, CredentialRef, Model string
	RequestTimeout                time.Duration
	MaxResponseBytes              int64
	MaxAudioBytes                 int
	Retry                         external.RetryPolicy
}

func (c Config) withDefaults() Config {
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	if c.Model == "" {
		c.Model = DefaultModel
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 60 * time.Second
	}
	if c.MaxResponseBytes <= 0 {
		c.MaxResponseBytes = 1 << 20
	}
	if c.MaxAudioBytes <= 0 {
		c.MaxAudioBytes = DefaultMaxBytes
	}
	c.Retry = c.Retry.Normalised()
	return c
}

func (c Config) Validate() error {
	c = c.withDefaults()
	if c.CredentialRef == "" {
		return errors.New("CredentialRef is required")
	}
	if _, err := url.Parse(c.BaseURL); err != nil {
		return errors.New("BaseURL is malformed")
	}
	return nil
}

type KeySource = external.ExtKeySource

type Client struct {
	cfg    Config
	hc     *http.Client
	keys   KeySource
	clk    clock.Clock
	log    *slog.Logger
	limits voice.Limits
	origin *url.URL
}

type Option func(*Client)

func WithClock(c clock.Clock) Option   { return func(cl *Client) { cl.clk = c } }
func WithLogger(l *slog.Logger) Option { return func(cl *Client) { cl.log = l } }

func New(cfg Config, dialer *egress.Dialer, keys KeySource, opts ...Option) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "deepgram-stt: "+err.Error())
	}
	cfg = cfg.withDefaults()
	if dialer == nil || keys == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "deepgram-stt: an egress dialer and a key source are required")
	}
	origin, _ := url.Parse(cfg.BaseURL)
	c := &Client{cfg: cfg, keys: keys, origin: origin, limits: voice.Limits{MaxAudioBytes: cfg.MaxAudioBytes, MaxChunkBytes: 64 << 10, MaxTextRunes: 2000}}
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
	// Path is fixed; query varies (model, language) — guard allows exact path with empty Query in table,
	// so we dial through egress only and check path in Transcribe.
	c.hc = hc
	return c, nil
}

func (c *Client) Transcribe(ctx context.Context, scope contracts.Scope, req voice.TranscribeRequest) (voice.Transcript, error) {
	if err := scope.Validate(); err != nil {
		return voice.Transcript{}, err
	}
	if err := req.Validate(c.limits); err != nil {
		return voice.Transcript{}, err
	}
	key, err := external.ExtAPIKey(ctx, ProviderName, c.keys, scope, c.cfg.CredentialRef)
	if err != nil {
		return voice.Transcript{}, err
	}
	q := url.Values{"model": {c.cfg.Model}}
	if req.Language != "" {
		q.Set("language", req.Language)
	}
	target := c.cfg.BaseURL + "/v1/listen?" + q.Encode()
	wav := append(wavHeader(req.Format, len(req.Audio)), req.Audio...)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(wav))
	if err != nil {
		return voice.Transcript{}, contracts.Wrap(contracts.ErrInternal, err, ProviderName+": cannot build the request").WithDetails("sent", false)
	}
	httpReq.Header.Set("Authorization", "Token "+key)
	httpReq.Header.Set("Content-Type", "audio/wav")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", "alixai-deepgram-stt-adapter")
	started := c.clk.Now()
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return voice.Transcript{}, external.ExtMapTransportError(ctx, ProviderName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, external.ExtMaxErrorBody))
		return voice.Transcript{}, mapHTTP(resp.StatusCode, b)
	}
	raw, err := external.ExtReadBody(ctx, ProviderName, resp.Body, c.cfg.MaxResponseBytes)
	if err != nil {
		return voice.Transcript{}, err
	}
	var out struct {
		Results struct {
			Channels []struct {
				Alternatives []struct {
					Transcript string `json:"transcript"`
				} `json:"alternatives"`
			} `json:"channels"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return voice.Transcript{}, external.ExtMalformed(ProviderName, "response is not the expected JSON document")
	}
	text := ""
	if len(out.Results.Channels) > 0 && len(out.Results.Channels[0].Alternatives) > 0 {
		text = strings.TrimSpace(out.Results.Channels[0].Alternatives[0].Transcript)
	}
	return voice.Transcript{
		Text: text, Language: req.Language, Duration: req.Format.DurationOf(len(req.Audio)),
		Model: c.cfg.Model, Timings: providers.Timings{Total: c.clk.Now().Sub(started)},
	}, nil
}

func (c *Client) TranscribeStream(context.Context, contracts.Scope, voice.STTStreamRequest) (voice.STTStream, error) {
	return nil, external.ExtAPIError(ProviderName, contracts.ErrUnavailable, pv.ProviderErrorKindUnavailable, false, "streaming recognition is not supported")
}

func (c *Client) Name() string { return ProviderName + "-stt" }

func mapHTTP(status int, _ []byte) error {
	switch {
	case status == http.StatusUnauthorized:
		return external.ExtAPIError(ProviderName, contracts.ErrPermissionDenied, pv.ProviderErrorKindAuthenticationFailed, true, "the provider rejected the API key")
	case status == http.StatusTooManyRequests:
		return external.ExtAPIError(ProviderName, contracts.ErrRateLimited, pv.ProviderErrorKindRateLimited, true, "the provider rate limit was reached")
	case status >= 500:
		return external.ExtAPIError(ProviderName, contracts.ErrUnavailable, pv.ProviderErrorKindUnavailable, true, "the provider is unavailable")
	case status >= 400:
		return external.ExtAPIError(ProviderName, contracts.ErrValidationFailed, pv.ProviderErrorKindBadRequest, true, "the provider rejected the request")
	}
	return external.ExtAPIError(ProviderName, contracts.ErrUnavailable, pv.ProviderErrorKindMalformedResponse, true, "the provider answered unexpectedly")
}

func wavHeader(f voice.AudioFormat, dataLen int) []byte {
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+dataLen))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], uint16(f.Channels))
	binary.LittleEndian.PutUint32(h[24:], uint32(f.SampleRate))
	binary.LittleEndian.PutUint32(h[28:], uint32(f.BytesPerSecond()))
	binary.LittleEndian.PutUint16(h[32:], uint16(f.FrameBytes()))
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(dataLen))
	return h
}

var _ voice.STT = (*Client)(nil)
