package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/buxee/alix-ai/internal/models"
)

// Config is one run: how long, under which profile and decision axis, which
// load, how often to sample and what to judge it by.
type Config struct {
	Duration time.Duration
	Profile  models.Profile
	// Decision is the decision-layer axis: off or on. The harness cannot
	// switch the scorer on a running stack; the axis is a precondition (the
	// stack must be in that state, and the run refuses to start when it is
	// not) and a label on the result, so two runs can be compared.
	Decision string
	// DecisionQualified asserts, for lite, that a qualified sub-1B scorer is
	// configured (resource-budgets R2).
	DecisionQualified bool
	SampleInterval    time.Duration
	// Warmup is the time before steady state. Headroom and role checks look
	// only at samples taken after it.
	Warmup     time.Duration
	Mix        Mix
	MixSHA256  string
	Thresholds Thresholds
	// Synthetic marks a run against the harness's own fakes.
	Synthetic bool
	// Target says what was soaked, for the record.
	Target TargetInfo
}

// TargetInfo describes what the run measured, with no credential in it.
type TargetInfo struct {
	// Driver is sdk (a running stack over its API) or fake.
	Driver  string           `json:"driver"`
	BaseURL string           `json:"base_url"`
	Memory  MemoryDescriptor `json:"memory"`
	// Probe is readyz+ops, metrics, fake or none.
	Probe string `json:"probe"`
}

// Validate checks the configuration. It refuses a run that could not give a
// verdict (a warm-up as long as the run leaves no steady state to measure).
func (c Config) Validate() error {
	if c.Duration <= 0 {
		return errors.New("--duration must be positive")
	}
	if c.SampleInterval <= 0 {
		return errors.New("--sample-interval must be positive")
	}
	if c.Warmup < 0 || c.Warmup >= c.Duration {
		return fmt.Errorf("--warmup %s must be shorter than --duration %s: no sample would be taken in steady state", c.Warmup, c.Duration)
	}
	if _, ok := profileMemMiB(c.Profile); !ok {
		return fmt.Errorf("unknown profile %q", c.Profile)
	}
	if err := checkDecisionAxis(c.Profile, c.Decision, c.DecisionQualified); err != nil {
		return err
	}
	if err := c.Mix.Validate(); err != nil {
		return err
	}
	t := c.Thresholds
	switch {
	case t.MaxSwapMiB < 0 || t.MaxTurnP95MS < 0 || t.MaxQueueDepth < 0:
		return errors.New("thresholds must not be negative")
	case t.MaxErrorRate < 0 || t.MaxErrorRate > 1 || t.MaxSampleErrorRate < 0 || t.MaxSampleErrorRate > 1:
		return errors.New("error rates are fractions between 0 and 1")
	}
	return nil
}

// Rules derives what the verdict needs. probe says whether a stack probe is
// part of the run.
func (c Config) Rules(probe bool) Rules {
	limit, _ := profileMemMiB(c.Profile)
	return Rules{
		Profile:             string(c.Profile),
		LimitMiB:            limit,
		RequiredHeadroomMiB: RequiredHeadroomMiB(limit),
		WarmupMS:            c.Warmup.Milliseconds(),
		Thresholds:          c.Thresholds,
		Decision:            c.Decision,
		InteractiveEnabled:  c.Mix.Interactive.Enabled,
		IngestionEnabled:    c.Mix.Ingestion.Enabled,
		ProbeEnabled:        probe,
	}
}
