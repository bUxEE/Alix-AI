package clm

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

// Modes of the scoring call.
const (
	// ModeRank scores with POST /v1/rank (the default).
	ModeRank = "rank"
	// ModeSystemOne scores with one choice question on POST /v1/systemone.
	ModeSystemOne = "systemone"
)

// Defaults applied to zero values of Config.
const (
	DefaultEndpoint         = "http://127.0.0.1:8700"
	DefaultRequestTimeout   = 10 * time.Second
	DefaultHealthTimeout    = 5 * time.Second
	DefaultMaxResponseBytes = 1 << 20
	// DefaultMaxStateTokens is the documented encoder limit; a longer state is
	// truncated by the service, so it is refused here.
	DefaultMaxStateTokens = 2048
	// DefaultMaxBodyBytes caps a request body, as LocalAI does for its text bodies.
	DefaultMaxBodyBytes = 64 << 10
	// DefaultMaxCandidates equals the candidate-set schema's maximum.
	DefaultMaxCandidates  = 32
	DefaultModelCheckTTL  = time.Minute
	DefaultSecondaryFloor = 0.85
	// UnverifiedRevision is the revision reported when the operator pinned none.
	UnverifiedRevision = "unverified"
	// ProviderID is the provider identifier in the manifest.
	ProviderID = "clm-sidecar"
	// EncoderID names the documented frozen encoder.
	EncoderID = "qwen3-8b"
)

// Config configures the adapter. A plain struct built by the composition root; the
// adapter registers no configuration section. Zero values take the defaults.
type Config struct {
	// Endpoint is where the CLM service listens: http://127.0.0.1:8700 (loopback),
	// https://host:port (TLS verified) or unix:///path/to.sock.
	Endpoint string    `yaml:"endpoint"`
	TLS      TLSConfig `yaml:"tls"`
	// APIKeyRef names the CLM service key: "env:NAME" or "file:PATH" (same rules as
	// adapters/localai/voice). Empty sends no key. The key itself is never accepted here.
	APIKeyRef string `yaml:"api_key_ref"`
	// Model is the model name the service lists in /v1/models and echoes. Required;
	// it may be an alias ("clm-latest"), in which case ModelRevision is what pins it.
	Model string `yaml:"model"`
	// ModelRevision pins the revision the service must report for Model. Unset, no
	// revision check is made and the manifest says "unverified" (no calibration
	// entry can match, so the engine fails closed).
	ModelRevision string `yaml:"model_revision"`
	// Mode is ModeRank (default) or ModeSystemOne.
	Mode string `yaml:"mode"`
	// MaxStateTokens, MaxBodyBytes and MaxCandidates are the shape limits.
	MaxStateTokens int `yaml:"max_state_tokens"`
	MaxBodyBytes   int `yaml:"max_body_bytes"`
	MaxCandidates  int `yaml:"max_candidates"`
	// SecondaryFloor is the probability a runner-up needs to be selected beside the
	// pick.
	SecondaryFloor   float64       `yaml:"secondary_floor"`
	RequestTimeout   time.Duration `yaml:"request_timeout"`
	HealthTimeout    time.Duration `yaml:"health_timeout"`
	MaxResponseBytes int64         `yaml:"max_response_bytes"`
	// ModelCheckTTL is how long a passed model and revision check is reused.
	ModelCheckTTL time.Duration `yaml:"model_check_ttl"`
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

// Validate checks the configuration without touching the network.
func (c Config) Validate() error {
	ep, err := wire.ParseEndpoint("clm", c.endpoint())
	if err != nil {
		return err
	}
	if err := wire.ValidateTLS("clm", ep, c.TLS); err != nil {
		return err
	}
	if !wire.ValidKeyRef(c.APIKeyRef) {
		return errors.New("clm: api_key_ref must start with env: or file: (a key itself is never accepted in configuration)")
	}
	if !modelNameRE.MatchString(c.Model) {
		return errors.New("clm: model is required and must be a valid model name")
	}
	if c.ModelRevision != "" && !revisionRE.MatchString(c.ModelRevision) {
		return errors.New("clm: model_revision is not a valid revision token")
	}
	if c.Mode != "" && c.Mode != ModeRank && c.Mode != ModeSystemOne {
		return fmt.Errorf("clm: mode must be %q or %q", ModeRank, ModeSystemOne)
	}
	if c.MaxStateTokens < 0 || c.MaxStateTokens > DefaultMaxStateTokens {
		return fmt.Errorf("clm: max_state_tokens must be between 0 and %d (the encoder limit)", DefaultMaxStateTokens)
	}
	if c.MaxBodyBytes < 0 || c.MaxBodyBytes > 1<<20 {
		return errors.New("clm: max_body_bytes must be between 0 and 1 MiB")
	}
	if c.MaxCandidates < 0 || c.MaxCandidates > DefaultMaxCandidates {
		return fmt.Errorf("clm: max_candidates must be between 0 and %d", DefaultMaxCandidates)
	}
	if c.SecondaryFloor < 0 || c.SecondaryFloor > 1 {
		return errors.New("clm: secondary_floor must be in [0, 1]")
	}
	if c.RequestTimeout < 0 || c.HealthTimeout < 0 || c.ModelCheckTTL < 0 || c.MaxResponseBytes < 0 {
		return errors.New("clm: timeouts and the response cap must not be negative")
	}
	return nil
}

func (c Config) withDefaults() Config {
	c.Endpoint = c.endpoint()
	if c.Mode == "" {
		c.Mode = ModeRank
	}
	if c.MaxStateTokens == 0 {
		c.MaxStateTokens = DefaultMaxStateTokens
	}
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if c.MaxCandidates == 0 {
		c.MaxCandidates = DefaultMaxCandidates
	}
	if c.SecondaryFloor == 0 {
		c.SecondaryFloor = DefaultSecondaryFloor
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = DefaultRequestTimeout
	}
	if c.HealthTimeout == 0 {
		c.HealthTimeout = DefaultHealthTimeout
	}
	if c.MaxResponseBytes == 0 {
		c.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if c.ModelCheckTTL == 0 {
		c.ModelCheckTTL = DefaultModelCheckTTL
	}
	return c
}
