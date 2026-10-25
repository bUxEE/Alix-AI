package sdkgo

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// RAGClient is the RAG area of the public API (master plan 14.2): sources, indexing, scoped search, answers
// with citations and the index generations. Every method takes the verified scope from the context token the
// host supplies and nothing else: no request names a tenant, a workspace, a principal or a folder. Get one
// with Client.RAG; it shares the client's options, transport and token provider.
type RAGClient struct{ c *Client }

// RAG returns the client of the RAG area.
func (c *Client) RAG() *RAGClient { return &RAGClient{c: c} }

const (
	maxSourceIDs = 32
	maxLimit     = 50
	maxDecl      = 128
)

func ragRequest(op Operation, params map[string]string, body []byte) request {
	req := request{op: op, params: params, accept: "application/json"}
	if body != nil {
		req.contentType = "application/json"
		req.body = body
	}
	return req
}

func sourceParam(sourceID string) (map[string]string, error) {
	if err := patternID("sourceID", sourceIDPattern, sourceID); err != nil {
		return nil, err
	}
	return map[string]string{"source_id": sourceID}, nil
}

// RegisterSource registers the source the application's active package declares under `declaration`. A source
// is never defined by a client: a name the package does not declare is `validation_failed` (details.reason
// not_declared). Registering the same declaration again returns the same source. It needs the deployment's
// operator action `manage_sources`; without it the call is `permission_denied`.
func (r *RAGClient) RegisterSource(ctx context.Context, declaration string, options ...CallOption) (*Source, error) {
	if declaration == "" || declaration != strings.TrimSpace(declaration) || len(declaration) > maxDecl ||
		strings.IndexFunc(declaration, unicode.IsControl) >= 0 {
		return nil, invalidArgument("declaration",
			"it must be the name the package declares: 1 to 128 characters, no padding, no control character")
	}
	body := jsonBody(map[string]string{"declaration": declaration})
	return doJSON(ctx, r.c.t, ragRequest(opRegisterRagSource, nil, body), applyOptions(options).deadline(r.c.timeout), (*Source).validate)
}

// ListSources lists every source registered for the verified tenant and workspace.
func (r *RAGClient) ListSources(ctx context.Context, options ...CallOption) ([]Source, error) {
	l, err := doJSON(ctx, r.c.t, ragRequest(opListRagSources, nil, nil), applyOptions(options).deadline(r.c.timeout), (*sourceList).validate)
	if err != nil {
		return nil, err
	}
	return l.Sources, nil
}

// GetSource is one source with its indexing status. A source of another scope is `validation_failed`
// (details.reason not_found), like one that does not exist.
func (r *RAGClient) GetSource(ctx context.Context, sourceID string, options ...CallOption) (*Source, error) {
	params, err := sourceParam(sourceID)
	if err != nil {
		return nil, err
	}
	return doJSON(ctx, r.c.t, ragRequest(opGetRagSource, params, nil), applyOptions(options).deadline(r.c.timeout), (*Source).validate)
}

// RemoveSource removes a source: it stops being served at once and its declared deletion behaviour is
// applied. It needs `manage_sources`.
func (r *RAGClient) RemoveSource(ctx context.Context, sourceID string, options ...CallOption) error {
	params, err := sourceParam(sourceID)
	if err != nil {
		return err
	}
	return doEmpty(ctx, r.c.t, ragRequest(opRemoveRagSource, params, nil), applyOptions(options).deadline(r.c.timeout))
}

// ReauditSources re-binds the caller's sources to the active package after it was replaced or re-audited. It
// needs `manage_sources`.
func (r *RAGClient) ReauditSources(ctx context.Context, options ...CallOption) (*ReauditReport, error) {
	return doJSON[ReauditReport](ctx, r.c.t, ragRequest(opReauditRagSources, nil, []byte("{}")), applyOptions(options).deadline(r.c.timeout), nil)
}

// SyncSource queues a sync of a source. Requests that arrive while one is queued or running are the same job.
// It needs `manage_sources`.
func (r *RAGClient) SyncSource(ctx context.Context, sourceID string, options ...CallOption) (*SyncAccepted, error) {
	params, err := sourceParam(sourceID)
	if err != nil {
		return nil, err
	}
	return doJSON(ctx, r.c.t, ragRequest(opSyncRagSource, params, []byte("{}")), applyOptions(options).deadline(r.c.timeout), (*SyncAccepted).validate)
}

// RebuildSource builds a replacement index generation; the one that serves keeps serving until it is
// activated. It needs `manage_sources`.
func (r *RAGClient) RebuildSource(ctx context.Context, sourceID string, options ...CallOption) (*SyncAccepted, error) {
	params, err := sourceParam(sourceID)
	if err != nil {
		return nil, err
	}
	return doJSON(ctx, r.c.t, ragRequest(opRebuildRagSource, params, []byte("{}")), applyOptions(options).deadline(r.c.timeout), (*SyncAccepted).validate)
}

// ListGenerations lists the index generations of a source, newest first.
func (r *RAGClient) ListGenerations(ctx context.Context, sourceID string, options ...CallOption) ([]rag.IndexGeneration, error) {
	params, err := sourceParam(sourceID)
	if err != nil {
		return nil, err
	}
	l, err := doJSON[generationList](ctx, r.c.t, ragRequest(opListRagGenerations, params, nil), applyOptions(options).deadline(r.c.timeout), nil)
	if err != nil {
		return nil, err
	}
	for i := range l.Generations {
		if err := checkGeneration(&l.Generations[i]); err != nil {
			return nil, unexpectedShape(opListRagGenerations, err)
		}
	}
	return l.Generations, nil
}

// InvalidateGeneration withdraws a generation: it is never retrievable again. It needs `manage_sources`.
func (r *RAGClient) InvalidateGeneration(ctx context.Context, sourceID, generationID string, options ...CallOption) (*rag.IndexGeneration, error) {
	params, err := sourceParam(sourceID)
	if err != nil {
		return nil, err
	}
	if err := patternID("generationID", generationPattern, generationID); err != nil {
		return nil, err
	}
	params["generation_id"] = generationID
	return doJSON(ctx, r.c.t, ragRequest(opInvalidateRagGeneration, params, []byte("{}")), applyOptions(options).deadline(r.c.timeout), checkGeneration)
}

func checkSourceIDs(ids []actions.SourceID) error {
	if ids == nil {
		return nil
	}
	if len(ids) == 0 || len(ids) > maxSourceIDs {
		return invalidArgument("SourceIDs", "it must list 1 to 32 source ids")
	}
	seen := map[actions.SourceID]bool{}
	for _, id := range ids {
		if err := patternID("SourceIDs", sourceIDPattern, string(id)); err != nil {
			return err
		}
		if seen[id] {
			return invalidArgument("SourceIDs", "it must not list a source twice")
		}
		seen[id] = true
	}
	return nil
}

// Search returns passages of the authorised sources that match, best first. An empty Results is a search that
// matched nothing, not an error. Notices label a degraded search.
func (r *RAGClient) Search(ctx context.Context, request rag.SearchRequest, options ...CallOption) (*rag.SearchResponse, error) {
	if err := validText("Query", request.Query); err != nil {
		return nil, err
	}
	if err := checkSourceIDs(request.SourceIDs); err != nil {
		return nil, err
	}
	if request.Limit != nil && (*request.Limit < 1 || *request.Limit > maxLimit) {
		return nil, invalidArgument("Limit", "it must be an integer from 1 to 50")
	}
	return doJSON(ctx, r.c.t, ragRequest(opSearchRag, nil, jsonBody(request)), applyOptions(options).deadline(r.c.timeout), checkSearch)
}

// Answer returns a grounded answer with a citation for every claim, or the typed no_supported_evidence result:
// test with Answered and NoEvidence. Never an uncited answer: a response that is neither is an error. A
// request that could not run is a typed error, never no_supported_evidence.
func (r *RAGClient) Answer(ctx context.Context, request rag.AnswerRequest, options ...CallOption) (*rag.AnswerResult, error) {
	if err := validText("Question", string(request.Question)); err != nil {
		return nil, err
	}
	if err := checkSourceIDs(request.SourceIDs); err != nil {
		return nil, err
	}
	// The union decodes strictly: an outcome that is neither answered nor no_supported_evidence is refused.
	return doJSON(ctx, r.c.t, ragRequest(opAnswerRag, nil, jsonBody(request)), applyOptions(options).deadline(r.c.answerTimeout), func(a *rag.AnswerResult) error {
		if a.Value == nil {
			return fmt.Errorf("answer: the outcome is neither answered nor no_supported_evidence")
		}
		return nil
	})
}

// ResolveCitation is what the `file` open reference of a citation resolves to now: the source's state, the
// cited passage and the way to open the source. A reference that does not resolve for the caller is
// `permission_denied`, an expired one `expired`. Never use the files API for it.
func (r *RAGClient) ResolveCitation(ctx context.Context, fileID string, options ...CallOption) (*CitationResolution, error) {
	if err := patternID("fileID", filePattern, fileID); err != nil {
		return nil, err
	}
	return doJSON(ctx, r.c.t, ragRequest(opResolveRagCitation, map[string]string{"file_id": fileID}, nil),
		applyOptions(options).deadline(r.c.timeout), (*CitationResolution).validate)
}
