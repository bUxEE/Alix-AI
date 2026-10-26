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

// ProviderName is the provider id for egress and credentials (same as chat).
const ProviderName = "mistral"

// DefaultBaseURL is the API origin.
const DefaultBaseURL = "https://api.mistral.ai"

// DefaultModel is the recommended file-transcription model (2026-10-26).
const DefaultModel = "voxtral-mini-transcribe-3"

// DefaultMaxBytes is a conservative upload bound (25 MB, same order as peers).
const DefaultMaxBytes = 25 << 20

// Config configures the STT client. CredentialRef is never a key.
type Config struct {
	BaseURL          string
	CredentialRef    string
	Model            string
	RequestTimeout   time.Duration
	MaxResponseBytes int64
	MaxAudioBytes    int
	Retry            external.RetryPolicy
}

var modelIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func (c Config) String() string {
	return fmt.Sprintf("stt/mistral.Config{BaseURL:%s Model:%s}", c.baseURL(), c.model())
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

// Validate rejects a malformed configuration.
func (c Config) Validate() error {
	c = c.withDefaults()
	if c.CredentialRef == "" {
		return errors.New("CredentialRef is required")
	}
	if !modelIDRE.MatchString(c.Model) {
		return errors.New("Model is malformed")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" && u.Path != "/" {
		return errors.New("BaseURL must be a bare http(s) origin")
	}
	return nil
}

func configError(err error) error {
	return contracts.New(contracts.ErrValidationFailed, "mistral-stt: "+err.Error())
}
