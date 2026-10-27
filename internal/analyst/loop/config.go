package loop

// Caps are the configurable analyst loop limits (dialogue.analyst.*). Zero fields
// take the schema defaults.
type Caps struct {
	MaxSteps             int
	MaxReads             int
	MaxRecordsPerDataset int
	MaxTotalRecords      int
	MaxTurnTokens        int
	MaxTurnWallMS        int
	MaxTurnCost          *int64 // micro-units; nil = no extra ceiling
	MaxSearches          int
	MaxFetches           int
	MaxDatasetPromptToks int // soft budget for summarising datasets into the prompt
}

// DefaultCaps matches schemas/analyst/config.schema.json defaults.
func DefaultCaps() Caps {
	return Caps{
		MaxSteps:             12,
		MaxReads:             8,
		MaxRecordsPerDataset: 500,
		MaxTotalRecords:      2000,
		MaxTurnTokens:        32000,
		MaxTurnWallMS:        60000,
		MaxSearches:          3,
		MaxFetches:           3,
		MaxDatasetPromptToks: 8000,
	}
}

// Normalized fills zeros with defaults.
func (c Caps) Normalized() Caps {
	d := DefaultCaps()
	set := func(p *int, def int) {
		if *p <= 0 {
			*p = def
		}
	}
	set(&c.MaxSteps, d.MaxSteps)
	set(&c.MaxReads, d.MaxReads)
	set(&c.MaxRecordsPerDataset, d.MaxRecordsPerDataset)
	set(&c.MaxTotalRecords, d.MaxTotalRecords)
	set(&c.MaxTurnTokens, d.MaxTurnTokens)
	set(&c.MaxTurnWallMS, d.MaxTurnWallMS)
	set(&c.MaxSearches, d.MaxSearches)
	set(&c.MaxFetches, d.MaxFetches)
	set(&c.MaxDatasetPromptToks, d.MaxDatasetPromptToks)
	return c
}

// CapKind names which limit was exceeded.
type CapKind string

const (
	CapSteps    CapKind = "steps"
	CapReads    CapKind = "reads"
	CapRecords  CapKind = "records"
	CapTokens   CapKind = "tokens"
	CapWall     CapKind = "wall"
	CapCost     CapKind = "cost"
	CapSearches CapKind = "searches"
	CapFetches  CapKind = "fetches"
)
