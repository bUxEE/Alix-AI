package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/rag/answer"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// The RAG area runs the real NewRAGQueries over a scripted retrieval pipeline, the
// real internal/rag/answer.Answerer over the deterministic text provider and a fake
// source catalog; the source registry and the indexing service are fakes that
// apply the rules their real implementations apply (declarations, scope, the
// operator gate). The area's own services are provided here, so the inventory and
// OpenAPI tests build a server with it wired.

const (
	// ragAPIOperator is the one principal the fake deployment's rag.operators
	// lists with the action manage_sources. Everybody else is denied.
	ragAPIOperator = "usr_ops_01"
	ragAPIOtherTen = "ten_initech"
)

// ragAPIDeclared are the sources the fake active package declares.
var ragAPIDeclared = []string{"handbook", "warranty-terms"}

type ragAPIMode int

const (
	ragAPIEverything ragAPIMode = iota
	ragAPISourcesOnly
)

// ragAPIProvideMode says what the next environment provides. Tests that change it
// restore it, and none of them runs in parallel.
var ragAPIProvideMode = ragAPIEverything

type ragAPIFakes struct {
	sources  *ragAPISourcesFake
	retr     *ragAPIRetriever
	model    *pfake.Provider
	catalog  *ragAPICatalog
	answerer *answer.Answerer
	indexing *ragAPIIndexingFake
	queries  RAGQueries
}

var ragAPIFakesByEnv sync.Map

func ragAPIOf(e *apiEnv) *ragAPIFakes {
	e.t.Helper()
	v, ok := ragAPIFakesByEnv.Load(e)
	if !ok {
		e.t.Fatal("the RAG fakes of this environment are missing")
	}
	return v.(*ragAPIFakes)
}

func init() {
	registerTestServices("v1_rag", func(t testing.TB, e *apiEnv, s *Services) {
		f := &ragAPIFakes{
			sources:  ragAPINewSources(ragAPIDeclared...),
			catalog:  ragAPINewCatalog(),
			model:    pfake.New(pfake.WithClock(e.clk)),
			indexing: ragAPINewIndexing(),
		}
		f.sources.operators[ragAPIOperator] = true
		f.indexing.operators[ragAPIOperator] = true
		qcfg, acfg := query.DefaultConfig(), answer.DefaultConfig()
		gen, err := providers.WithBudget(f.model, RAGAnswererBudget(qcfg, acfg))
		if err != nil {
			t.Fatal(err)
		}
		f.answerer, err = answer.New(answer.Deps{Generator: gen, Catalog: f.catalog, Clock: e.clk}, acfg)
		if err != nil {
			t.Fatal(err)
		}
		f.retr = &ragAPIRetriever{cfg: qcfg, catalog: f.catalog}
		if f.queries, err = NewRAGQueries(f.retr, f.answerer); err != nil {
			t.Fatal(err)
		}
		// The registry is provided the way the composition root provides it, wrapped
		// in the scope guard; the area wraps it again, which changes nothing.
		provide := []error{Provide[RAGSources](s, RAGSourcesScoped(f.sources))}
		if ragAPIProvideMode == ragAPIEverything {
			provide = append(provide, Provide[RAGQueries](s, f.queries), Provide[RAGIndexing](s, f.indexing))
		}
		for _, perr := range provide {
			if perr != nil {
				t.Fatal(perr)
			}
		}
		ragAPIFakesByEnv.Store(e, f)
	})
}

// ---- the evidence ----

type ragAPISpec struct {
	source  string
	text    string
	version string
	avail   conversation.CitationAvailability
}

var (
	ragAPISrcMu  sync.Mutex
	ragAPISrcIDs = map[string]string{}
)

// ragAPISourceID is the id of a source key inside one tenant, stable in a run.
func ragAPISourceID(tenant, key string) string {
	ragAPISrcMu.Lock()
	defer ragAPISrcMu.Unlock()
	k := tenant + "/" + key
	if id, ok := ragAPISrcIDs[k]; ok {
		return id
	}
	id := ids.New("src")
	ragAPISrcIDs[k] = id
	return id
}

const (
	ragAPIWarrantyText = "The Model X kettle has a warranty of 24 months from the date of purchase. Keep the receipt as proof of purchase."
	ragAPIReturnsText  = "Returns are accepted within 30 days of delivery."
)

// ragAPIDefaultSpecs are what the scripted pipeline finds for a scope: two sources
// whose display names carry the tenant, so a result of another tenant is visible.
func ragAPIDefaultSpecs(scope contracts.Scope) []ragAPISpec {
	return []ragAPISpec{
		{source: "warranty-terms", text: ragAPIWarrantyText},
		{source: "handbook", text: ragAPIReturnsText},
	}
}

// ragAPIPackage builds the package the query pipeline would build for the scope.
// Every item carries the internal egress mode and classification: the tests of
// the wire format look for them in what comes out.
func ragAPIPackage(scope contracts.Scope, question string, specs ...ragAPISpec) rag.EvidencePackage {
	items := make([]rag.EvidenceItem, len(specs))
	toks := 0
	for i, s := range specs {
		if s.version == "" {
			s.version = "v1"
		}
		if s.avail == "" {
			s.avail = conversation.CitationAvailabilityAvailable
		}
		section := "Section " + strconv.Itoa(i+1)
		loc := conversation.CitationLocator{Section: &section}
		egress, class := rag.SourceEgressLocalOnly, rag.SourceClassificationConfidential
		est := (len(s.text) + 2) / 3
		toks += est
		items[i] = rag.EvidenceItem{
			EvidenceID:           rag.EvidenceID("e" + strconv.Itoa(i+1)),
			Rank:                 int64(i + 1),
			ChunkID:              common.Identifier("chk_" + strconv.Itoa(i+1)),
			GenerationID:         rag.GenerationID(ids.New("gen")),
			SourceID:             actions.SourceID(ragAPISourceID(scope.TenantID, s.source)),
			DisplayName:          s.source + " (" + scope.TenantID + ")",
			Version:              actions.OpaqueVersion(s.version),
			Locator:              &loc,
			Availability:         s.avail,
			Staleness:            conversation.CitationStalenessCurrent,
			OpenRef:              conversation.CitationOpenRef{Value: conversation.CitationOpenRefSource{Kind: "source"}},
			Text:                 s.text,
			EstToks:              rag.EstToks(est),
			SourceEgress:         &egress,
			SourceClassification: &class,
		}
	}
	return rag.EvidencePackage{
		PackageID: rag.EvidencePackageID(ids.New("evp")),
		Scope: rag.EvidencePackageScope{
			ApplicationID: common.Identifier(scope.ApplicationID), TenantID: common.Identifier(scope.TenantID),
			WorkspaceID: common.Identifier(scope.WorkspaceID), PrincipalID: common.Identifier(scope.PrincipalID),
		},
		Query:     rag.SearchRequestQuery(question),
		CreatedAt: common.Timestamp("2026-10-03T09:00:00.000Z"),
		Items:     items,
		Budget:    rag.EvidencePackageBudget{MaxItems: 8, MaxToks: 3000, EstToks: rag.EstToks(toks)},
		Retrieval: rag.EvidencePackageRetrieval{Methods: []rag.RetrievalMethod{rag.RetrievalMethodFts}, Notices: rag.RetrievalNotices{}},
	}
}

// ragAPIAsk is one call of the scripted pipeline.
type ragAPIAsk struct {
	scope contracts.Scope
	req   query.Request
}

// ragAPIRetriever is the retrieval pipeline of the tests: it records what it was
// asked, in whose scope, and answers with the specs it was given.
type ragAPIRetriever struct {
	mu      sync.Mutex
	cfg     query.Config
	catalog *ragAPICatalog
	asked   []ragAPIAsk
	specs   func(contracts.Scope) []ragAPISpec
	notices rag.RetrievalNotices
	err     error
}

func (r *ragAPIRetriever) Config() query.Config { return r.cfg }

func (r *ragAPIRetriever) Query(_ context.Context, scope contracts.Scope, req query.Request) (query.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, ragAPIAsk{scope: scope, req: req})
	if r.err != nil {
		return query.Result{}, r.err
	}
	specs := ragAPIDefaultSpecs(scope)
	if r.specs != nil {
		specs = r.specs(scope)
	}
	// The pipeline's own rules: a source filter narrows to the sources the caller
	// may read (an id it may not read is ignored), a request may lower the size of
	// the package and asking for more than the configuration allows is an error.
	if req.MaxItems > r.cfg.MaxItems {
		return query.Result{}, contracts.New(contracts.ErrValidationFailed, "query: max_items is over the configured size")
	}
	limit := r.cfg.MaxItems
	if req.MaxItems > 0 {
		limit = req.MaxItems
	}
	want := map[string]bool{}
	for _, id := range req.SourceIDs {
		want[id] = true
	}
	var kept []ragAPISpec
	for _, s := range specs {
		if (len(want) == 0 || want[ragAPISourceID(scope.TenantID, s.source)]) && len(kept) < limit {
			kept = append(kept, s)
		}
	}
	specs = kept
	pkg := ragAPIPackage(scope, req.Text, specs...)
	pkg.Retrieval.Notices = append(rag.RetrievalNotices{}, r.notices...)
	r.catalog.grant(scope, pkg)
	return query.Result{Package: pkg}, nil
}

func (r *ragAPIRetriever) calls() []ragAPIAsk {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ragAPIAsk(nil), r.asked...)
}

func (r *ragAPIRetriever) set(f func(contracts.Scope) []ragAPISpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.specs = f
}

// ragAPICatalog is the source catalog of the tests: it applies the scope it is
// handed and can be changed between calls.
type ragAPICatalog struct {
	mu     sync.Mutex
	states map[string]query.SourceState
	err    error
}

func ragAPINewCatalog() *ragAPICatalog { return &ragAPICatalog{states: map[string]query.SourceState{}} }

func (c *ragAPICatalog) grant(scope contracts.Scope, pkg rag.EvidencePackage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, it := range pkg.Items {
		c.states[scope.TenantID+"/"+string(it.SourceID)] = query.SourceState{
			DisplayName: it.DisplayName, Authorised: true, Availability: it.Availability, Staleness: it.Staleness,
		}
	}
}

func (c *ragAPICatalog) revokeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.states = map[string]query.SourceState{}
}

func (c *ragAPICatalog) Sources(_ context.Context, scope contracts.Scope, sourceIDs []string) (map[string]query.SourceState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	out := map[string]query.SourceState{}
	for _, id := range sourceIDs {
		if st, ok := c.states[scope.TenantID+"/"+id]; ok {
			out[id] = st
		}
	}
	return out, nil
}

// ragAPIClaim is a claim as the scripted model writes it.
type ragAPIClaim struct {
	Text string   `json:"text"`
	IDs  []string `json:"evidence_ids"`
}

func (f *ragAPIFakes) reply(claims ...ragAPIClaim) {
	if claims == nil {
		claims = []ragAPIClaim{}
	}
	f.model.Reset()
	f.model.On(pfake.Any()).ReplyJSON(map[string]any{"claims": claims})
}

// ---- the source registry ----

// ragAPISourcesFake is a RAGSources that applies the rules of the real registry:
// only a declaration of the active package can be registered, the id is minted
// here, registering twice is one registration, a source belongs to the scope that
// registered it, and registering, re-auditing and removing are for operators
// only (deny by default).
type ragAPISourcesFake struct {
	mu        sync.Mutex
	declared  map[string]bool
	byID      map[string]*RAGSourcesRecord
	operators map[string]bool
	calls     []string
	statusErr error
}

func ragAPINewSources(declared ...string) *ragAPISourcesFake {
	f := &ragAPISourcesFake{declared: map[string]bool{}, byID: map[string]*RAGSourcesRecord{}, operators: map[string]bool{}}
	for _, d := range declared {
		f.declared[d] = true
	}
	return f
}

var _ RAGSources = (*ragAPISourcesFake)(nil)

func (f *ragAPISourcesFake) authorize(scope contracts.Scope) error {
	if !f.operators[scope.PrincipalID] {
		return contracts.New(contracts.ErrPermissionDenied, "this needs the operator action manage_sources")
	}
	return nil
}

func (f *ragAPISourcesFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.byID)
}

func (f *ragAPISourcesFake) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *ragAPISourcesFake) Register(_ context.Context, scope contracts.Scope, in RAGSourcesRegistration) (RAGSourcesRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "register:"+scope.TenantID+"/"+scope.WorkspaceID+":"+in.PackageSourceID)
	if err := f.authorize(scope); err != nil {
		return RAGSourcesRecord{}, err
	}
	if !f.declared[in.PackageSourceID] {
		return RAGSourcesRecord{}, contracts.New(contracts.ErrValidationFailed, "the application's active package declares no such source").
			WithDetails("reason", "not_declared")
	}
	for _, rec := range f.byID {
		if rec.TenantID == scope.TenantID && rec.WorkspaceID == scope.WorkspaceID && rec.PackageSourceID == in.PackageSourceID {
			return *rec, nil
		}
	}
	id := ids.New("src")
	rec := &RAGSourcesRecord{
		ApplicationID: scope.ApplicationID, TenantID: scope.TenantID, WorkspaceID: scope.WorkspaceID,
		PackageSourceID: in.PackageSourceID, PackageVersion: "1.0.0", Serving: true,
		Status: rag.SourceStatus{
			SourceID: actions.SourceID(id), DisplayName: in.PackageSourceID, State: rag.SourceStatusStateRegistered,
			Availability: conversation.CitationAvailabilityUnavailable, Staleness: conversation.CitationStalenessUnknown,
			// A source that was never scanned has not been reached: the schema asks for
			// the typed reason, and the real registry gives it.
			Error: &common.Error{Code: common.ErrorCodeUnavailable, Message: "the source has not been scanned yet", Retryable: true},
		},
	}
	f.byID[id] = rec
	return *rec, nil
}

func (f *ragAPISourcesFake) Reaudit(_ context.Context, scope contracts.Scope) (RAGSourcesReaudit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "reaudit:"+scope.TenantID)
	if err := f.authorize(scope); err != nil {
		return RAGSourcesReaudit{}, err
	}
	n := 0
	for _, rec := range f.byID {
		if rec.TenantID == scope.TenantID && rec.WorkspaceID == scope.WorkspaceID {
			n++
		}
	}
	return RAGSourcesReaudit{Checked: n, Rebound: n}, nil
}

func (f *ragAPISourcesFake) Remove(_ context.Context, scope contracts.Scope, sourceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "remove:"+scope.TenantID+":"+sourceID)
	if err := f.authorize(scope); err != nil {
		return err
	}
	rec, ok := f.byID[sourceID]
	if !ok || rec.TenantID != scope.TenantID || rec.WorkspaceID != scope.WorkspaceID {
		return ragSourcesNotFound()
	}
	delete(f.byID, sourceID)
	return nil
}

func (f *ragAPISourcesFake) Status(_ context.Context, scope contracts.Scope, sourceID string) ([]RAGSourcesRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "status:"+scope.TenantID+":"+sourceID)
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	var out []RAGSourcesRecord
	for id, rec := range f.byID {
		if rec.TenantID != scope.TenantID || rec.WorkspaceID != scope.WorkspaceID {
			continue
		}
		if sourceID == "" || sourceID == id {
			out = append(out, *rec)
		}
	}
	if sourceID != "" && len(out) == 0 {
		return nil, ragSourcesNotFound()
	}
	ragAPISortRecords(out)
	return out, nil
}

func ragAPISortRecords(recs []RAGSourcesRecord) {
	for i := 1; i < len(recs); i++ {
		for j := i; j > 0 && recs[j].Status.SourceID < recs[j-1].Status.SourceID; j-- {
			recs[j], recs[j-1] = recs[j-1], recs[j]
		}
	}
}

// ---- the indexing service ----

// ragAPIIndexingFake is a RAGIndexing over the sources the test made known: a
// source of another tenant is not found, and everything but listing is for
// operators only.
type ragAPIIndexingFake struct {
	mu        sync.Mutex
	operators map[string]bool
	tenantOf  map[string]string // source id -> tenant
	gens      map[string][]rag.IndexGeneration
	jobs      map[string]string // source id -> job id of the queued sync
	calls     []string
	// answerAs, when set, replaces what Invalidate and the jobs answer with, to
	// prove the gateway refuses an answer that is not about what was asked.
	wrongSource string
}

func ragAPINewIndexing() *ragAPIIndexingFake {
	return &ragAPIIndexingFake{
		operators: map[string]bool{}, tenantOf: map[string]string{}, gens: map[string][]rag.IndexGeneration{}, jobs: map[string]string{},
	}
}

var _ RAGIndexing = (*ragAPIIndexingFake)(nil)

func (f *ragAPIIndexingFake) known(sourceID, tenant string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tenantOf[sourceID] = tenant
}

func (f *ragAPIIndexingFake) check(scope contracts.Scope, sourceID string, operator bool) error {
	if operator && !f.operators[scope.PrincipalID] {
		return contracts.New(contracts.ErrPermissionDenied, "this needs the operator action manage_sources")
	}
	if t, ok := f.tenantOf[sourceID]; !ok || t != scope.TenantID {
		return ragSourcesNotFound()
	}
	return nil
}

func (f *ragAPIIndexingFake) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *ragAPIIndexingFake) job(sourceID string) RAGSyncJob {
	id, ok := f.jobs[sourceID]
	if !ok {
		id = ids.New("job")
		f.jobs[sourceID] = id
	}
	if f.wrongSource != "" {
		sourceID = f.wrongSource
	}
	return RAGSyncJob{SourceID: sourceID, JobID: id}
}

func (f *ragAPIIndexingFake) Sync(_ context.Context, scope contracts.Scope, sourceID string) (RAGSyncJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "sync:"+sourceID)
	if err := f.check(scope, sourceID, true); err != nil {
		return RAGSyncJob{}, err
	}
	return f.job(sourceID), nil
}

func (f *ragAPIIndexingFake) Rebuild(_ context.Context, scope contracts.Scope, sourceID string) (RAGSyncJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "rebuild:"+sourceID)
	if err := f.check(scope, sourceID, true); err != nil {
		return RAGSyncJob{}, err
	}
	return f.job(sourceID), nil
}

func (f *ragAPIIndexingFake) Generations(_ context.Context, scope contracts.Scope, sourceID string) ([]rag.IndexGeneration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "generations:"+sourceID)
	if err := f.check(scope, sourceID, false); err != nil {
		return nil, err
	}
	out := append([]rag.IndexGeneration(nil), f.gens[sourceID]...)
	// A careless implementation that also lists a generation of another source.
	if f.wrongSource != "" {
		out = append(out, ragAPIGeneration(f.wrongSource, rag.IndexGenerationStateActive))
	}
	return out, nil
}

func (f *ragAPIIndexingFake) Invalidate(_ context.Context, scope contracts.Scope, sourceID, generationID string) (rag.IndexGeneration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "invalidate:"+sourceID+":"+generationID)
	if err := f.check(scope, sourceID, true); err != nil {
		return rag.IndexGeneration{}, err
	}
	for i, g := range f.gens[sourceID] {
		if string(g.GenerationID) != generationID {
			continue
		}
		retired := common.Timestamp("2026-10-03T10:00:00.000Z")
		g.State, g.RetiredAt = rag.IndexGenerationStateInvalidated, &retired
		f.gens[sourceID][i] = g
		if f.wrongSource != "" {
			g.SourceID = actions.SourceID(f.wrongSource)
		}
		return g, nil
	}
	return rag.IndexGeneration{}, ragSourcesNotFound()
}

// ragAPIGeneration is an index generation of a source in a state.
func ragAPIGeneration(sourceID string, state rag.IndexGenerationState) rag.IndexGeneration {
	at := common.Timestamp("2026-10-03T08:00:00.000Z")
	g := rag.IndexGeneration{
		GenerationID: rag.GenerationID(ids.New("gen")), SourceID: actions.SourceID(sourceID), State: state,
		Components: rag.IndexGenerationComponents{
			Parser:  rag.ComponentVersion{Name: "alixai-parse", Version: "1"},
			Chunker: rag.ComponentVersion{Name: "alixai-chunk", Version: "1"},
		},
		Counts:    rag.IndexGenerationCounts{Documents: 3, Chunks: 12},
		CreatedAt: at,
	}
	if state == rag.IndexGenerationStateActive {
		g.ActivatedAt = &at
	}
	return g
}

// ---- schemas ----

var (
	ragAPISpecOnce sync.Once
	ragAPISpecComp *jsonschema.Compiler
	ragAPISpecErr  error
)

// ragAPICompiler compiles the published schemas of every area the RAG area refers
// to, and the components of the OpenAPI document.
func ragAPICompiler(t testing.TB) *jsonschema.Compiler {
	t.Helper()
	ragAPISpecOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		for _, area := range []string{"common", "actions", "conversation", "memory", "analyst", "rag"} {
			err := filepath.WalkDir(filepath.Join(apiSchemaRoot, area), func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() || !strings.HasSuffix(path, ".schema.json") {
					return err
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
				if err != nil {
					return err
				}
				return c.AddResource(doc.(map[string]any)["$id"].(string), doc)
			})
			if err != nil {
				ragAPISpecErr = err
				return
			}
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(mergedComponents(t)))
		if err != nil {
			ragAPISpecErr = err
			return
		}
		if err := c.AddResource("urn:alixai:test:ragapi", doc); err != nil {
			ragAPISpecErr = err
			return
		}
		ragAPISpecComp = c
	})
	if ragAPISpecErr != nil || ragAPISpecComp == nil {
		t.Fatalf("the schemas of the RAG area cannot be compiled: %v", ragAPISpecErr)
	}
	return ragAPISpecComp
}

// ragAPIValidate checks a response body against a named schema of the OpenAPI
// document, so the document and the handlers cannot drift apart.
func ragAPIValidate(t testing.TB, component string, raw []byte) {
	t.Helper()
	s, err := ragAPICompiler(t).Compile("urn:alixai:test:ragapi#/components/schemas/" + component)
	if err != nil {
		t.Fatalf("compile %s: %v", component, err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("%s: %v: %s", component, err, raw)
	}
	if err := s.Validate(inst); err != nil {
		t.Fatalf("the response does not match the %s schema of the OpenAPI document: %v\n%s", component, err, raw)
	}
}

// ---- requests ----

func (e *apiEnv) ragAPIPost(path string, body any, mods ...claims) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.call(http.MethodPost, "/v1/rag"+path, body, mods...)
}

func (e *apiEnv) ragAPIGet(path string, mods ...claims) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.call(http.MethodGet, "/v1/rag"+path, nil, mods...)
}

func ragAPIJSON(t testing.TB, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("not a JSON object: %v: %s", err, rec.Body.String())
	}
	return v
}

// ragAPIOperatorOf is the claim that makes the caller the deployment's operator.
func ragAPIOperatorOf() claims { return withPrincipal(ragAPIOperator) }

// ragAPIMustRegister registers a declaration as the operator of a tenant and
// returns the source's id.
func ragAPIMustRegister(e *apiEnv, declaration string, mods ...claims) string {
	e.t.Helper()
	rec := e.ragAPIPost("/sources", map[string]any{"declaration": declaration}, append([]claims{ragAPIOperatorOf()}, mods...)...)
	wantStatus(e.t, rec, http.StatusOK)
	ragAPIValidate(e.t, "RagSource", rec.Body.Bytes())
	status, _ := ragAPIJSON(e.t, rec)["status"].(map[string]any)
	id, _ := status["source_id"].(string)
	if !ids.Valid(id, "src") {
		e.t.Fatalf("the source id %q is not a src_ id", id)
	}
	return id
}

// ---- search ----

func TestRAGSearchRequiresScope(t *testing.T) {
	e := newAPIEnv(t)
	f := ragAPIOf(e)
	body := map[string]any{"query": "kettle warranty"}

	t.Run("no identity reaches nothing", func(t *testing.T) {
		stranger := identitytest.NewIssuer(t, identitytest.IssuerConfig{ApplicationID: apiApp, Clock: e.clk, KeyID: "stranger-key"})
		for name, tok := range map[string]string{"no token": "", "malformed": "x.y.z", "stranger": stranger.Token()} {
			rec := e.callAs(tok, http.MethodPost, "/v1/rag/search", body)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s: status = %d, want 401: %s", name, rec.Code, rec.Body.String())
			}
		}
		if n := len(f.retr.calls()); n != 0 {
			t.Fatalf("an unauthenticated search reached the pipeline %d times", n)
		}
	})

	t.Run("the body can only narrow: it cannot name a scope", func(t *testing.T) {
		for _, member := range []string{"tenant_id", "workspace_id", "application_id", "principal_id", "session_id", "scope", "roles", "deployment_id"} {
			rec := e.ragAPIPost("/search", map[string]any{"query": "kettle warranty", member: ragAPIOtherTen})
			wantError(t, rec, http.StatusBadRequest, contracts.ErrValidationFailed)
		}
		if n := len(f.retr.calls()); n != 0 {
			t.Fatalf("a body that named a scope reached the pipeline %d times", n)
		}
	})

	t.Run("the scope is the token's", func(t *testing.T) {
		rec := e.ragAPIPost("/search", body)
		wantStatus(t, rec, http.StatusOK)
		ragAPIValidate(t, "RagSearchResponse", rec.Body.Bytes())
		calls := f.retr.calls()
		if len(calls) != 1 {
			t.Fatalf("the pipeline was called %d times", len(calls))
		}
		if want := e.scopeOf(); !reflect.DeepEqual(calls[0].scope, want) {
			t.Fatalf("the pipeline ran under %+v, want exactly the token's scope %+v", calls[0].scope, want)
		}
		if calls[0].req.Text != "kettle warranty" || len(calls[0].req.SourceIDs) != 0 {
			t.Fatalf("request = %+v", calls[0].req)
		}
		results, _ := ragAPIJSON(t, rec)["results"].([]any)
		if len(results) != 2 {
			t.Fatalf("results = %d, want 2: %s", len(results), rec.Body.String())
		}
		for _, r := range results {
			if name, _ := r.(map[string]any)["display_name"].(string); !strings.Contains(name, apiTenant) {
				t.Errorf("a hit of display name %q is not of the caller's tenant", name)
			}
		}
	})

	t.Run("another tenant's token searches another tenant's world", func(t *testing.T) {
		rec := e.ragAPIPost("/search", body, withTenant(apiOther))
		wantStatus(t, rec, http.StatusOK)
		calls := f.retr.calls()
		if got := calls[len(calls)-1].scope.TenantID; got != apiOther {
			t.Fatalf("the pipeline ran for tenant %q, want %q", got, apiOther)
		}
		results, _ := ragAPIJSON(t, rec)["results"].([]any)
		for _, r := range results {
			name, _ := r.(map[string]any)["display_name"].(string)
			if strings.Contains(name, apiTenant) || !strings.Contains(name, apiOther) {
				t.Errorf("a hit of display name %q is not of tenant %s", name, apiOther)
			}
		}
	})

	t.Run("source_ids only narrow, and the limit is capped by the evidence budget", func(t *testing.T) {
		before := len(f.retr.calls())
		// A source of another tenant: the pipeline ignores an id the caller may not
		// read exactly like one that does not exist, so the search finds nothing and
		// does not say whether the source exists.
		foreign := ragAPISourceID(apiOther, "warranty-terms")
		rec := e.ragAPIPost("/search", map[string]any{"query": "kettle", "source_ids": []string{foreign}, "limit": 50})
		wantStatus(t, rec, http.StatusOK)
		calls := f.retr.calls()
		if len(calls) != before+1 {
			t.Fatalf("calls = %d", len(calls))
		}
		last := calls[len(calls)-1]
		if !reflect.DeepEqual(last.req.SourceIDs, []string{foreign}) || last.req.MaxItems != f.retr.cfg.MaxItems {
			t.Fatalf("request = %+v, want the source filter and max_items %d", last.req, f.retr.cfg.MaxItems)
		}
		if last.scope.TenantID != apiTenant {
			t.Fatalf("the scope changed to tenant %q", last.scope.TenantID)
		}
		if results, _ := ragAPIJSON(t, rec)["results"].([]any); len(results) != 0 {
			t.Fatalf("a source of another tenant was searched: %s", rec.Body.String())
		}
		// A narrower limit is passed on as asked, and an own source is found.
		own := ragAPISourceID(apiTenant, "handbook")
		rec = e.ragAPIPost("/search", map[string]any{"query": "returns", "source_ids": []string{own}, "limit": 1})
		wantStatus(t, rec, http.StatusOK)
		results, _ := ragAPIJSON(t, rec)["results"].([]any)
		if len(results) != 1 || results[0].(map[string]any)["source_id"] != own {
			t.Fatalf("search = %s", rec.Body.String())
		}
		calls = f.retr.calls()
		if got := calls[len(calls)-1].req.MaxItems; got != 1 {
			t.Fatalf("max_items = %d, want the asked limit 1", got)
		}
	})

	t.Run("requests outside the schema never reach the pipeline", func(t *testing.T) {
		before := len(f.retr.calls())
		src := ids.New("src")
		for name, b := range map[string]any{
			"empty":         map[string]any{"query": ""},
			"blank":         map[string]any{"query": "  \t "},
			"missing":       map[string]any{},
			"too long":      map[string]any{"query": strings.Repeat("a", 2001)},
			"wrong type":    map[string]any{"query": 7},
			"33 sources":    map[string]any{"query": "x", "source_ids": ragAPIManyIDs(33)},
			"bad source id": map[string]any{"query": "x", "source_ids": []string{"src_nope"}},
			"repeated id":   map[string]any{"query": "x", "source_ids": []string{src, src}},
			"limit zero":    map[string]any{"query": "x", "limit": 0},
			"limit 51":      map[string]any{"query": "x", "limit": 51},
			"NUL":           map[string]any{"query": "a\u0000b"},
		} {
			rec := e.ragAPIPost("/search", b)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s: status = %d, want 400: %s", name, rec.Code, rec.Body.String())
			}
		}
		if n := len(f.retr.calls()); n != before {
			t.Fatalf("%d invalid requests reached the pipeline", n-before)
		}
	})

	t.Run("a failed search is a typed error, never an empty result", func(t *testing.T) {
		f.retr.mu.Lock()
		f.retr.err = contracts.New(contracts.ErrTimeout, "the full-text stage ran out of time")
		f.retr.mu.Unlock()
		defer func() {
			f.retr.mu.Lock()
			f.retr.err = nil
			f.retr.mu.Unlock()
		}()
		env := wantError(t, e.ragAPIPost("/search", body), http.StatusGatewayTimeout, contracts.ErrTimeout)
		if !env.Retryable {
			t.Error("a timeout is not marked retryable")
		}
	})

	t.Run("an application that is not active serves no search", func(t *testing.T) {
		before := len(f.retr.calls())
		e.fapps.quarantine(true)
		defer e.fapps.quarantine(false)
		wantError(t, e.ragAPIPost("/search", body), http.StatusForbidden, contracts.ErrQuarantined)
		if len(f.retr.calls()) != before {
			t.Fatal("a quarantined application reached the pipeline")
		}
	})
}

func ragAPIManyIDs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = ids.New("src")
	}
	return out
}

// ---- answers ----

func TestAnswerEndpointReturnsCitations(t *testing.T) {
	e := newAPIEnv(t)
	f := ragAPIOf(e)
	const question = "How long is the warranty on the Model X kettle?"
	supported := ragAPIClaim{Text: ragAPIWarrantyText, IDs: []string{"e1"}}
	f.reply(supported)

	rec := e.ragAPIPost("/answer", map[string]any{"question": question})
	wantStatus(t, rec, http.StatusOK)
	raw := rec.Body.Bytes()
	ragAPIValidate(t, "RagAnswerResult", raw)
	validateAgainst(t, "urn:alixai:schema:conversation:citation:v1", ragAPIMustJSON(t, ragAPIJSON(t, rec)["citations"].([]any)[0]))

	var ans rag.AnswerResult
	if err := json.Unmarshal(raw, &ans); err != nil {
		t.Fatalf("the answer does not decode: %v: %s", err, raw)
	}
	ga, ok := ans.Value.(rag.GroundedAnswer)
	if !ok {
		t.Fatalf("outcome = %T, want a grounded answer: %s", ans.Value, raw)
	}
	if len(ga.Claims) != 1 || ga.Claims[0].Text != supported.Text {
		t.Fatalf("claims = %+v", ga.Claims)
	}
	if len(ga.Citations) == 0 {
		t.Fatal("an answer without a citation")
	}
	wantSource := ragAPISourceID(apiTenant, "warranty-terms")
	for i, c := range ga.Citations {
		if string(c.SourceID) != wantSource || len(c.Claims) == 0 || c.DisplayName == "" || c.Version == "" {
			t.Errorf("citation %d = %+v", i, c)
		}
		file, isFile := c.OpenRef.Value.(conversation.CitationOpenRefFile)
		if !isFile || !ids.Valid(string(file.FileID), "fil") {
			t.Errorf("citation %d open_ref = %#v, want a fil_ file reference", i, c.OpenRef.Value)
		}
	}
	for _, cl := range ga.Claims {
		if len(cl.CitationIndexes) == 0 {
			t.Errorf("claim %q has no citation", cl.Text)
		}
	}

	// The internal metadata of a source never reaches a client, and neither does
	// anything else of the evidence package.
	for _, banned := range []string{"source_egress", "source_classification", "local_only", "confidential", "chunk_id", "generation_id", "est_toks", "package_id"} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("the answer carries %q: %s", banned, raw)
		}
	}

	// The question ran in the token's scope, and the model never saw an identifier.
	calls := f.retr.calls()
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].scope, e.scopeOf()) || calls[0].req.Text != question {
		t.Fatalf("pipeline calls = %+v", calls)
	}
	for _, p := range f.model.Prompts() {
		for _, s := range p.Texts() {
			for _, secret := range []string{apiTenant, apiWsp, wantSource, "usr_alice", apiSess} {
				if strings.Contains(s, secret) {
					t.Errorf("the model was shown %q", secret)
				}
			}
		}
	}

	t.Run("the reference of a citation resolves for its principal only", func(t *testing.T) {
		file := ga.Citations[0].OpenRef.Value.(conversation.CitationOpenRefFile)
		path := "/citations/" + string(file.FileID)
		res := e.ragAPIGet(path)
		wantStatus(t, res, http.StatusOK)
		ragAPIValidate(t, "RagCitationResolution", res.Body.Bytes())
		body := ragAPIJSON(t, res)
		if body["source_id"] != wantSource || body["availability"] != "available" || body["preview"] == "" {
			t.Fatalf("resolution = %s", res.Body.String())
		}
		if open, _ := body["open_ref"].(map[string]any); open["kind"] != "source" {
			t.Fatalf("open_ref = %v, want the source's own reference", body["open_ref"])
		}
		// Another principal, another tenant and an id that never existed are one
		// and the same answer.
		unknown := e.ragAPIGet("/citations/" + ids.New("fil"))
		denied := wantError(t, unknown, http.StatusForbidden, contracts.ErrPermissionDenied)
		for name, mod := range map[string]claims{"principal": withPrincipal("usr_mallory"), "tenant": withTenant(apiOther)} {
			got := wantError(t, e.ragAPIGet(path, mod), http.StatusForbidden, contracts.ErrPermissionDenied)
			if got.Message != denied.Message {
				t.Errorf("another %s learns more than a stranger does: %q vs %q", name, got.Message, denied.Message)
			}
		}
		// It is not a file of the files service.
		wantError(t, e.call(http.MethodGet, "/v1/files/"+string(file.FileID), nil), http.StatusForbidden, contracts.ErrScopeViolation)
		// A malformed reference never reaches the resolver.
		wantError(t, e.ragAPIGet("/citations/fil_nope"), http.StatusBadRequest, contracts.ErrValidationFailed)
		// A source the principal may no longer read is refused, however recently
		// the reference was issued.
		f.catalog.revokeAll()
		wantError(t, e.ragAPIGet(path), http.StatusForbidden, contracts.ErrPermissionDenied)
	})

	t.Run("an expired reference is 410", func(t *testing.T) {
		f.reply(supported)
		res := e.ragAPIPost("/answer", map[string]any{"question": question})
		wantStatus(t, res, http.StatusOK)
		var again rag.AnswerResult
		if err := json.Unmarshal(res.Body.Bytes(), &again); err != nil {
			t.Fatal(err)
		}
		file := again.Value.(rag.GroundedAnswer).Citations[0].OpenRef.Value.(conversation.CitationOpenRefFile)
		e.clk.Advance(answer.DefaultRefTTL + time.Minute)
		wantError(t, e.ragAPIGet("/citations/"+string(file.FileID)), http.StatusGone, contracts.ErrExpired)
	})

	t.Run("nothing authorised matches: the typed result, a 200, and the model is not asked", func(t *testing.T) {
		f.model.Reset()
		f.retr.set(func(contracts.Scope) []ragAPISpec { return nil })
		defer f.retr.set(nil)
		res := e.ragAPIPost("/answer", map[string]any{"question": "what is the airspeed of a swallow"})
		wantStatus(t, res, http.StatusOK)
		ragAPIValidate(t, "RagAnswerResult", res.Body.Bytes())
		body := ragAPIJSON(t, res)
		if body["outcome"] != "no_supported_evidence" || body["reason"] != "no_match" {
			t.Fatalf("answer = %s", res.Body.String())
		}
		if _, has := body["citations"]; has {
			t.Fatal("a no_supported_evidence result carries citations")
		}
		if f.model.Calls() != 0 {
			t.Fatalf("the model was called %d times for an empty package", f.model.Calls())
		}
	})

	t.Run("a claim no passage supports is dropped, and with none left the answer is no_supported_evidence", func(t *testing.T) {
		f.reply(ragAPIClaim{Text: "The Model X kettle has a lifetime warranty of 99 years.", IDs: []string{"e1"}})
		res := e.ragAPIPost("/answer", map[string]any{"question": question})
		wantStatus(t, res, http.StatusOK)
		body := ragAPIJSON(t, res)
		if body["outcome"] != "no_supported_evidence" || body["reason"] != "claims_unsupported" {
			t.Fatalf("answer = %s", res.Body.String())
		}
	})

	t.Run("a model that fails is an error, never an answer and never no evidence", func(t *testing.T) {
		f.model.Reset()
		f.model.FailTimes(pfake.FailUnavailable, 1)
		env := wantError(t, e.ragAPIPost("/answer", map[string]any{"question": question}), http.StatusServiceUnavailable, contracts.ErrUnavailable)
		if !env.Retryable {
			t.Error("an unavailable model is not marked retryable")
		}
	})

	t.Run("source_ids narrow and requests outside the schema never reach the pipeline", func(t *testing.T) {
		f.reply(supported)
		before := len(f.retr.calls())
		src := ragAPISourceID(apiTenant, "warranty-terms")
		wantStatus(t, e.ragAPIPost("/answer", map[string]any{"question": question, "source_ids": []string{src}}), http.StatusOK)
		calls := f.retr.calls()
		if len(calls) != before+1 || !reflect.DeepEqual(calls[before].req.SourceIDs, []string{src}) {
			t.Fatalf("calls = %+v", calls[before:])
		}
		for name, b := range map[string]any{
			"missing":      map[string]any{},
			"empty":        map[string]any{"question": ""},
			"a tenant":     map[string]any{"question": question, "tenant_id": apiOther},
			"a principal":  map[string]any{"question": question, "principal_id": "usr_mallory"},
			"unknown":      map[string]any{"question": question, "limit": 3},
			"33 sources":   map[string]any{"question": question, "source_ids": ragAPIManyIDs(33)},
			"a bad source": map[string]any{"question": question, "source_ids": []string{"nope"}},
		} {
			if rec := e.ragAPIPost("/answer", b); rec.Code != http.StatusBadRequest {
				t.Errorf("%s: status = %d, want 400: %s", name, rec.Code, rec.Body.String())
			}
		}
		if len(f.retr.calls()) != before+1 {
			t.Fatal("an invalid request reached the pipeline")
		}
	})
}

func ragAPIMustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A search carries the same client projection: no member of the internal record
// that holds an egress mode or a classification can appear, and a degraded
// search says so.
func TestRAGSearchHitsAreTheClientProjection(t *testing.T) {
	e := newAPIEnv(t)
	f := ragAPIOf(e)
	f.retr.notices = rag.RetrievalNotices{rag.RetrievalNoticeReducedSemanticRecall}
	rec := e.ragAPIPost("/search", map[string]any{"query": "returns"})
	wantStatus(t, rec, http.StatusOK)
	ragAPIValidate(t, "RagSearchResponse", rec.Body.Bytes())
	body := ragAPIJSON(t, rec)
	results, _ := body["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results = %d: %s", len(results), rec.Body.String())
	}
	allowed := map[string]bool{
		"evidence_id": true, "rank": true, "source_id": true, "display_name": true, "version": true, "locator": true,
		"availability": true, "staleness": true, "open_ref": true, "excerpt": true,
	}
	for i, r := range results {
		hit := r.(map[string]any)
		for member := range hit {
			if !allowed[member] {
				t.Errorf("hit %d carries %q, which is not a member of SearchHit", i, member)
			}
		}
		if hit["rank"] != float64(i+1) || hit["excerpt"] == "" {
			t.Errorf("hit %d = %v", i, hit)
		}
	}
	for _, banned := range []string{"source_egress", "source_classification", "local_only", "confidential"} {
		if strings.Contains(rec.Body.String(), banned) {
			t.Errorf("the search result carries %q: %s", banned, rec.Body.String())
		}
	}
	if notices, _ := body["notices"].([]any); len(notices) != 1 || notices[0] != "reduced_semantic_recall" {
		t.Errorf("notices = %v: a degraded search must say so", body["notices"])
	}
	// An excerpt is bounded to the schema's length at a character boundary and is
	// otherwise the passage as it is.
	f.retr.set(func(contracts.Scope) []ragAPISpec {
		return []ragAPISpec{{source: "handbook", text: strings.Repeat("é", ragAPIMaxExcerptRunes+10)}}
	})
	rec = e.ragAPIPost("/search", map[string]any{"query": "long"})
	wantStatus(t, rec, http.StatusOK)
	ragAPIValidate(t, "RagSearchResponse", rec.Body.Bytes())
	hit := ragAPIJSON(t, rec)["results"].([]any)[0].(map[string]any)
	if got := []rune(hit["excerpt"].(string)); len(got) != ragAPIMaxExcerptRunes || string(got) != strings.Repeat("é", ragAPIMaxExcerptRunes) {
		t.Errorf("excerpt = %d characters", len(got))
	}
	// A search that matched nothing is a successful empty result.
	f.retr.set(func(contracts.Scope) []ragAPISpec { return nil })
	rec = e.ragAPIPost("/search", map[string]any{"query": "nothing here"})
	wantStatus(t, rec, http.StatusOK)
	ragAPIValidate(t, "RagSearchResponse", rec.Body.Bytes())
	if got := ragAPIJSON(t, rec)["results"]; !reflect.DeepEqual(got, []any{}) {
		t.Errorf("results = %#v, want an empty array", got)
	}
}

// ---- sources ----

// SEC-054: a client can register only a source the active package declares.
func TestSourceRegistrationRequiresPackageDeclaration(t *testing.T) {
	e := newAPIEnv(t)
	f := ragAPIOf(e)

	t.Run("a declared source is registered and its id is minted by the core", func(t *testing.T) {
		rec := e.ragAPIPost("/sources", map[string]any{"declaration": "handbook"}, ragAPIOperatorOf())
		wantStatus(t, rec, http.StatusOK)
		ragAPIValidate(t, "RagSource", rec.Body.Bytes())
		body := ragAPIJSON(t, rec)
		status := body["status"].(map[string]any)
		id, _ := status["source_id"].(string)
		if !ids.Valid(id, "src") || body["declaration"] != "handbook" || body["serving"] != true {
			t.Fatalf("registration = %s", rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/v1/rag/sources/"+id {
			t.Errorf("Location = %q", loc)
		}
		// Registering it again is the same registration, not a second source.
		again := e.ragAPIPost("/sources", map[string]any{"declaration": "handbook"}, ragAPIOperatorOf())
		wantStatus(t, again, http.StatusOK)
		if got := ragAPIJSON(t, again)["status"].(map[string]any)["source_id"]; got != id {
			t.Fatalf("the second registration made source %v, want %s", got, id)
		}
		if f.sources.count() != 1 {
			t.Fatalf("the registry holds %d sources", f.sources.count())
		}
	})

	t.Run("a name the package does not declare creates nothing", func(t *testing.T) {
		before := f.sources.count()
		for _, name := range []string{"payroll", "../etc", "handbook ", "HANDBOOK", "hand\nbook", strings.Repeat("a", ragSourcesMaxName+1)} {
			rec := e.ragAPIPost("/sources", map[string]any{"declaration": name}, ragAPIOperatorOf())
			wantError(t, rec, http.StatusBadRequest, contracts.ErrValidationFailed)
		}
		rec := e.ragAPIPost("/sources", map[string]any{"declaration": "payroll"}, ragAPIOperatorOf())
		env := wantError(t, rec, http.StatusBadRequest, contracts.ErrValidationFailed)
		if env.Details["reason"] != "not_declared" {
			t.Errorf("details = %v, want reason not_declared", env.Details)
		}
		if f.sources.count() != before {
			t.Fatalf("an undeclared source was registered: %d sources", f.sources.count())
		}
	})

	t.Run("the body can name nothing but the declaration", func(t *testing.T) {
		calls := len(f.sources.called())
		for name, b := range map[string]any{
			"a root":          map[string]any{"declaration": "handbook", "root": "/etc"},
			"a path":          map[string]any{"declaration": "handbook", "path": "../../"},
			"a tenant":        map[string]any{"declaration": "handbook", "tenant_id": apiOther},
			"a workspace":     map[string]any{"declaration": "handbook", "workspace_id": apiWsp2},
			"an id":           map[string]any{"declaration": "handbook", "source_id": ids.New("src")},
			"a free-form one": map[string]any{"type": "filesystem", "root": "/", "include": []string{"**"}},
			"no declaration":  map[string]any{},
			"a number":        map[string]any{"declaration": 7},
			"a null":          map[string]any{"declaration": nil},
			"an empty name":   map[string]any{"declaration": ""},
		} {
			rec := e.ragAPIPost("/sources", b, ragAPIOperatorOf())
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s: status = %d, want 400: %s", name, rec.Code, rec.Body.String())
			}
		}
		if got := len(f.sources.called()); got != calls {
			t.Fatalf("%d malformed registrations reached the registry", got-calls)
		}
		// There is no route that takes a source definition.
		for _, path := range []string{"/v1/rag/sources/definitions", "/v1/rag/connectors", "/v1/rag/sources/import"} {
			if rec := e.call(http.MethodPost, path, map[string]any{"declaration": "handbook"}, ragAPIOperatorOf()); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("POST %s = %d, want 404", path, rec.Code)
			}
		}
	})

	t.Run("only the deployment's operators register, re-audit or remove: roles and scopes are never read", func(t *testing.T) {
		before := f.sources.count()
		// The default caller holds every role and capability scope the token can
		// carry, and the deployment lists it nowhere.
		powerful := []claims{withRoles("admin", "owner", "operator"), withScopes("rag.operator", "rag.manage_sources", "manage_sources")}
		wantError(t, e.ragAPIPost("/sources", map[string]any{"declaration": "warranty-terms"}, powerful...), http.StatusForbidden, contracts.ErrPermissionDenied)
		wantError(t, e.ragAPIPost("/sources/reaudit", nil, powerful...), http.StatusForbidden, contracts.ErrPermissionDenied)
		id := ragAPIMustRegister(e, "warranty-terms")
		wantError(t, e.call(http.MethodDelete, "/v1/rag/sources/"+id, nil, powerful...), http.StatusForbidden, contracts.ErrPermissionDenied)
		if f.sources.count() != before+1 {
			t.Fatalf("a caller the deployment does not list changed the registry: %d sources", f.sources.count())
		}
		// Reading a status needs the verified scope and nothing more.
		wantStatus(t, e.ragAPIGet("/sources/"+id), http.StatusOK)
		wantStatus(t, e.ragAPIGet("/sources"), http.StatusOK)
	})

	t.Run("sources belong to the scope that registered them", func(t *testing.T) {
		a := ragAPIMustRegister(e, "handbook", withTenant("ten_umbrella"))
		b := ragAPIMustRegister(e, "handbook", withTenant("ten_hooli"))
		if a == b {
			t.Fatal("two tenants share one source id")
		}
		// A source of another tenant is reported exactly like one that does not exist.
		missing := e.ragAPIGet("/sources/" + ids.New("src"))
		wantErr := wantError(t, missing, http.StatusNotFound, contracts.ErrValidationFailed)
		foreign := e.ragAPIGet("/sources/"+a, withTenant("ten_hooli"))
		got := wantError(t, foreign, http.StatusNotFound, contracts.ErrValidationFailed)
		if got.Message != wantErr.Message || got.Details["reason"] != "not_found" {
			t.Errorf("a source of another tenant is told apart from one that does not exist: %q vs %q", got.Message, wantErr.Message)
		}
		wantError(t, e.call(http.MethodDelete, "/v1/rag/sources/"+a, nil, ragAPIOperatorOf(), withTenant("ten_hooli")), http.StatusNotFound, contracts.ErrValidationFailed)
		list := ragAPIJSON(t, e.ragAPIGet("/sources", withTenant("ten_umbrella")))["sources"].([]any)
		if len(list) != 1 || list[0].(map[string]any)["status"].(map[string]any)["source_id"] != a {
			t.Fatalf("tenant ten_umbrella lists %v", list)
		}
		// Another workspace of the same tenant is another scope too.
		if rec := e.ragAPIGet("/sources/"+a, withTenant("ten_umbrella"), withWorkspace(apiWsp2)); rec.Code != http.StatusNotFound {
			t.Errorf("a source of another workspace answered %d", rec.Code)
		}
	})

	t.Run("the registry's answers keep their statuses", func(t *testing.T) {
		id := ragAPIMustRegister(e, "handbook")
		list := e.ragAPIGet("/sources")
		wantStatus(t, list, http.StatusOK)
		ragAPIValidate(t, "RagSourceList", list.Body.Bytes())
		re := e.ragAPIPost("/sources/reaudit", nil, ragAPIOperatorOf())
		wantStatus(t, re, http.StatusOK)
		ragAPIValidate(t, "RagReaudit", re.Body.Bytes())
		wantStatus(t, e.ragAPIPost("/sources/reaudit", map[string]any{}, ragAPIOperatorOf()), http.StatusOK)
		wantError(t, e.ragAPIPost("/sources/reaudit", map[string]any{"tenant_id": apiOther}, ragAPIOperatorOf()), http.StatusBadRequest, contracts.ErrValidationFailed)
		wantError(t, e.ragAPIGet("/sources/src_nope"), http.StatusBadRequest, contracts.ErrValidationFailed)
		wantError(t, e.call(http.MethodDelete, "/v1/rag/sources/not-an-id", nil, ragAPIOperatorOf()), http.StatusBadRequest, contracts.ErrValidationFailed)
		// A registration read back after a restart and not yet re-bound answers a
		// retryable unavailable (503): not 404 and not 410.
		f.sources.mu.Lock()
		f.sources.statusErr = contracts.New(contracts.ErrUnavailable, "the registration has not been re-bound to the package yet")
		f.sources.mu.Unlock()
		for _, path := range []string{"/sources", "/sources/" + id} {
			env := wantError(t, e.ragAPIGet(path), http.StatusServiceUnavailable, contracts.ErrUnavailable)
			if !env.Retryable {
				t.Errorf("GET %s: the 503 is not retryable", path)
			}
		}
		f.sources.mu.Lock()
		f.sources.statusErr = nil
		f.sources.mu.Unlock()
		// A source whose removal began is a conflict until it is finished.
		// Removing is a 204 and the source is gone.
		rm := e.call(http.MethodDelete, "/v1/rag/sources/"+id, nil, ragAPIOperatorOf())
		wantStatus(t, rm, http.StatusNoContent)
		if rm.Body.Len() != 0 {
			t.Errorf("a 204 with a body: %q", rm.Body.String())
		}
		wantError(t, e.ragAPIGet("/sources/"+id), http.StatusNotFound, contracts.ErrValidationFailed)
	})

	t.Run("an application that is not active serves no source traffic", func(t *testing.T) {
		calls := len(f.sources.called())
		e.fapps.quarantine(true)
		defer e.fapps.quarantine(false)
		wantError(t, e.ragAPIPost("/sources", map[string]any{"declaration": "handbook"}, ragAPIOperatorOf()), http.StatusForbidden, contracts.ErrQuarantined)
		wantError(t, e.ragAPIGet("/sources"), http.StatusForbidden, contracts.ErrQuarantined)
		if len(f.sources.called()) != calls {
			t.Fatal("a quarantined application reached the registry")
		}
	})
}

// ---- indexing ----

func TestRAGIndexingRoutes(t *testing.T) {
	e := newAPIEnv(t)
	f := ragAPIOf(e)
	id := ragAPIMustRegister(e, "handbook")
	f.indexing.known(id, apiTenant)
	active, building := ragAPIGeneration(id, rag.IndexGenerationStateActive), ragAPIGeneration(id, rag.IndexGenerationStateBuilding)
	f.indexing.mu.Lock()
	f.indexing.gens[id] = []rag.IndexGeneration{building, active}
	f.indexing.mu.Unlock()
	base := "/sources/" + id

	t.Run("a sync is a queued job, and the same job while it waits", func(t *testing.T) {
		rec := e.ragAPIPost(base+"/sync", nil, ragAPIOperatorOf())
		wantStatus(t, rec, http.StatusAccepted)
		ragAPIValidate(t, "RagSyncAccepted", rec.Body.Bytes())
		body := ragAPIJSON(t, rec)
		if body["source_id"] != id || !ids.Valid(body["job_id"].(string), "job") {
			t.Fatalf("sync = %s", rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/v1/rag/sources/"+id {
			t.Errorf("Location = %q: progress is read from the source's status", loc)
		}
		again := ragAPIJSON(t, e.ragAPIPost(base+"/sync", map[string]any{}, ragAPIOperatorOf()))
		if again["job_id"] != body["job_id"] {
			t.Errorf("a second request made job %v, want %v", again["job_id"], body["job_id"])
		}
		rb := e.ragAPIPost(base+"/rebuild", nil, ragAPIOperatorOf())
		wantStatus(t, rb, http.StatusAccepted)
		ragAPIValidate(t, "RagSyncAccepted", rb.Body.Bytes())
	})

	t.Run("generations are listed to the caller's scope and withdrawn by operators", func(t *testing.T) {
		rec := e.ragAPIGet(base + "/generations")
		wantStatus(t, rec, http.StatusOK)
		ragAPIValidate(t, "RagGenerationList", rec.Body.Bytes())
		gens := ragAPIJSON(t, rec)["generations"].([]any)
		if len(gens) != 2 {
			t.Fatalf("generations = %s", rec.Body.String())
		}
		path := base + "/generations/" + string(active.GenerationID) + "/invalidate"
		wantError(t, e.ragAPIPost(path, nil), http.StatusForbidden, contracts.ErrPermissionDenied)
		inv := e.ragAPIPost(path, nil, ragAPIOperatorOf())
		wantStatus(t, inv, http.StatusOK)
		ragAPIValidate(t, "RagIndexGeneration", inv.Body.Bytes())
		if body := ragAPIJSON(t, inv); body["state"] != "invalidated" || body["generation_id"] != string(active.GenerationID) || body["retired_at"] == nil {
			t.Fatalf("invalidated = %s", inv.Body.String())
		}
		wantError(t, e.ragAPIPost(base+"/generations/"+ids.New("gen")+"/invalidate", nil, ragAPIOperatorOf()), http.StatusNotFound, contracts.ErrValidationFailed)
	})

	t.Run("only operators queue indexing, and a source of another tenant is not found", func(t *testing.T) {
		calls := len(f.indexing.called())
		wantError(t, e.ragAPIPost(base+"/sync", nil), http.StatusForbidden, contracts.ErrPermissionDenied)
		wantError(t, e.ragAPIPost(base+"/rebuild", nil), http.StatusForbidden, contracts.ErrPermissionDenied)
		other := []claims{ragAPIOperatorOf(), withTenant(apiOther)}
		wantError(t, e.ragAPIPost(base+"/sync", nil, other...), http.StatusNotFound, contracts.ErrValidationFailed)
		wantError(t, e.ragAPIGet(base+"/generations", withTenant(apiOther)), http.StatusNotFound, contracts.ErrValidationFailed)
		if got := len(f.indexing.called()) - calls; got != 4 {
			t.Errorf("the service saw %d calls, want the 4 that were well formed", got)
		}
	})

	t.Run("malformed ids and bodies never reach the service", func(t *testing.T) {
		calls := len(f.indexing.called())
		wantError(t, e.ragAPIPost("/sources/src_nope/sync", nil, ragAPIOperatorOf()), http.StatusBadRequest, contracts.ErrValidationFailed)
		wantError(t, e.ragAPIGet("/sources/src_nope/generations"), http.StatusBadRequest, contracts.ErrValidationFailed)
		wantError(t, e.ragAPIPost(base+"/generations/gen_nope/invalidate", nil, ragAPIOperatorOf()), http.StatusBadRequest, contracts.ErrValidationFailed)
		wantError(t, e.ragAPIPost(base+"/sync", map[string]any{"tenant_id": apiOther}, ragAPIOperatorOf()), http.StatusBadRequest, contracts.ErrValidationFailed)
		if len(f.indexing.called()) != calls {
			t.Fatal("a malformed request reached the indexing service")
		}
	})

	t.Run("a service that answers about something else is not believed", func(t *testing.T) {
		f.indexing.mu.Lock()
		f.indexing.wrongSource = ids.New("src")
		f.indexing.mu.Unlock()
		defer func() {
			f.indexing.mu.Lock()
			f.indexing.wrongSource = ""
			f.indexing.mu.Unlock()
		}()
		wantError(t, e.ragAPIPost(base+"/sync", nil, ragAPIOperatorOf()), http.StatusInternalServerError, contracts.ErrInternal)
		// A generation of another source is dropped from a listing, not passed on.
		rec := e.ragAPIGet(base + "/generations")
		wantStatus(t, rec, http.StatusOK)
		for _, g := range ragAPIJSON(t, rec)["generations"].([]any) {
			if g.(map[string]any)["source_id"] != id {
				t.Errorf("a generation of another source was listed: %v", g)
			}
		}
		other := ragAPIGeneration(id, rag.IndexGenerationStateActive)
		f.indexing.mu.Lock()
		f.indexing.gens[id] = append(f.indexing.gens[id], other)
		f.indexing.mu.Unlock()
		wantError(t, e.ragAPIPost(base+"/generations/"+string(other.GenerationID)+"/invalidate", nil, ragAPIOperatorOf()), http.StatusInternalServerError, contracts.ErrInternal)
	})
}

// ---- what is mounted ----

// A deployment that has provided the registry but not yet the retrieval and
// indexing services still starts: the sources are served and the rest is a 404,
// never a handler with nothing behind it. (Without this, wiring the RAG area in
// two steps would make `alixai serve` refuse to start in between.)
func TestRAGRoutesMountOnlyWhatIsProvided(t *testing.T) {
	prev := ragAPIProvideMode
	ragAPIProvideMode = ragAPISourcesOnly
	defer func() { ragAPIProvideMode = prev }()
	e := newAPIEnv(t)
	got := strings.Join(e.srv.AreaRoutes("v1_rag"), ",")
	for _, want := range []string{"POST /v1/rag/sources", "GET /v1/rag/sources", "POST /v1/rag/sources/reaudit", "GET /v1/rag/sources/{source_id}", "DELETE /v1/rag/sources/{source_id}"} {
		if !strings.Contains(got, want) {
			t.Errorf("route %q is not mounted (mounted: %s)", want, got)
		}
	}
	for _, not := range []string{"/search", "/answer", "/citations", "/sync", "/rebuild", "/generations"} {
		if strings.Contains(got, not) {
			t.Errorf("a route with %q is mounted without its service: %s", not, got)
		}
	}
	for _, path := range []string{"/v1/rag/search", "/v1/rag/answer"} {
		if rec := e.call(http.MethodPost, path, map[string]any{"query": "x", "question": "x"}); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 404", path, rec.Code)
		}
	}
	if un := e.srv.Unmounted(); len(un) != 0 {
		t.Errorf("areas left unmounted: %v", un)
	}
}

// ---- the guards ----

// ragAPIQueriesFake is a RAGQueries that answers what a test says, to prove the
// gateway refuses what the contract forbids whatever the service does.
type ragAPIQueriesFake struct {
	search func(rag.SearchRequest) (rag.SearchResponse, error)
	answer func(rag.AnswerRequest) (rag.AnswerResult, error)
	calls  int
	scopes []contracts.Scope
}

func (f *ragAPIQueriesFake) Search(_ context.Context, scope contracts.Scope, req rag.SearchRequest) (rag.SearchResponse, error) {
	f.calls++
	f.scopes = append(f.scopes, scope)
	return f.search(req)
}

func (f *ragAPIQueriesFake) Answer(_ context.Context, scope contracts.Scope, req rag.AnswerRequest) (rag.AnswerResult, error) {
	f.calls++
	f.scopes = append(f.scopes, scope)
	return f.answer(req)
}

func (f *ragAPIQueriesFake) Open(_ context.Context, scope contracts.Scope, _ string) (RAGCitationBody, error) {
	f.calls++
	f.scopes = append(f.scopes, scope)
	return RAGCitationBody{}, nil
}

func ragAPIScope() contracts.Scope {
	sc := identitytest.ExpectedScope()
	sc.CapabilityScopes = append([]string(nil), defaultCaps...)
	return sc
}

func ragAPIHit(source string, rank int) rag.SearchHit {
	return rag.SearchHit{
		EvidenceID: rag.EvidenceID("e" + strconv.Itoa(rank)), Rank: int64(rank), SourceID: actions.SourceID(source), DisplayName: "Doc", Version: "v1",
		Availability: conversation.CitationAvailabilityAvailable, Staleness: conversation.CitationStalenessCurrent,
		OpenRef: conversation.CitationOpenRef{Value: conversation.CitationOpenRefSource{Kind: "source"}}, Excerpt: "text",
	}
}

func ragAPICitation(source string) conversation.Citation {
	return conversation.Citation{
		SourceID: actions.SourceID(source), DisplayName: "Doc", Version: "v1",
		Availability: conversation.CitationAvailabilityAvailable, Staleness: conversation.CitationStalenessCurrent,
		OpenRef: conversation.CitationOpenRef{Value: conversation.CitationOpenRefSource{Kind: "source"}}, Claims: []string{"A claim."},
	}
}

func TestRAGQueriesScopedRefusesWhatTheContractForbids(t *testing.T) {
	ctx := context.Background()
	src := ids.New("src")
	other := ids.New("src")
	good := rag.GroundedAnswer{
		Claims:    []rag.GroundedAnswerClaim{{Text: "A claim.", CitationIndexes: []int64{0}}},
		Citations: []conversation.Citation{ragAPICitation(src)},
	}
	three := int64(1)

	t.Run("the scope and the request are checked before the service sees them", func(t *testing.T) {
		f := &ragAPIQueriesFake{}
		g := RAGQueriesScoped(f)
		bad := ragAPIScope()
		bad.TenantID = ""
		for name, sc := range map[string]contracts.Scope{"no tenant": bad, "no scope": {}, "wildcard": func() contracts.Scope { s := ragAPIScope(); s.WorkspaceID = "*"; return s }()} {
			if _, err := g.Search(ctx, sc, rag.SearchRequest{Query: "x"}); err == nil {
				t.Errorf("%s: search ran", name)
			}
			if _, err := g.Answer(ctx, sc, rag.AnswerRequest{Question: "x"}); err == nil {
				t.Errorf("%s: answer ran", name)
			}
			if _, err := g.Open(ctx, sc, ids.New("fil")); err == nil {
				t.Errorf("%s: open ran", name)
			}
		}
		if _, err := g.Search(ctx, ragAPIScope(), rag.SearchRequest{Query: "  "}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("a blank query: %v", err)
		}
		if _, err := g.Open(ctx, ragAPIScope(), "fil_nope"); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("a malformed reference: %v", err)
		}
		if f.calls != 0 {
			t.Fatalf("%d refused calls reached the service", f.calls)
		}
		f.search = func(rag.SearchRequest) (rag.SearchResponse, error) { return rag.SearchResponse{}, nil }
		if _, err := g.Search(ctx, ragAPIScope(), rag.SearchRequest{Query: "x"}); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(f.scopes, []contracts.Scope{ragAPIScope()}) {
			t.Fatalf("the service was given %+v, want exactly the verified scope", f.scopes)
		}
	})

	t.Run("a search is never wider than it was asked to be", func(t *testing.T) {
		f := &ragAPIQueriesFake{}
		g := RAGQueriesScoped(f)
		f.search = func(rag.SearchRequest) (rag.SearchResponse, error) {
			return rag.SearchResponse{Results: []rag.SearchHit{ragAPIHit(src, 1), ragAPIHit(src, 2)}}, nil
		}
		if _, err := g.Search(ctx, ragAPIScope(), rag.SearchRequest{Query: "x", Limit: &three}); contracts.CodeOf(err) != contracts.ErrInternal {
			t.Errorf("more hits than the limit: %v", err)
		}
		f.search = func(rag.SearchRequest) (rag.SearchResponse, error) {
			return rag.SearchResponse{Results: []rag.SearchHit{ragAPIHit(other, 1)}}, nil
		}
		if _, err := g.Search(ctx, ragAPIScope(), rag.SearchRequest{Query: "x", SourceIDs: []actions.SourceID{actions.SourceID(src)}}); contracts.CodeOf(err) != contracts.ErrInternal {
			t.Errorf("a hit of a source the request excluded: %v", err)
		}
		bad := ragAPIHit(src, 1)
		bad.Excerpt = " "
		f.search = func(rag.SearchRequest) (rag.SearchResponse, error) {
			return rag.SearchResponse{Results: []rag.SearchHit{bad}}, nil
		}
		if _, err := g.Search(ctx, ragAPIScope(), rag.SearchRequest{Query: "x"}); contracts.CodeOf(err) != contracts.ErrInternal {
			t.Errorf("a hit with no text: %v", err)
		}
		f.search = func(rag.SearchRequest) (rag.SearchResponse, error) {
			return rag.SearchResponse{Results: []rag.SearchHit{ragAPIHit(src, 1)}}, nil
		}
		if _, err := g.Search(ctx, ragAPIScope(), rag.SearchRequest{Query: "x"}); err != nil {
			t.Errorf("a good search: %v", err)
		}
	})

	t.Run("an answer is a grounded one or the typed absence of one", func(t *testing.T) {
		f := &ragAPIQueriesFake{}
		g := RAGQueriesScoped(f)
		with := func(mut func(*rag.GroundedAnswer)) rag.AnswerResult {
			a := good
			a.Claims = append([]rag.GroundedAnswerClaim(nil), good.Claims...)
			a.Citations = append([]conversation.Citation(nil), good.Citations...)
			mut(&a)
			return rag.AnswerResult{Value: a}
		}
		for name, res := range map[string]rag.AnswerResult{
			"no variant":            {},
			"no claim":              with(func(a *rag.GroundedAnswer) { a.Claims = nil }),
			"no citation":           with(func(a *rag.GroundedAnswer) { a.Citations = nil }),
			"a claim without cites": with(func(a *rag.GroundedAnswer) { a.Claims[0].CitationIndexes = nil }),
			"a cite out of range":   with(func(a *rag.GroundedAnswer) { a.Claims[0].CitationIndexes = []int64{1} }),
			"a negative cite":       with(func(a *rag.GroundedAnswer) { a.Claims[0].CitationIndexes = []int64{-1} }),
			"a citation with no claims": with(func(a *rag.GroundedAnswer) {
				c := a.Citations[0]
				c.Claims = nil
				a.Citations[0] = c
			}),
			"a blank claim": with(func(a *rag.GroundedAnswer) { a.Claims[0].Text = " " }),
			"a source the request excluded": with(func(a *rag.GroundedAnswer) {
				a.Citations[0] = ragAPICitation(other)
			}),
			"an unlisted absence reason": {Value: rag.NoSupportedEvidence{Reason: "because"}},
		} {
			res := res
			f.answer = func(rag.AnswerRequest) (rag.AnswerResult, error) { return res, nil }
			_, err := g.Answer(ctx, ragAPIScope(), rag.AnswerRequest{Question: "x", SourceIDs: []actions.SourceID{actions.SourceID(src)}})
			if contracts.CodeOf(err) != contracts.ErrInternal {
				t.Errorf("%s: err = %v, want internal", name, err)
			}
		}
		for name, res := range map[string]rag.AnswerResult{
			"grounded":   {Value: good},
			"absence":    {Value: rag.NoSupportedEvidence{Reason: rag.NoSupportedEvidenceReasonNoMatch}},
			"egress":     {Value: rag.NoSupportedEvidence{Reason: rag.NoSupportedEvidenceReasonEgressRestricted}},
			"by pointer": {Value: &good},
		} {
			res := res
			f.answer = func(rag.AnswerRequest) (rag.AnswerResult, error) { return res, nil }
			if _, err := g.Answer(ctx, ragAPIScope(), rag.AnswerRequest{Question: "x"}); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("nil in, nil out", func(t *testing.T) {
		if RAGQueriesScoped(nil) != nil || RAGIndexingScoped(nil) != nil {
			t.Error("a nil service was wrapped")
		}
		if err := Provide[RAGQueries](NewServices(), RAGQueriesScoped(nil)); err == nil {
			t.Error("a nil service was provided")
		}
		if _, err := NewRAGQueries(nil, nil); err == nil {
			t.Error("queries were built over nothing")
		}
	})
}

// The request budget of the Answerer's text generator is derived from the two
// configurations, and a full package of evidence fits it.
func TestRAGAnswererBudgetFitsAFullPackage(t *testing.T) {
	qcfg, acfg := query.DefaultConfig(), answer.DefaultConfig()
	b := RAGAnswererBudget(qcfg, acfg)
	if err := b.Validate(); err != nil {
		t.Fatalf("the budget is not valid: %v", err)
	}
	if b.MaxOutputTokens != acfg.MaxOutputToks || b.Timeout != acfg.GenerateTimeout || b.MaxPromptTokens < qcfg.MaxToks {
		t.Fatalf("budget = %+v", b)
	}
	model := pfake.New()
	gen, err := providers.WithBudget(model, b)
	if err != nil {
		t.Fatal(err)
	}
	catalog := ragAPINewCatalog()
	a, err := answer.New(answer.Deps{Generator: gen, Catalog: catalog}, acfg)
	if err != nil {
		t.Fatal(err)
	}
	// Eight passages that together use the whole evidence budget, and the longest
	// question the API accepts.
	scope := ragAPIScope()
	var specs []ragAPISpec
	per := qcfg.MaxToks * 3 / qcfg.MaxItems
	for i := 0; i < qcfg.MaxItems; i++ {
		specs = append(specs, ragAPISpec{source: "doc" + strconv.Itoa(i), text: strings.Repeat("kettle ", per/7)})
	}
	pkg := ragAPIPackage(scope, strings.Repeat("é", ragAPIMaxQuestionRunes), specs...)
	catalog.grant(scope, pkg)
	model.On(pfake.Any()).ReplyJSON(map[string]any{"claims": []any{}})
	res, err := a.Answer(context.Background(), scope, pkg)
	if err != nil {
		t.Fatalf("a full package does not fit the budget: %v", err)
	}
	if _, ok := res.Answer.Value.(rag.NoSupportedEvidence); !ok {
		t.Fatalf("outcome = %T", res.Answer.Value)
	}
	if model.Calls() != 1 {
		t.Fatalf("the model was called %d times", model.Calls())
	}
}
