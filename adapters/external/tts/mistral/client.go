package mistral

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
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
)

var _ voice.TTS = (*Client)(nil)

// KeySource resolves the credential reference for one call.
type KeySource = external.ExtKeySource

// Client talks to Mistral speech. Safe for concurrent use.
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

// WithLogger sets the debug logger (no text or audio).
func WithLogger(l *slog.Logger) Option { return func(cl *Client) { cl.log = l } }

var routeSpeech = external.ExtRoute{Method: http.MethodPost, Path: "/v1/audio/speech", Auth: true}

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
	format := voice.PCM16Mono(NativeSampleRate)
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
	payload := map[string]string{
		"model":           c.cfg.model(),
		"input":           text,
		"voice_id":        voiceID,
		"response_format": "wav",
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
	httpReq.Header.Set("Authorization", "Bearer "+key)
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", "alixai-"+ProviderName+"-tts-adapter")
	started := c.clk.Now()
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, external.ExtMapTransportError(ctx, ProviderName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, external.ExtMaxErrorBody))
		mapped, _ := mapHTTP(resp.StatusCode, resp.Header, b)
		external.ExtLogCall(ctx, c.log, c.clk, ProviderName, routeSpeech, resp.StatusCode, started, mapped, nil)
		return nil, mapped
	}
	raw, err := external.ExtReadBody(ctx, ProviderName, resp.Body, c.cfg.MaxResponseBytes)
	if err != nil {
		return nil, err
	}
	var out struct {
		AudioData string `json:"audio_data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.AudioData == "" {
		return nil, external.ExtMalformed(ProviderName, "response is not the expected JSON document")
	}
	wav, err := base64.StdEncoding.DecodeString(out.AudioData)
	if err != nil {
		return nil, external.ExtMalformed(ProviderName, "audio_data is not valid base64")
	}
	pcm, err := stripWAV(wav)
	if err != nil {
		return nil, err
	}
	external.ExtLogCall(ctx, c.log, c.clk, ProviderName, routeSpeech, resp.StatusCode, started, nil, nil)
	return pcm, nil
}

// Name reports the adapter name for logs.
func (c *Client) Name() string { return ProviderName + "-tts" }

// stripWAV removes a RIFF/WAVE header and returns the PCM data chunk (LINEAR16).
func stripWAV(b []byte) ([]byte, error) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		if len(b) == 0 {
			return nil, external.ExtMalformed(ProviderName, "the provider returned no audio")
		}
		return b, nil
	}
	i := 12
	for i+8 <= len(b) {
		id := string(b[i : i+4])
		size := int(binary.LittleEndian.Uint32(b[i+4 : i+8]))
		i += 8
		if size < 0 || i+size > len(b) {
			return nil, external.ExtMalformed(ProviderName, "the WAV container is malformed")
		}
		if id == "data" {
			return b[i : i+size], nil
		}
		i += size
		if size%2 == 1 {
			i++
		}
	}
	return nil, external.ExtMalformed(ProviderName, "the WAV container has no data chunk")
}
