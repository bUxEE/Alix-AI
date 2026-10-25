package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"

	"github.com/buxee/alix-ai/adapters/localai"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// fakeDim is the dimensionality of the fake embeddings: the one of Granite
// Embedding 97M, the Lite default (ADR 0010), so the vector index is sized as
// it will be with the real model.
const fakeDim = 384

// fakeModel is the model id the fake embedder reports and the index records.
const fakeModel = "rageval-fake-384"

// hashEmbedder is the fake embedder: a deterministic hashed bag of words
// (internal/providers/fake.EmbedVector). It costs microseconds, so a run with
// it measures the index, the vector scan, the fusion and the checks around the
// models and nothing about a model. It shares words, not meaning: a paraphrase
// or a question in another language is out of its reach, and the per-kind
// breakdown of a fake run shows exactly that.
//
// It is the provider's role interface and nothing else: it records no prompts
// (the stock fake provider keeps every input it was sent, which a 50,000-chunk
// build would hold in memory).
type hashEmbedder struct {
	model string
	dim   int
	calls atomic.Int64
	items atomic.Int64
}

func newHashEmbedder() *hashEmbedder { return &hashEmbedder{model: fakeModel, dim: fakeDim} }

func (h *hashEmbedder) Embed(ctx context.Context, scope contracts.Scope, req providers.EmbedRequest) (providers.EmbedResult, error) {
	if err := scope.Validate(); err != nil {
		return providers.EmbedResult{}, err
	}
	if err := req.Validate(); err != nil {
		return providers.EmbedResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return providers.EmbedResult{}, providers.ContextError(err)
	}
	h.calls.Add(1)
	h.items.Add(int64(len(req.Inputs)))
	out := make([][]float32, len(req.Inputs))
	toks := 0
	for i, in := range req.Inputs {
		out[i] = fake.EmbedVector(in, h.dim)
		toks += providers.EstimateTokens(in)
	}
	return providers.EmbedResult{
		Vectors: out, Dimensions: h.dim, Model: h.model,
		Usage: providers.Usage{InputTokens: toks},
	}, nil
}

// Name and Health make it an AIRuntimeProvider, so the registry can probe it.
func (h *hashEmbedder) Name() string { return "rageval-fake" }

func (h *hashEmbedder) Health(context.Context) (providers.HealthReport, error) {
	return providers.HealthReport{Status: providers.HealthOK, Version: "fake"}, nil
}

// pinnedModel makes an embedder report one model id, whatever name its runtime
// answers under: the vector index refuses vectors that arrive under another
// model than the one it was configured for, and a runtime may spell its model
// differently from the registry (a file name, a tag). The name the runtime
// reported is kept so the result can say what ran.
type pinnedModel struct {
	inner    providers.Embedder
	model    string
	reported atomic.Value // string
	dims     atomic.Int64
}

func (p *pinnedModel) Embed(ctx context.Context, scope contracts.Scope, req providers.EmbedRequest) (providers.EmbedResult, error) {
	res, err := p.inner.Embed(ctx, scope, req)
	if err != nil {
		return res, err
	}
	if res.Model != "" {
		p.reported.Store(res.Model)
	}
	if res.Dimensions > 0 {
		p.dims.Store(int64(res.Dimensions))
	}
	res.Model = p.model
	return res, nil
}

// Dimensions is the dimension of the vectors the runtime last answered with, or 0.
func (p *pinnedModel) Dimensions() int { return int(p.dims.Load()) }

// Reported is the model name the runtime last answered under, or "".
func (p *pinnedModel) Reported() string {
	s, _ := p.reported.Load().(string)
	return s
}

// localaiEmbedder connects to a running LocalAI and returns its embedder. The
// tool never starts LocalAI: a runtime that is not ready, or that lacks the
// model, is an error before anything is embedded. It returns the runtime
// version the probe reported.
func localaiEmbedder(ctx context.Context, o runOptions, stderr io.Writer) (providers.Embedder, string, error) {
	endpoint := o.endpoint
	if endpoint == "" {
		endpoint = os.Getenv("ALIXAI_LOCALAI_URL")
	}
	keyRef := o.apiKeyRef
	if keyRef == "" {
		if _, ok := os.LookupEnv("ALIXAI_LOCALAI_KEY"); ok {
			keyRef = "env:ALIXAI_LOCALAI_KEY"
		}
	}
	runtimeName := o.localaiModel
	if runtimeName == "" {
		runtimeName = o.embeddingModel
	}
	client, err := localai.New(localai.Config{
		Endpoint:       endpoint,
		APIKeyRef:      keyRef,
		Models:         localai.ModelSet{Embedder: runtimeName},
		RequestTimeout: o.timeout,
	})
	if err != nil {
		return nil, "", err
	}
	rep, err := client.Health(ctx)
	if err != nil {
		return nil, "", err
	}
	switch rep.Status {
	case providers.HealthOK:
	case providers.HealthDegraded:
		fmt.Fprintf(stderr, "rageval: warning: LocalAI is degraded: %s\n", rep.Reason)
	default:
		return nil, "", fmt.Errorf("LocalAI is not ready: %s (is the model installed under the name %q? use --localai-model)", rep.Reason, runtimeName)
	}
	pm := &pinnedModel{inner: client, model: o.embeddingModel}
	// One throwaway embedding before anything is timed: it loads the model (a cold
	// start is seconds, and would land in the first measured stage), and it is how
	// the run learns the dimension and the name the runtime answers under.
	if _, err := pm.Embed(ctx, scaleScopeOf(0), providers.EmbedRequest{Inputs: []string{"rageval probe"}}); err != nil {
		return nil, "", fmt.Errorf("the probe embedding failed: %w", err)
	}
	if pm.Dimensions() < 1 {
		return nil, "", errors.New("the runtime answered the probe embedding with no dimension")
	}
	return pm, rep.Version, nil
}

// newSpecEmbedder builds the embedder a scale phase asked for, in the process
// of the phase.
func newSpecEmbedder(ctx context.Context, e embedderSpec) (providers.Embedder, error) {
	switch e.Kind {
	case "fake":
		return newHashEmbedder(), nil
	case "localai":
		emb, _, err := localaiEmbedder(ctx, runOptions{
			embeddingModel: e.Model, localaiModel: e.RuntimeModel, endpoint: e.Endpoint, apiKeyRef: e.APIKeyRef,
			timeout: time.Duration(e.TimeoutMS) * time.Millisecond,
		}, io.Discard)
		return emb, err
	}
	return nil, fmt.Errorf("embedder kind %q has no embedder", e.Kind)
}

// lexicalReranker is the harness's reranker: the fraction of the question's
// words a passage holds (fake.RerankScore). It exists so the retrieval
// pipeline's rerank path can be exercised and costed without a model; no
// adapter serves the Reranker role yet, so a run with it is always synthetic.
type lexicalReranker struct{}

func (lexicalReranker) Rerank(ctx context.Context, scope contracts.Scope, req providers.RerankRequest) (providers.RerankResult, error) {
	if err := ctx.Err(); err != nil {
		return providers.RerankResult{}, providers.ContextError(err)
	}
	hits := make([]providers.RerankHit, len(req.Documents))
	for i, d := range req.Documents {
		hits[i] = providers.RerankHit{Index: i, Score: fake.RerankScore(req.Query, d)}
	}
	// Best first, ties by position: the order is deterministic.
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hits[j].Score > hits[j-1].Score; j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
	if req.TopN > 0 && req.TopN < len(hits) {
		hits = hits[:req.TopN]
	}
	return providers.RerankResult{Hits: hits, Model: "rageval-lexical"}, nil
}

func (lexicalReranker) Name() string { return "rageval-lexical" }

func (lexicalReranker) Health(context.Context) (providers.HealthReport, error) {
	return providers.HealthReport{Status: providers.HealthOK, Version: "fake"}, nil
}
