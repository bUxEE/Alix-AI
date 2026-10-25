package cli

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/rag/answer"
	"github.com/buxee/alix-ai/internal/rag/ingest"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// The part of the RAG composition that faces the /v1 API (M2-17): the registry
// as gateway.RAGSources, and the configuration sections of the RAG packages for a
// registry other than config.Default.
//
// # The registry as gateway.RAGSources
//
// ragSourcesService implements the interface over the catalogue (ragSources, with
// its durable registry) and the ingestion service, and ragServeProvide hands it to
// the gateway wrapped in gateway.RAGSourcesScoped. Three rules, none of which a
// caller can loosen:
//
//   - Operator authority. Registering, re-auditing and removing a source are
//     operator acts: they bind a tenant's workspace to a folder or an API a
//     package declares, and removing one deletes derived data. The verified
//     principal must be listed in the "rag" section's operators with the
//     manage_sources action (a deployment with no such operator registers nothing
//     through the API). Nothing a token's roles or capability scopes say is read,
//     and nothing a package or a model says either.
//   - The package decides what a source is. A caller names a declaration of the
//     application's active package and nothing else; the definition is built from
//     the package (source.FromPackage), bound to the verified scope's tenant and
//     workspace the way the declaration says, and refused unless it was built from
//     the active package's very content.
//   - Scope. Everything is keyed by the verified application, tenant and
//     workspace. Re-audit touches the scope's registrations only: a tenant's
//     request cannot re-bind, or purge (the package may have dropped a
//     declaration), the sources of another tenant. The process's own Reconcile
//     does the application-wide re-bind.
//
// The service identity "rag-ingest" keeps its three conditions (ragAuthorizer):
// the pipeline's job types, an active package and the source's own scope.

// ragSourcesActionManage is the operator action of registering, re-auditing and
// removing sources.
const ragSourcesActionManage ingest.OperatorAction = "manage_sources"

// ragSourcesService is gateway.RAGSources over the catalogue and ingestion.
type ragSourcesService struct {
	sources   *ragSources
	svc       *ingest.Service
	operators ingest.OperatorAuthorizer
	packages  ragPackageSource

	// mu serialises registrations, so that registering one declaration twice at
	// once finds one id, not two.
	mu sync.Mutex
}

var _ gateway.RAGSources = (*ragSourcesService)(nil)

func ragSourcesNewService(sources *ragSources, svc *ingest.Service, operators ingest.OperatorAuthorizer, packages ragPackageSource) *ragSourcesService {
	return &ragSourcesService{sources: sources, svc: svc, operators: operators, packages: packages}
}

// authorize is the scope check and the operator decision of the acts that change
// the registry.
func (g *ragSourcesService) authorize(ctx context.Context, scope contracts.Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	return g.operators.AuthorizeOperator(ctx, scope, ragSourcesActionManage)
}

// active is the application's active package, or contracts.ErrQuarantined.
func (g *ragSourcesService) active(ctx context.Context, applicationID string) (ragActivePackage, error) {
	pkg, active, err := ragActive(ctx, g.packages, applicationID)
	switch {
	case err != nil:
		return nil, err
	case !active:
		return nil, contracts.New(contracts.ErrQuarantined, "the application has no active package")
	}
	return pkg, nil
}

// Register implements gateway.RAGSources.
func (g *ragSourcesService) Register(ctx context.Context, scope contracts.Scope, in gateway.RAGSourcesRegistration) (gateway.RAGSourcesRecord, error) {
	if err := g.authorize(ctx, scope); err != nil {
		return gateway.RAGSourcesRecord{}, err
	}
	if in.PackageSourceID == "" {
		return gateway.RAGSourcesRecord{}, contracts.New(contracts.ErrValidationFailed, "name the source the package declares")
	}
	if _, err := g.active(ctx, scope.ApplicationID); err != nil {
		return gateway.RAGSourcesRecord{}, err
	}
	if g.sources.reaudit == nil {
		return gateway.RAGSourcesRecord{}, contracts.New(contracts.ErrUnavailable, "rag: the package's declarations cannot be read in this process")
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	id := ""
	if e, ok := g.sources.byDeclaration(scope, in.PackageSourceID); ok {
		if e.removing {
			return gateway.RAGSourcesRecord{}, contracts.New(contracts.ErrConflict, "the source is being removed; remove it again to finish before registering it")
		}
		id = e.def.SourceID
	} else {
		id = ids.New(source.IDPrefix)
	}
	def, declared, err := g.sources.reaudit.Declare(ctx, scope, in.PackageSourceID, id)
	switch {
	case err != nil:
		return gateway.RAGSourcesRecord{}, err
	case !declared:
		return gateway.RAGSourcesRecord{}, contracts.New(contracts.ErrValidationFailed, "the application's active package declares no such source").
			WithDetails("reason", "not_declared")
	}
	if err := g.sources.Register(ctx, scope, def); err != nil {
		return gateway.RAGSourcesRecord{}, err
	}
	recs, err := g.status(ctx, scope, id)
	if err != nil {
		return gateway.RAGSourcesRecord{}, err
	}
	if len(recs) != 1 {
		return gateway.RAGSourcesRecord{}, contracts.New(contracts.ErrInternal, "rag: the registered source cannot be read back")
	}
	return recs[0], nil
}

// Reaudit implements gateway.RAGSources.
func (g *ragSourcesService) Reaudit(ctx context.Context, scope contracts.Scope) (gateway.RAGSourcesReaudit, error) {
	if err := g.authorize(ctx, scope); err != nil {
		return gateway.RAGSourcesReaudit{}, err
	}
	pkg, err := g.active(ctx, scope.ApplicationID)
	if err != nil {
		return gateway.RAGSourcesReaudit{}, err
	}
	if g.sources.reaudit == nil {
		return gateway.RAGSourcesReaudit{}, contracts.New(contracts.ErrUnavailable, "rag: the package's declarations cannot be read in this process")
	}
	hash, rebuild, err := g.sources.reaudit.Active(ctx, scope.ApplicationID)
	switch {
	case err != nil:
		return gateway.RAGSourcesReaudit{}, err
	case hash != pkg.ContentHash():
		return gateway.RAGSourcesReaudit{}, contracts.New(contracts.ErrConflict, "rag: the active package changed while it was being read; repeat the request")
	}
	rep, err := g.sources.reauditScope(ctx, scope, rebuild)
	return gateway.RAGSourcesReaudit{Checked: rep.Checked, Rebound: rep.Rebound, Queued: rep.Queued, Removed: rep.Removed, Failed: rep.Failed}, err
}

// Remove implements gateway.RAGSources.
func (g *ragSourcesService) Remove(ctx context.Context, scope contracts.Scope, sourceID string) error {
	if err := g.authorize(ctx, scope); err != nil {
		return err
	}
	return g.sources.Remove(ctx, scope, sourceID)
}

// Status implements gateway.RAGSources. It is a read inside the caller's own
// scope and needs no operator.
func (g *ragSourcesService) Status(ctx context.Context, scope contracts.Scope, sourceID string) ([]gateway.RAGSourcesRecord, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return g.status(ctx, scope, sourceID)
}

func (g *ragSourcesService) status(ctx context.Context, scope contracts.Scope, sourceID string) ([]gateway.RAGSourcesRecord, error) {
	held := g.sources.inScope(scope, sourceID)
	if sourceID != "" && len(held) == 0 {
		return nil, contracts.New(contracts.ErrValidationFailed, "rag: no such source").WithDetails("reason", "not_found")
	}
	hash, active := "", false
	if len(held) > 0 {
		pkg, ok, err := ragActive(ctx, g.packages, scope.ApplicationID)
		if err != nil {
			return nil, err
		}
		if ok {
			hash, active = pkg.ContentHash(), true
		}
	}
	out := make([]gateway.RAGSourcesRecord, 0, len(held))
	for _, e := range held {
		rec := gateway.RAGSourcesRecord{
			ApplicationID: scope.ApplicationID, TenantID: scope.TenantID, WorkspaceID: scope.WorkspaceID,
			PackageSourceID: e.def.PackageSourceID, PackageVersion: e.def.PackageVersion,
			Serving: active && e.bound && !e.removing && e.def.ManifestHash == hash && e.def.CheckScope(scope) == nil,
		}
		st, err := g.statusOf(ctx, scope, e)
		if err != nil {
			return nil, err
		}
		rec.Status = st
		out = append(out, rec)
	}
	return out, nil
}

// statusOf is the source status of one registration: what ingestion observed,
// and nothing it did not. A source whose removal began is removing and gone; a
// source ingestion has never scanned is registered and not available (it was
// never reached), with its staleness unknown.
func (g *ragSourcesService) statusOf(ctx context.Context, scope contracts.Scope, e ragEntry) (rag.SourceStatus, error) {
	name := e.def.Title
	if name == "" {
		name = e.def.PackageSourceID
	}
	st := rag.SourceStatus{
		SourceID: actions.SourceID(e.def.SourceID), DisplayName: name, State: rag.SourceStatusStateRegistered,
		Availability: conversation.CitationAvailabilityUnavailable, Staleness: conversation.CitationStalenessUnknown,
	}
	if e.removing {
		st.State, st.Availability = rag.SourceStatusStateRemoving, conversation.CitationAvailabilityRemoved
		return st, nil
	}
	info, err := g.svc.Status(ctx, scope, e.def.SourceID)
	switch {
	case err == nil:
	case contracts.CodeOf(err) == contracts.ErrValidationFailed:
		st.Error = &common.Error{Code: common.ErrorCodeUnavailable, Message: "the source has not been scanned yet", Retryable: true}
		return st, nil
	default:
		return rag.SourceStatus{}, err
	}
	label := g.sources.stateOf(e.def, info)
	st.State, st.Availability, st.Staleness = info.State, label.Availability, label.Staleness
	if !st.State.Valid() {
		st.State = rag.SourceStatusStateRegistered
	}
	st.LastAttemptAt, st.LastSuccessAt = ragSourcesTimestamp(info.LastAttempt), ragSourcesTimestamp(info.LastSuccess)
	st.Counts = rag.SourceStatusCounts{
		Discovered: rag.Count(info.Counts.Discovered), Indexed: rag.Count(info.Counts.Indexed), Skipped: rag.Count(info.Counts.Skipped),
		Failed: rag.Count(info.Counts.Failed), Tombstoned: rag.Count(info.Counts.Tombstoned),
	}
	if info.ActiveGenerationID != "" {
		gen := rag.GenerationID(info.ActiveGenerationID)
		st.ActiveGenerationID = &gen
	}
	st.Error = ragSourcesErrorOf(info)
	return st, nil
}

// ragSourcesTimestamp is a time as the contract spells it, nil for none.
func ragSourcesTimestamp(t time.Time) *common.Timestamp {
	if t.IsZero() {
		return nil
	}
	ts := common.Timestamp(t.UTC().Format("2006-01-02T15:04:05.000Z"))
	return &ts
}

// ragSourcesErrorOf is the typed, non-sensitive reason a source is not available,
// nil when nothing is wrong. The code is the one ingestion recorded when it is a
// contract code, else the one its reason maps to.
func ragSourcesErrorOf(info ingest.SourceInfo) *common.Error {
	if info.Available && info.ErrorCode == "" {
		return nil
	}
	code := common.ErrorCode(info.ErrorCode)
	if !code.Valid() {
		switch info.Reason {
		case ingest.ReasonLimitExceeded:
			code = common.ErrorCodeResourceExhausted
		case ingest.ReasonMassDeletion:
			code = common.ErrorCodeConflict
		default:
			code = common.ErrorCodeUnavailable
		}
	}
	msg := info.Message
	if msg == "" {
		msg = "the source is not available: " + strings.ReplaceAll(info.Reason, "_", " ")
	}
	return &common.Error{Code: code, Message: msg, Retryable: contracts.ErrCode(code).Retryable()}
}

// inScope returns the registrations of the scope (the one with that id, when an
// id is given), ordered by id. A removing registration is included: its status
// says so.
func (r *ragSources) inScope(scope contracts.Scope, id string) []ragEntry {
	r.mu.RLock()
	var out []ragEntry
	for k, e := range r.entries {
		if k.app == scope.ApplicationID && k.tenant == scope.TenantID && k.workspace == scope.WorkspaceID && (id == "" || k.id == id) {
			out = append(out, *e)
		}
	}
	r.mu.RUnlock()
	slices.SortFunc(out, func(a, b ragEntry) int { return strings.Compare(a.def.SourceID, b.def.SourceID) })
	return out
}

// byDeclaration returns the scope's registration of a declaration of the package,
// removing ones included.
func (r *ragSources) byDeclaration(scope contracts.Scope, packageSourceID string) (ragEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for k, e := range r.entries {
		if k.app == scope.ApplicationID && k.tenant == scope.TenantID && k.workspace == scope.WorkspaceID && e.def.PackageSourceID == packageSourceID {
			return *e, true
		}
	}
	return ragEntry{}, false
}

// ---- configuration sections of a registry other than config.Default ------------

// ragSourcesSections are the configuration sections of the RAG packages that
// serve reads.
//
// Each of these packages registers its section on config.Default from its own
// init and keeps the live pointer to itself (FromConfig returns a copy), so a
// registry of any other kind (a test's, a tool's) cannot be given that pointer:
// registerServeSections registers this package's copies of the same name and
// type instead, and the provider reads them through ragSourcesSectionsOf. The
// effect is the same as on config.Default: a block of the YAML file is decoded
// against the package's own type, validated by the package's own Validate, and
// reaches the pipeline. (The packages should export a RegisterSections the way
// the gateway does; until they do, this is the way.)
type ragSourcesSections struct {
	ingest ingest.Config
	query  query.Config
	vector vector.Config
	// answer has no consumer in serve yet (the answer endpoint of M2-11 is its
	// first): its block is accepted and validated, and read where it is used.
	answer answer.Config
	jobs   jobs.Config
}

// ragSourcesSectionsByRegistry holds the copies registered on each registry.
var ragSourcesSectionsByRegistry sync.Map // *config.Registry -> *ragSourcesSections

// ragSourcesRegisterSections registers on reg the sections the RAG composition
// reads: rag (this package's own), rag_ingest, rag_query, rag_vector, rag_answer
// and jobs. The names and types are the packages', so a block that is accepted
// by config.Default is accepted here and refused for the same reasons.
func ragSourcesRegisterSections(reg *config.Registry) {
	s := &ragSourcesSections{
		ingest: ingest.DefaultConfig(), query: query.DefaultConfig(), vector: vector.DefaultConfig(),
		answer: answer.DefaultConfig(), jobs: jobs.DefaultConfig(),
	}
	reg.Register(ragSectionName, &ragSection)
	reg.Register(ingest.SectionName, &s.ingest)
	reg.Register(query.SectionName, &s.query)
	reg.Register("rag_vector", &s.vector)
	reg.Register(answer.SectionName, &s.answer)
	reg.Register("jobs", &s.jobs)
	ragSourcesSectionsByRegistry.Store(reg, s)
}

// ragSourcesSectionsOf returns the sections as the registry serve ran on loaded
// them: the copies registerServeSections registered on it, or, for config.Default
// (nil), the packages' own.
func ragSourcesSectionsOf(reg *config.Registry) ragSourcesSections {
	if reg != nil && reg != config.Default {
		if v, ok := ragSourcesSectionsByRegistry.Load(reg); ok {
			return *v.(*ragSourcesSections)
		}
	}
	return ragSourcesSections{
		ingest: ingest.FromConfig(), query: query.FromConfig(), vector: vector.FromConfig(),
		answer: answer.FromConfig(), jobs: jobs.FromConfig(),
	}
}
