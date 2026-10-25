package vector

import (
	"fmt"

	"github.com/buxee/alix-ai/internal/config"
)

// Defaults, the Lite-profile values (docs/efficiency/M2-07.md has what they
// cost and ADR 0011 why).
const (
	// DefaultCacheMiB is the resident-cache budget for the whole process.
	DefaultCacheMiB = 64
	// DefaultMaxVectorsPerWorkspace bounds what one workspace may hold across
	// every generation, building and superseded ones included. The supported
	// ceiling for a workspace's ACTIVE vectors is half of it (100,000): the
	// other half is headroom for the generation being built beside it.
	DefaultMaxVectorsPerWorkspace = 200_000
	// DefaultMaxVectorsPerDeployment bounds the whole database the same way
	// (supported active ceiling 250,000).
	DefaultMaxVectorsPerDeployment = 500_000

	maxCacheMiB = 1 << 16
	maxVectors  = 50_000_000
)

// Config is the "rag_vector" configuration section. A zero field takes its
// default, except CacheMiB where zero is a real value (no cache); use
// DefaultConfig as the starting point and Validate before use.
type Config struct {
	// CacheMiB is the byte budget of the resident cache that keeps the int8
	// codes of hot generations in memory. 0 turns it off: every search then
	// streams its generation from SQLite (four to six times slower, no extra
	// RAM). Size it at about 456 bytes per hot vector (ADR 0011): 64 MiB holds
	// one 100,000-vector workspace, 128 MiB two. Generations beyond what the
	// budget holds are streamed, so a too-small value costs speed on the
	// overflow, never more than having no cache. The Lite default is 64. The
	// experimental-4g profile starts at 0, because every resident byte counts
	// there; the supported ceiling is then 25,000 vectors per workspace instead
	// of 100,000, and spending RAM here is the first way to raise it (about 37
	// MiB per 100,000 hot vectors).
	CacheMiB int `yaml:"cache_mib"`
	// MaxVectorsPerWorkspace is the most vectors one workspace may hold; an
	// upsert beyond it fails with resource_exhausted, never silently drops.
	MaxVectorsPerWorkspace int `yaml:"max_vectors_per_workspace"`
	// MaxVectorsPerDeployment is the same bound over the whole database.
	MaxVectorsPerDeployment int `yaml:"max_vectors_per_deployment"`
}

// DefaultConfig returns the Lite-profile defaults.
func DefaultConfig() Config {
	return Config{
		CacheMiB:                DefaultCacheMiB,
		MaxVectorsPerWorkspace:  DefaultMaxVectorsPerWorkspace,
		MaxVectorsPerDeployment: DefaultMaxVectorsPerDeployment,
	}
}

// Validate implements the config section validation hook.
func (c Config) Validate() error {
	switch {
	case c.CacheMiB < 0 || c.CacheMiB > maxCacheMiB:
		return fmt.Errorf("cache_mib %d out of range (0-%d)", c.CacheMiB, maxCacheMiB)
	case c.MaxVectorsPerWorkspace < 0 || c.MaxVectorsPerWorkspace > maxVectors:
		return fmt.Errorf("max_vectors_per_workspace %d out of range (0-%d)", c.MaxVectorsPerWorkspace, maxVectors)
	case c.MaxVectorsPerDeployment < 0 || c.MaxVectorsPerDeployment > maxVectors:
		return fmt.Errorf("max_vectors_per_deployment %d out of range (0-%d)", c.MaxVectorsPerDeployment, maxVectors)
	}
	return nil
}

// withDefaults replaces the zero ceilings with their defaults. CacheMiB zero
// stays zero: the cache is off.
func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.MaxVectorsPerWorkspace == 0 {
		c.MaxVectorsPerWorkspace = d.MaxVectorsPerWorkspace
	}
	if c.MaxVectorsPerDeployment == 0 {
		c.MaxVectorsPerDeployment = d.MaxVectorsPerDeployment
	}
	return c
}

// section is the registered "rag_vector" config section. Owners read it through
// this pointer (see config.Register).
var section = DefaultConfig()

func init() { config.Register("rag_vector", &section) }

// FromConfig returns the process configuration as loaded.
func FromConfig() Config { return section }
