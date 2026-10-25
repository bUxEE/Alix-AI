package realtime

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	pv "github.com/buxee/alix-ai/internal/providers/voice"
)

// Config configures the Realtime client. The connection (endpoint, TLS, API key
// reference, request timeout) is the localai.Config the composition root
// already has for the text client, passed to New beside this value. A plain
// struct, like localai.Config and voice.Config: the adapter registers no
// configuration section. Zero values take the defaults.
type Config struct {
	// Model is the LocalAI pipeline model (a model whose pipeline names a VAD
	// and a transcription model) the Realtime session runs. Empty means the
	// client refuses to open a session (validation_failed).
	Model string `yaml:"model"`
	// Profiles maps a request's Profile hint to another pipeline model (a
	// language pack's, for instance). An unknown profile is refused.
	Profiles map[string]string `yaml:"profiles"`
	// ServerVAD lets the runtime cut the audio into utterances itself (turn
	// detection with automatic answers and interruptions off); each utterance
	// becomes one final transcript segment. The default is manual: the whole
	// stream is one utterance, committed by CloseSend. The runtime refuses a
	// manual commit while its own turn detection is active, so the two modes
	// are exclusive.
	ServerVAD bool `yaml:"server_vad"`
	// SilenceMs is the silence that ends an utterance in ServerVAD mode
	// (default 500).
	SilenceMs int `yaml:"silence_ms"`
	// SettleTimeout is how long, in ServerVAD mode, the client waits after the
	// trailing silence for the runtime's last utterance to finish before it
	// ends the stream (default 1500 ms).
	SettleTimeout time.Duration `yaml:"settle_timeout"`
	// MaxAudioBytes and MaxChunkBytes are the voice.Limits applied before
	// anything is sent (defaults: voice.DefaultLimits).
	MaxAudioBytes int `yaml:"max_audio_bytes"`
	MaxChunkBytes int `yaml:"max_chunk_bytes"`
}

const (
	defaultSilenceMs     = 500
	defaultSettleTimeout = 1500 * time.Millisecond
	// vadTickMs is the runtime's turn-detection poll interval (LocalAI
	// v4.10.0 handleVAD): trailing silence must outlast it.
	vadTickMs = 300
)

var (
	modelNameRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@+/-]{0,199}$`)
	profileNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// Validate checks the configuration without touching the network or the
// secret store. It returns the first problem.
func (c Config) Validate() error {
	if c.Model != "" && !modelNameRE.MatchString(c.Model) {
		return errors.New("localai realtime: model is not a valid model name")
	}
	for name, m := range c.Profiles {
		if !profileNameRE.MatchString(name) {
			return fmt.Errorf("localai realtime: profile name %q is not valid", name)
		}
		if m != "" && !modelNameRE.MatchString(m) {
			return fmt.Errorf("localai realtime: profiles.%s is not a valid model name", name)
		}
	}
	if c.MaxAudioBytes < 0 || c.MaxChunkBytes < 0 || c.SilenceMs < 0 || c.SettleTimeout < 0 {
		return errors.New("localai realtime: limits and timings must not be negative")
	}
	if c.SilenceMs > 10000 {
		return errors.New("localai realtime: silence_ms must be at most 10000")
	}
	return c.limits().Validate()
}

func (c Config) limits() pv.Limits {
	l := pv.DefaultLimits()
	if c.MaxAudioBytes > 0 {
		l.MaxAudioBytes = c.MaxAudioBytes
	}
	if c.MaxChunkBytes > 0 {
		l.MaxChunkBytes = c.MaxChunkBytes
	}
	return l
}

func (c Config) silenceMs() int {
	if c.SilenceMs > 0 {
		return c.SilenceMs
	}
	return defaultSilenceMs
}

func (c Config) settle() time.Duration {
	if c.SettleTimeout > 0 {
		return c.SettleTimeout
	}
	return defaultSettleTimeout
}

func (c Config) withCopies() Config {
	p := make(map[string]string, len(c.Profiles))
	for k, v := range c.Profiles {
		p[k] = v
	}
	c.Profiles = p
	return c
}
