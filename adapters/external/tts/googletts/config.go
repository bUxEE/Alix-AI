package googletts

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
	ProviderName     = "google-tts"
	DefaultBaseURL   = "https://texttospeech.googleapis.com"
	DefaultLang      = "en-US"
	DefaultVoiceName = "en-US-Neural2-A"
	NativeSampleRate = 24000
	MaxInputBytes    = 5000
)

// Config configures the Google TTS client.
type Config struct {
	BaseURL          string
	CredentialRef    string
	LanguageCode     string
	VoiceName        string
	SampleRateHertz  int
	RequestTimeout   time.Duration
	MaxResponseBytes int64
	MaxInputBytes    int
	Retry            external.RetryPolicy
}

var (
	langRE  = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)
	voiceRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

func (c Config) String() string {
	return fmt.Sprintf("tts/googletts.Config{BaseURL:%s Voice:%s}", c.baseURL(), c.voiceName())
}

func (c Config) baseURL() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	return c.BaseURL
}

func (c Config) language() string {
	if c.LanguageCode == "" {
		return DefaultLang
	}
	return c.LanguageCode
}

func (c Config) voiceName() string {
	if c.VoiceName == "" {
		return DefaultVoiceName
	}
	return c.VoiceName
}

func (c Config) sampleRate() int {
	if c.SampleRateHertz <= 0 {
		return NativeSampleRate
	}
	return c.SampleRateHertz
}

func (c Config) maxBytes() int {
	if c.MaxInputBytes <= 0 {
		return MaxInputBytes
	}
	return c.MaxInputBytes
}

func (c Config) withDefaults() Config {
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	if c.LanguageCode == "" {
		c.LanguageCode = DefaultLang
	}
	if c.VoiceName == "" {
		c.VoiceName = DefaultVoiceName
	}
	if c.SampleRateHertz <= 0 {
		c.SampleRateHertz = NativeSampleRate
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 60 * time.Second
	}
	if c.MaxResponseBytes <= 0 {
		c.MaxResponseBytes = 16 << 20
	}
	if c.MaxInputBytes <= 0 {
		c.MaxInputBytes = MaxInputBytes
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
	if !langRE.MatchString(c.LanguageCode) {
		return errors.New("LanguageCode is malformed")
	}
	if !voiceRE.MatchString(c.VoiceName) {
		return errors.New("VoiceName is malformed")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" && u.Path != "/" {
		return errors.New("BaseURL must be a bare http(s) origin")
	}
	return nil
}

func configError(err error) error {
	return contracts.New(contracts.ErrValidationFailed, "google-tts: "+err.Error())
}
