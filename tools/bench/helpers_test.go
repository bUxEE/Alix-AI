package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// repoPath returns an absolute path inside the repository.
func repoPath(t testing.TB, rel string) string {
	t.Helper()
	root, err := FindRepoRoot()
	if err != nil {
		t.Fatalf("repository root: %v", err)
	}
	return filepath.Join(root, filepath.FromSlash(rel))
}

const corpusRel = "tests/performance/corpus/routing-v0.jsonl"

var (
	corpusOnce sync.Once
	corpusV0   *Corpus
	corpusErr  error

	schemaOnce       sync.Once
	proposalSchemaV  *jsonschema.Schema
	resultSchemaV    *jsonschema.Schema
	corpusCaseSchema *jsonschema.Schema
	schemaErr        error
)

func realCorpus(t testing.TB) *Corpus {
	t.Helper()
	corpusOnce.Do(func() { corpusV0, corpusErr = LoadCorpus(repoPath(t, corpusRel)) })
	if corpusErr != nil {
		t.Fatalf("corpus: %v", corpusErr)
	}
	return corpusV0
}

func realSchemas(t testing.TB) (proposal, result, corpusCase *jsonschema.Schema) {
	t.Helper()
	schemaOnce.Do(func() {
		dir := repoPath(t, "schemas")
		if proposalSchemaV, schemaErr = LoadProposalSchema(dir); schemaErr != nil {
			return
		}
		if resultSchemaV, schemaErr = LoadResultSchema(dir); schemaErr != nil {
			return
		}
		corpusCaseSchema, schemaErr = LoadCorpusCaseSchema(dir)
	})
	if schemaErr != nil {
		t.Fatalf("schemas: %v", schemaErr)
	}
	return proposalSchemaV, resultSchemaV, corpusCaseSchema
}

// timedGen wraps a provider and moves a fake clock by scripted amounts, so the
// harness's own latency measurement is exact: the first non-empty piece of a
// stream arrives after ttft, the end of the response after total.
type timedGen struct {
	inner   providers.TextGenerator
	clk     *clock.Fake
	latency func(caseID string) (ttft, total time.Duration)
}

func (g *timedGen) Chat(ctx context.Context, scope contracts.Scope, req providers.ChatRequest) (providers.ChatResult, error) {
	res, err := g.inner.Chat(ctx, scope, req)
	if err == nil {
		_, total := g.latency(scope.SessionID)
		g.clk.Advance(total)
	}
	return res, err
}

func (g *timedGen) ChatStream(ctx context.Context, scope contracts.Scope, req providers.ChatRequest) (providers.ChatStream, error) {
	s, err := g.inner.ChatStream(ctx, scope, req)
	if err != nil {
		return nil, err
	}
	ttft, total := g.latency(scope.SessionID)
	return &timedStream{inner: s, clk: g.clk, ttft: ttft, rest: total - ttft}, nil
}

type timedStream struct {
	inner providers.ChatStream
	clk   *clock.Fake
	ttft  time.Duration
	rest  time.Duration
	first bool
}

func (s *timedStream) Recv() (providers.ChatChunk, error) {
	ch, err := s.inner.Recv()
	if err != nil {
		return ch, err
	}
	if ch.Delta != "" && !s.first {
		s.first = true
		s.clk.Advance(s.ttft)
	}
	if ch.Done {
		s.clk.Advance(s.rest)
	}
	return ch, nil
}

func (s *timedStream) Close() error { return s.inner.Close() }

// testRunConfig builds a RunConfig around gen with the fake clock clk.
func testRunConfig(t testing.TB, corpus *Corpus, gen providers.TextGenerator, clk clock.Clock) RunConfig {
	t.Helper()
	proposal, _, _ := realSchemas(t)
	return RunConfig{
		Provider: "fake", Gen: gen, Clock: clk,
		Corpus: corpus, Warmup: 1,
		Role: providers.RoleController, Constraint: ConstraintSchema, Stream: true,
		MaxToks: 256, PromptBudget: 4096, Timeout: time.Minute,
		Profile:  models.ProfileLite,
		Model:    ModelInfo{RegistryID: fakeModelID, ArtifactHashSource: HashNone},
		Backend:  backendFor("fake", nil, ""),
		Proposal: proposal,
		Host:     HostInfo{LogicalCores: 4, OS: "testos", Arch: "testarch"},
	}
}

// caseIs matches calls made for one case.
func caseIs(id string) fake.Matcher {
	return func(c fake.Call) bool { return c.Scope.SessionID == id }
}

func floatEq(p *float64, want float64) bool {
	return p != nil && *p == want
}
