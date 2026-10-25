package profile

import (
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// SectionName is the configuration section this package owns:
//
//	voice:
//	  enabled: false
//	  active_pack: ""
//
// with ALIXAI_VOICE_ENABLED and ALIXAI_VOICE_ACTIVE_PACK as overrides. Other
// voice packages needing settings register sections of their own.
const SectionName = "voice"

// Config is the voice section. The zero value is the default: voice off.
type Config struct {
	// Enabled switches the voice profile on. Off by default; an installed but
	// disabled profile is not loaded at all.
	Enabled bool `yaml:"enabled"`
	// ActivePack names an installed language pack to activate when the
	// profile loads. Empty means none: the operator activates one explicitly.
	ActivePack string `yaml:"active_pack"`
}

// Validate rejects a malformed pack id.
func (c Config) Validate() error {
	if c.ActivePack != "" && !ValidPackID(c.ActivePack) {
		return contracts.Newf(contracts.ErrValidationFailed, "voice.active_pack %q is not a language pack id", c.ActivePack)
	}
	return nil
}

// registered is the section registered with the configuration loader. Owners
// read it through this pointer (config.Load replaces the fields wholesale).
var registered = &Config{}

func init() { config.Register(SectionName, registered) }

// CurrentConfig returns the voice section as of the last successful
// config.Load (the defaults before any).
func CurrentConfig() Config { return *registered }
