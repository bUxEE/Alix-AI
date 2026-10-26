package memory

import (
	"fmt"
	"time"

	"github.com/buxee/alix-ai/internal/config"
)

// SectionName is the configuration section this package owns.
//
//	memory:
//	  enabled: false                 # off by default (ADR 0042 silent → safe)
//	  max_items_per_scope: 200
//	  max_retrieve: 8
//	  max_proposals_per_turn: 1
//	  max_proposals_per_day: 20
//	  proposal_ttl: 168h             # seven days; confirm after that is memory_proposal_expired
//
// with ALIXAI_MEMORY_ENABLED and so on as overrides.
const SectionName = "memory"

// Config bounds the service. Zero fields take defaults; Enabled defaults false.
type Config struct {
	Enabled             bool          `yaml:"enabled"`
	MaxItemsPerScope    int           `yaml:"max_items_per_scope"`
	MaxRetrieve         int           `yaml:"max_retrieve"`
	MaxProposalsPerTurn int           `yaml:"max_proposals_per_turn"`
	MaxProposalsPerDay  int           `yaml:"max_proposals_per_day"`
	ProposalTTL         time.Duration `yaml:"proposal_ttl"`
}

// Defaults.
const (
	DefaultMaxItemsPerScope    = 200
	DefaultMaxRetrieve         = 8
	DefaultMaxProposalsPerTurn = 1
	DefaultMaxProposalsPerDay  = 20
	DefaultProposalTTL         = 7 * 24 * time.Hour
	MaxTextLen                 = 4000
	MaxListLimit               = 200
	DefaultListLimit           = 50
	MaxStructuredKeys          = 32
)

var registered = DefaultConfig()

func init() { config.Register(SectionName, &registered) }

// DefaultConfig returns the safe defaults: memory off.
func DefaultConfig() Config {
	return Config{
		Enabled:             false,
		MaxItemsPerScope:    DefaultMaxItemsPerScope,
		MaxRetrieve:         DefaultMaxRetrieve,
		MaxProposalsPerTurn: DefaultMaxProposalsPerTurn,
		MaxProposalsPerDay:  DefaultMaxProposalsPerDay,
		ProposalTTL:         DefaultProposalTTL,
	}
}

// Live returns the registered section (after a successful config.Load).
func Live() Config { return registered }

// Validate implements the configuration validation hook.
func (c Config) Validate() error {
	d := c.withDefaults()
	check := func(name string, n, min, max int) error {
		if n < min || n > max {
			return fmt.Errorf("memory.%s must be between %d and %d", name, min, max)
		}
		return nil
	}
	if err := check("max_items_per_scope", d.MaxItemsPerScope, 1, 10_000); err != nil {
		return err
	}
	if err := check("max_retrieve", d.MaxRetrieve, 1, 64); err != nil {
		return err
	}
	if err := check("max_proposals_per_turn", d.MaxProposalsPerTurn, 1, 8); err != nil {
		return err
	}
	if err := check("max_proposals_per_day", d.MaxProposalsPerDay, 1, 1000); err != nil {
		return err
	}
	if d.ProposalTTL < time.Minute || d.ProposalTTL > 365*24*time.Hour {
		return fmt.Errorf("memory.proposal_ttl must be between 1m and 8760h")
	}
	return nil
}

// WithDefaults fills zero fields. withDefaults is the internal alias.
func (c Config) WithDefaults() Config { return c.withDefaults() }

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.MaxItemsPerScope <= 0 {
		c.MaxItemsPerScope = d.MaxItemsPerScope
	}
	if c.MaxRetrieve <= 0 {
		c.MaxRetrieve = d.MaxRetrieve
	}
	if c.MaxProposalsPerTurn <= 0 {
		c.MaxProposalsPerTurn = d.MaxProposalsPerTurn
	}
	if c.MaxProposalsPerDay <= 0 {
		c.MaxProposalsPerDay = d.MaxProposalsPerDay
	}
	if c.ProposalTTL <= 0 {
		c.ProposalTTL = d.ProposalTTL
	}
	return c
}
