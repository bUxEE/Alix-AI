package metrics

import "github.com/buxee/alix-ai/internal/config"

// SectionName is the configuration section this package owns:
//
//	metrics:
//	  enabled: false              # off until the operator turns it on
//	  listen: 127.0.0.1:9464      # loopback only; anything else fails the load
//
// with ALIXAI_METRICS_ENABLED and ALIXAI_METRICS_LISTEN as overrides. There is
// no member for a remote address, a push URL or an interval, and there never
// will be: a number leaves the machine only when the operator points their own
// scraper at the loopback listener (master plan 3 rule 3).
const SectionName = "metrics"

// Config is the metrics section.
type Config struct {
	// Enabled starts the loopback listener. Disabled by default.
	Enabled bool `yaml:"enabled"`
	// Listen is the loopback host:port the scraper connects to.
	Listen string `yaml:"listen"`
}

// DefaultConfig returns the defaults: disabled, on the default loopback port.
func DefaultConfig() Config { return Config{Listen: DefaultListen} }

// Validate implements the config section validation hook: the address must be
// loopback whether or not the listener is enabled, so a file that would
// publish metrics once switched on is refused now.
func (c Config) Validate() error { return CheckLoopbackAddr(c.Listen) }

var section = DefaultConfig()

func init() { config.Register(SectionName, &section) }

// FromConfig returns the loaded `metrics` section (call after config.Load).
func FromConfig() Config { return section }
