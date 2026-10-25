package store

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
)

// Defaults and bounds. They are the Lite-profile values; see
// docs/efficiency/M0-05.md for what they cost.
const (
	// DefaultFileName is the database file inside data_dir.
	DefaultFileName = "alixai.db"

	DefaultCacheKiB       = 8000 // writer page cache (8 MB)
	DefaultReaderCacheKiB = 2000 // per reader connection (2 MB)
	DefaultReaderConns    = 4
	DefaultBusyTimeoutMs  = 5000

	maxCacheKiB       = 1 << 20 // 1 GiB, a sanity bound rather than a recommendation
	maxReaderCacheKiB = 1 << 18
	maxReaderConns    = 32
	maxMmapMiB        = 4096
	maxBusyTimeoutMs  = 600_000

	// journalSizeLimit caps the WAL file left behind after a checkpoint.
	journalSizeLimit = 64 << 20

	// defaultBusyAttempts and defaultBusyBackoff bound the Tx retry loop: five
	// attempts with 20, 40, 80, 160 ms (jittered) between them.
	defaultBusyAttempts = 5
	defaultBusyBackoff  = 20 * time.Millisecond
	maxBusyBackoff      = time.Second

	// readerIdleTime releases idle reader connections (and their page caches)
	// when the service is quiet.
	readerIdleTime = 5 * time.Minute
)

// Config is the "store" configuration section: how much memory the database
// may use and how many connections it opens. A zero field means "default"
// (MmapMiB zero means mmap off, which is also the default).
type Config struct {
	// CacheKiB is the writer connection's page cache, in KiB (SQLite
	// cache_size = -CacheKiB).
	CacheKiB int `yaml:"cache_kib"`
	// ReaderCacheKiB is each reader connection's page cache, in KiB.
	ReaderCacheKiB int `yaml:"reader_cache_kib"`
	// ReaderConns bounds the read-only connection pool.
	ReaderConns int `yaml:"reader_conns"`
	// MmapMiB is SQLite's memory-mapped I/O window per connection. Off by
	// default: mapped pages count against resident memory on small hosts.
	MmapMiB int `yaml:"mmap_mib"`
	// BusyTimeoutMs is how long SQLite itself waits on a lock before a call
	// returns SQLITE_BUSY (which Tx then retries).
	BusyTimeoutMs int `yaml:"busy_timeout_ms"`
}

// DefaultConfig returns the Lite-profile defaults.
func DefaultConfig() Config {
	return Config{
		CacheKiB:       DefaultCacheKiB,
		ReaderCacheKiB: DefaultReaderCacheKiB,
		ReaderConns:    DefaultReaderConns,
		MmapMiB:        0,
		BusyTimeoutMs:  DefaultBusyTimeoutMs,
	}
}

// Validate implements the config section validation hook.
func (c Config) Validate() error {
	check := func(name string, v, max int) error {
		if v < 0 || v > max {
			return fmt.Errorf("%s %d out of range (0-%d)", name, v, max)
		}
		return nil
	}
	for _, e := range []error{
		check("cache_kib", c.CacheKiB, maxCacheKiB),
		check("reader_cache_kib", c.ReaderCacheKiB, maxReaderCacheKiB),
		check("reader_conns", c.ReaderConns, maxReaderConns),
		check("mmap_mib", c.MmapMiB, maxMmapMiB),
		check("busy_timeout_ms", c.BusyTimeoutMs, maxBusyTimeoutMs),
	} {
		if e != nil {
			return e
		}
	}
	return nil
}

// withDefaults replaces zero fields with the defaults.
func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.CacheKiB == 0 {
		c.CacheKiB = d.CacheKiB
	}
	if c.ReaderCacheKiB == 0 {
		c.ReaderCacheKiB = d.ReaderCacheKiB
	}
	if c.ReaderConns == 0 {
		c.ReaderConns = d.ReaderConns
	}
	if c.BusyTimeoutMs == 0 {
		c.BusyTimeoutMs = d.BusyTimeoutMs
	}
	return c
}

// PageCacheBudgetKiB is the most page cache the database can hold: the writer
// cache plus one reader cache per pooled reader connection. It excludes SQLite's
// own working memory (statements, sorter, schema) and the Go runtime.
func (c Config) PageCacheBudgetKiB() int {
	c = c.withDefaults()
	return c.CacheKiB + c.ReaderConns*c.ReaderCacheKiB
}

// section is the registered "store" config section. Owners read it through
// this pointer (see config.Register).
var section = DefaultConfig()

func init() { config.Register("store", &section) }

// Options is everything Open needs.
type Options struct {
	// Path is the database file. It is created (mode 0600, parent directory
	// 0700) if missing. In-memory and URI names are rejected: the writer and
	// the reader pool must share one file.
	Path string
	// Config sizes the caches and pools; zero fields take the defaults.
	Config Config
	// Migrations is the registry applied by Open. Nil means Default, the
	// registry RegisterMigrations adds to.
	Migrations *Registry
	// Clock stamps schema_migrations.applied_at. Nil means clock.Real.
	Clock clock.Clock
	// Logger receives migration progress. Nil discards.
	Logger *slog.Logger
	// BusyAttempts and BusyBackoff bound the Tx retry loop; zero takes the
	// defaults (5 attempts, 20 ms doubling, jittered).
	BusyAttempts int
	BusyBackoff  time.Duration
}

// FromConfig returns Options for the process configuration: the database at
// <data_dir>/alixai.db with the loaded "store" section, the Default
// migration registry and the real clock. The caller adds a Logger.
func FromConfig() Options {
	return Options{
		Path:   filepath.Join(config.Core().DataDir.String(), DefaultFileName),
		Config: section,
	}
}

func (o Options) validate() error {
	p := o.Path
	switch {
	case strings.TrimSpace(p) == "":
		return fmt.Errorf("path is empty")
	case strings.ContainsRune(p, 0):
		return fmt.Errorf("path contains NUL")
	case strings.ContainsRune(p, '?'):
		return fmt.Errorf("path contains '?', which the driver would read as a DSN query")
	case strings.HasPrefix(p, "file:") || strings.Contains(p, ":memory:") || strings.HasPrefix(p, ":"):
		return fmt.Errorf("path %q is a URI or in-memory name; the store needs a real file shared by all connections", p)
	case o.BusyAttempts < 0 || o.BusyBackoff < 0:
		return fmt.Errorf("busy retry settings must not be negative")
	}
	return o.Config.Validate()
}
