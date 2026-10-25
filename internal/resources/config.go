package resources

import (
	"fmt"
	"time"

	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/jobs"
)

// Defaults and bounds (Lite profile; docs/efficiency/M3-17.md records the
// cost). Every limit has a finite default and a finite ceiling: a zero field
// means "the default", never "unlimited" (master plan 17.2: no unbounded loops,
// and work is rejected or queued before memory runs out).
const (
	DefaultMemoryBudget      = 6 << 30 // 8 GB Lite host minus the OS and the app database
	DefaultBackgroundPercent = 70
	DefaultProbeInterval     = 2 * time.Second
	DefaultStaleAfter        = 10 * time.Second
	DefaultYieldPoll         = 100 * time.Millisecond
	DefaultYieldMaxWait      = 30 * time.Second
	DefaultIdleUnloadAfter   = 15 * time.Minute
	DefaultMinWarm           = 2 * time.Minute
	DefaultMaxUnloads        = 4
	DefaultDiskPauseFree     = 2 << 30
	DefaultDiskResumeFree    = 3 << 30
	DefaultDiskReserve       = 512 << 20
	DefaultMaxLoopSteps      = 8
	DefaultMaxLoopWall       = 90 * time.Second

	minBudget       = 64 << 20
	minPercent      = 10
	maxPercent      = 95
	minProbe        = 10 * time.Millisecond
	maxProbe        = time.Minute
	maxStale        = 10 * time.Minute
	minYieldPoll    = time.Millisecond
	maxYieldPoll    = 10 * time.Second
	maxYieldWait    = 10 * time.Minute
	maxIdle         = 7 * 24 * time.Hour
	maxUnloadsCap   = 32
	maxBytes        = 1 << 50
	maxLoopStepsCap = 64
	maxLoopWallCap  = 10 * time.Minute
	maxPinned       = 64
)

// Config is the "resources" configuration section. A zero field means "the
// default"; there is no way to ask for an unbounded setting.
type Config struct {
	// MemoryBudgetBytes bounds process RSS plus provider-reported runtime
	// memory plus outstanding admission reservations.
	MemoryBudgetBytes uint64 `yaml:"memory_budget_bytes"`
	// BackgroundPercent is the share of the budget background work may use;
	// the rest is the interactive reserve background work can never take.
	BackgroundPercent int `yaml:"background_memory_percent"`
	// InteractiveUpTo and BackgroundFrom mirror internal/jobs: a live piece of
	// work of a class <= InteractiveUpTo makes classes >= BackgroundFrom yield.
	InteractiveUpTo int `yaml:"interactive_up_to_class"`
	BackgroundFrom  int `yaml:"background_from_class"`

	// ProbeInterval is how often Run refreshes the cached readings; StaleAfter
	// is how old a reading may be before it counts as unknown.
	ProbeInterval time.Duration `yaml:"probe_interval"`
	StaleAfter    time.Duration `yaml:"stale_after"`

	// YieldPoll and YieldMaxWait bound how long a running background handler
	// waits for interactive work to finish (Yield).
	YieldPoll    time.Duration `yaml:"yield_poll"`
	YieldMaxWait time.Duration `yaml:"yield_max_wait"`

	// IdleUnloadAfter unloads a model idle for that long; MinWarm is the
	// shortest time a model stays loaded whatever its idleness, so a burst of
	// requests cannot force load/unload thrash. IdleUnloadDisabled turns the
	// policy off (LocalAI's own watchdog may still apply).
	IdleUnloadAfter    time.Duration `yaml:"idle_unload_after"`
	MinWarm            time.Duration `yaml:"min_warm"`
	IdleUnloadDisabled bool          `yaml:"idle_unload_disabled"`
	// PinnedModels are model ids the policy never unloads.
	PinnedModels []string `yaml:"pinned_models"`
	// MaxUnloadsPerSweep bounds the work of one sweep.
	MaxUnloadsPerSweep int `yaml:"max_unloads_per_sweep"`

	// Disk floors, on free bytes of every watched volume. Ingestion pauses
	// below DiskPauseFreeBytes and resumes only at DiskResumeFreeBytes
	// (hysteresis); below DiskReserveBytes the volume is critical: what is left
	// belongs to the transactional store.
	DiskPauseFreeBytes  uint64 `yaml:"disk_pause_free_bytes"`
	DiskResumeFreeBytes uint64 `yaml:"disk_resume_free_bytes"`
	DiskReserveBytes    uint64 `yaml:"disk_reserve_bytes"`

	// MaxLoopSteps and MaxLoopWall bound every agent or tool loop.
	MaxLoopSteps int           `yaml:"max_loop_steps"`
	MaxLoopWall  time.Duration `yaml:"max_loop_wall"`
}

// DefaultConfig returns the Lite-profile defaults.
func DefaultConfig() Config {
	return Config{
		MemoryBudgetBytes:   DefaultMemoryBudget,
		BackgroundPercent:   DefaultBackgroundPercent,
		InteractiveUpTo:     jobs.DefaultInteractiveUpTo,
		BackgroundFrom:      jobs.DefaultBackgroundFrom,
		ProbeInterval:       DefaultProbeInterval,
		StaleAfter:          DefaultStaleAfter,
		YieldPoll:           DefaultYieldPoll,
		YieldMaxWait:        DefaultYieldMaxWait,
		IdleUnloadAfter:     DefaultIdleUnloadAfter,
		MinWarm:             DefaultMinWarm,
		MaxUnloadsPerSweep:  DefaultMaxUnloads,
		DiskPauseFreeBytes:  DefaultDiskPauseFree,
		DiskResumeFreeBytes: DefaultDiskResumeFree,
		DiskReserveBytes:    DefaultDiskReserve,
		MaxLoopSteps:        DefaultMaxLoopSteps,
		MaxLoopWall:         DefaultMaxLoopWall,
	}
}

// WithDefaults replaces zero fields with the defaults.
func (c Config) WithDefaults() Config {
	d := DefaultConfig()
	if c.MemoryBudgetBytes == 0 {
		c.MemoryBudgetBytes = d.MemoryBudgetBytes
	}
	if c.BackgroundPercent == 0 {
		c.BackgroundPercent = d.BackgroundPercent
	}
	if c.InteractiveUpTo == 0 {
		c.InteractiveUpTo = d.InteractiveUpTo
	}
	if c.BackgroundFrom == 0 {
		c.BackgroundFrom = d.BackgroundFrom
	}
	if c.ProbeInterval == 0 {
		c.ProbeInterval = d.ProbeInterval
	}
	if c.StaleAfter == 0 {
		c.StaleAfter = d.StaleAfter
	}
	if c.YieldPoll == 0 {
		c.YieldPoll = d.YieldPoll
	}
	if c.YieldMaxWait == 0 {
		c.YieldMaxWait = d.YieldMaxWait
	}
	if c.IdleUnloadAfter == 0 {
		c.IdleUnloadAfter = d.IdleUnloadAfter
	}
	if c.MinWarm == 0 {
		c.MinWarm = d.MinWarm
	}
	if c.MaxUnloadsPerSweep == 0 {
		c.MaxUnloadsPerSweep = d.MaxUnloadsPerSweep
	}
	if c.DiskPauseFreeBytes == 0 {
		c.DiskPauseFreeBytes = d.DiskPauseFreeBytes
	}
	if c.DiskResumeFreeBytes == 0 {
		c.DiskResumeFreeBytes = d.DiskResumeFreeBytes
	}
	if c.DiskReserveBytes == 0 {
		c.DiskReserveBytes = d.DiskReserveBytes
	}
	if c.MaxLoopSteps == 0 {
		c.MaxLoopSteps = d.MaxLoopSteps
	}
	if c.MaxLoopWall == 0 {
		c.MaxLoopWall = d.MaxLoopWall
	}
	return c
}

// Validate implements the config validation hook. Zero is the default;
// negative, absurd and "unlimited" values are errors.
func (c Config) Validate() error {
	// Bounds are checked on the raw value (a negative number is never zero, so
	// it is caught); the relations below are checked on the defaulted one.
	rng := func(name string, v, lo, hi int) error {
		if v != 0 && (v < lo || v > hi) {
			return fmt.Errorf("resources: %s %d out of range (%d-%d)", name, v, lo, hi)
		}
		return nil
	}
	dur := func(name string, v, lo, hi time.Duration) error {
		if v != 0 && (v < lo || v > hi) {
			return fmt.Errorf("resources: %s %s out of range (%s-%s)", name, v, lo, hi)
		}
		return nil
	}
	size := func(name string, v, lo, hi uint64) error {
		if v != 0 && (v < lo || v > hi) {
			return fmt.Errorf("resources: %s %d out of range (%d-%d)", name, v, lo, hi)
		}
		return nil
	}
	for _, err := range []error{
		size("memory_budget_bytes", c.MemoryBudgetBytes, minBudget, maxBytes),
		rng("background_memory_percent", c.BackgroundPercent, minPercent, maxPercent),
		rng("interactive_up_to_class", c.InteractiveUpTo, 1, int(jobs.ClassMaintenance)-1),
		rng("background_from_class", c.BackgroundFrom, 2, int(jobs.ClassMaintenance)+1),
		dur("probe_interval", c.ProbeInterval, minProbe, maxProbe),
		dur("stale_after", c.StaleAfter, minProbe, maxStale),
		dur("yield_poll", c.YieldPoll, minYieldPoll, maxYieldPoll),
		dur("yield_max_wait", c.YieldMaxWait, minYieldPoll, maxYieldWait),
		dur("idle_unload_after", c.IdleUnloadAfter, time.Second, maxIdle),
		dur("min_warm", c.MinWarm, time.Nanosecond, maxIdle),
		rng("max_unloads_per_sweep", c.MaxUnloadsPerSweep, 1, maxUnloadsCap),
		size("disk_pause_free_bytes", c.DiskPauseFreeBytes, 1, maxBytes),
		size("disk_resume_free_bytes", c.DiskResumeFreeBytes, 1, maxBytes),
		size("disk_reserve_bytes", c.DiskReserveBytes, 1, maxBytes),
		rng("max_loop_steps", c.MaxLoopSteps, 1, maxLoopStepsCap),
		dur("max_loop_wall", c.MaxLoopWall, time.Second, maxLoopWallCap),
	} {
		if err != nil {
			return err
		}
	}
	if len(c.PinnedModels) > maxPinned {
		return fmt.Errorf("resources: %d pinned models (max %d)", len(c.PinnedModels), maxPinned)
	}
	d := c.WithDefaults()
	switch {
	case d.InteractiveUpTo >= d.BackgroundFrom:
		return fmt.Errorf("resources: interactive_up_to_class %d must be below background_from_class %d", d.InteractiveUpTo, d.BackgroundFrom)
	case d.StaleAfter < d.ProbeInterval:
		return fmt.Errorf("resources: stale_after %s is shorter than probe_interval %s, so every reading would be stale", d.StaleAfter, d.ProbeInterval)
	case d.YieldPoll > d.YieldMaxWait:
		return fmt.Errorf("resources: yield_poll %s exceeds yield_max_wait %s", d.YieldPoll, d.YieldMaxWait)
	case d.IdleUnloadAfter < d.MinWarm:
		return fmt.Errorf("resources: idle_unload_after %s is shorter than min_warm %s, so min_warm would silently win", d.IdleUnloadAfter, d.MinWarm)
	case !(d.DiskReserveBytes < d.DiskPauseFreeBytes && d.DiskPauseFreeBytes < d.DiskResumeFreeBytes):
		return fmt.Errorf("resources: disk floors must satisfy reserve (%d) < pause (%d) < resume (%d)", d.DiskReserveBytes, d.DiskPauseFreeBytes, d.DiskResumeFreeBytes)
	}
	return nil
}

// section is the registered "resources" config section. Owners read it
// through this pointer (see config.Register).
var section = DefaultConfig()

func init() { config.Register("resources", &section) }

// FromConfig returns the loaded "resources" section (call after config.Load).
func FromConfig() Config { return section }
