package localai

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/buxee/alix-ai/adapters/decision/internal/wire"
)

// TLSConfig is the TLS client configuration for an https endpoint (certificates
// are always verified; there is no switch to disable it).
type TLSConfig = wire.TLSConfig

// Defaults applied to zero values of Config.
const (
	DefaultEndpoint         = "http://127.0.0.1:8080"
	DefaultRequestTimeout   = 10 * time.Second
	DefaultMaxResponseBytes = 1 << 20
	// DefaultSecondaryFloor is the probability a runner-up needs to be selected
	// beside the scorer's pick; it equals the engine's default confidence floor.
	DefaultSecondaryFloor = 0.85
	// UnverifiedRevision is the revision reported when the operator pinned none.
	UnverifiedRevision = "unverified"
	// ProviderID is the provider identifier in the manifest.
	ProviderID = "localai-decisions"
)

// Documented LocalAI limits, enforced client-side before any connection.
const (
	// MaxBodyBytes is the text request body cap (64 KiB).
	MaxBodyBytes = 64 << 10
	// MaxQuestions is the question cap per request.
	MaxQuestions = 64
	// MaxChoiceOptions is the option cap of a choice question (26 typical; the
	// documentation lists fewer for some models, which the operator can set lower
	// through Config.MaxOptions).
	MaxChoiceOptions = 26
	// MinChoiceOptions and MinScoreLevels are the documented minimums.
	MinChoiceOptions = 2
	MinScoreLevels   = 2
)

// Config configures the adapter. A plain struct: the composition root builds it from
// the "localai" section and its own decision settings; the adapter registers no
// configuration section. Zero values take the defaults.
type Config struct {
	// Endpoint is where LocalAI listens: http://127.0.0.1:8080 (loopback),
	// https://host:port (TLS verified) or unix:///path/to.sock.
	Endpoint string `yaml:"endpoint"`
	// APIKeyRef names the API key: "env:NAME" or "file:PATH". Empty sends no key.
	APIKeyRef string    `yaml:"api_key_ref"`
	TLS       TLSConfig `yaml:"tls"`
	// Model is the LocalAI model that declares known_usecases: [decisions]. Required.
	Model string `yaml:"model"`
	// ModelRevision is the operator's pin of the model revision. LocalAI does not
	// report one.
	ModelRevision string `yaml:"model_revision"`
	// MaxOptions lowers the choice option cap for a model that documents fewer than
	// 26 (0 means MaxChoiceOptions); it can never raise it.
	MaxOptions int `yaml:"max_options"`
	// SecondaryFloor, RequestTimeout and MaxResponseBytes: see the defaults.
	SecondaryFloor   float64       `yaml:"secondary_floor"`
	RequestTimeout   time.Duration `yaml:"request_timeout"`
	MaxResponseBytes int64         `yaml:"max_response_bytes"`
}

var (
	modelNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@+/-]{0,127}$`)
	revisionRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:+-]{0,63}$`)
)

func (c Config) endpoint() string {
	if strings.TrimSpace(c.Endpoint) == "" {
		return DefaultEndpoint
	}
	return strings.TrimSpace(c.Endpoint)
}

// Validate checks the configuration without touching the network or the secret
// store. Messages never include the key.
func (c Config) Validate() error {
	ep, err := wire.ParseEndpoint("localai decisions", c.endpoint())
	if err != nil {
		return err
	}
	if err := wire.ValidateTLS("localai decisions", ep, c.TLS); err != nil {
		return err
	}
	if !wire.ValidKeyRef(c.APIKeyRef) {
		return errors.New("localai decisions: api_key_ref must start with env: or file: (a key itself is never accepted in configuration)")
	}
	if !modelNameRE.MatchString(c.Model) {
		return errors.New("localai decisions: model is required and must be a valid model name")
	}
	if c.ModelRevision != "" && !revisionRE.MatchString(c.ModelRevision) {
		return errors.New("localai decisions: model_revision is not a valid revision token")
	}
	if c.MaxOptions < 0 || c.MaxOptions > MaxChoiceOptions {
		return fmt.Errorf("localai decisions: max_options must be between 0 and %d", MaxChoiceOptions)
	}
	if c.SecondaryFloor < 0 || c.SecondaryFloor > 1 {
		return errors.New("localai decisions: secondary_floor must be in [0, 1]")
	}
	if c.RequestTimeout < 0 || c.MaxResponseBytes < 0 {
		return errors.New("localai decisions: timeout and response cap must not be negative")
	}
	return nil
}

func (c Config) withDefaults() Config {
	c.Endpoint = c.endpoint()
	if c.RequestTimeout == 0 {
		c.RequestTimeout = DefaultRequestTimeout
	}
	if c.MaxResponseBytes == 0 {
		c.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if c.SecondaryFloor == 0 {
		c.SecondaryFloor = DefaultSecondaryFloor
	}
	if c.MaxOptions == 0 {
		c.MaxOptions = MaxChoiceOptions
	}
	if c.ModelRevision == "" {
		c.ModelRevision = UnverifiedRevision
	}
	return c
}
