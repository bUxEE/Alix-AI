package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/rag/answer"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// The RAG area of the public API (master plan 14.2 "RAG", 12.4, 12.5, rule 9).
// The contract is schemas/api/paths/rag.yaml; TestRoutesMatchOpenAPISpec fails
// on any difference in either direction.
//
//	POST   /v1/rag/sources                                                register a source the active package declares
//	GET    /v1/rag/sources                                                the caller's sources and their status
//	GET    /v1/rag/sources/{source_id}                                    one source and its status (monitor indexing)
//	DELETE /v1/rag/sources/{source_id}                                    remove a source
//	POST   /v1/rag/sources/reaudit                                        re-bind the caller's sources to the active package (update)
//	POST   /v1/rag/sources/{source_id}/sync                               request indexing
//	POST   /v1/rag/sources/{source_id}/rebuild                            build a replacement index generation
//	GET    /v1/rag/sources/{source_id}/generations                        the index generations of a source
//	POST   /v1/rag/sources/{source_id}/generations/{generation_id}/invalidate   withdraw a generation
//	POST   /v1/rag/search                                                 scoped search
//	POST   /v1/rag/answer                                                 an answer with citations
//	GET    /v1/rag/citations/{file_id}                                    resolve the open/preview reference of a citation
//
// # Identity and scope
//
// Every handler starts from identity.RequireScope: the application, tenant,
// workspace and principal are the verified context token and nothing a path or a
// body says. The request types are closed (a body that names a tenant, a
// workspace, a principal or a folder is refused as malformed before any service
// is called), and the services below only ever receive the verified scope. An
// application whose package is not active serves no RAG traffic either.
//
// # Sources
//
// A source is registered by the name the application's active package declares it
// under and by nothing else (RAGSources.Register): the root, rules, limits and
// deletion behaviour come from the signed, audited package and the tenant and
// workspace from the scope, so a client can never create a source the package
// does not declare, point one at another folder or register one for another
// tenant. The id of the source is minted by the core; registering the same
// declaration again is the same registration (idempotent per scope and
// declaration). Registering, re-auditing, removing, requesting a sync, building a
// replacement generation and invalidating one are operator acts: the
// implementations decide from the deployment's own configuration of operators
// (the action manage_sources of rag.operators), never from a role or a capability
// scope, and a deployment with no such entry lets none of them through
// (permission_denied, 403). Reading the status of a source needs only the
// verified scope.
//
// A registration read back after a restart is served again when the process has
// re-bound it to the package; until then the implementation answers unavailable
// (503, retryable), which is neither not_found nor gone.
//
// # Search and answers
//
// POST /v1/rag/search returns authorised passages (SearchResponse) and
// POST /v1/rag/answer a grounded answer with a citation for every claim, or the
// typed no_supported_evidence result (AnswerResult): an answer, never an uncited
// one, and never a 4xx for "nothing found" (it is a 200 whose outcome says so). A
// request that could not run is a typed error (unavailable, timeout), never an
// empty result. Both are built over the same retrieval pipeline and one
// answer.Answerer per process (NewRAGQueries); what goes on the wire is the
// client types only (SearchHit, rag.AnswerResult with conversation/citation
// citations). The internal EvidenceItem, EvidenceCitation and EvidencePackage
// carry each source's data-egress mode and classification and never leave this
// package: the projection is written here and TestAnswerEndpointReturnsCitations
// checks the raw JSON for both members.
//
// The open_ref of an answer's citation is a file-kind reference (fil_ and a ULID)
// held in memory by the Answerer's resolver (bounded by rag_answer.max_refs,
// valid for rag_answer.ref_ttl, invalid after a restart). It is resolved by
// GET /v1/rag/citations/{file_id}, never by the files service: the resolver
// checks the scope, then the expiry (410), then asks the source catalog, fresh,
// whether the principal may still read the source, and one and the same
// permission_denied answers every reference that does not resolve for the caller.
// The host's own record or file reference comes back in open_ref.

func init() { RegisterArea("v1_rag", registerV1RAG) }

// registerV1RAG needs the source registry (RAGSources) and the application
// service. The retrieval half (RAGQueries) and the indexing half (RAGIndexing)
// are optional: a route of one of them is mounted only when the composition
// provides it, so a deployment that has not wired them serves the sources and
// answers the rest with a 404, never with a handler that holds nothing.
func registerV1RAG(rt *Router, d *Deps) {
	n := d.Needs()
	sources := Need[RAGSources](n)
	apps := Need[ApplicationService](n)
	if !n.Ready() {
		return
	}
	h := &ragAPIHandler{sources: RAGSourcesScoped(sources), apps: apps}
	if q, ok := Want[RAGQueries](n); ok {
		h.queries = RAGQueriesScoped(q)
	}
	if ix, ok := Want[RAGIndexing](n); ok {
		h.indexing = RAGIndexingScoped(ix)
	}
	v1 := rt.V1()
	v1.HandleFunc("POST /rag/sources", h.registerSource)
	v1.HandleFunc("GET /rag/sources", h.listSources)
	v1.HandleFunc("POST /rag/sources/reaudit", h.reauditSources)
	v1.HandleFunc("GET /rag/sources/{source_id}", h.getSource)
	v1.HandleFunc("DELETE /rag/sources/{source_id}", h.removeSource)
	if h.indexing != nil {
		v1.HandleFunc("POST /rag/sources/{source_id}/sync", h.syncSource)
		v1.HandleFunc("POST /rag/sources/{source_id}/rebuild", h.rebuildSource)
		v1.HandleFunc("GET /rag/sources/{source_id}/generations", h.listGenerations)
		v1.HandleFunc("POST /rag/sources/{source_id}/generations/{generation_id}/invalidate", h.invalidateGeneration)
	}
	if h.queries != nil {
		v1.HandleFunc("POST /rag/search", h.search)
		v1.HandleFunc("POST /rag/answer", h.answer)
		v1.HandleFunc("GET /rag/citations/{file_id}", h.resolveCitation)
	}
}

// The ID prefixes of the RAG area, as constants so that the repository's gate
// can check them against the typed-ID grammar of schemas/common/ids.
const (
	ragAPIIDSource     = "src"
	ragAPIIDGeneration = "gen"
	ragAPIIDReference  = "fil"
	ragAPIIDJob        = "job"
)

// The bounds of schemas/rag/search-request and answer-request.
const (
	ragAPIMaxQuestionRunes = query.MaxQuestionChars
	ragAPIMaxSourceIDs     = query.MaxRequestSources
	ragAPIMaxLimit         = 50
	// ragAPIMaxExcerptRunes is the schema's bound of a search hit's excerpt.
	ragAPIMaxExcerptRunes = 8192
)

// ---- the services ----

// RAGQueries is the retrieval half of the RAG area as the /v1 API sees it: search,
// grounded answers and the resolution of a citation's reference. Every method
// takes the verified scope and works inside it, and every type on it is a client
// type: nothing of the internal evidence records (their data-egress mode and
// classification) can be returned through it.
//
// NewRAGQueries is the implementation; the composition root provides it with
// Provide[RAGQueries]. The dialogue engine's knowledge seam (dialogue.Knowledge)
// is the Answer method of this interface, so one value serves both.
type RAGQueries interface {
	// Search returns the passages of the principal's authorised sources that
	// match, best first. An empty result is a search that matched nothing, not
	// an error; a search that could not run is a typed error.
	Search(ctx context.Context, scope contracts.Scope, req rag.SearchRequest) (rag.SearchResponse, error)
	// Answer returns a grounded answer with a citation for each claim, or the
	// no_supported_evidence result. It never returns an uncited answer.
	Answer(ctx context.Context, scope contracts.Scope, req rag.AnswerRequest) (rag.AnswerResult, error)
	// Open resolves the file-kind reference of a citation: scope, expiry, then a
	// fresh permission check of the source. A reference that does not resolve for
	// the caller is permission_denied, whatever the reason; an expired one is
	// expired.
	Open(ctx context.Context, scope contracts.Scope, fileID string) (RAGCitationBody, error)
}

// A RAGQueries is a dialogue.Knowledge: one value serves the answer endpoint and the
// dialogue engine, so a citation of a dialogue turn resolves through this API too.
var _ dialogue.Knowledge = RAGQueries(nil)

// RAGIndexing is the indexing half of the RAG area: requesting a sync and
// monitoring and changing the index generations of a source. The composition root
// implements it over the ingestion service and the index and provides it with
// Provide[RAGIndexing].
//
// # Who may call
//
// Sync, Rebuild and Invalidate need the operator action manage_sources, exactly
// like RAGSources.Register, Reaudit and Remove: the implementation decides from
// the deployment's own configuration (rag.operators), never from a role the host
// application or a package asserts, and a deployment with no such entry lets none
// of them through (permission_denied). Generations is a read inside the caller's
// own scope and needs nothing more. A source or generation the scope does not hold
// is reported exactly like one that does not exist: validation_failed with
// details.reason not_found.
type RAGIndexing interface {
	// Sync queues a sync of one source of the scope: ingestion scans it again and
	// indexes what changed. Requests that arrive while a sync is queued or
	// running are the same job.
	Sync(ctx context.Context, scope contracts.Scope, sourceID string) (RAGSyncJob, error)
	// Rebuild builds a replacement for the source's index generation from the
	// source itself: the generation that serves keeps serving until the
	// replacement has been validated and is activated, so there is never a
	// moment without a serving generation once one existed.
	Rebuild(ctx context.Context, scope contracts.Scope, sourceID string) (RAGSyncJob, error)
	// Generations lists the source's index generations, newest first.
	Generations(ctx context.Context, scope contracts.Scope, sourceID string) ([]rag.IndexGeneration, error)
	// Invalidate withdraws a generation: it is never retrievable again. Invalidating
	// the generation that serves leaves the source without one until a rebuild
	// activates a replacement, and the answer is the generation as it now stands.
	Invalidate(ctx context.Context, scope contracts.Scope, sourceID, generationID string) (rag.IndexGeneration, error)
}

// RAGSyncJob is the durable job a sync or a rebuild was queued as.
type RAGSyncJob struct {
	// SourceID is the source the job is for.
	SourceID string
	// JobID is the job (job_ and a ULID). Its progress is the source's status.
	JobID string
}

// RAGCitationBody is what a citation's reference resolves to, read at the moment
// it is resolved: the source's name, version and state now, the passage that was
// cited and the way to open the source itself.
type RAGCitationBody struct {
	SourceID    actions.SourceID              `json:"source_id"`
	DisplayName string                        `json:"display_name"`
	Version     actions.OpaqueVersion         `json:"version"`
	Locator     *conversation.CitationLocator `json:"locator,omitzero"`
	// Availability and Staleness are the source's state now, not at retrieval.
	Availability conversation.CitationAvailability `json:"availability"`
	Staleness    conversation.CitationStaleness    `json:"staleness"`
	// OpenRef is the way to open the source itself: the host's own record or
	// file reference when the source has one, else the source resolved by id.
	OpenRef conversation.CitationOpenRef `json:"open_ref"`
	// Preview is the passage the citation was grounded on, as it was cited. It
	// may be older than the source: Staleness says so. Empty when previews are
	// off (rag_answer.preview_runes 0).
	Preview string `json:"preview,omitempty"`
	// ExpiresAt is when the reference stops resolving.
	ExpiresAt common.Timestamp `json:"expires_at"`
}

// ---- bodies ----

// RAGSourceRegistration is the body of POST /v1/rag/sources: the name the
// application's active package declares the source under, and nothing else.
type RAGSourceRegistration struct {
	Declaration string `json:"declaration"`
}

// RAGSourceBody is one registered source as the API reports it.
type RAGSourceBody struct {
	// Declaration is the name the package declares the source under and
	// PackageVersion the version of the package it was last bound to.
	Declaration    string `json:"declaration"`
	PackageVersion string `json:"package_version"`
	// Serving is true while the source is bound to the active package and may be
	// read. A source registered under a package that was replaced is false until
	// it is re-bound, and so is one whose removal began.
	Serving bool `json:"serving"`
	// Status is the status of the source (schema rag/source-status).
	Status rag.SourceStatus `json:"status"`
}

// RAGSourcesBody is the body of GET /v1/rag/sources.
type RAGSourcesBody struct {
	Sources []RAGSourceBody `json:"sources"`
}

// RAGReauditBody is what a re-audit did, counted.
type RAGReauditBody struct {
	Checked int `json:"checked"`
	Rebound int `json:"rebound"`
	Queued  int `json:"queued"`
	Removed int `json:"removed"`
	Failed  int `json:"failed"`
}

// RAGSyncBody is the body of an accepted sync or rebuild.
type RAGSyncBody struct {
	SourceID string `json:"source_id"`
	JobID    string `json:"job_id"`
}

// RAGGenerationsBody is the body of GET /v1/rag/sources/{source_id}/generations.
type RAGGenerationsBody struct {
	Generations []rag.IndexGeneration `json:"generations"`
}

// ---- the handlers ----

type ragAPIHandler struct {
	sources  RAGSources
	queries  RAGQueries
	indexing RAGIndexing
	apps     ApplicationService
}

// ragAPIError writes the error of a RAG operation. The statuses beyond the
// gateway's mapping of the code are the ones this API declares: a source, a
// generation or a reference the scope does not hold is 404 when the service says
// not_found, and an expired citation reference is 410 (not the 401 of an expired
// credential: the client's token is fine). A retryable unavailable is 503.
func ragAPIError(w http.ResponseWriter, r *http.Request, err error) {
	var ce *contracts.Error
	if errors.As(err, &ce) && ce != nil {
		reason, _ := ce.Details["reason"].(string)
		switch {
		case ce.Code == contracts.ErrExpired:
			WriteErrorStatus(w, r, http.StatusGone, err)
			return
		case ce.Code == contracts.ErrValidationFailed && reason == "not_found":
			WriteErrorStatus(w, r, http.StatusNotFound, err)
			return
		}
	}
	WriteError(w, r, err)
}

// begin is the common start of a RAG route: the verified scope and an active
// application.
func (h *ragAPIHandler) begin(w http.ResponseWriter, r *http.Request) (contracts.Scope, bool) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return scope, false
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return scope, false
	}
	return scope, true
}

// ragAPIPathID reads a path id and checks it with valid (an ids.Valid of one of
// the prefixes above) before anything else sees it. The prefix is a constant at
// each ids.Valid call, so the repository gate can check it against the typed-ID
// grammar.
func ragAPIPathID(w http.ResponseWriter, r *http.Request, name string, valid func(string) bool) (string, bool) {
	id := r.PathValue(name)
	if !valid(id) {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "the "+strings.ReplaceAll(name, "_", " ")+" is malformed"))
		return "", false
	}
	return id, true
}

func ragAPIPathSource(w http.ResponseWriter, r *http.Request) (string, bool) {
	return ragAPIPathID(w, r, "source_id", func(s string) bool { return ids.Valid(s, ragAPIIDSource) })
}

func ragAPIPathGeneration(w http.ResponseWriter, r *http.Request) (string, bool) {
	return ragAPIPathID(w, r, "generation_id", func(s string) bool { return ids.Valid(s, ragAPIIDGeneration) })
}

func ragAPIPathReference(w http.ResponseWriter, r *http.Request) (string, bool) {
	return ragAPIPathID(w, r, "file_id", func(s string) bool { return ids.Valid(s, ragAPIIDReference) })
}

func ragAPISourceBody(rec RAGSourcesRecord) RAGSourceBody {
	return RAGSourceBody{
		Declaration: rec.PackageSourceID, PackageVersion: rec.PackageVersion, Serving: rec.Serving, Status: rec.Status,
	}
}

func (h *ragAPIHandler) registerSource(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	var req RAGSourceRegistration
	if !decodeJSON(w, r, &req) {
		return
	}
	// The scope guard refuses a name that is not well formed; the package decides
	// whether it declares it.
	rec, err := h.sources.Register(r.Context(), scope, RAGSourcesRegistration{PackageSourceID: req.Declaration})
	if err != nil {
		ragAPIError(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/rag/sources/"+string(rec.Status.SourceID))
	writeJSON(w, http.StatusOK, ragAPISourceBody(rec))
}

func (h *ragAPIHandler) listSources(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	recs, err := h.sources.Status(r.Context(), scope, "")
	if err != nil {
		ragAPIError(w, r, err)
		return
	}
	body := RAGSourcesBody{Sources: make([]RAGSourceBody, 0, len(recs))}
	for _, rec := range recs {
		body.Sources = append(body.Sources, ragAPISourceBody(rec))
	}
	writeJSON(w, http.StatusOK, body)
}

func (h *ragAPIHandler) getSource(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	id, ok := ragAPIPathSource(w, r)
	if !ok {
		return
	}
	recs, err := h.sources.Status(r.Context(), scope, id)
	if err != nil {
		ragAPIError(w, r, err)
		return
	}
	if len(recs) != 1 {
		ragAPIError(w, r, ragSourcesNotFound())
		return
	}
	writeJSON(w, http.StatusOK, ragAPISourceBody(recs[0]))
}

func (h *ragAPIHandler) removeSource(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	id, ok := ragAPIPathSource(w, r)
	if !ok {
		return
	}
	extendWriteDeadline(w)
	if err := h.sources.Remove(r.Context(), scope, id); err != nil {
		ragAPIError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ragAPIEmpty is the body of an operation that takes none: an empty object is
// accepted, any member is refused.
type ragAPIEmpty struct{}

func (h *ragAPIHandler) reauditSources(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	var none ragAPIEmpty
	if !decodeOptionalJSON(w, r, &none) {
		return
	}
	extendWriteDeadline(w)
	rep, err := h.sources.Reaudit(r.Context(), scope)
	if err != nil {
		ragAPIError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, RAGReauditBody{
		Checked: rep.Checked, Rebound: rep.Rebound, Queued: rep.Queued, Removed: rep.Removed, Failed: rep.Failed,
	})
}

// accepted answers a sync or a rebuild: the job is queued, and the status of the
// source is where its progress is read.
func ragAPIAccepted(w http.ResponseWriter, job RAGSyncJob) {
	w.Header().Set("Location", "/v1/rag/sources/"+job.SourceID)
	writeJSON(w, http.StatusAccepted, RAGSyncBody{SourceID: job.SourceID, JobID: job.JobID})
}

func (h *ragAPIHandler) syncSource(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	id, ok := ragAPIPathSource(w, r)
	if !ok {
		return
	}
	var none ragAPIEmpty
	if !decodeOptionalJSON(w, r, &none) {
		return
	}
	job, err := h.indexing.Sync(r.Context(), scope, id)
	if err != nil {
		ragAPIError(w, r, err)
		return
	}
	ragAPIAccepted(w, job)
}

func (h *ragAPIHandler) rebuildSource(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	id, ok := ragAPIPathSource(w, r)
	if !ok {
		return
	}
	var none ragAPIEmpty
	if !decodeOptionalJSON(w, r, &none) {
		return
	}
	job, err := h.indexing.Rebuild(r.Context(), scope, id)
	if err != nil {
		ragAPIError(w, r, err)
		return
	}
	ragAPIAccepted(w, job)
}

func (h *ragAPIHandler) listGenerations(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	id, ok := ragAPIPathSource(w, r)
	if !ok {
		return
	}
	gens, err := h.indexing.Generations(r.Context(), scope, id)
	if err != nil {
		ragAPIError(w, r, err)
		return
	}
	if gens == nil {
		gens = []rag.IndexGeneration{}
	}
	writeJSON(w, http.StatusOK, RAGGenerationsBody{Generations: gens})
}

func (h *ragAPIHandler) invalidateGeneration(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	id, ok := ragAPIPathSource(w, r)
	if !ok {
		return
	}
	gid, ok := ragAPIPathGeneration(w, r)
	if !ok {
		return
	}
	var none ragAPIEmpty
	if !decodeOptionalJSON(w, r, &none) {
		return
	}
	gen, err := h.indexing.Invalidate(r.Context(), scope, id, gid)
	if err != nil {
		ragAPIError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, gen)
}

func (h *ragAPIHandler) search(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	var req rag.SearchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	extendWriteDeadline(w)
	res, err := h.queries.Search(r.Context(), scope, req)
	if err != nil {
		ragAPIError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *ragAPIHandler) answer(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	var req rag.AnswerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	extendWriteDeadline(w)
	res, err := h.queries.Answer(r.Context(), scope, req)
	if err != nil {
		ragAPIError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *ragAPIHandler) resolveCitation(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	id, ok := ragAPIPathReference(w, r)
	if !ok {
		return
	}
	body, err := h.queries.Open(r.Context(), scope, id)
	if err != nil {
		ragAPIError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// ---- request checks ----

// ragAPICheckQuestion checks a question or a search text against the bounds of
// the schemas: valid UTF-8, a non-space character, no NUL, at most 2000 characters.
func ragAPICheckQuestion(what, s string) error {
	switch {
	case !utf8.ValidString(s):
		return contracts.New(contracts.ErrValidationFailed, "the "+what+" is not valid UTF-8")
	case strings.TrimSpace(s) == "":
		return contracts.New(contracts.ErrValidationFailed, "the "+what+" is empty")
	case strings.ContainsRune(s, 0):
		return contracts.New(contracts.ErrValidationFailed, "the "+what+" contains a NUL")
	case utf8.RuneCountInString(s) > ragAPIMaxQuestionRunes:
		return contracts.Newf(contracts.ErrValidationFailed, "the %s is over %d characters", what, ragAPIMaxQuestionRunes)
	}
	return nil
}

// ragAPICheckSourceIDs checks the optional list of sources a request narrows to:
// at most 32, each a well-formed source id, no repeats.
func ragAPICheckSourceIDs(list []actions.SourceID) error {
	if len(list) > ragAPIMaxSourceIDs {
		return contracts.Newf(contracts.ErrValidationFailed, "at most %d source ids", ragAPIMaxSourceIDs)
	}
	seen := make(map[actions.SourceID]struct{}, len(list))
	for _, id := range list {
		if !ids.Valid(string(id), ragAPIIDSource) {
			return contracts.New(contracts.ErrValidationFailed, "a source id is malformed")
		}
		if _, dup := seen[id]; dup {
			return contracts.New(contracts.ErrValidationFailed, "a source id is listed twice")
		}
		seen[id] = struct{}{}
	}
	return nil
}

func ragAPICheckSearch(req rag.SearchRequest) error {
	if err := ragAPICheckQuestion("query", req.Query); err != nil {
		return err
	}
	if err := ragAPICheckSourceIDs(req.SourceIDs); err != nil {
		return err
	}
	if req.Limit != nil && (*req.Limit < 1 || *req.Limit > ragAPIMaxLimit) {
		return contracts.Newf(contracts.ErrValidationFailed, "the limit is 1 to %d", ragAPIMaxLimit)
	}
	return nil
}

func ragAPICheckAnswer(req rag.AnswerRequest) error {
	if err := ragAPICheckQuestion("question", string(req.Question)); err != nil {
		return err
	}
	return ragAPICheckSourceIDs(req.SourceIDs)
}

// ragAPISourceSet is the set a request narrows to, nil when it names none (every
// authorised source).
func ragAPISourceSet(list []actions.SourceID) map[actions.SourceID]struct{} {
	if len(list) == 0 {
		return nil
	}
	set := make(map[actions.SourceID]struct{}, len(list))
	for _, id := range list {
		set[id] = struct{}{}
	}
	return set
}

// ---- the scope guard of RAGQueries ----

// RAGQueriesScoped wraps a RAGQueries so that the checks of the API are made in
// the gateway whatever the implementation does:
//
//   - a call with an incomplete or malformed scope is refused before it reaches
//     the implementation, with the scope's own code, and the implementation is only
//     ever given the exact scope the middleware verified;
//   - a request that does not satisfy its schema (an empty or over-long text, more
//     than 32 sources, a malformed or repeated source id, a limit outside 1 to 50,
//     a reference that is not a fil_ id) is validation_failed and never a query;
//   - what comes back is checked against the request: a search answers no more than
//     it was asked for and nothing outside the sources it was narrowed to, an answer
//     is either a GroundedAnswer with a citation for every claim or the typed
//     no_supported_evidence result, and a citation is never of a source the request
//     excluded. A result that fails is an internal error, never passed on.
//
// It returns nil for a nil implementation.
func RAGQueriesScoped(inner RAGQueries) RAGQueries {
	if inner == nil {
		return nil
	}
	return ragQueriesScoped{inner: inner}
}

type ragQueriesScoped struct{ inner RAGQueries }

var _ RAGQueries = ragQueriesScoped{}

func ragAPIBadResult(what string) error {
	return contracts.New(contracts.ErrInternal, "the RAG service answered with a "+what+" that does not satisfy the contract")
}

func (g ragQueriesScoped) Search(ctx context.Context, scope contracts.Scope, req rag.SearchRequest) (rag.SearchResponse, error) {
	if err := scope.Validate(); err != nil {
		return rag.SearchResponse{}, err
	}
	if err := ragAPICheckSearch(req); err != nil {
		return rag.SearchResponse{}, err
	}
	res, err := g.inner.Search(ctx, scope, req)
	if err != nil {
		return rag.SearchResponse{}, err
	}
	allowed := ragAPISourceSet(req.SourceIDs)
	if len(res.Results) > ragAPIMaxLimit || (req.Limit != nil && int64(len(res.Results)) > *req.Limit) {
		return rag.SearchResponse{}, ragAPIBadResult("search result")
	}
	for _, h := range res.Results {
		if !ids.Valid(string(h.SourceID), ragAPIIDSource) || strings.TrimSpace(h.Excerpt) == "" {
			return rag.SearchResponse{}, ragAPIBadResult("search result")
		}
		if _, ok := allowed[h.SourceID]; allowed != nil && !ok {
			return rag.SearchResponse{}, ragAPIBadResult("search result")
		}
	}
	return res, nil
}

func (g ragQueriesScoped) Answer(ctx context.Context, scope contracts.Scope, req rag.AnswerRequest) (rag.AnswerResult, error) {
	if err := scope.Validate(); err != nil {
		return rag.AnswerResult{}, err
	}
	if err := ragAPICheckAnswer(req); err != nil {
		return rag.AnswerResult{}, err
	}
	res, err := g.inner.Answer(ctx, scope, req)
	if err != nil {
		return rag.AnswerResult{}, err
	}
	if err := ragAPICheckAnswerResult(res, ragAPISourceSet(req.SourceIDs)); err != nil {
		return rag.AnswerResult{}, err
	}
	return res, nil
}

func (g ragQueriesScoped) Open(ctx context.Context, scope contracts.Scope, fileID string) (RAGCitationBody, error) {
	if err := scope.Validate(); err != nil {
		return RAGCitationBody{}, err
	}
	if !ids.Valid(fileID, ragAPIIDReference) {
		return RAGCitationBody{}, contracts.New(contracts.ErrValidationFailed, "the reference is malformed")
	}
	return g.inner.Open(ctx, scope, fileID)
}

// ragAPICheckAnswerResult is the provenance rule of master plan 3 rule 9 and 12.5
// on the wire: an answer has at least one claim and one citation, every claim cites
// at least one citation that exists, every citation has claims and a source the
// request did not exclude. The no_supported_evidence result needs a reason. There
// is no third shape.
func ragAPICheckAnswerResult(res rag.AnswerResult, allowed map[actions.SourceID]struct{}) error {
	switch v := res.Value.(type) {
	case rag.GroundedAnswer:
		return ragAPICheckGrounded(v, allowed)
	case *rag.GroundedAnswer:
		if v == nil {
			return ragAPIBadResult("answer")
		}
		return ragAPICheckGrounded(*v, allowed)
	case rag.NoSupportedEvidence:
		if !v.Reason.Valid() {
			return ragAPIBadResult("answer")
		}
		return nil
	case *rag.NoSupportedEvidence:
		if v == nil || !v.Reason.Valid() {
			return ragAPIBadResult("answer")
		}
		return nil
	}
	return ragAPIBadResult("answer")
}

func ragAPICheckGrounded(a rag.GroundedAnswer, allowed map[actions.SourceID]struct{}) error {
	if len(a.Claims) == 0 || len(a.Claims) > 64 || len(a.Citations) == 0 || len(a.Citations) > 32 {
		return ragAPIBadResult("answer")
	}
	for _, c := range a.Citations {
		if !ids.Valid(string(c.SourceID), ragAPIIDSource) || len(c.Claims) == 0 {
			return ragAPIBadResult("answer")
		}
		if _, ok := allowed[c.SourceID]; allowed != nil && !ok {
			return ragAPIBadResult("answer")
		}
	}
	for _, cl := range a.Claims {
		if strings.TrimSpace(cl.Text) == "" || len(cl.CitationIndexes) == 0 {
			return ragAPIBadResult("answer")
		}
		for _, i := range cl.CitationIndexes {
			if i < 0 || i >= int64(len(a.Citations)) {
				return ragAPIBadResult("answer")
			}
		}
	}
	return nil
}

// ---- the scope guard of RAGIndexing ----

// RAGIndexingScoped wraps a RAGIndexing so that the scope is checked in the
// gateway, whatever the implementation does: an incomplete or malformed scope, a
// source or generation id that is not well formed, and a result that is not about
// what was asked (a generation of another source, a job for another source, a job
// id that is not a job_ id) are refused before the implementation sees them or the
// caller sees them. It returns nil for a nil implementation.
func RAGIndexingScoped(inner RAGIndexing) RAGIndexing {
	if inner == nil {
		return nil
	}
	return ragIndexingScoped{inner: inner}
}

type ragIndexingScoped struct{ inner RAGIndexing }

var _ RAGIndexing = ragIndexingScoped{}

func ragAPICheckSourceID(scope contracts.Scope, sourceID string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if !ids.Valid(sourceID, ragAPIIDSource) {
		return contracts.New(contracts.ErrValidationFailed, "the source id is malformed")
	}
	return nil
}

func ragAPICheckJob(sourceID string, job RAGSyncJob) (RAGSyncJob, error) {
	if job.SourceID != sourceID || !ids.Valid(job.JobID, ragAPIIDJob) {
		return RAGSyncJob{}, contracts.New(contracts.ErrInternal, "the indexing service answered with a job that is not the one asked for")
	}
	return job, nil
}

func (g ragIndexingScoped) Sync(ctx context.Context, scope contracts.Scope, sourceID string) (RAGSyncJob, error) {
	if err := ragAPICheckSourceID(scope, sourceID); err != nil {
		return RAGSyncJob{}, err
	}
	job, err := g.inner.Sync(ctx, scope, sourceID)
	if err != nil {
		return RAGSyncJob{}, err
	}
	return ragAPICheckJob(sourceID, job)
}

func (g ragIndexingScoped) Rebuild(ctx context.Context, scope contracts.Scope, sourceID string) (RAGSyncJob, error) {
	if err := ragAPICheckSourceID(scope, sourceID); err != nil {
		return RAGSyncJob{}, err
	}
	job, err := g.inner.Rebuild(ctx, scope, sourceID)
	if err != nil {
		return RAGSyncJob{}, err
	}
	return ragAPICheckJob(sourceID, job)
}

func (g ragIndexingScoped) Generations(ctx context.Context, scope contracts.Scope, sourceID string) ([]rag.IndexGeneration, error) {
	if err := ragAPICheckSourceID(scope, sourceID); err != nil {
		return nil, err
	}
	gens, err := g.inner.Generations(ctx, scope, sourceID)
	if err != nil {
		return nil, err
	}
	own := make([]rag.IndexGeneration, 0, len(gens))
	for _, gen := range gens {
		if string(gen.SourceID) == sourceID { // another source's generation is never passed on
			own = append(own, gen)
		}
	}
	return own, nil
}

func (g ragIndexingScoped) Invalidate(ctx context.Context, scope contracts.Scope, sourceID, generationID string) (rag.IndexGeneration, error) {
	if err := ragAPICheckSourceID(scope, sourceID); err != nil {
		return rag.IndexGeneration{}, err
	}
	if !ids.Valid(generationID, ragAPIIDGeneration) {
		return rag.IndexGeneration{}, contracts.New(contracts.ErrValidationFailed, "the generation id is malformed")
	}
	gen, err := g.inner.Invalidate(ctx, scope, sourceID, generationID)
	if err != nil {
		return rag.IndexGeneration{}, err
	}
	if string(gen.SourceID) != sourceID || string(gen.GenerationID) != generationID {
		return rag.IndexGeneration{}, contracts.New(contracts.ErrInternal, "the indexing service answered with a generation that is not the one asked for")
	}
	return gen, nil
}

// ---- the implementation of RAGQueries ----

// RAGRetriever is the retrieval pipeline as NewRAGQueries uses it. *query.Pipeline
// implements it.
type RAGRetriever interface {
	Query(ctx context.Context, scope contracts.Scope, req query.Request) (query.Result, error)
	Config() query.Config
}

var _ RAGRetriever = (*query.Pipeline)(nil)

// RAGAnswererBudget is the request budget the text generator of the Answerer is
// wrapped in (providers.WithBudget): the evidence the pipeline may put in a prompt,
// room for the question and the fixed frame, and the output and time the
// rag_answer section allows. Both configurations are validated, so the budget is
// too.
func RAGAnswererBudget(q query.Config, a answer.Config) providers.Budget {
	return providers.Budget{
		MaxPromptTokens: q.MaxToks + 4096,
		MaxOutputTokens: a.MaxOutputToks,
		MaxBatchItems:   1,
		Timeout:         a.GenerateTimeout,
	}
}

// NewRAGQueries is RAGQueries over the retrieval pipeline and the grounded
// answerer. The composition root builds both once per process and provides the
// result with Provide[RAGQueries]:
//
//	answerer, err := answer.New(answer.Deps{
//		Generator: budgetedController, // providers.WithBudget(registry.TextGenerator(providers.RoleController), RAGAnswererBudget(qcfg, acfg))
//		Catalog:   catalog,            // the same query.SourceCatalog the pipeline was built with
//		Clock:     clk,
//	}, acfg)                           // acfg is answer.FromConfig(): the "rag_answer" section
//	queries, err := gateway.NewRAGQueries(pipeline, answerer)
//
// # One Answerer per process
//
// The references an answer hands out live in the Answerer's resolver, so the same
// Answerer must also resolve them: this value does both (Answer and Open), and the
// dialogue engine's knowledge seam is given the same value, so a citation of a
// dialogue turn resolves here too. A second Answerer would issue references no one
// can resolve.
//
// # The rag_answer section
//
//	rag_answer:
//	  max_claims: 12          # claims of one answer (1 to 64); the model may write no more
//	  max_citations: 16       # citations of one answer (1 to 32)
//	  max_output_toks: 768    # tokens the model may generate for the claims
//	  min_coverage: 0.85      # share of a claim's content terms one cited passage window must hold (0.5 to 1)
//	  window_sentences: 2     # consecutive sentences of a passage that may together support a claim
//	  min_claim_terms: 2      # fewest content terms a claim needs to be checked at all
//	  generate_timeout: 30s   # the model call
//	  ref_ttl: 15m            # how long a citation reference resolves
//	  max_refs: 4096          # references remembered, oldest first; a restart forgets them all
//	  preview_runes: 600      # the passage a reference previews; 0 turns previews off
//
// Nothing is clamped: a value outside its range refuses to start.
//
// # Egress
//
// Search and Answer never produce egress_restricted: the data-egress filter of the
// external provider mode acts on the evidence package before Answer, in the
// composition that owns it, and Answer only ever sees what the filter let through.
func NewRAGQueries(r RAGRetriever, a *answer.Answerer) (RAGQueries, error) {
	switch {
	case r == nil:
		return nil, contracts.New(contracts.ErrInternal, "gateway: the RAG queries need a retrieval pipeline")
	case a == nil:
		return nil, contracts.New(contracts.ErrInternal, "gateway: the RAG queries need an answerer: citations could not be resolved")
	}
	return &ragQueries{r: r, a: a}, nil
}

type ragQueries struct {
	r RAGRetriever
	a *answer.Answerer
}

var _ RAGQueries = (*ragQueries)(nil)

// retrieve runs the pipeline for a question in the caller's scope, narrowed to the
// sources the request names (the pipeline intersects them with the readable ones),
// and refuses a package that is not the caller's.
func (q *ragQueries) retrieve(ctx context.Context, scope contracts.Scope, text string, sourceIDs []actions.SourceID, limit int) (rag.EvidencePackage, error) {
	req := query.Request{Text: text, MaxItems: limit}
	for _, id := range sourceIDs {
		req.SourceIDs = append(req.SourceIDs, string(id))
	}
	res, err := q.r.Query(ctx, scope, req)
	if err != nil {
		return rag.EvidencePackage{}, err
	}
	ps := res.Package.Scope
	if string(ps.ApplicationID) != scope.ApplicationID || string(ps.TenantID) != scope.TenantID ||
		string(ps.WorkspaceID) != scope.WorkspaceID || string(ps.PrincipalID) != scope.PrincipalID {
		return rag.EvidencePackage{}, contracts.New(contracts.ErrScopeViolation, "the evidence package was built for another scope")
	}
	return res.Package, nil
}

// Search implements RAGQueries.
func (q *ragQueries) Search(ctx context.Context, scope contracts.Scope, req rag.SearchRequest) (rag.SearchResponse, error) {
	if err := scope.Validate(); err != nil {
		return rag.SearchResponse{}, err
	}
	if err := ragAPICheckSearch(req); err != nil {
		return rag.SearchResponse{}, err
	}
	limit := 0 // the configured package size
	if req.Limit != nil {
		// The evidence budget caps a search whatever is asked: a larger limit is
		// the configured size, never an error.
		limit = int(min(*req.Limit, int64(q.r.Config().MaxItems)))
	}
	pkg, err := q.retrieve(ctx, scope, req.Query, req.SourceIDs, limit)
	if err != nil {
		return rag.SearchResponse{}, err
	}
	return ragAPISearchResponse(pkg), nil
}

// Answer implements RAGQueries.
func (q *ragQueries) Answer(ctx context.Context, scope contracts.Scope, req rag.AnswerRequest) (rag.AnswerResult, error) {
	if err := scope.Validate(); err != nil {
		return rag.AnswerResult{}, err
	}
	if err := ragAPICheckAnswer(req); err != nil {
		return rag.AnswerResult{}, err
	}
	pkg, err := q.retrieve(ctx, scope, string(req.Question), req.SourceIDs, 0)
	if err != nil {
		return rag.AnswerResult{}, err
	}
	res, err := q.a.Answer(ctx, scope, pkg)
	if err != nil {
		return rag.AnswerResult{}, err
	}
	// res.Citations are the internal records with the egress mode and the
	// classification; the answer's own citations are their client projection and
	// are all that is returned.
	return res.Answer, nil
}

// Open implements RAGQueries.
func (q *ragQueries) Open(ctx context.Context, scope contracts.Scope, fileID string) (RAGCitationBody, error) {
	if err := scope.Validate(); err != nil {
		return RAGCitationBody{}, err
	}
	if !ids.Valid(fileID, ragAPIIDReference) {
		return RAGCitationBody{}, contracts.New(contracts.ErrValidationFailed, "the reference is malformed")
	}
	res, err := q.a.Resolver().Resolve(ctx, scope, actions.FileID(fileID))
	if err != nil {
		return RAGCitationBody{}, err
	}
	return RAGCitationBody{
		SourceID: res.SourceID, DisplayName: res.DisplayName, Version: res.Version, Locator: res.Locator,
		Availability: res.Availability, Staleness: res.Staleness, OpenRef: res.Open, Preview: res.Preview,
		ExpiresAt: common.Timestamp(res.ExpiresAt.UTC().Format(ragAPITimestamp)),
	}, nil
}

// ragAPITimestamp is the spelling of a timestamp of the contract: RFC 3339 in UTC,
// milliseconds, a literal Z.
const ragAPITimestamp = "2006-01-02T15:04:05.000Z"

// ragAPISearchResponse is the client projection of an evidence package: each item
// becomes a SearchHit with exactly the members of that type, which has no
// data-egress mode and no classification, so neither can be returned. The text of
// an item is relayed as the excerpt, bounded to the schema's length at a character
// boundary; it is never rewritten.
func ragAPISearchResponse(pkg rag.EvidencePackage) rag.SearchResponse {
	hits := make([]rag.SearchHit, 0, len(pkg.Items))
	for _, it := range pkg.Items {
		excerpt := ragAPIExcerpt(it.Text)
		if excerpt == "" {
			continue
		}
		hits = append(hits, rag.SearchHit{
			EvidenceID: it.EvidenceID, Rank: it.Rank, SourceID: it.SourceID, DisplayName: it.DisplayName, Version: it.Version,
			Locator: ragAPILocator(it.Locator), Availability: it.Availability, Staleness: it.Staleness, OpenRef: it.OpenRef,
			Excerpt: excerpt,
		})
	}
	return rag.SearchResponse{Results: hits, Notices: append(rag.RetrievalNotices{}, pkg.Retrieval.Notices...)}
}

// ragAPIExcerpt bounds a passage to the length of a hit's excerpt.
func ragAPIExcerpt(s string) string {
	if utf8.RuneCountInString(s) <= ragAPIMaxExcerptRunes {
		return s
	}
	return string([]rune(s)[:ragAPIMaxExcerptRunes])
}

// ragAPILocator copies a locator, so a hit shares nothing with the package.
func ragAPILocator(l *conversation.CitationLocator) *conversation.CitationLocator {
	if l == nil {
		return nil
	}
	c := *l
	if l.Page != nil {
		v := *l.Page
		c.Page = &v
	}
	if l.Section != nil {
		v := *l.Section
		c.Section = &v
	}
	if l.Record != nil {
		v := *l.Record
		c.Record = &v
	}
	if l.Span != nil {
		v := *l.Span
		c.Span = &v
	}
	return &c
}
