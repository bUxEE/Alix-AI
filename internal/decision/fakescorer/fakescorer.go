// Package fakescorer is the deterministic scripted scorer used by the decision-layer
// tests. It implements decision.DecisionModelProvider structurally (it imports only
// the generated contracts, the clock and contracts, so the decision package's own
// tests can use it) and injects failures and latency on demand.
//
//	s := fakescorer.New(fakescorer.DefaultManifest(), clk).Script(
//		fakescorer.Step{Picks: []fakescorer.Pick{{ID: "c2", Score: 0.9, Confidence: 0.97}}},
//		fakescorer.Step{Fail: fakescorer.FailTimeout},
//	)
//
// Steps are consumed in order and the last one repeats. With no script it selects the
// first candidate with confidence 0.95. Like a real adapter it never returns partial
// output as a success, and it honours its context.
package fakescorer

import (
	"context"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// Failure is an injectable failure.
type Failure string

// The injectable failures.
const (
	FailNone Failure = ""
	// FailTimeout fails with timeout (wrapping context.DeadlineExceeded).
	FailTimeout Failure = "timeout"
	// FailUnavailable fails with unavailable (the sidecar is down).
	FailUnavailable Failure = "unavailable"
	// FailMalformed fails with schema_invalid (undecodable output).
	FailMalformed Failure = "malformed"
	// FailResourceExhausted fails with resource_exhausted (the model cannot load).
	FailResourceExhausted Failure = "resource_exhausted"
)

// Pick is one scripted selection.
type Pick struct {
	ID         string
	Score      float64
	Confidence float64
}

// Step scripts one call.
type Step struct {
	// Picks are the selections; nil selects the first candidate (confidence 0.95).
	Picks []Pick
	// Latency is the simulated duration. With a fake clock it advances that clock
	// (no sleeping); without one the call waits for it or for ctx, whichever ends first.
	Latency time.Duration
	// Fail makes the call fail instead of returning a result.
	Fail Failure
	// Hang blocks until ctx ends, then fails with timeout.
	Hang bool
	// Mutate edits the finished result: use it to corrupt echoes, ood status, model
	// identity or dispositions.
	Mutate func(*gen.DecisionScoreResult)
}

// Scorer is the scripted provider.
type Scorer struct {
	manifest gen.ScorerManifest
	clk      *clock.Fake

	mu    sync.Mutex
	steps []Step
	calls int
	last  gen.DecisionRequest
}

// DefaultManifest is a valid manifest for the fake.
func DefaultManifest() gen.ScorerManifest {
	return gen.ScorerManifest{
		ProviderID:    "fake-scorer",
		ModelID:       "fake-model",
		ModelRevision: "rev1",
		Encoder:       gen.ScorerManifestEncoder{EncoderID: "fake-encoder", Revision: "enc1"},
		Files:         []gen.ScorerManifestFileHash{},
		Licence:       gen.ScorerManifestLicence{SpdxID: "Apache-2.0", Redistribution: gen.ScorerManifestLicenceRedistributionAllowed},
	}
}

// New returns a scorer for manifest. clk may be nil.
func New(manifest gen.ScorerManifest, clk *clock.Fake) *Scorer {
	return &Scorer{manifest: manifest, clk: clk}
}

// Script replaces the script and returns s.
func (s *Scorer) Script(steps ...Step) *Scorer {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append([]Step(nil), steps...)
	return s
}

// Manifest returns the manifest.
func (s *Scorer) Manifest() gen.ScorerManifest { return s.manifest }

// Calls returns how many times ScoreDecision was entered.
func (s *Scorer) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// LastRequest returns the last request the scorer received.
func (s *Scorer) LastRequest() gen.DecisionRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *Scorer) next() Step {
	if len(s.steps) == 0 {
		return Step{}
	}
	st := s.steps[0]
	if len(s.steps) > 1 {
		s.steps = s.steps[1:]
	}
	return st
}

// ScoreDecision implements decision.DecisionModelProvider.
func (s *Scorer) ScoreDecision(ctx context.Context, scope contracts.Scope, req gen.DecisionRequest) (gen.DecisionScoreResult, error) {
	s.mu.Lock()
	s.calls++
	s.last = req
	st := s.next()
	s.mu.Unlock()

	if err := scope.Validate(); err != nil {
		return gen.DecisionScoreResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return gen.DecisionScoreResult{}, contracts.Wrap(contracts.ErrTimeout, err, "fakescorer: context ended")
	}
	if st.Hang {
		<-ctx.Done()
		return gen.DecisionScoreResult{}, contracts.Wrap(contracts.ErrTimeout, ctx.Err(), "fakescorer: hung until the context ended")
	}
	if st.Latency > 0 {
		if s.clk != nil {
			s.clk.Advance(st.Latency)
		} else {
			t := time.NewTimer(st.Latency)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return gen.DecisionScoreResult{}, contracts.Wrap(contracts.ErrTimeout, ctx.Err(), "fakescorer: context ended")
			}
		}
	}
	switch st.Fail {
	case FailTimeout:
		return gen.DecisionScoreResult{}, contracts.Wrap(contracts.ErrTimeout, context.DeadlineExceeded, "fakescorer: injected timeout")
	case FailUnavailable:
		return gen.DecisionScoreResult{}, contracts.New(contracts.ErrUnavailable, "fakescorer: injected unavailable")
	case FailMalformed:
		return gen.DecisionScoreResult{}, contracts.New(contracts.ErrSchemaInvalid, "fakescorer: injected malformed output")
	case FailResourceExhausted:
		return gen.DecisionScoreResult{}, contracts.New(contracts.ErrResourceExhausted, "fakescorer: injected resource exhaustion")
	}

	picks := st.Picks
	if picks == nil && len(req.CandidateSet.Candidates) > 0 {
		picks = []Pick{{ID: string(req.CandidateSet.Candidates[0].CandidateID), Score: 0.9, Confidence: 0.95}}
	}
	sel := make([]gen.DecisionScoreResultSelection, len(picks))
	for i, p := range picks {
		sel[i] = gen.DecisionScoreResultSelection{
			CandidateID: common.Identifier(p.ID), Score: gen.UnitScore(p.Score), CalibratedConfidence: gen.UnitScore(p.Confidence),
		}
	}
	model, rev := s.manifest.ModelID, s.manifest.ModelRevision
	res := gen.DecisionScoreResult{
		Disposition:     gen.DecisionScoreResultDispositionAccepted,
		ProviderID:      s.manifest.ProviderID,
		ModelID:         &model,
		ModelRevision:   &rev,
		ContractVersion: req.ContractVersion,
		DecisionType:    req.DecisionType,
		SchemaVersion:   req.SchemaVersion,
		TenantID:        req.TenantID,
		ApplicationID:   req.ApplicationID,
		Selections:      sel,
		OodStatus:       gen.DecisionScoreResultOodStatusInDistribution,
		LatencyMS:       common.DurationMS(st.Latency.Milliseconds()),
		CacheStatus:     gen.DecisionScoreResultCacheStatusBypassed,
		TraceID:         "trc_fake",
		AuditRef:        actions.AuditRef(""),
	}
	if st.Mutate != nil {
		st.Mutate(&res)
	}
	return res, nil
}
