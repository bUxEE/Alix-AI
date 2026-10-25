package voice

import (
	"errors"
	"fmt"
	"regexp"

	pv "github.com/buxee/alix-ai/internal/providers/voice"
)

// ModelSet names the LocalAI model used for each voice role.
type ModelSet struct {
	STT string `yaml:"stt"`
	TTS string `yaml:"tts"`
	VAD string `yaml:"vad"`
}

// Config configures the voice client. The connection (endpoint, TLS, API key
// reference, request timeout, response size cap) is the localai.Config the
// composition root already has for the text client, passed to New beside this
// value, so both clients reach the same runtime under the same endpoint
// policy. A plain struct, like localai.Config: the adapter registers no
// configuration section itself. Zero values take the defaults.
type Config struct {
	// Models names the model each role uses. A role left empty is refused at
	// call time (validation_failed), so a deployment that installed only STT
	// simply has no TTS.
	Models ModelSet `yaml:"models"`
	// Profiles maps a request's Profile hint to model names (a language
	// pack's models, for instance). A field left empty falls back to Models.
	// An unknown profile is refused, so callers never reach a model that was
	// not configured.
	Profiles map[string]ModelSet `yaml:"profiles"`
	// MaxAudioBytes, MaxChunkBytes and MaxTextRunes are the voice.Limits
	// applied before a request reaches the runtime (defaults:
	// voice.DefaultLimits).
	MaxAudioBytes int `yaml:"max_audio_bytes"`
	MaxChunkBytes int `yaml:"max_chunk_bytes"`
	MaxTextRunes  int `yaml:"max_text_runes"`
	// TTSNoStream asks the runtime for a complete audio file instead of a
	// stream, for a backend that cannot stream. The audio is still delivered
	// to the caller in chunks.
	TTSNoStream bool `yaml:"tts_no_stream"`
}

var (
	modelNameRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@+/-]{0,199}$`)
	profileNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// Validate checks the configuration without touching the network or the
// secret store. It returns the first problem.
func (c Config) Validate() error {
	if err := validateModelSet("models", c.Models); err != nil {
		return err
	}
	for name, set := range c.Profiles {
		if !profileNameRE.MatchString(name) {
			return fmt.Errorf("localai voice: profile name %q is not valid", name)
		}
		if err := validateModelSet("profiles."+name, set); err != nil {
			return err
		}
	}
	if c.MaxAudioBytes < 0 || c.MaxChunkBytes < 0 || c.MaxTextRunes < 0 {
		return errors.New("localai voice: limits must not be negative")
	}
	return c.limits().Validate()
}

func validateModelSet(where string, s ModelSet) error {
	for role, name := range map[string]string{"stt": s.STT, "tts": s.TTS, "vad": s.VAD} {
		if name != "" && !modelNameRE.MatchString(name) {
			return fmt.Errorf("localai voice: %s.%s is not a valid model name", where, role)
		}
	}
	return nil
}

// limits returns the effective limits: configured values over the defaults.
func (c Config) limits() pv.Limits {
	l := pv.DefaultLimits()
	if c.MaxAudioBytes > 0 {
		l.MaxAudioBytes = c.MaxAudioBytes
	}
	if c.MaxChunkBytes > 0 {
		l.MaxChunkBytes = c.MaxChunkBytes
	}
	if c.MaxTextRunes > 0 {
		l.MaxTextRunes = c.MaxTextRunes
	}
	return l
}

// withCopies returns a copy whose map is private, so later changes by the
// caller cannot reach the client.
func (c Config) withCopies() Config {
	profiles := make(map[string]ModelSet, len(c.Profiles))
	for k, v := range c.Profiles {
		profiles[k] = v
	}
	c.Profiles = profiles
	return c
}
