package dialogue

import "github.com/buxee/alix-ai/internal/config"

// SectionName is the configuration section this package owns.
//
//	dialogue:
//	  stream_verified: false   # ADR 0046; off keeps 1.3 whole-reply behaviour
//	  analyst:                 # ADR 0047; every ability defaults false
//	    enabled: false
//	    abilities:
//	      loop: false
//	    max_steps: 12
//
// with ALIXAI_DIALOGUE_STREAM_VERIFIED / ALIXAI_DIALOGUE_ANALYST_* as overrides.
const SectionName = "dialogue"

// FileConfig is the YAML section. StreamVerified and Analyst default off (safe additive).
type FileConfig struct {
	StreamVerified bool          `yaml:"stream_verified"`
	Analyst        AnalystConfig `yaml:"analyst"`
}

// AnalystConfig is dialogue.analyst.* (schemas/analyst/config.schema.json).
type AnalystConfig struct {
	Enabled              bool                `yaml:"enabled"`
	Abilities            AnalystAbilityFlags `yaml:"abilities"`
	MaxSteps             int                 `yaml:"max_steps"`
	MaxReads             int                 `yaml:"max_reads"`
	MaxRecordsPerDataset int                 `yaml:"max_records_per_dataset"`
	MaxTotalRecords      int                 `yaml:"max_total_records"`
	MaxTurnTokens        int                 `yaml:"max_turn_tokens"`
	MaxTurnWallMS        int                 `yaml:"max_turn_wall_ms"`
	MaxTurnCost          *int64              `yaml:"max_turn_cost"`
	Web                  AnalystWebConfig    `yaml:"web"`
}

// AnalystAbilityFlags are per-ability toggles at the deployment level.
type AnalystAbilityFlags struct {
	Loop         bool `yaml:"loop"`
	Compute      bool `yaml:"compute"`
	Forecasts    bool `yaml:"forecasts"`
	Presentation bool `yaml:"presentation"`
	Drafts       bool `yaml:"drafts"`
	Knowledge    bool `yaml:"knowledge"`
	WebSearch    bool `yaml:"web_search"`
	WebFetch     bool `yaml:"web_fetch"`
}

// AnalystWebConfig is dialogue.analyst.web.*.
type AnalystWebConfig struct {
	Provider    string   `yaml:"provider"`
	CredRef     string   `yaml:"cred_ref"`
	Origin      string   `yaml:"origin"`
	MaxSearches int      `yaml:"max_searches"`
	MaxFetches  int      `yaml:"max_fetches"`
	AllowHosts  []string `yaml:"allow_hosts"`
	DenyHosts   []string `yaml:"deny_hosts"`
}

var registered = FileConfig{}

func init() { config.Register(SectionName, &registered) }

// Loaded returns the registered dialogue section after a successful config.Load
// (or the defaults). Composition roots copy StreamVerified / Analyst into Config.
func Loaded() FileConfig { return registered }

// AnalystCapsFromFile maps FileConfig.Analyst to loop caps (defaults applied by the runner).
func (c AnalystConfig) Caps() (maxSteps, maxReads, maxRecordsPerDataset, maxTotalRecords, maxTurnTokens, maxTurnWallMS, maxSearches, maxFetches int, maxTurnCost *int64) {
	return c.MaxSteps, c.MaxReads, c.MaxRecordsPerDataset, c.MaxTotalRecords, c.MaxTurnTokens, c.MaxTurnWallMS, c.Web.MaxSearches, c.Web.MaxFetches, c.MaxTurnCost
}
