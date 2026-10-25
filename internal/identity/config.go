package identity

import (
	"fmt"
	"time"

	"github.com/buxee/alix-ai/internal/config"
)

// SectionName is the configuration section this package owns:
//
//	identity:
//	  deployment_id: dep_main
//	  audience: ""            # empty means deployment_id
//	  max_token_lifetime: 15m # never above 15m
//
// with ALIXAI_IDENTITY_DEPLOYMENT_ID, ALIXAI_IDENTITY_AUDIENCE and
// ALIXAI_IDENTITY_MAX_TOKEN_LIFETIME as overrides. Issuer keys are not
// configuration: an operator adds them to the issuer key store with `alixai
// identity add-issuer-key` (IssuerKeyStore, a KeyResolver backed by SQLite).
const SectionName = "identity"

// Config is the identity section.
type Config struct {
	// DeploymentID identifies this AlixAI installation. It is the deployment
	// level of every Scope and, unless Audience is set, the audience tokens
	// must carry. There is no default: a process that serves requests refuses
	// to build a Verifier without it.
	DeploymentID string `yaml:"deployment_id"`
	// Audience overrides the audience tokens must carry (for example a URL
	// the host application knows the deployment by).
	Audience string `yaml:"audience"`
	// MaxTokenLifetime is the longest accepted validity window. It may be set
	// lower than the 15 minute ceiling, never higher.
	MaxTokenLifetime time.Duration `yaml:"max_token_lifetime"`
}

// DefaultConfig returns the defaults: no deployment ID, the 15 minute ceiling.
func DefaultConfig() Config { return Config{MaxTokenLifetime: MaxLifetime} }

// Validate implements the config section validation hook. An empty
// DeploymentID is accepted here (a process that never serves requests has no
// use for one) and refused by NewVerifier.
func (c Config) Validate() error {
	if c.DeploymentID != "" && !validIdentifier(c.DeploymentID) {
		return fmt.Errorf("deployment_id %q is not a valid scope identifier (no wildcards, padding or control characters, at most 128 characters)", c.DeploymentID)
	}
	if c.Audience != "" && !validAudience(c.Audience) {
		return fmt.Errorf("audience must be 1-%d characters without whitespace or control characters", maxIssuerLen)
	}
	if c.MaxTokenLifetime <= 0 || c.MaxTokenLifetime > MaxLifetime {
		return fmt.Errorf("max_token_lifetime %s must be above zero and at most %s", c.MaxTokenLifetime, MaxLifetime)
	}
	return nil
}

// Options returns the Verifier options this section determines; the caller
// supplies Keys, Replay, Clock and Logger.
func (c Config) Options() Options {
	return Options{DeploymentID: c.DeploymentID, Audience: c.Audience, MaxLifetime: c.MaxTokenLifetime}
}

// registered is the section registered with the configuration loader. Owners
// read it through this pointer (config.Load replaces the fields wholesale).
var registered = func() *Config { c := DefaultConfig(); return &c }()

func init() { config.Register(SectionName, registered) }

// CurrentConfig returns the identity section as of the last successful
// config.Load (the defaults before any).
func CurrentConfig() Config { return *registered }

// ConfigSection returns the registered section, for a composition root that
// loads configuration through a registry other than config.Default (tests,
// tools): it registers this same pointer there under SectionName, and
// CurrentConfig then reads what that registry loaded. Read the section through
// CurrentConfig, never by keeping this pointer's fields.
func ConfigSection() *Config { return registered }
