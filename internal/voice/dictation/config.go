package dictation

import (
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// SectionName is the configuration section this package owns:
//
//	dictation:
//	  max_seconds: 60
//	  audio_class: internal
//
// with ALIXAI_DICTATION_MAX_SECONDS / ALIXAI_DICTATION_AUDIO_CLASS as overrides.
// Safe defaults: 60 s, class internal.
const SectionName = "dictation"

// HardMaxSeconds is the absolute ceiling for max_seconds (and thus body size).
const HardMaxSeconds = 300

// DefaultMaxSeconds is the default clip length when the operator sets nothing.
const DefaultMaxSeconds = 60

// Valid data classes for external dictation (egress audio max_class).
const (
	AudioClassPublic       = "public"
	AudioClassInternal     = "internal"
	AudioClassConfidential = "confidential"
)

// Config is the dictation section. The zero value uses DefaultMaxSeconds and
// AudioClassInternal.
type Config struct {
	// MaxSeconds is the longest clip the route accepts, in whole seconds.
	// Zero means DefaultMaxSeconds. Values above HardMaxSeconds are refused at Validate.
	MaxSeconds int `yaml:"max_seconds"`
	// AudioClass is the data class attached to an external dictation clip for
	// the data-egress guard. Empty means AudioClassInternal. Must be one of
	// public, internal, confidential.
	AudioClass string `yaml:"audio_class"`
}

// Validate rejects an out-of-range max_seconds or an unknown audio_class.
func (c Config) Validate() error {
	if c.MaxSeconds < 0 || c.MaxSeconds > HardMaxSeconds {
		return contracts.Newf(contracts.ErrValidationFailed, "dictation.max_seconds must be between 0 and %d", HardMaxSeconds)
	}
	switch c.AudioClass {
	case "", AudioClassPublic, AudioClassInternal, AudioClassConfidential:
	default:
		return contracts.New(contracts.ErrValidationFailed, "dictation.audio_class must be public, internal or confidential")
	}
	return nil
}

// EffectiveMaxSeconds returns the operator cap, or the default when unset.
func (c Config) EffectiveMaxSeconds() int {
	if c.MaxSeconds <= 0 {
		return DefaultMaxSeconds
	}
	return c.MaxSeconds
}

// EffectiveAudioClass returns the egress class for external dictation clips.
func (c Config) EffectiveAudioClass() string {
	if c.AudioClass == "" {
		return AudioClassInternal
	}
	return c.AudioClass
}

var registered = &Config{}

func init() { config.Register(SectionName, registered) }

// CurrentConfig returns the dictation section as of the last successful config.Load.
func CurrentConfig() Config { return *registered }
