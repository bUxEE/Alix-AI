package polly

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
	ProviderName     = "polly"
	DefaultRegion    = "us-east-1"
	DefaultEngine    = "neural"
	DefaultVoice     = "Joanna"
	NativeSampleRate = 16000
	MaxInputChars    = 3000 // billed-character limit; total hard cap is 6000
)

// Config configures the Polly client. CredentialRef resolves to
// "ACCESS_KEY_ID:SECRET_ACCESS_KEY" or
// "ACCESS_KEY_ID:SECRET_ACCESS_KEY:SESSION_TOKEN".
type Config struct {
	BaseURL          string // empty builds https://polly.{Region}.amazonaws.com
	Region           string
	CredentialRef    string
	Engine           string
	VoiceID          string
	SampleRate       string
	RequestTimeout   time.Duration
	MaxResponseBytes int64
	MaxInputChars    int
	Retry            external.RetryPolicy
}

var (
	regionRE = regexp.MustCompile(`^[a-z]{2}-[a-z]+-\d$`)
	engineRE = regexp.MustCompile(`^(standard|neural|long-form|generative)$`)
	voiceRE  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
)

func (c Config) String() string {
	return fmt.Sprintf("tts/polly.Config{Region:%s Engine:%s}", c.region(), c.engine())
}

func (c Config) region() string {
	if c.Region == "" {
		return DefaultRegion
	}
	return c.Region
}

func (c Config) engine() string {
	if c.Engine == "" {
		return DefaultEngine
	}
	return c.Engine
}

func (c Config) voice() string {
	if c.VoiceID == "" {
		return DefaultVoice
	}
	return c.VoiceID
}

func (c Config) sampleRate() string {
	if c.SampleRate == "" {
		return "16000"
	}
	return c.SampleRate
}

func (c Config) sampleRateHz() int {
	switch c.sampleRate() {
	case "8000":
		return 8000
	default:
		return NativeSampleRate
	}
}

func (c Config) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return "https://polly." + c.region() + ".amazonaws.com"
}

func (c Config) maxChars() int {
	if c.MaxInputChars <= 0 {
		return MaxInputChars
	}
	return c.MaxInputChars
}

func (c Config) withDefaults() Config {
	if c.Region == "" {
		c.Region = DefaultRegion
	}
	if c.Engine == "" {
		c.Engine = DefaultEngine
	}
	if c.VoiceID == "" {
		c.VoiceID = DefaultVoice
	}
	if c.SampleRate == "" {
		c.SampleRate = "16000"
	}
	if c.BaseURL == "" {
		c.BaseURL = "https://polly." + c.Region + ".amazonaws.com"
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
	if !regionRE.MatchString(c.Region) {
		return errors.New("Region is malformed")
	}
	if !engineRE.MatchString(c.Engine) {
		return errors.New("Engine must be standard, neural, long-form or generative")
	}
	if !voiceRE.MatchString(c.VoiceID) {
		return errors.New("VoiceID is malformed")
	}
	switch c.SampleRate {
	case "8000", "16000":
	default:
		return errors.New("SampleRate must be 8000 or 16000 for pcm")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" && u.Path != "/" {
		return errors.New("BaseURL must be a bare http(s) origin")
	}
	return nil
}

func configError(err error) error {
	return contracts.New(contracts.ErrValidationFailed, "polly: "+err.Error())
}
