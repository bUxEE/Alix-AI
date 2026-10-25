package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"github.com/buxee/alix-ai/adapters/decision/clm"
	"github.com/buxee/alix-ai/adapters/decision/localai"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	core "github.com/buxee/alix-ai/internal/decision"
	"github.com/buxee/alix-ai/internal/decision/fakescorer"
	ddec "github.com/buxee/alix-ai/internal/dialogue/decision"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// contractItem is the conversation-contract version of an item-selection decision. The
// routing and act versions are the dialogue layer's own.
const contractItem = "dialogue-item-v1"

func contractFor(decisionType string) string {
	switch decisionType {
	case TypeAct:
		return ddec.ContractAct
	case TypeItem:
		return contractItem
	}
	return ddec.ContractRouting
}

func genType(decisionType string) gen.DecisionType {
	switch decisionType {
	case TypeAct:
		return gen.DecisionTypeDialogueAct
	case TypeItem:
		return gen.DecisionTypeItemSelection
	}
	return gen.DecisionTypeCapabilityRouting
}

// fixtureResolver is the deterministic side of the harness: it answers for the case being
// run the way the real adapter would. A candidate marked unauthorised in the fixture is
// refused, both when a scorer is shown the set (Exposed) and when a selected id is
// re-resolved (Resolve). Fixtures are test data; nothing here is runtime policy.
type fixtureResolver struct {
	mu  sync.Mutex
	cur *Case
}

func (r *fixtureResolver) set(c *Case) {
	r.mu.Lock()
	r.cur = c
	r.mu.Unlock()
}

func (r *fixtureResolver) refused(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		return true // no case: nothing is authorised
	}
	for _, cd := range r.cur.Candidates {
		if cd.ID == id {
			return cd.Unauthorised
		}
	}
	return true // not a candidate of the case
}

func (r *fixtureResolver) Exposed(_ context.Context, _ contracts.Scope, c gen.CandidateSetCandidate) error {
	if r.refused(string(c.CandidateID)) {
		return contracts.New(contracts.ErrPermissionDenied, "decbench: candidate is not authorised")
	}
	return nil
}

func (r *fixtureResolver) Resolve(_ context.Context, _ contracts.Scope, id string) (core.Resolved, error) {
	if r.refused(id) {
		return core.Resolved{}, contracts.New(contracts.ErrPermissionDenied, "decbench: candidate is not authorised")
	}
	return core.Resolved{ID: id, Kind: "candidate", Version: "v1"}, nil
}

// countingAppender is an in-memory audit.Appender that keeps only what the harness needs:
// how many times a scorer was invoked. It is not an audit log and stores nothing.
type countingAppender struct {
	mu      sync.Mutex
	invoked int
	events  int
}

func (a *countingAppender) AppendAt(_ context.Context, loc audit.Locator, ev audit.NewEvent) (audit.Event, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if loc.Level() == 0 {
		return audit.Event{}, contracts.New(contracts.ErrScopeMissing, "decbench: no locator")
	}
	a.events++
	if _, ok := ev.Data.(core.InvokedAudit); ok {
		a.invoked++
	}
	return audit.Event{ID: fmt.Sprintf("aud_%026d", a.events), Type: ev.Type}, nil
}

func (a *countingAppender) AppendTx(context.Context, *store.Tx, audit.Locator, audit.NewEvent) (audit.Event, error) {
	return audit.Event{}, fmt.Errorf("decbench: transactional audit is not used")
}

func (a *countingAppender) invokedCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.invoked
}

// scorerRig is a scorer plus what the harness needs to drive and describe it.
type scorerRig struct {
	provider core.DecisionModelProvider
	fake     *fakescorer.Scorer // nil for a real scorer
	flavour  string
	info     ScorerInfo
}

func fnv32(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}

// scriptedScorerLatency is the fake scorer's simulated latency. It is a stand-in that
// makes a synthetic run exercise the latency plumbing; it says nothing about a model.
func scriptedScorerLatency(caseID, flavour string) time.Duration {
	h := time.Duration(fnv32(caseID))
	if flavour == flavourCLM {
		return (35 + h%20) * time.Millisecond
	}
	return (6 + h%5) * time.Millisecond
}

// scriptedControllerMS is the scripted controller's simulated per-call latency.
func scriptedControllerMS(caseID string) float64 {
	return float64(400 + fnv32(caseID)%200)
}

func newFakeRig(flavour string, clk *clock.Fake) *scorerRig {
	m := fakescorer.DefaultManifest()
	m.ModelID = "fake-sub1b-scorer"
	if flavour == flavourCLM {
		m.ModelID = "fake-clm-8b-scorer"
	}
	f := fakescorer.New(m, clk)
	return &scorerRig{
		provider: f, fake: f, flavour: flavour,
		info: ScorerInfo{Kind: ScorerFake, ProviderID: string(m.ProviderID), ModelID: string(m.ModelID),
			ModelRevision: string(m.ModelRevision), ArtifactHashSource: HashNone},
	}
}

// Confidence a fake scorer reports for a correct pick, by category. Scripted.
var fakeConfidence = map[string]float64{
	CatExact: 0.99, CatParaphrase: 0.94, CatConfirmation: 0.97, CatCancellation: 0.96,
	CatCorrection: 0.93, CatAmbiguous: 0.92, CatRAG: 0.95, CatDocument: 0.95,
}

// fakeScript sets the fake scorer's next answer for c.
func fakeScript(f *fakescorer.Scorer, c Case, flavour string) {
	st := fakescorer.Step{Latency: scriptedScorerLatency(c.ID, flavour)}
	switch c.Failure {
	case FailTimeout:
		st.Fail = fakescorer.FailTimeout
	case FailMalformed:
		st.Fail = fakescorer.FailMalformed
	case FailUnavailable:
		st.Fail = fakescorer.FailUnavailable
	}
	switch {
	case st.Fail != fakescorer.FailNone:
	case c.Script != nil:
		st.Picks = []fakescorer.Pick{{ID: c.Script.Pick, Score: c.Script.Confidence, Confidence: c.Script.Confidence}}
	case c.Expect.Outcome == OutSelect:
		conf, ok := fakeConfidence[c.Category]
		if !ok {
			conf = 0.9
		}
		st.Picks = []fakescorer.Pick{{ID: c.Expect.CandidateID, Score: conf, Confidence: conf}}
	case c.Category == CatOOD:
		st.Mutate = func(r *gen.DecisionScoreResult) { r.OodStatus = gen.DecisionScoreResultOodStatusOutOfDistribution }
	default:
		// Nothing to select: the fake leans to the first candidate but without conviction,
		// below the confidence floor, as a calibrated scorer does when unsure.
		st.Picks = []fakescorer.Pick{{ID: c.Candidates[0].ID, Score: 0.4, Confidence: 0.4}}
	}
	f.Script(st)
}

// RealScorerOptions configures a real scorer adapter.
type RealScorerOptions struct {
	Endpoint string
	Model    string
	Revision string
	Timeout  time.Duration
}

func newRealRig(kind, flavour string, o RealScorerOptions) (*scorerRig, error) {
	if o.Model == "" {
		return nil, fmt.Errorf("--scorer %s needs --model", kind)
	}
	switch kind {
	case ScorerLocalAI:
		p, err := localai.New(localai.Config{Endpoint: o.Endpoint, Model: o.Model, ModelRevision: o.Revision, RequestTimeout: o.Timeout})
		if err != nil {
			return nil, err
		}
		return realRig(p, flavour, kind), nil
	case ScorerCLM:
		p, err := clm.New(clm.Config{Endpoint: o.Endpoint, Model: o.Model, ModelRevision: o.Revision, RequestTimeout: o.Timeout})
		if err != nil {
			return nil, err
		}
		return realRig(p, flavour, kind), nil
	}
	return nil, fmt.Errorf("unknown scorer %q", kind)
}

func realRig(p core.DecisionModelProvider, flavour, kind string) *scorerRig {
	m := p.Manifest()
	return &scorerRig{provider: p, flavour: flavour, info: ScorerInfo{
		Kind: kind, ProviderID: string(m.ProviderID), ModelID: string(m.ModelID),
		ModelRevision: string(m.ModelRevision), ArtifactHashSource: HashNone,
	}}
}

// Artifact hash sources.
const (
	HashComputed = "computed"
	HashNone     = "none"
)

// buildEngine returns the engine for a scorer rig. The calibration entry is DECLARED here
// (ECE 0 against a bound of 0.1) so that the engine's own calibration gate lets the
// harness observe the scorer; the measured Brier score and ECE are the output of the run,
// and an operator's real entry is the one measured on held-out data (see the README).
func buildEngine(clk clock.Clock, rig *scorerRig, res *fixtureResolver, app *countingAppender, timeout time.Duration, minConf float64) (*core.Engine, error) {
	cfg := core.DefaultConfig()
	cfg.Enabled = true
	cfg.Timeout = timeout
	cfg.MinConfidence = minConf
	cfg.SupportedTypes = []gen.DecisionType{gen.DecisionTypeCapabilityRouting, gen.DecisionTypeDialogueAct, gen.DecisionTypeItemSelection}
	m := rig.provider.Manifest()
	for _, cv := range []string{ddec.ContractRouting, ddec.ContractAct, contractItem} {
		cfg.Calibration = append(cfg.Calibration, core.Calibration{
			ModelID: string(m.ModelID), ModelRevision: string(m.ModelRevision), ContractVersion: cv, ECE: 0, MaxECE: 0.1,
		})
	}
	return core.New(core.Deps{Clock: clk, Provider: rig.provider, Resolver: res, Audit: app, Config: cfg})
}

const tsLayout = "2006-01-02T15:04:05.000Z"

func tsOf(t time.Time) common.Timestamp { return common.Timestamp(t.UTC().Format(tsLayout)) }
