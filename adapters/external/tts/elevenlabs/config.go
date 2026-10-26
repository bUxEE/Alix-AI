package elevenlabs

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"time"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const (
	ProviderName     = "elevenlabs"
	DefaultBaseURL   = "https://api.elevenlabs.io"
	DefaultModel     = "eleven_multilingual_v2"
	DefaultFormat    = "pcm_24000"
	NativeSampleRate = 24000
	MaxInputChars    = 10000 // eleven_multilingual_v2; flash_v2_5 allows 40000
)

// Config configures the TTS client. CredentialRef is never a key. VoiceID is
// the default ElevenLabs voice_id; a request Voice overrides it when set.
type Config struct {
	BaseURL          string
	CredentialRef    string
	Model            string
	VoiceID          string
	OutputFormat     string
	RequestTimeout   time.Duration
	MaxResponseBytes int64
	MaxInputChars    int
	Retry            external.RetryPolicy
}

var (
	modelIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	voiceIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
)

func (c Config) String() string {
	return fmt.Sprintf("tts/elevenlabs.Config{BaseURL:%s Model:%s}", c.baseURL(), c.model())
}

func (c Config) baseURL() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	return c.BaseURL
}

func (c Config) model() string {
	if c.Model == "" {
		return DefaultModel
	}
	return c.Model
}

func (c Config) format() string {
	if c.OutputFormat == "" {
		return DefaultFormat
	}
	return c.OutputFormat
}

func (c Config) sampleRate() int {
	switch c.format() {
	case "pcm_16000":
		return 16000
	case "pcm_24000":
		return 24000
	default:
		return NativeSampleRate
	}
}

func (c Config) maxChars() int {
	if c.MaxInputChars <= 0 {
		return MaxInputChars
	}
	return c.MaxInputChars
}

func (c Config) withDefaults() Config {
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	if c.Model == "" {
		c.Model = DefaultModel
	}
	if c.OutputFormat == "" {
		c.OutputFormat = DefaultFormat
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 60 * time.Second
	}
	if c.MaxResponseBytes <= 0 {
		c.MaxResponseBytes = 16 << 20
	}
	if c.MaxInputChars <= 0 {
		c.MaxInputChars = MaxInputChars
	}
	c.Retry = c.Retry.Normalised()
	return c
}

// Validate rejects a malformed configuration.
func (c Config) Validate() error {
	c = c.withDefaults()
	if c.CredentialRef == "" {
		return errors.New("CredentialRef is required")
	}
	if c.VoiceID == "" || !voiceIDRE.MatchString(c.VoiceID) {
		return errors.New("VoiceID is required and must be a voice id")
	}
	if !modelIDRE.MatchString(c.Model) {
		return errors.New("Model is malformed")
	}
	switch c.OutputFormat {
	case "pcm_16000", "pcm_24000":
	default:
		return errors.New("OutputFormat must be pcm_16000 or pcm_24000")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" && u.Path != "/" {
		return errors.New("BaseURL must be a bare http(s) origin")
	}
	return nil
}

func configError(err error) error {
	return contracts.New(contracts.ErrValidationFailed, "elevenlabs-tts: "+err.Error())
}
