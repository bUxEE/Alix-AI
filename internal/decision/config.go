package decision

import (
	"fmt"
	"math"
	"time"

	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// Config is the engine's policy. The zero value is OFF: the layer is opt-in and a
// forgotten field never enables it.
type Config struct {
	// Enabled turns the layer on. When false the provider is never called.
	Enabled bool
	// MaxCandidates caps the candidate set (the candidate-size gate).
	MaxCandidates int
	// MaxLabelLen and MaxTextLen cap one candidate's label and optional text, in bytes.
	MaxLabelLen int
	MaxTextLen  int
	// MaxStateKeys and MaxStateStringLen cap the bounded state.
	MaxStateKeys      int
	MaxStateStringLen int
	// MaxCandidateAge is how old CandidateSet.BuiltAt may be; MaxClockSkew is how far
	// in the future it may be.
	MaxCandidateAge time.Duration
	MaxClockSkew    time.Duration
	// Timeout caps one provider call (the request deadline may shorten it).
	Timeout time.Duration
	// MinConfidence is the calibrated-confidence floor for every selection.
	MinConfidence float64
	// SupportedTypes lists the decision types the provider handles; any other type is
	// an unsupported shape. Empty supports none.
	SupportedTypes []gen.DecisionType
	// PinnedModelID and PinnedRevision, when set, must equal the provider manifest.
	PinnedModelID  string
	PinnedRevision string
	// Calibration holds the measured calibration of each (model, revision, contract
	// version) the operator allows. A result from a combination with no entry, or an
	// entry that fails its own bound, is calibration_failed.
	Calibration []Calibration
	// CacheEntries bounds the cache (0 disables it); CacheTTL is an entry's life.
	CacheEntries int
	CacheTTL     time.Duration
}

// Calibration is the measured calibration of one model revision under one
// conversation-contract version: the expected calibration error from the held-out set
// and the largest the operator accepts.
type Calibration struct {
	ModelID         string
	ModelRevision   string
	ContractVersion string
	ECE             float64
	MaxECE          float64
}

// DefaultConfig returns the defaults with the layer OFF.
func DefaultConfig() Config {
	return Config{
		MaxCandidates:     16,
		MaxLabelLen:       256,
		MaxTextLen:        1024,
		MaxStateKeys:      32,
		MaxStateStringLen: 256,
		MaxCandidateAge:   30 * time.Second,
		MaxClockSkew:      5 * time.Second,
		Timeout:           250 * time.Millisecond,
		MinConfidence:     0.85,
		CacheEntries:      256,
		CacheTTL:          30 * time.Second,
	}
}

func (c Config) validate() error {
	switch {
	case c.MaxCandidates < 1, c.MaxLabelLen < 1, c.MaxTextLen < 0, c.MaxStateKeys < 0, c.MaxStateStringLen < 1:
		return fmt.Errorf("decision: size caps must be positive")
	case c.MaxCandidateAge <= 0, c.MaxClockSkew < 0, c.Timeout <= 0:
		return fmt.Errorf("decision: age and timeout must be positive")
	case math.IsNaN(c.MinConfidence) || c.MinConfidence <= 0 || c.MinConfidence > 1:
		return fmt.Errorf("decision: min confidence must be in (0, 1]")
	case c.CacheEntries < 0, c.CacheEntries > 0 && c.CacheTTL <= 0:
		return fmt.Errorf("decision: cache needs a positive ttl")
	}
	for _, cal := range c.Calibration {
		if cal.ModelID == "" || cal.ModelRevision == "" || cal.ContractVersion == "" ||
			math.IsNaN(cal.ECE) || cal.ECE < 0 || cal.ECE > 1 || math.IsNaN(cal.MaxECE) || cal.MaxECE <= 0 || cal.MaxECE > 1 {
			return fmt.Errorf("decision: malformed calibration entry")
		}
	}
	for _, t := range c.SupportedTypes {
		if !t.Valid() {
			return fmt.Errorf("decision: unknown supported decision type %q", string(t))
		}
	}
	return nil
}

func (c Config) supports(t gen.DecisionType) bool {
	for _, s := range c.SupportedTypes {
		if s == t {
			return true
		}
	}
	return false
}

func (c Config) calibration(model, revision, contract string) (Calibration, bool) {
	for _, cal := range c.Calibration {
		if cal.ModelID == model && cal.ModelRevision == revision && cal.ContractVersion == contract {
			return cal, true
		}
	}
	return Calibration{}, false
}
