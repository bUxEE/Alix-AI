package mistral

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"time"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// ProviderName is the provider id for egress and credentials (same as chat/STT).
const ProviderName = "mistral"

// DefaultBaseURL is the API origin.
const DefaultBaseURL = "https://api.mistral.ai"

// DefaultModel is the offline Voxtral TTS 3 model (checked 2026-10-26).
const DefaultModel = "voxtral-mini-tts-3-0"

// DefaultVoice is a documented preset slug used when the request names none.
const DefaultVoice = "gb_jane_neutral"

// NativeSampleRate is the sample rate of generated speech (24 kHz mono).
const NativeSampleRate = 24000

// MaxInputChars is a conservative per-request character limit (aligned with peers).
const MaxInputChars = 4096

// Config configures the TTS client. CredentialRef is never a key.
type Config struct {
	BaseURL          string
	CredentialRef    string
	Model            string
	Voice            string
	RequestTimeout   time.Duration
	MaxResponseBytes int64
	MaxInputChars    int
	Retry            external.RetryPolicy
}

var modelIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var voiceRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func (c Config) String() string {
	return fmt.Sprintf("tts/mistral.Config{BaseURL:%s Model:%s}", c.baseURL(), c.model())
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

func (c Config) voice() string {
	if c.Voice == "" {
		return DefaultVoice
	}
	return c.Voice
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
	if c.Voice == "" {
		c.Voice = DefaultVoice
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
	if !modelIDRE.MatchString(c.Model) {
		return errors.New("Model is malformed")
	}
	if !voiceRE.MatchString(c.Voice) {
		return errors.New("Voice is malformed")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" && u.Path != "/" {
		return errors.New("BaseURL must be a bare http(s) origin")
	}
	return nil
}

func configError(err error) error {
	return contracts.New(contracts.ErrValidationFailed, "mistral-tts: "+err.Error())
}
