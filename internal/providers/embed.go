package providers

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// EmbedPurpose says how an embedding will be used. Some embedding models need
// a different instruction prefix for queries and documents; adapters apply it.
type EmbedPurpose string

const (
	EmbedDocument EmbedPurpose = "document"
	EmbedQuery    EmbedPurpose = "query"
)

// EmbedRequest asks for one embedding per input, in order.
type EmbedRequest struct {
	Inputs  []string     `json:"inputs"`
	Purpose EmbedPurpose `json:"purpose,omitempty"`
	// Profile is an optional model-profile hint, as in ChatRequest.
	Profile string `json:"profile,omitempty"`
}

// EmbedResult holds one vector per input, in input order.
type EmbedResult struct {
	Vectors    [][]float32 `json:"vectors"`
	Dimensions int         `json:"dimensions"`
	Usage      Usage       `json:"usage"`
	Timings    Timings     `json:"timings"`
	TraceID    string      `json:"trace_id,omitempty"`
	// Model is the model the runtime reports having used; ingestion records
	// it as the embedder component version.
	Model string `json:"model,omitempty"`
}

// Embedder serves the embedding role. Implementations validate scope and
// request, honour ctx and return *contracts.Error values.
type Embedder interface {
	Embed(ctx context.Context, scope contracts.Scope, req EmbedRequest) (EmbedResult, error)
}

// Validate checks the request's shape (not budgets).
func (r EmbedRequest) Validate() error {
	if len(r.Inputs) == 0 {
		return invalidRequest("embed request has no inputs")
	}
	for i, in := range r.Inputs {
		if strings.TrimSpace(in) == "" {
			return invalidRequest(fmt.Sprintf("embed input %d is empty", i))
		}
	}
	switch r.Purpose {
	case "", EmbedDocument, EmbedQuery:
	default:
		return invalidRequest(fmt.Sprintf("unknown embed purpose %q", string(r.Purpose)))
	}
	return nil
}

// CheckResult verifies a result answers a request: one non-empty, finite,
// equally sized vector per input. A NaN in a vector index poisons every
// search, so this is a safety check, not politeness.
func (res EmbedResult) CheckResult(req EmbedRequest) error {
	if len(res.Vectors) != len(req.Inputs) {
		return contracts.Newf(contracts.ErrSchemaInvalid, "providers: runtime returned %d vectors for %d inputs", len(res.Vectors), len(req.Inputs))
	}
	dim := len(res.Vectors[0])
	if dim == 0 {
		return contracts.New(contracts.ErrSchemaInvalid, "providers: runtime returned an empty vector")
	}
	for i, v := range res.Vectors {
		if len(v) != dim {
			return contracts.Newf(contracts.ErrSchemaInvalid, "providers: vector %d has %d dimensions, want %d", i, len(v), dim)
		}
		for _, x := range v {
			if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
				return contracts.Newf(contracts.ErrSchemaInvalid, "providers: vector %d holds a non-finite value", i)
			}
		}
	}
	if res.Dimensions != 0 && res.Dimensions != dim {
		return contracts.Newf(contracts.ErrSchemaInvalid, "providers: result claims %d dimensions, vectors have %d", res.Dimensions, dim)
	}
	return nil
}

// RerankRequest asks for the documents ordered by relevance to the query.
type RerankRequest struct {
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	// TopN limits the hits returned; zero means all.
	TopN    int    `json:"top_n,omitempty"`
	Profile string `json:"profile,omitempty"`
}

// RerankHit scores one document. Index is the document's position in the
// request.
type RerankHit struct {
	Index int     `json:"index"`
	Score float64 `json:"score"`
}

// RerankResult lists hits best first.
type RerankResult struct {
	Hits    []RerankHit `json:"hits"`
	Usage   Usage       `json:"usage"`
	Timings Timings     `json:"timings"`
	TraceID string      `json:"trace_id,omitempty"`
	Model   string      `json:"model,omitempty"`
}

// Reranker serves the reranker role. It only reorders a shortlist the caller
// has already authorized; it never sees anything else. Implementations
// validate scope and request, honour ctx and return *contracts.Error values.
type Reranker interface {
	Rerank(ctx context.Context, scope contracts.Scope, req RerankRequest) (RerankResult, error)
}

// Validate checks the request's shape (not budgets).
func (r RerankRequest) Validate() error {
	if strings.TrimSpace(r.Query) == "" {
		return invalidRequest("rerank query is empty")
	}
	if len(r.Documents) == 0 {
		return invalidRequest("rerank request has no documents")
	}
	if r.TopN < 0 {
		return invalidRequest("top_n is negative")
	}
	return nil
}

// Normalize checks a result against its request (indexes in range and unique,
// scores finite), orders hits best first (stable, so ties keep runtime order)
// and applies TopN. It returns a new result.
func (res RerankResult) Normalize(req RerankRequest) (RerankResult, error) {
	seen := make(map[int]bool, len(res.Hits))
	hits := make([]RerankHit, 0, len(res.Hits))
	for _, h := range res.Hits {
		if h.Index < 0 || h.Index >= len(req.Documents) {
			return RerankResult{}, contracts.Newf(contracts.ErrSchemaInvalid, "providers: rerank hit index %d out of range", h.Index)
		}
		if seen[h.Index] {
			return RerankResult{}, contracts.Newf(contracts.ErrSchemaInvalid, "providers: rerank hit index %d repeated", h.Index)
		}
		if math.IsNaN(h.Score) || math.IsInf(h.Score, 0) {
			return RerankResult{}, contracts.Newf(contracts.ErrSchemaInvalid, "providers: rerank hit %d has a non-finite score", h.Index)
		}
		seen[h.Index] = true
		hits = append(hits, h)
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if req.TopN > 0 && len(hits) > req.TopN {
		hits = hits[:req.TopN]
	}
	res.Hits = hits
	return res, nil
}
