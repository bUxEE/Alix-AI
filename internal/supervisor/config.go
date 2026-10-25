package supervisor

import (
	"fmt"
	"time"
)

// Defaults and bounds. A zero field means "the default"; there is no way to ask
// for unbounded restarts or an unbounded wait.
const (
	DefaultMaxRestarts    = 5
	DefaultRestartWindow  = 5 * time.Minute // matches the unit's StartLimitIntervalSec
	DefaultStartTimeout   = 5 * time.Minute // LocalAI preloads its models before /readyz answers
	DefaultProbeInterval  = time.Second
	DefaultProbeTimeout   = 5 * time.Second
	DefaultHealthInterval = 5 * time.Second
	DefaultUnhealthyAfter = 3

	DefaultBackoffInitial = time.Second
	DefaultBackoffMax     = 30 * time.Second
	DefaultBackoffFactor  = 2.0
	DefaultBackoffJitter  = 0.2

	maxRestartsCap  = 100
	maxWindow       = 24 * time.Hour
	maxStartTimeout = time.Hour
	maxInterval     = 10 * time.Minute
	minInterval     = time.Millisecond
	maxUnhealthy    = 100
	maxFactor       = 10.0
)

// Backoff is the delay schedule between a failure and the next attempt.
type Backoff struct {
	// Initial is the delay after the first failure in the window.
	Initial time.Duration `yaml:"initial"`
	// Max caps the delay.
	Max time.Duration `yaml:"max"`
	// Factor multiplies the delay for each further failure in the window.
	Factor float64 `yaml:"factor"`
	// Jitter spreads each delay by up to this fraction either way (0 to 1), so
	// several hosts do not retry in step. The supervisor draws it from an
	// injectable source; tests pin it.
	Jitter float64 `yaml:"jitter"`
}

// Delay is the wait after the n-th failure inside the window (n starts at 1).
// r is a uniform draw from [0, 1); 0.5 applies no jitter. The result never
// exceeds Max.
func (b Backoff) Delay(n int, r float64) time.Duration {
	if n < 1 {
		n = 1
	}
	d := float64(b.Initial)
	limit := float64(b.Max)
	for i := 1; i < n && d < limit; i++ {
		d *= b.Factor
	}
	if d > limit {
		d = limit
	}
	if b.Jitter > 0 {
		d *= 1 + b.Jitter*(2*r-1)
	}
	if d > limit {
		d = limit
	}
	if d < float64(time.Millisecond) {
		d = float64(time.Millisecond)
	}
	return time.Duration(d)
}

// Config tunes the supervision loop.
type Config struct {
	Backoff Backoff `yaml:"backoff"`
	// MaxRestarts is how many restarts one RestartWindow allows; the failure
	// after that makes the supervisor give up.
	MaxRestarts   int           `yaml:"max_restarts"`
	RestartWindow time.Duration `yaml:"restart_window"`
	// StartTimeout is how long a started runtime has to become healthy.
	StartTimeout time.Duration `yaml:"start_timeout"`
	// ProbeInterval spaces probes while the runtime is starting; HealthInterval
	// spaces them once it is running. ProbeTimeout bounds one probe.
	ProbeInterval  time.Duration `yaml:"probe_interval"`
	ProbeTimeout   time.Duration `yaml:"probe_timeout"`
	HealthInterval time.Duration `yaml:"health_interval"`
	// UnhealthyAfter is how many consecutive unavailable probes make a running
	// runtime a failure. A degraded runtime is healthy enough: it is up.
	UnhealthyAfter int `yaml:"unhealthy_after"`
}

// DefaultConfig returns the defaults.
func DefaultConfig() Config {
	return Config{
		Backoff: Backoff{
			Initial: DefaultBackoffInitial,
			Max:     DefaultBackoffMax,
			Factor:  DefaultBackoffFactor,
			Jitter:  DefaultBackoffJitter,
		},
		MaxRestarts:    DefaultMaxRestarts,
		RestartWindow:  DefaultRestartWindow,
		StartTimeout:   DefaultStartTimeout,
		ProbeInterval:  DefaultProbeInterval,
		ProbeTimeout:   DefaultProbeTimeout,
		HealthInterval: DefaultHealthInterval,
		UnhealthyAfter: DefaultUnhealthyAfter,
	}
}

// WithDefaults replaces zero fields with the defaults. A zero Jitter stays
// zero (no jitter is a legitimate choice); every other zero means "default".
func (c Config) WithDefaults() Config {
	d := DefaultConfig()
	if c.Backoff.Initial == 0 {
		c.Backoff.Initial = d.Backoff.Initial
	}
	if c.Backoff.Max == 0 {
		c.Backoff.Max = d.Backoff.Max
	}
	if c.Backoff.Factor == 0 {
		c.Backoff.Factor = d.Backoff.Factor
	}
	if c.MaxRestarts == 0 {
		c.MaxRestarts = d.MaxRestarts
	}
	if c.RestartWindow == 0 {
		c.RestartWindow = d.RestartWindow
	}
	if c.StartTimeout == 0 {
		c.StartTimeout = d.StartTimeout
	}
	if c.ProbeInterval == 0 {
		c.ProbeInterval = d.ProbeInterval
	}
	if c.ProbeTimeout == 0 {
		c.ProbeTimeout = d.ProbeTimeout
	}
	if c.HealthInterval == 0 {
		c.HealthInterval = d.HealthInterval
	}
	if c.UnhealthyAfter == 0 {
		c.UnhealthyAfter = d.UnhealthyAfter
	}
	return c
}

// Validate rejects negative and absurd values. Zero means the default.
func (c Config) Validate() error {
	dur := func(name string, v, lo, hi time.Duration) error {
		if v != 0 && (v < lo || v > hi) {
			return fmt.Errorf("supervisor: %s %s out of range (%s-%s)", name, v, lo, hi)
		}
		return nil
	}
	if c.MaxRestarts < 0 || c.MaxRestarts > maxRestartsCap {
		return fmt.Errorf("supervisor: max_restarts %d out of range (1-%d)", c.MaxRestarts, maxRestartsCap)
	}
	if c.UnhealthyAfter < 0 || c.UnhealthyAfter > maxUnhealthy {
		return fmt.Errorf("supervisor: unhealthy_after %d out of range (1-%d)", c.UnhealthyAfter, maxUnhealthy)
	}
	if f := c.Backoff.Factor; f != 0 && (f < 1 || f > maxFactor) {
		return fmt.Errorf("supervisor: backoff factor %v out of range (1-%v)", f, maxFactor)
	}
	if j := c.Backoff.Jitter; j < 0 || j >= 1 {
		return fmt.Errorf("supervisor: backoff jitter %v out of range [0, 1)", j)
	}
	for _, err := range []error{
		dur("backoff initial", c.Backoff.Initial, minInterval, maxInterval),
		dur("backoff max", c.Backoff.Max, minInterval, maxWindow),
		dur("restart_window", c.RestartWindow, time.Second, maxWindow),
		dur("start_timeout", c.StartTimeout, minInterval, maxStartTimeout),
		dur("probe_interval", c.ProbeInterval, minInterval, maxInterval),
		dur("probe_timeout", c.ProbeTimeout, minInterval, maxInterval),
		dur("health_interval", c.HealthInterval, minInterval, maxInterval),
	} {
		if err != nil {
			return err
		}
	}
	d := c.WithDefaults()
	if d.Backoff.Initial > d.Backoff.Max {
		return fmt.Errorf("supervisor: backoff initial %s exceeds max %s", d.Backoff.Initial, d.Backoff.Max)
	}
	return nil
}
