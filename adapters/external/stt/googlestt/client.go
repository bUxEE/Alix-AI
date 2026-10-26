package googlestt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
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
	ProviderName    = "google-stt"
	DefaultLocation = "us"
	DefaultModel    = "chirp_3"
	DefaultMaxBytes = 10 << 20 // recognize is for short audio
)

// Config configures the Google STT v2 client.
type Config struct {
	// BaseURL overrides the regional origin (tests). Empty builds from Location.
	BaseURL string
	// CredentialRef is an OAuth access-token or API-key reference.
	CredentialRef    string
	Project          string
	Location         string
	Recognizer       string // default "_"
	Model            string
	RequestTimeout   time.Duration
	MaxResponseBytes int64
	MaxAudioBytes    int
	Retry            external.RetryPolicy
}

func (c Config) withDefaults() Config {
	if c.Location == "" {
		c.Location = DefaultLocation
	}
	if c.Recognizer == "" {
		c.Recognizer = "_"
	}
	if c.Model == "" {
		c.Model = DefaultModel
	}
	if c.BaseURL == "" {
		c.BaseURL = fmt.Sprintf("https://%s-speech.googleapis.com", c.Location)
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
	if c.CredentialRef == "" || c.Project == "" {
		return errors.New("CredentialRef and Project are required")
	}
	return nil
}

func (c Config) recognizePath() string {
	c = c.withDefaults()
	return fmt.Sprintf("/v2/projects/%s/locations/%s/recognizers/%s:recognize", c.Project, c.Location, c.Recognizer)
}

type KeySource = external.ExtKeySource

type Client struct {
	cfg    Config
	hc     *http.Client
	keys   KeySource
	clk    clock.Clock
	log    *slog.Logger
	limits voice.Limits
	route  external.ExtRoute
}

type Option func(*Client)

func WithClock(c clock.Clock) Option   { return func(cl *Client) { cl.clk = c } }
func WithLogger(l *slog.Logger) Option { return func(cl *Client) { cl.log = l } }

func New(cfg Config, dialer *egress.Dialer, keys KeySource, opts ...Option) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "google-stt: "+err.Error())
	}
	cfg = cfg.withDefaults()
	if dialer == nil || keys == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "google-stt: an egress dialer and a key source are required")
	}
	origin, _ := url.Parse(cfg.BaseURL)
	route := external.ExtRoute{Method: http.MethodPost, Path: cfg.recognizePath(), Auth: true}
	c := &Client{cfg: cfg, keys: keys, route: route, limits: voice.Limits{MaxAudioBytes: cfg.MaxAudioBytes, MaxChunkBytes: 64 << 10, MaxTextRunes: 2000}}
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
	hc.Transport = &external.ExtRouteGuard{Next: hc.Transport, Origin: origin, Routes: []external.ExtRoute{route}, Err: external.ExtForbiddenRoute(ProviderName)}
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
	tok, err := external.ExtAPIKey(ctx, ProviderName, c.keys, scope, c.cfg.CredentialRef)
	if err != nil {
		return voice.Transcript{}, err
	}
	langs := []string{"en-US"}
	if req.Language != "" {
		langs = []string{req.Language}
	}
	// Wrap PCM as WAV so auto-decoding has a container; raw LINEAR16 without a
	// header is ambiguous for Chirp recognize.
	wav := append(wavHeader(req.Format, len(req.Audio)), req.Audio...)
	payload := map[string]any{
		"config": map[string]any{
			"auto_decoding_config": map[string]any{},
			"language_codes":       langs,
			"model":                c.cfg.Model,
		},
		"content": base64.StdEncoding.EncodeToString(wav),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return voice.Transcript{}, contracts.Wrap(contracts.ErrInternal, err, ProviderName+": cannot encode the request")
	}
	httpReq, err := http.NewRequestWithContext(ctx, c.route.Method, c.route.Target(c.cfg.BaseURL), bytes.NewReader(body))
	if err != nil {
		return voice.Transcript{}, contracts.Wrap(contracts.ErrInternal, err, ProviderName+": cannot build the request").WithDetails("sent", false)
	}
	httpReq.Header.Set("Authorization", "Bearer "+tok)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", "alixai-google-stt-adapter")
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
		Results []struct {
			Alternatives []struct {
				Transcript string `json:"transcript"`
			} `json:"alternatives"`
			LanguageCode string `json:"languageCode"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return voice.Transcript{}, external.ExtMalformed(ProviderName, "response is not the expected JSON document")
	}
	var parts []string
	lang := req.Language
	for _, r := range out.Results {
		if r.LanguageCode != "" {
			lang = r.LanguageCode
		}
		if len(r.Alternatives) > 0 {
			if t := strings.TrimSpace(r.Alternatives[0].Transcript); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return voice.Transcript{
		Text: strings.Join(parts, " "), Language: lang, Duration: req.Format.DurationOf(len(req.Audio)),
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
