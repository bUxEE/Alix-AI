package localai

import (
	"context"
	"fmt"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

type embedRequestWire struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
	// EncodingFormat is sent explicitly: some OpenAI clients default to
	// base64, and this adapter decodes plain float arrays only.
	EncodingFormat string `json:"encoding_format"`
}

type embedItemWire struct {
	Index     *int      `json:"index"`
	Embedding []float32 `json:"embedding"`
}

type embedResponseWire struct {
	ID    string          `json:"id"`
	Model string          `json:"model"`
	Data  []embedItemWire `json:"data"`
	Usage *usageWire      `json:"usage"`
	Error *apiError       `json:"error"`
}

// Embed implements providers.Embedder. Inputs are sent in batches of at most
// Config.EmbedBatchSize, one request after another (the runtime embeds each
// input in turn, so parallel requests would only compete for memory). Any
// failing batch fails the whole call: a partial set of vectors is never
// returned, because an index built from it would silently miss documents.
func (c *Client) Embed(ctx context.Context, scope contracts.Scope, req providers.EmbedRequest) (providers.EmbedResult, error) {
	if err := scope.Validate(); err != nil {
		return providers.EmbedResult{}, err
	}
	if err := req.Validate(); err != nil {
		return providers.EmbedResult{}, err
	}
	model, err := c.model(providers.RoleEmbedder, req.Profile)
	if err != nil {
		return providers.EmbedResult{}, err
	}
	prefix := ""
	switch req.Purpose {
	case providers.EmbedQuery:
		prefix = c.cfg.EmbedQueryPrefix
	case providers.EmbedDocument:
		prefix = c.cfg.EmbedDocumentPrefix
	}
	inputs := make([]string, len(req.Inputs))
	for i, in := range req.Inputs {
		inputs[i] = prefix + in
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer cancel()
	started := c.clk.Now()
	res := providers.EmbedResult{Vectors: make([][]float32, 0, len(inputs)), Model: model}
	for from := 0; from < len(inputs); from += c.cfg.EmbedBatchSize {
		to := min(from+c.cfg.EmbedBatchSize, len(inputs))
		vectors, usage, id, reported, err := c.embedBatch(ctx, model, inputs[from:to])
		if err != nil {
			return providers.EmbedResult{}, err
		}
		res.Vectors = append(res.Vectors, vectors...)
		res.Usage.InputTokens += usage.InputTokens
		res.Usage.OutputTokens += usage.OutputTokens
		if res.TraceID == "" {
			res.TraceID = id
		}
		res.Model = reported
	}
	if len(res.Vectors) > 0 {
		res.Dimensions = len(res.Vectors[0])
	}
	res.Timings = providers.Timings{Total: c.clk.Now().Sub(started)}
	if err := res.CheckResult(req); err != nil {
		return providers.EmbedResult{}, err
	}
	c.logDone(ctx, "embed", model, res.Usage, started)
	return res, nil
}

// embedBatch sends one embeddings request and returns its vectors in input
// order. Every index in [0, len(batch)) must appear exactly once.
func (c *Client) embedBatch(ctx context.Context, model string, batch []string) (vectors [][]float32, usage providers.Usage, id, reported string, err error) {
	resp, err := c.send(ctx, "POST", pathEmbed, embedRequestWire{Model: model, Input: batch, EncodingFormat: "float"}, "application/json")
	if err != nil {
		return nil, usage, "", "", err
	}
	defer resp.Body.Close()
	var out embedResponseWire
	if err := c.decodeBody(ctx, resp.Body, &out); err != nil {
		return nil, usage, "", "", err
	}
	if out.Error != nil {
		return nil, usage, "", "", mapEventError(*out.Error)
	}
	if len(out.Data) != len(batch) {
		return nil, usage, "", "", schemaInvalid(fmt.Sprintf("runtime returned %d embeddings for %d inputs", len(out.Data), len(batch)))
	}
	vectors = make([][]float32, len(batch))
	for _, item := range out.Data {
		if item.Index == nil || *item.Index < 0 || *item.Index >= len(batch) || vectors[*item.Index] != nil {
			return nil, usage, "", "", schemaInvalid("embedding indexes are missing, out of range or repeated")
		}
		if len(item.Embedding) == 0 {
			return nil, usage, "", "", schemaInvalid("runtime returned an empty embedding")
		}
		vectors[*item.Index] = item.Embedding
	}
	if usage, err = out.Usage.usage(); err != nil {
		return nil, usage, "", "", err
	}
	return vectors, usage, traceID(out.ID), reportedModel(out.Model, model), nil
}
