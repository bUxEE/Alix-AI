package ingest

import (
	"fmt"
	"time"

	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/rag/parse"
)

// SectionName is the name of the configuration section of this package.
const SectionName = "rag_ingest"

// Defaults, the Lite-profile values.
const (
	// DefaultMaxFraction is the share of a source's previous items that one scan
	// may delete before the scan is held for an operator.
	DefaultMaxFraction = 0.5
	// DefaultMaxDeleted is the number of items one scan may delete.
	DefaultMaxDeleted = 1000
	// DefaultMinPrevious is the size from which the fraction rule applies; a
	// source of three files losing two is a change, not an incident. The count
	// rule and the empty-root rule have no such floor.
	DefaultMinPrevious = 10

	// DefaultStartupTimeout is how long the parser worker may take to start. It
	// is three times the parser's own default: the first start of a freshly
	// built worker on Windows took 26.5 s under Avast against a 30 s timeout
	// (M2-04), and a start that is merely slow must not be mistaken for a hung
	// parser. Excluding the worker binary from antivirus scanning is the fix;
	// this is the margin.
	DefaultStartupTimeout = 90 * time.Second
	// DefaultProbeTimeout bounds the readiness probe at service start.
	DefaultProbeTimeout = 3 * time.Minute

	// DefaultEnqueueBatch is how many stage jobs one sync (or one pass of the
	// reconciler) enqueues; a larger backlog is drained by a follow-up.
	DefaultEnqueueBatch = 500
	// DefaultPressureRecheck is how long a verdict of the disk-pressure hook is
	// reused, and how long a paused job waits before it is looked at again.
	DefaultPressureRecheck = 30 * time.Second
	// DefaultConfirmTTL is how long an operator's confirmation of a mass deletion
	// stays valid.
	DefaultConfirmTTL = time.Hour
	// DefaultCompactGrace is the grace period of the physical purge of
	// tombstoned rows (index.CompactOptions.Grace).
	DefaultCompactGrace = 24 * time.Hour
)

// GuardConfig configures the mass-deletion guard (master plan 18, 19).
//
// An unmounted NAS share or removable drive leaves a reachable, empty
// directory at its mount point, which a scan cannot tell from a source whose
// owner deleted everything. A scan that would delete more than these limits is
// refused and held until an operator confirms exactly that deletion. There is
// no setting that turns the guard off: raise the limits instead.
type GuardConfig struct {
	// MaxFraction is the largest share of the previous live items one scan may
	// delete, in (0, 1]. It applies from MinPrevious items.
	MaxFraction float64 `yaml:"max_fraction"`
	// MaxDeleted is the largest number of items one scan may delete.
	MaxDeleted int `yaml:"max_deleted"`
	// MinPrevious is the number of previous items from which MaxFraction
	// applies.
	MinPrevious int `yaml:"min_previous"`
}

// ParserConfig is what an operator may set about the parser worker. The rest
// of parse.Config keeps its (validated) defaults.
type ParserConfig struct {
	// WorkerPath is the absolute path of the alixai-parser executable. Empty
	// means the one installed next to the service binary (parse.DefaultWorkerPath).
	WorkerPath string `yaml:"worker_path"`
	// StartupTimeout is how long the worker may take to start and report its
	// confinement. Raise it where an antivirus scans a fresh executable.
	StartupTimeout time.Duration `yaml:"startup_timeout"`
	// WallTimeout is the wall-clock time one document may take.
	WallTimeout time.Duration `yaml:"wall_timeout"`
	// MaxConcurrent is how many workers may run at once. One process is started
	// per document, so this and the class-5 worker count are the throughput
	// dial.
	MaxConcurrent int `yaml:"max_concurrent"`
	// ProbeTimeout bounds the readiness probe at service start.
	ProbeTimeout time.Duration `yaml:"probe_timeout"`
}

// Config is the "rag_ingest" configuration section. A zero field takes its
// default.
type Config struct {
	Guard  GuardConfig  `yaml:"mass_deletion"`
	Parser ParserConfig `yaml:"parser"`
	// EnqueueBatch bounds the stage jobs enqueued by one sync.
	EnqueueBatch int `yaml:"enqueue_batch"`
	// PressureRecheck is the disk-pressure verdict cache and re-look interval.
	PressureRecheck time.Duration `yaml:"pressure_recheck"`
	// ConfirmTTL is how long an operator's confirmation stays valid.
	ConfirmTTL time.Duration `yaml:"confirm_ttl"`
	// CompactGrace is the grace period before tombstoned rows are physically
	// purged by the maintenance job.
	CompactGrace time.Duration `yaml:"compact_grace"`
}

// DefaultConfig returns the Lite-profile defaults.
func DefaultConfig() Config {
	return Config{
		Guard: GuardConfig{MaxFraction: DefaultMaxFraction, MaxDeleted: DefaultMaxDeleted, MinPrevious: DefaultMinPrevious},
		Parser: ParserConfig{
			StartupTimeout: DefaultStartupTimeout,
			WallTimeout:    parse.DefaultConfig().WallTimeout,
			MaxConcurrent:  parse.DefaultConfig().MaxConcurrent,
			ProbeTimeout:   DefaultProbeTimeout,
		},
		EnqueueBatch:    DefaultEnqueueBatch,
		PressureRecheck: DefaultPressureRecheck,
		ConfirmTTL:      DefaultConfirmTTL,
		CompactGrace:    DefaultCompactGrace,
	}
}

// withDefaults replaces zero fields with their defaults.
func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.Guard.MaxFraction == 0 {
		c.Guard.MaxFraction = d.Guard.MaxFraction
	}
	if c.Guard.MaxDeleted == 0 {
		c.Guard.MaxDeleted = d.Guard.MaxDeleted
	}
	if c.Guard.MinPrevious == 0 {
		c.Guard.MinPrevious = d.Guard.MinPrevious
	}
	if c.Parser.StartupTimeout == 0 {
		c.Parser.StartupTimeout = d.Parser.StartupTimeout
	}
	if c.Parser.WallTimeout == 0 {
		c.Parser.WallTimeout = d.Parser.WallTimeout
	}
	if c.Parser.MaxConcurrent == 0 {
		c.Parser.MaxConcurrent = d.Parser.MaxConcurrent
	}
	if c.Parser.ProbeTimeout == 0 {
		c.Parser.ProbeTimeout = d.Parser.ProbeTimeout
	}
	if c.EnqueueBatch == 0 {
		c.EnqueueBatch = d.EnqueueBatch
	}
	if c.PressureRecheck == 0 {
		c.PressureRecheck = d.PressureRecheck
	}
	if c.ConfirmTTL == 0 {
		c.ConfirmTTL = d.ConfirmTTL
	}
	if c.CompactGrace == 0 {
		c.CompactGrace = d.CompactGrace
	}
	return c
}

// Validate implements the config section validation hook. Zero means the
// default; a negative or absurd value is an error.
func (c Config) Validate() error {
	d := c.withDefaults()
	switch {
	case c.Guard.MaxFraction < 0 || d.Guard.MaxFraction > 1:
		return fmt.Errorf("mass_deletion.max_fraction %v is outside (0, 1]", c.Guard.MaxFraction)
	case c.Guard.MaxDeleted < 0:
		return fmt.Errorf("mass_deletion.max_deleted %d is negative", c.Guard.MaxDeleted)
	case c.Guard.MinPrevious < 0:
		return fmt.Errorf("mass_deletion.min_previous %d is negative", c.Guard.MinPrevious)
	case c.Parser.StartupTimeout < 0 || (c.Parser.StartupTimeout != 0 && c.Parser.StartupTimeout < time.Second):
		return fmt.Errorf("parser.startup_timeout %s is under one second", c.Parser.StartupTimeout)
	case c.Parser.WallTimeout < 0 || (c.Parser.WallTimeout != 0 && c.Parser.WallTimeout < time.Second):
		return fmt.Errorf("parser.wall_timeout %s is under one second", c.Parser.WallTimeout)
	case c.Parser.MaxConcurrent < 0 || c.Parser.MaxConcurrent > 64:
		return fmt.Errorf("parser.max_concurrent %d is outside 1 to 64", c.Parser.MaxConcurrent)
	case c.Parser.ProbeTimeout < 0:
		return fmt.Errorf("parser.probe_timeout %s is negative", c.Parser.ProbeTimeout)
	case c.EnqueueBatch < 0 || c.EnqueueBatch > 100_000:
		return fmt.Errorf("enqueue_batch %d is outside 1 to 100000", c.EnqueueBatch)
	case c.PressureRecheck < 0 || c.ConfirmTTL < 0 || c.CompactGrace < 0:
		return fmt.Errorf("a duration in the %s section is negative", SectionName)
	}
	return nil
}

// ParseConfig returns the parser's configuration: the parser defaults with the
// operator's worker path and limits laid over them. The result is not
// validated here; parse.NewRunner does that, so a bad worker path fails at
// service start.
func (c Config) ParseConfig() parse.Config {
	d := c.withDefaults()
	pc := parse.DefaultConfig()
	pc.WorkerPath = d.Parser.WorkerPath
	pc.StartupTimeout = d.Parser.StartupTimeout
	pc.WallTimeout = d.Parser.WallTimeout
	pc.MaxConcurrent = d.Parser.MaxConcurrent
	return pc
}

// section is the registered "rag_ingest" config section. Owners read it through
// this pointer (see config.Register).
var section = DefaultConfig()

func init() { config.Register(SectionName, &section) }

// FromConfig returns the process configuration as loaded.
func FromConfig() Config { return section }
