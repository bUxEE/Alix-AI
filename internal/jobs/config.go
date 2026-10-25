package jobs

import (
	"fmt"
	"time"

	"github.com/buxee/alix-ai/internal/config"
)

// Defaults and bounds. They are the Lite-profile values; docs/efficiency/M0-06.md
// records what they cost.
const (
	DefaultPollInterval    = time.Second
	DefaultLeaseTTL        = 30 * time.Second
	DefaultMaxAttempts     = 5
	DefaultBackoffBase     = 2 * time.Second
	DefaultBackoffMax      = 5 * time.Minute
	DefaultMaxPayloadBytes = 64 << 10
	DefaultRetainFinished  = 7 * 24 * time.Hour
	DefaultPolicyWindow    = 256
	DefaultShutdownGrace   = 10 * time.Second

	// DefaultInteractiveUpTo and DefaultBackgroundFrom are the section 17.1
	// yield rule: while a job of class 1 or 2 is waiting, workers of class 5
	// and 6 lease nothing.
	DefaultInteractiveUpTo = int(ClassTurn)
	DefaultBackgroundFrom  = int(ClassIngest)

	maxWorkers      = 64
	maxDepth        = 1_000_000
	maxAttemptsCap  = 100
	maxPayloadCap   = 4 << 20
	maxPolicyWindow = 4096
	minPoll         = time.Millisecond
	maxPoll         = time.Minute
	minLeaseTTL     = 3 * time.Millisecond
	maxLeaseTTL     = time.Hour
	maxBackoff      = 24 * time.Hour

	// pruneBatch bounds one DELETE so retention never holds the writer long.
	pruneBatch = 1000
	// maxLeaseBatch bounds how many jobs one Lease call takes.
	maxLeaseBatch = 64
)

// ClassConfig is the worker limit and queue depth of one priority class.
type ClassConfig struct {
	// Workers is how many jobs of this class run at once in one Pool. Zero
	// means the default.
	Workers int `yaml:"workers"`
	// MaxDepth is how many jobs of this class may be waiting (pending). An
	// enqueue beyond it is refused with resource_exhausted. Zero means the
	// default; there is no unbounded setting.
	MaxDepth int `yaml:"max_depth"`
}

// Config is the "jobs" configuration section. A zero field means "default".
type Config struct {
	// PollInterval is how often an idle Pool looks for work that did not
	// arrive through an in-process enqueue: another process's enqueue, a retry
	// whose backoff elapsed, an expired lease.
	PollInterval time.Duration `yaml:"poll_interval"`
	// LeaseTTL is how long a worker may hold a job without a heartbeat before
	// the job returns to pending. A worker heartbeats every third of it.
	LeaseTTL time.Duration `yaml:"lease_ttl"`
	// MaxAttempts is the default attempt limit of a job type that sets none.
	MaxAttempts int `yaml:"max_attempts"`
	// BackoffBase and BackoffMax shape the default retry delay:
	// base * 2^(attempt-1), capped at max, then jittered into [d/2, d].
	BackoffBase time.Duration `yaml:"backoff_base"`
	BackoffMax  time.Duration `yaml:"backoff_max"`
	// MaxPayloadBytes bounds one job's payload. A payload is a reference to the
	// work, not the data itself.
	MaxPayloadBytes int `yaml:"max_payload_bytes"`
	// RetainFinished is how long a finished job is kept for inspection (and
	// for its idempotency key) before Prune deletes it.
	RetainFinished time.Duration `yaml:"retain_finished"`
	// PolicyWindow bounds how many candidates a custom DequeuePolicy sees.
	PolicyWindow int `yaml:"policy_window"`
	// InteractiveUpTo and BackgroundFrom are the yield rule: while a job of a
	// class <= InteractiveUpTo is waiting, classes >= BackgroundFrom lease
	// nothing. BackgroundFrom 7 turns the rule off.
	InteractiveUpTo int `yaml:"interactive_up_to_class"`
	BackgroundFrom  int `yaml:"background_from_class"`
	// ShutdownGrace is how long Pool.Run waits for running jobs after its
	// context ends before cancelling them (they return to pending).
	ShutdownGrace time.Duration `yaml:"shutdown_grace"`

	Class1 ClassConfig `yaml:"class1"`
	Class2 ClassConfig `yaml:"class2"`
	Class3 ClassConfig `yaml:"class3"`
	Class4 ClassConfig `yaml:"class4"`
	Class5 ClassConfig `yaml:"class5"`
	Class6 ClassConfig `yaml:"class6"`
}

// DefaultConfig returns the Lite-profile defaults: two workers for each
// interactive class, one for each background class, and finite depths.
func DefaultConfig() Config {
	return Config{
		PollInterval:    DefaultPollInterval,
		LeaseTTL:        DefaultLeaseTTL,
		MaxAttempts:     DefaultMaxAttempts,
		BackoffBase:     DefaultBackoffBase,
		BackoffMax:      DefaultBackoffMax,
		MaxPayloadBytes: DefaultMaxPayloadBytes,
		RetainFinished:  DefaultRetainFinished,
		PolicyWindow:    DefaultPolicyWindow,
		InteractiveUpTo: DefaultInteractiveUpTo,
		BackgroundFrom:  DefaultBackgroundFrom,
		ShutdownGrace:   DefaultShutdownGrace,
		Class1:          ClassConfig{Workers: 2, MaxDepth: 256},
		Class2:          ClassConfig{Workers: 2, MaxDepth: 256},
		Class3:          ClassConfig{Workers: 2, MaxDepth: 256},
		Class4:          ClassConfig{Workers: 2, MaxDepth: 512},
		Class5:          ClassConfig{Workers: 1, MaxDepth: 5000},
		Class6:          ClassConfig{Workers: 1, MaxDepth: 1000},
	}
}

// Validate implements the config section validation hook. Zero is allowed
// (it means the default); negative and absurd values are not.
func (c Config) Validate() error {
	inRange := func(name string, v, lo, hi int) error {
		if v != 0 && (v < lo || v > hi) {
			return fmt.Errorf("%s %d out of range (%d-%d)", name, v, lo, hi)
		}
		return nil
	}
	dur := func(name string, v, lo, hi time.Duration) error {
		if v != 0 && (v < lo || v > hi) {
			return fmt.Errorf("%s %s out of range (%s-%s)", name, v, lo, hi)
		}
		return nil
	}
	checks := []error{
		dur("poll_interval", c.PollInterval, minPoll, maxPoll),
		dur("lease_ttl", c.LeaseTTL, minLeaseTTL, maxLeaseTTL),
		inRange("max_attempts", c.MaxAttempts, 1, maxAttemptsCap),
		dur("backoff_base", c.BackoffBase, time.Millisecond, maxBackoff),
		dur("backoff_max", c.BackoffMax, time.Millisecond, maxBackoff),
		inRange("max_payload_bytes", c.MaxPayloadBytes, 1, maxPayloadCap),
		dur("retain_finished", c.RetainFinished, time.Minute, 3650*24*time.Hour),
		inRange("policy_window", c.PolicyWindow, 1, maxPolicyWindow),
		inRange("interactive_up_to_class", c.InteractiveUpTo, 1, numClasses-1),
		inRange("background_from_class", c.BackgroundFrom, 2, numClasses+1),
		dur("shutdown_grace", c.ShutdownGrace, time.Millisecond, time.Hour),
	}
	for i, cc := range []ClassConfig{c.Class1, c.Class2, c.Class3, c.Class4, c.Class5, c.Class6} {
		checks = append(checks,
			inRange(fmt.Sprintf("class%d.workers", i+1), cc.Workers, 1, maxWorkers),
			inRange(fmt.Sprintf("class%d.max_depth", i+1), cc.MaxDepth, 1, maxDepth))
	}
	for _, e := range checks {
		if e != nil {
			return e
		}
	}
	d := c.withDefaults()
	if d.BackoffBase > d.BackoffMax {
		return fmt.Errorf("backoff_base %s exceeds backoff_max %s", d.BackoffBase, d.BackoffMax)
	}
	if d.InteractiveUpTo >= d.BackgroundFrom {
		return fmt.Errorf("interactive_up_to_class %d must be below background_from_class %d", d.InteractiveUpTo, d.BackgroundFrom)
	}
	return nil
}

// withDefaults replaces zero fields with the defaults.
func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.PollInterval == 0 {
		c.PollInterval = d.PollInterval
	}
	if c.LeaseTTL == 0 {
		c.LeaseTTL = d.LeaseTTL
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = d.MaxAttempts
	}
	if c.BackoffBase == 0 {
		c.BackoffBase = d.BackoffBase
	}
	if c.BackoffMax == 0 {
		c.BackoffMax = d.BackoffMax
	}
	if c.MaxPayloadBytes == 0 {
		c.MaxPayloadBytes = d.MaxPayloadBytes
	}
	if c.RetainFinished == 0 {
		c.RetainFinished = d.RetainFinished
	}
	if c.PolicyWindow == 0 {
		c.PolicyWindow = d.PolicyWindow
	}
	if c.InteractiveUpTo == 0 {
		c.InteractiveUpTo = d.InteractiveUpTo
	}
	if c.BackgroundFrom == 0 {
		c.BackgroundFrom = d.BackgroundFrom
	}
	if c.ShutdownGrace == 0 {
		c.ShutdownGrace = d.ShutdownGrace
	}
	fill := func(c, d ClassConfig) ClassConfig {
		if c.Workers == 0 {
			c.Workers = d.Workers
		}
		if c.MaxDepth == 0 {
			c.MaxDepth = d.MaxDepth
		}
		return c
	}
	c.Class1, c.Class2, c.Class3 = fill(c.Class1, d.Class1), fill(c.Class2, d.Class2), fill(c.Class3, d.Class3)
	c.Class4, c.Class5, c.Class6 = fill(c.Class4, d.Class4), fill(c.Class5, d.Class5), fill(c.Class6, d.Class6)
	return c
}

// ForClass returns the worker limit and depth of one class (defaults applied
// to a zero field).
func (c Config) ForClass(class Class) ClassConfig {
	return c.withDefaults().classConfig(class)
}

// classConfig is ForClass of a Config whose defaults are already applied (the
// queue's own, defaulted once when it is opened), without applying them again.
func (c Config) classConfig(class Class) ClassConfig {
	switch class {
	case ClassAuthCommit:
		return c.Class1
	case ClassTurn:
		return c.Class2
	case ClassRAGQuery:
		return c.Class3
	case ClassRead:
		return c.Class4
	case ClassIngest:
		return c.Class5
	case ClassMaintenance:
		return c.Class6
	}
	return ClassConfig{}
}

// isBackground reports whether a class yields to waiting interactive work.
// Call it on a defaulted Config.
func (c Config) isBackground(class Class) bool { return int(class) >= c.BackgroundFrom }

// section is the registered "jobs" config section. Owners read it through
// this pointer (see config.Register).
var section = DefaultConfig()

func init() { config.Register("jobs", &section) }

// FromConfig returns the loaded "jobs" section (call after config.Load).
func FromConfig() Config { return section }
