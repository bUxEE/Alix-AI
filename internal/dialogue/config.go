package dialogue

import "github.com/buxee/alix-ai/internal/config"

// SectionName is the configuration section this package owns.
//
//	dialogue:
//	  stream_verified: false   # ADR 0046; off keeps 1.3 whole-reply behaviour
//
// with ALIXAI_DIALOGUE_STREAM_VERIFIED as an override.
const SectionName = "dialogue"

// FileConfig is the YAML section. StreamVerified defaults false (safe additive default).
type FileConfig struct {
	StreamVerified bool `yaml:"stream_verified"`
}

var registered = FileConfig{}

func init() { config.Register(SectionName, &registered) }

// Loaded returns the registered dialogue section after a successful config.Load
// (or the defaults). Composition roots copy StreamVerified into Config.
func Loaded() FileConfig { return registered }
