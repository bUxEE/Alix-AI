package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/ingest"
	"github.com/buxee/alix-ai/internal/rag/parse"
	"github.com/buxee/alix-ai/internal/rag/parse/wire"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/internal/rag/source"
	ragsources "github.com/buxee/alix-ai/internal/rag/sources"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/internal/upgrade"
	"github.com/buxee/alix-ai/internal/version"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// The RAG composition of `alixai serve` (master plan 12.3 to 12.5, 17.2, 18; M2-16).
//
// internal/rag/ingest, internal/rag/query and the index and vector stores under
// them are libraries; this file is where they become one running part of the
// process, and where the three pieces they leave to the deployment are supplied:
// the disk-pressure hook, the operator authority, and the deterministic
// authority on sources (the registry and catalog below). It reaches the rest of
// serve through registerServeProvider and the two hooks of serveDeps
// (AddCheck, AddService); nothing else in cmd_serve.go knows about it.
//
// # Order
//
//	pause    ingest.NewPause: the disk-pressure gate, before the queue exists. Its hook
//	         is the resource governor's DiskGuard (resources.disk_*) when serve gives one
//	queue    jobs.NewQueue with Options.Policy = pause.Policy(nil), wrapped by
//	         gov.Policy(...) when serve gives a governor (cmd_resources.go), and the
//	         jobs section's interactive_up_to_class raised to 4 (ragEffectiveJobs), so
//	         ingestion (classes 5 and 6) leases nothing while any class 1 to 4 job
//	         waits, while a live turn is active, or while memory is unknown or at its
//	         ceiling
//	index    the lexical index, the chunker and, with an embedder, the vector index
//	ingest   ingest.New, with the same Pause. It registers vector.EmbedJobSpec
//	         itself, so vector.Pipeline.Register is never called as well
//	pool     jobs.NewPool, then ingest.Service.Handle(pool)
//	query    query.New over the index, the catalog and the reader by reference
//	start    (AddService) ingest.Service.Start probes the parser worker, then
//	         the pool runs; both stop before the store closes
//
// ragStack.steps records it and TestRAGServeWiringOrder asserts it.
//
// # What it never does
//
// Nothing here decides who may read a document or who may delete one. A source
// is readable only through the catalog, which answers from the active package
// and the registration and fails closed (an error is a failed check, an unknown
// or stale source is an unauthorised one). The operator actions of ingest
// (ActionConfirmMassDeletion, ActionDeleteTenant) are authorised by the
// deployment's own configuration of verified principals (the "rag" section's
// operators list), never by a role the host application or a package asserts
// and never by the model.
//
// # Durable registrations (M2-17)
//
// The registry is the catalogue's own memory over a durable one
// (internal/rag/sources). Register records the registration (the declaration it
// was built from, the package content hash it is bound to, the scope it was
// made under) before the catalogue serves it; Remove records that the removal
// began before ingestion deletes anything and drops the record when it ends; a
// re-bind moves the stored hash only after ingestion accepted the new
// definition. When the process starts it reads the registrations back (load) and
// re-binds each to the active package (Reconcile, once, before the pool leases a
// job), so a restart keeps the index usable and the pending jobs of ingestion
// are answered, not denied as jobs of an unknown source. A registration that has
// been read back and not yet rebuilt from the package is not served and a job
// for it is retried, never ended. cmd_rag_sources.go exposes the registry to the
// /v1 API as gateway.RAGSources and registers the configuration sections.
//
// # The rest of the area (M2-18)
//
// ragServeProvide also provides gateway.RAGQueries (cmd_rag_queries.go: search, grounded answers,
// the references of citations, and the same answers for the dialogue engine's text turns that no
// capability handles) and gateway.RAGIndexing (cmd_rag_indexing.go: sync, rebuild, generations,
// invalidate), and registers the application-document HTTP provider next to the filesystem one
// (cmd_rag_providers.go). None of them is a step of the order above: they are views over what the
// stack has already built, so the order and TestRAGServeWiringOrder are unchanged.
//
// # One queue and one pool for every area (M3-21)
//
// The stack is also provided to the registry (as *ragStack, a type only this package can name), and
// a later provider runs its own job type on the same queue and pool through shareJobs: the pool
// accepts the area's service identity through the authority the area registers, and the area
// attaches its handlers. The history retention job is the first (cmd_history_retention.go), the
// re-audit watch job of the integration audit the second (cmd_reaudit.go). An identity nobody
// registered is denied, and ragServiceIdentity cannot be taken over.

// ragSectionName is the "rag" configuration section.
const ragSectionName = "rag"

// ragServiceIdentity is the declared service identity ingestion runs under when
// the system, not a principal, queues the work (registration and re-audit). The
// pool accepts it only through ragAuthorizer.AuthorizeService.
const ragServiceIdentity = "rag-ingest"

// ragInteractiveUpTo is the jobs section's interactive_up_to_class this
// composition requires: ingestion (classes 5 and 6) yields to everything a user
// is waiting for, classes 1 to 4.
const ragInteractiveUpTo = 4

// ragReadyCheck is the name of the /readyz check.
const ragReadyCheck = "rag_ingest"

// ragDefaultDiskFloor is the free space of the data directory under which
// ingestion pauses, when the section sets none.
const ragDefaultDiskFloor int64 = 1 << 30

// ragConfig is the "rag" configuration section: what only the deployment can
// say about RAG. The tuning of each stage stays in its own section (rag_ingest,
// rag_query, rag_vector, jobs).
//
//	rag:
//	  disk_floor_bytes: 1073741824   # superseded by resources.disk_pause_free_bytes while serve runs the resource governor
//	  mounts:                        # operator-registered mounts, by the id a package declares
//	    nas-1: /mnt/nas
//	  operators:                     # who may confirm a mass deletion, delete a tenant, or register, re-audit and remove sources
//	    - application_id: acme-clinic
//	      principal_id: ops-lead
//	      actions: [confirm_mass_deletion, delete_tenant, manage_sources]
//	  embedding:                     # off unless an embedder role is configured and this is set
//	    model: granite-embedding-97m
//	    component_version: 97m-q8
type ragConfig struct {
	DiskFloorBytes int64             `yaml:"disk_floor_bytes"`
	Mounts         map[string]string `yaml:"mounts"`
	Operators      []ragOperator     `yaml:"operators"`
	Embedding      ragEmbeddingConf  `yaml:"embedding"`
}

// ragOperator names one verified principal of one application and what it may
// do beyond a user's rights. The principal id is the one in the context token
// the operator's own host application signed with a key the deployment's
// operator provisioned; roles in a token are never consulted.
type ragOperator struct {
	ApplicationID string   `yaml:"application_id"`
	PrincipalID   string   `yaml:"principal_id"`
	Actions       []string `yaml:"actions"`
}

// ragEmbeddingConf names the embedding model of the indexes. ComponentName
// defaults to a lower-case form of Model; ComponentVersion has no default: a
// generation records the version it was built with, and an unknown version is
// not recorded as a known one.
type ragEmbeddingConf struct {
	Model            string `yaml:"model"`
	ComponentName    string `yaml:"component_name"`
	ComponentVersion string `yaml:"component_version"`
	BatchSize        int    `yaml:"batch_size"`
}

// ragSection is the registered section; read it through this pointer after a load.
var ragSection = ragConfig{DiskFloorBytes: ragDefaultDiskFloor}

var (
	ragIDRE        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$`)
	ragComponentRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// ragOperatorActions are the actions an operator entry may list.
var ragOperatorActions = []string{string(ingest.ActionConfirmMassDeletion), string(ingest.ActionDeleteTenant), string(ragSourcesActionManage)}

// Validate implements the section validation hook.
func (c ragConfig) Validate() error {
	if c.DiskFloorBytes < 0 {
		return errors.New("disk_floor_bytes is negative")
	}
	for id, p := range c.Mounts {
		if !ragIDRE.MatchString(id) {
			return fmt.Errorf("mounts: %q is not a mount id", id)
		}
		if !filepath.IsAbs(p) {
			return fmt.Errorf("mounts.%s: the host path must be absolute", id)
		}
	}
	seen := map[string]bool{}
	for i, o := range c.Operators {
		switch {
		case !ragIDRE.MatchString(o.ApplicationID) || !ragIDRE.MatchString(o.PrincipalID):
			return fmt.Errorf("operators[%d]: application_id and principal_id are required", i)
		case len(o.Actions) == 0:
			return fmt.Errorf("operators[%d]: name the actions explicitly (%s)", i, strings.Join(ragOperatorActions, ", "))
		case seen[o.ApplicationID+"\x00"+o.PrincipalID]:
			return fmt.Errorf("operators[%d]: %s of %s is listed twice", i, o.PrincipalID, o.ApplicationID)
		}
		seen[o.ApplicationID+"\x00"+o.PrincipalID] = true
		for _, a := range o.Actions {
			if !slices.Contains(ragOperatorActions, a) {
				return fmt.Errorf("operators[%d]: unknown action %q (want one of %s)", i, a, strings.Join(ragOperatorActions, ", "))
			}
		}
	}
	e := c.Embedding
	if e.Model == "" {
		if e.ComponentName != "" || e.ComponentVersion != "" || e.BatchSize != 0 {
			return errors.New("embedding: model is required when the embedding section is used")
		}
		return nil
	}
	if e.ComponentVersion == "" || len(e.ComponentVersion) > 128 {
		return errors.New("embedding.component_version is required (at most 128 bytes): a generation records the version it was built with")
	}
	if e.ComponentName != "" && !ragComponentRE.MatchString(e.ComponentName) {
		return fmt.Errorf("embedding.component_name %q is not a component name (lower case letters, digits, '.', '_', '-')", e.ComponentName)
	}
	if e.BatchSize < 0 {
		return errors.New("embedding.batch_size is negative")
	}
	return nil
}

func init() {
	config.Register(ragSectionName, &ragSection)
	registerServeProvider("rag", ragServeProvide)
}

// Test seams, replaced only by tests.
var (
	// ragNewParser builds the document parser. The default starts nothing and
	// looks for the worker when it is first used.
	ragNewParser = func(cfg ingest.Config, log *slog.Logger) ingest.Parser { return newRagLazyParser(cfg, log) }
	// ragNewDiskPressure builds the production disk-pressure hook over a
	// directory and a floor in bytes.
	ragNewDiskPressure = func(dir string, floor int64) ingest.DiskPressure {
		return ragDiskPressure{dir: dir, floor: floor, free: ragFreeBytes}
	}
	// ragOnCompose sees every stack serve builds.
	ragOnCompose func(*ragStack)
)

// ragServeProvide builds the RAG stack, gives the two services other areas use
// (ingest.Service and query.Pipeline) to the gateway's registry, and hands serve
// its readiness check and its start and stop.
func ragServeProvide(ctx context.Context, d serveDeps) error {
	apps, ok := gateway.Lookup[gateway.ApplicationService](d.Services)
	if !ok {
		return errors.New("rag: the application service is not provided")
	}
	cfg := ragSection
	sections := ragSourcesSectionsOf(d.Config)
	icfg := sections.ingest
	// The re-audit trigger reads the active package the way the application
	// service does (the same installer over the same store and data directory).
	inst, err := apppkg.NewInstaller(apppkg.InstallerOptions{
		DB: d.DB, Clock: d.Clock, DataDir: d.DataDir,
		Verifier: trust.NewVerifier(trust.NewStore(d.DB, d.Clock), d.Clock), CoreVersion: apppkg.CoreVersionOrDev(version.Get().Version),
	})
	if err != nil {
		return fmt.Errorf("rag: the package installer: %w", err)
	}
	// With the resource governor the disk guard decides when ingestion pauses; the older floor of
	// this section would be a second, silent setting for the same thing.
	if d.Resources != nil && cfg.DiskFloorBytes != ragDefaultDiskFloor {
		d.Log.Warn("rag.disk_floor_bytes is ignored: resources.disk_pause_free_bytes and resources.disk_resume_free_bytes decide when ingestion pauses")
	}
	// The source providers: the filesystem one and the application-document HTTP one, which reaches
	// only what the active package's own source declarations allow (cmd_rag_providers.go).
	net := ragServeNewNetwork(d.Log)
	provs, err := ragServeSourceProviders(d.Clock, d.Log, net, d.DataDir)
	if err != nil {
		return err
	}
	st, err := ragCompose(ragInput{
		DB: d.DB, Clock: d.Clock, Log: d.Log, Audit: d.Audit,
		Providers:       d.Providers,
		Packages:        ragPackagesOf(apps),
		Config:          cfg,
		Ingest:          icfg,
		Jobs:            sections.jobs,
		Query:           sections.query,
		Vector:          sections.vector,
		Parser:          ragNewParser(icfg, d.Log),
		Disk:            ragNewDiskPressure(d.DataDir, cfg.floor()),
		Reaudit:         ragInstallerSource{inst: inst, mounts: ragMountsOf(cfg), net: net},
		SourceProviders: provs,
		Resources:       d.Resources,
		Quarantine:      quarantineOf(d.DataDir, d.Clock),
		DeploymentID:    identity.CurrentConfig().DeploymentID,
	})
	if err != nil {
		return err
	}
	if st.input.DeploymentID == "" {
		st.log.Warn("rag: the maintenance of the indexes is queued only by a source removal: identity.deployment_id is not configured")
	}
	// The registrations of the last run are read back before anything can ask
	// the catalogue about a source: the jobs the pool leases, the queries the
	// pipeline answers. A store that cannot be read refuses the start.
	if err := st.sources.load(ctx); err != nil {
		return err
	}
	// The stack itself is provided so that a later provider (name order) can run its own job type on
	// the one queue and pool (shareJobs); only this package can name the type.
	for _, perr := range []error{
		gateway.Provide(d.Services, st), gateway.Provide(d.Services, st.svc), gateway.Provide(d.Services, st.pipeline),
		gateway.Provide[gateway.RAGSources](d.Services, gateway.RAGSourcesScoped(st.api)),
	} {
		if perr != nil {
			return perr
		}
	}
	// The retrieval and the indexing halves of the area (M2-18): without them search, answer,
	// citations, sync, rebuild, generations and invalidate answer 404.
	if err := ragServeProvideQueries(d, st, sections); err != nil {
		return err
	}
	if err := ragServeProvideIndexing(d, st); err != nil {
		return err
	}
	if d.AddCheck != nil {
		d.AddCheck(st.readiness())
	}
	if d.AddService != nil {
		d.AddService(st.start)
	}
	if ragOnCompose != nil {
		ragOnCompose(st)
	}
	return nil
}

func (c ragConfig) floor() int64 {
	if c.DiskFloorBytes == 0 {
		return ragDefaultDiskFloor
	}
	return c.DiskFloorBytes
}

// ragMountsOf is the operator's mounts as a source.MountResolver, nil when there are none.
func ragMountsOf(c ragConfig) source.MountResolver {
	if len(c.Mounts) == 0 {
		return nil
	}
	m := maps.Clone(c.Mounts)
	return func(id string) (string, bool) { p, ok := m[id]; return p, ok }
}

// ragInput is everything ragCompose builds from. serve fills it from the process;
// a test fills it with fakes.
type ragInput struct {
	DB        *store.DB
	Clock     clock.Clock
	Log       *slog.Logger
	Audit     audit.Appender
	Providers *providers.Registry
	Packages  ragPackageSource
	Config    ragConfig
	Ingest    ingest.Config
	Jobs      jobs.Config
	Query     query.Config
	Vector    vector.Config
	Parser    ingest.Parser
	// Disk is the disk-pressure hook; nil never pauses (tests only: serve always
	// supplies the production hook).
	Disk ingest.DiskPressure
	// SourceProviders serve the source types the packages declare.
	SourceProviders []source.Provider
	// Resources is the resource governor and its disk guard (cmd_resources.go). When set, the disk
	// guard is the pause's hook (Disk is not consulted) and the queue's policy is
	// gov.Policy(pause.Policy(nil)); the finished queue is handed back to it for the operations
	// surface. Nil (tests) keeps the pause over Disk and the pause's own policy.
	Resources *resourcesWiring
	// Reaudit finds the active package's declarations for a re-audit; nil turns
	// the re-audit trigger off (tests).
	Reaudit ragReauditSource
	// Quarantine is the set of derived indexes the upgrade isolated and has not rebuilt
	// (<data dir>/upgrade/quarantine). Retrieval consults it and labels what an isolated index
	// cannot answer (query.Deps.Quarantine); serve always supplies it (quarantineOf,
	// cmd_quarantine.go). Nil means nothing is isolated (tests).
	Quarantine *upgrade.Quarantine
	// DeploymentID is the deployment of the scope the scheduled maintenance jobs run under
	// (ragStack.maintainEvery); empty schedules none.
	DeploymentID string
}

// ragStack is the composed RAG part of the process.
type ragStack struct {
	log   *slog.Logger
	pause *ingest.Pause
	queue *jobs.Queue
	pool  *jobs.Pool
	// others are the service identities of other serve areas that run their jobs on this pool
	// (shareJobs).
	others   *ragServiceAuthorities
	ix       *index.Index
	vectors  vector.VectorIndexProvider
	svc      *ingest.Service
	pipeline *query.Pipeline
	sources  *ragSources
	// api is the registry as the /v1 API uses it (gateway.RAGSources, before the
	// gateway's scope guard wraps it).
	api    *ragSourcesService
	parser ingest.Parser
	// input is what the stack was composed from, with the configuration serve
	// resolved: the sections of the registry serve was started with.
	input ragInput
	// model is the embedding model of the indexes, "" when none is configured.
	model string
	// embedding is the embedder the ingestion pipeline embeds with, nil while ingestion is
	// text-only. A vector index the upgrade isolated is re-embedded with it (cmd_quarantine.go).
	embedding *ingest.Embedding

	mu    sync.Mutex
	steps []string
}

func (s *ragStack) step(name string) {
	s.mu.Lock()
	s.steps = append(s.steps, name)
	s.mu.Unlock()
}

// Steps returns the order the stack was built and started in.
func (s *ragStack) stepsDone() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.steps)
}

// ragCompose builds the stack. It touches no external resource: no process is
// started, no directory scanned, no connection opened (the parser worker is
// looked for when it is first used), so a deployment that never registers a
// source costs the structures below and nothing else.
func ragCompose(in ragInput) (*ragStack, error) {
	switch {
	case in.DB == nil:
		return nil, errors.New("rag: no store")
	case in.Audit == nil:
		return nil, errors.New("rag: no audit log: deletions cannot be recorded")
	case in.Packages == nil:
		return nil, errors.New("rag: no package source: sources cannot be authorised")
	case in.Parser == nil:
		return nil, errors.New("rag: no parser")
	}
	clk, log := in.Clock, in.Log
	if clk == nil {
		clk = clock.Real{}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	in.Log = log
	st := &ragStack{log: log, parser: in.Parser, input: in, others: &ragServiceAuthorities{}}

	// The pause is first: the queue takes its policy when it is built. With the resource
	// governor the disk guard (resources.disk_*) is the hook; without one (tests) it is in.Disk.
	disk := in.Disk
	if in.Resources != nil {
		disk = in.Resources.Disk
	}
	pause := ingest.NewPause(disk, clk, in.Ingest.PressureRecheck, log)
	st.pause = pause
	st.step("pause")

	// The governor's policy goes around the pause's: it holds classes 5 and 6 while a live turn
	// is active or memory is unknown or at its ceiling, then lets the pause decide.
	policy := pause.Policy(nil)
	if in.Resources != nil {
		policy = in.Resources.Gov.Policy(policy)
	}
	q, err := jobs.NewQueue(in.DB, jobs.Options{Config: ragEffectiveJobs(in.Jobs), Clock: clk, Logger: log, Policy: policy})
	if err != nil {
		return nil, fmt.Errorf("rag: the job queue: %w", err)
	}
	st.queue = q
	if in.Resources != nil {
		in.Resources.setQueue(q)
	}
	st.step("queue")

	ix, err := index.New(in.DB, clk, index.Config{})
	if err != nil {
		return nil, fmt.Errorf("rag: the index: %w", err)
	}
	chk, err := chunk.New(chunk.DefaultConfig())
	if err != nil {
		return nil, fmt.Errorf("rag: the chunker: %w", err)
	}
	st.ix = ix
	emb, err := ragEmbedding(in)
	if err != nil {
		return nil, err
	}
	var vectors vector.VectorIndexProvider
	if emb != nil {
		flat, err := vector.NewFlat(in.DB, vector.FlatOptions{Config: in.Vector, Clock: clk, Logger: log})
		if err != nil {
			return nil, fmt.Errorf("rag: the vector index: %w", err)
		}
		vectors = flat
		st.vectors = flat
		st.model = emb.Model
		st.embedding = emb
	}
	st.step("index")

	registry, err := ragsources.New(in.DB, clk)
	if err != nil {
		return nil, fmt.Errorf("rag: the source registry: %w", err)
	}
	sources := newRagSources(in.Packages, in.SourceProviders, ragMountsOf(in.Config), clk, log, registry)
	sources.reaudit = in.Reaudit
	st.sources = sources
	operators := ragOperators{entries: in.Config.Operators}

	svc, err := ingest.New(ingest.Deps{
		DB: in.DB, Clock: clk, Queue: q, Index: ix, Vectors: vectors, Embedding: emb,
		Registry: sources, Parser: in.Parser, Chunker: chk, Audit: in.Audit,
		Pause: pause, Operator: operators,
		Config: in.Ingest, Logger: log,
	})
	if err != nil {
		return nil, fmt.Errorf("rag: ingestion: %w", err)
	}
	st.svc = svc
	sources.bind(svc)
	st.api = ragSourcesNewService(sources, svc, operators, in.Packages)
	st.step("ingest")

	pool, err := jobs.NewPool(q, jobs.PoolOptions{
		ScopeChecker:      ragAuthorizer{packages: in.Packages, sources: sources},
		ServiceAuthorizer: ragAuthorizer{packages: in.Packages, sources: sources, others: st.others},
		Logger:            log,
	})
	if err != nil {
		return nil, fmt.Errorf("rag: the job pool: %w", err)
	}
	st.pool = pool
	st.step("pool")
	if err := svc.Handle(pool); err != nil {
		return nil, fmt.Errorf("rag: attach the ingestion handlers: %w", err)
	}
	st.step("handle")

	pipe, err := query.New(query.Deps{
		Lexical: ix, Catalog: sources, Vectors: ragVectorsOf(vectors), Hits: ix,
		Providers: in.Providers, EmbeddingModel: st.model, Clock: clk,
		Quarantine: quarantineQueryOf(in.Quarantine),
	}, in.Query)
	if err != nil {
		return nil, fmt.Errorf("rag: the query pipeline: %w", err)
	}
	st.pipeline = pipe
	st.step("query")
	return st, nil
}

// ragEffectiveJobs is the jobs configuration the queue is built with: the section's, with
// interactive_up_to_class raised to ragInteractiveUpTo. The resource governor copies its class
// boundaries from this (resourcesClassBoundaries), so the two never disagree.
func ragEffectiveJobs(c jobs.Config) jobs.Config {
	if c.InteractiveUpTo < ragInteractiveUpTo {
		c.InteractiveUpTo = ragInteractiveUpTo
	}
	return c
}

// ragVectorsOf converts an absent vector index to the nil interface query tests for.
func ragVectorsOf(v vector.VectorIndexProvider) query.Vectors {
	if v == nil {
		return nil
	}
	return v
}

// ragEmbedding enables queued embeddings when an embedder role is registered and
// the "rag" section names its model and version; otherwise ingestion indexes
// text only and queries are full-text, labelled reduced_semantic_recall.
func ragEmbedding(in ragInput) (*ingest.Embedding, error) {
	if in.Providers == nil || !in.Providers.Has(providers.RoleEmbedder) {
		return nil, nil
	}
	c := in.Config.Embedding
	if c.Model == "" {
		in.Log.Warn("rag: an embedder is registered but rag.embedding.model is not set: ingestion is text-only")
		return nil, nil
	}
	e, err := in.Providers.Embedder()
	if err != nil {
		in.Log.Warn("rag: the embedder role is not usable: ingestion is text-only", slog.String("code", string(contracts.CodeOf(err))))
		return nil, nil
	}
	name := c.ComponentName
	if name == "" {
		name = ragComponentName(c.Model)
	}
	return &ingest.Embedding{
		Embedder: e, Model: c.Model, BatchSize: c.BatchSize,
		Component: rag.ComponentVersion{Name: name, Version: actions.OpaqueVersion(c.ComponentVersion)},
	}, nil
}

// ragComponentName makes a component name (lower case letters, digits, '.',
// '_', '-') from a model identifier.
func ragComponentName(model string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(model) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s := strings.TrimLeft(b.String(), "-._")
	if len(s) > 64 {
		s = s[:64]
	}
	if s == "" {
		return "embedder"
	}
	return s
}

// start runs the ingestion side until ctx ends: the parser worker is probed
// first (a failed probe is logged and parse jobs fail closed and are retried,
// so nothing is parsed unconfined), then the pool leases work. It never blocks
// serve: both happen in a goroutine. The returned function stops the pool,
// waits for it, and closes the parser.
func (s *ragStack) start(ctx context.Context) (stop func()) {
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.sources.reconcileEvery(rctx, ragReconcileEvery)
	}()
	if s.input.DeploymentID != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.maintainEvery(rctx, ragMaintainEvery)
		}()
	}
	go func() {
		defer close(done)
		// The registrations read back at start are re-bound to the active package
		// before the pool leases its first job: a job for a registration that is
		// not bound yet is retried, never ended, but there is no reason to make it
		// wait for the first tick.
		if err := s.sources.Reconcile(rctx); err != nil && rctx.Err() == nil {
			s.log.Warn("rag: some registered sources could not be re-bound at start; they stay unserved and are retried",
				slog.String("code", string(contracts.CodeOf(err))))
		}
		rd, err := s.svc.Start(rctx)
		s.step("start")
		switch {
		case err != nil && rctx.Err() == nil:
			s.log.Warn("rag: ingestion starts without a ready parser worker; parse jobs fail closed and are retried",
				slog.String("err_text", err.Error()))
		case err == nil:
			s.log.Info("rag: ingestion started", slog.String("sandbox", rd.Level))
		}
		if rctx.Err() != nil {
			return
		}
		if err := s.pool.Run(rctx); err != nil && rctx.Err() == nil {
			s.log.Error("rag: the job pool stopped", slog.String("err_text", err.Error()))
		}
	}()
	return func() {
		cancel()
		<-done
		wg.Wait()
		if c, ok := s.parser.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	}
}

// ragMaintainEvery is how often the process queues the maintenance of every scope that holds a
// source (ingest.Service.ScheduleMaintenance).
var ragMaintainEvery = ingest.DefaultMaintenanceInterval

// maintainEvery queues the maintenance jobs at once and then every `every` until ctx ends, so
// what a deletion hid, a rebuild superseded or a failed build left behind is purged without
// waiting for a source removal. The jobs run on the stack's pool under ragServiceIdentity.
func (s *ragStack) maintainEvery(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		_, _ = s.maintainOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// maintainOnce is one pass of maintainEvery. A failed pass is logged and the next one goes on;
// a full queue stops a pass with resource_exhausted, which is the same.
func (s *ragStack) maintainOnce(ctx context.Context) (ingest.MaintenanceSchedule, error) {
	res, err := s.svc.ScheduleMaintenance(ctx, s.input.DeploymentID, ragServiceIdentity)
	if err != nil && ctx.Err() == nil {
		s.log.Warn("rag: queueing the maintenance jobs failed; the next pass goes on",
			slog.String("code", string(contracts.CodeOf(err))))
	}
	return res, err
}

// readiness is the /readyz check of ingestion. Registering a source is what
// puts the parser on the critical path, so the check fails only when a source is
// registered and the parser worker is not ready (or has not been checked yet);
// a deployment that never ingests is not made unready by a worker it never
// uses. The reason is a fixed phrase: /readyz is anonymous and the probe's own
// error carries the worker's host path, which goes to the operator's log (the
// "parser readiness" lines of ingest.Start), never to a caller.
func (s *ragStack) readiness() gateway.Check {
	return gateway.Check{Name: ragReadyCheck, Fn: func(context.Context) error {
		if s.sources.count() == 0 {
			return nil
		}
		rd, probed := s.svc.Readiness()
		switch {
		case !probed:
			return contracts.New(contracts.ErrUnavailable, "ingestion is starting: the document parser has not been checked yet")
		case !rd.Ready:
			return contracts.New(contracts.ErrUnavailable, "ingestion is unavailable: the document parser is not ready")
		}
		return nil
	}}
}

// ---- the parser -------------------------------------------------------------

// ragLazyParser is ingest.Parser over a parse.Runner that is built when it is
// first needed. serve starts with no worker installed (a deployment that never
// ingests), and a worker installed later is picked up by the next probe, which
// ingest repeats once a minute after a failure.
type ragLazyParser struct {
	cfg parse.Config
	log *slog.Logger

	mu sync.Mutex
	r  *parse.Runner
}

func newRagLazyParser(cfg ingest.Config, log *slog.Logger) *ragLazyParser {
	pc := cfg.ParseConfig()
	if pc.WorkerPath == "" {
		if p, err := parse.DefaultWorkerPath(); err == nil {
			pc.WorkerPath = p
		}
	}
	return &ragLazyParser{cfg: pc, log: log}
}

func (p *ragLazyParser) runner() (*parse.Runner, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.r != nil {
		return p.r, nil
	}
	r, err := parse.NewRunner(p.cfg)
	if err != nil {
		return nil, err
	}
	p.r = r
	return r, nil
}

// Parse implements ingest.Parser.
func (p *ragLazyParser) Parse(ctx context.Context, f wire.Format, doc []byte) (*parse.Result, error) {
	r, err := p.runner()
	if err != nil {
		return nil, err
	}
	return r.Parse(ctx, f, doc)
}

// Probe implements ingest.Parser.
func (p *ragLazyParser) Probe(ctx context.Context) (parse.SandboxReport, error) {
	r, err := p.runner()
	if err != nil {
		return parse.SandboxReport{}, err
	}
	return r.Probe(ctx)
}

// Component implements ingest.Parser.
func (p *ragLazyParser) Component() rag.ComponentVersion { return parse.Component() }

// Close removes the runner's working directory.
func (p *ragLazyParser) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.r == nil {
		return nil
	}
	err := p.r.Close()
	p.r = nil
	return err
}

// ---- disk pressure ----------------------------------------------------------

// ragDiskPressure is the production disk-pressure hook: ingestion pauses while
// the volume that holds the data directory has less than floor bytes available
// to this process (master plan 18, "pause ingestion before interactive or
// transactional storage is endangered"). A volume that cannot be measured is an
// error, which the Pause reads as pressure: ingestion fails closed rather than
// fill a disk it could not measure. The measurement is ragFreeBytes, one small
// file per platform family.
type ragDiskPressure struct {
	dir   string
	floor int64
	free  func(dir string) (uint64, error)
}

// UnderPressure implements ingest.DiskPressure.
func (d ragDiskPressure) UnderPressure(context.Context) (bool, error) {
	free, err := d.free(d.dir)
	if err != nil {
		return false, contracts.Wrap(contracts.ErrUnavailable, err, "rag: the free space of the data directory cannot be read")
	}
	return d.floor > 0 && free < uint64(d.floor), nil
}

// ---- who may do an operator action ------------------------------------------

// ragOperators is ingest.OperatorAuthorizer over the "rag" section's list of
// verified principals. The decision uses the application and principal of the
// verified Scope (identity.Middleware built it from a token signed by a key the
// deployment's operator provisioned) and nothing the scope's roles or
// capability scopes say: a host application asserts those itself, and a
// package or a model has no say at all. With no entry for the principal, and
// for an action the entry does not list, the answer is denied.
type ragOperators struct{ entries []ragOperator }

// AuthorizeOperator implements ingest.OperatorAuthorizer.
func (o ragOperators) AuthorizeOperator(_ context.Context, scope contracts.Scope, action ingest.OperatorAction) error {
	if err := scope.Validate(); err != nil {
		return contracts.Wrap(contracts.ErrPermissionDenied, err, "rag: the operator action needs a verified scope")
	}
	for _, e := range o.entries {
		if e.ApplicationID == scope.ApplicationID && e.PrincipalID == scope.PrincipalID && slices.Contains(e.Actions, string(action)) {
			return nil
		}
	}
	return contracts.New(contracts.ErrPermissionDenied, "rag: this principal is not a deployment operator for that action")
}

// ---- the job pool's authority -------------------------------------------------

// ragAuthorizer is jobs.ScopeChecker and jobs.ServiceAuthorizer for the pool.
//
// CheckScope re-validates a job's initiating scope just before it runs: the
// scope must be complete and the application must still have an active package
// (a quarantined, rejected or uninstalled application runs nothing). There is no
// principal or session revocation state in this build to consult, so that is all
// it can establish; a long ingestion is queued under the service identity below
// precisely so that it does not depend on a session that will have ended.
//
// AuthorizeService accepts only ragServiceIdentity, only for the job types of
// this pipeline, only while the application has an active package, and, for a
// job that names a source, only when that source is registered for the job's own
// scope. A declared identity alone grants nothing (jobs.ServiceAuthorizer).
//
// Another serve area that runs its own job type on this pool registers its identity
// with ragStack.shareJobs, and the authority for that identity is its own: this
// authorizer hands it the job and denies what it denies. An identity nobody
// registered is denied, as before.
type ragAuthorizer struct {
	packages ragPackageSource
	sources  *ragSources
	others   *ragServiceAuthorities
}

// ragServiceAuthorities are the service identities, other than ragServiceIdentity, the pool
// accepts, each with the authority that decides for it. The zero value accepts none.
type ragServiceAuthorities struct {
	mu sync.RWMutex
	by map[string]jobs.ServiceAuthorizer
}

func (r *ragServiceAuthorities) add(identity string, a jobs.ServiceAuthorizer) error {
	if r == nil || a == nil || identity == "" || identity == ragServiceIdentity {
		return contracts.New(contracts.ErrValidationFailed, "rag: a shared service identity needs its own name and authority")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.by[identity]; dup {
		return contracts.Newf(contracts.ErrConflict, "rag: the service identity %q is already registered on the pool", identity)
	}
	if r.by == nil {
		r.by = map[string]jobs.ServiceAuthorizer{}
	}
	r.by[identity] = a
	return nil
}

func (r *ragServiceAuthorities) lookup(identity string) jobs.ServiceAuthorizer {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.by[identity]
}

// shareJobs lets another serve area run a job type on the stack's one queue and pool (never a
// second pool): the pool accepts identity through authority, and handle attaches the area's
// handlers to the pool. It is for the providers that run after this one (name order) and finds
// the stack through the services registry; the pool starts leasing only when serve starts it.
func (s *ragStack) shareJobs(identity string, authority jobs.ServiceAuthorizer, handle func(*jobs.Pool) error) error {
	if err := s.others.add(identity, authority); err != nil {
		return err
	}
	return handle(s.pool)
}

// CheckScope implements jobs.ScopeChecker.
func (a ragAuthorizer) CheckScope(ctx context.Context, scope contracts.Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	_, active, err := ragActive(ctx, a.packages, scope.ApplicationID)
	switch {
	case err != nil:
		return ragRetryableOrDenied(err)
	case !active:
		return contracts.New(contracts.ErrPermissionDenied, "the application has no active package")
	}
	return nil
}

// ragJobTypes are the job types this pipeline runs, the embedding job included.
var ragJobTypes = func() map[string]bool {
	m := map[string]bool{vector.EmbedJobType: true}
	for _, sp := range ingest.JobSpecs() {
		m[sp.Name] = true
	}
	return m
}()

// AuthorizeService implements jobs.ServiceAuthorizer.
func (a ragAuthorizer) AuthorizeService(ctx context.Context, identity string, job jobs.Job) error {
	if identity != ragServiceIdentity {
		if other := a.others.lookup(identity); other != nil {
			return other.AuthorizeService(ctx, identity, job)
		}
		return contracts.New(contracts.ErrPermissionDenied, "unknown service identity")
	}
	if !ragJobTypes[job.Type] {
		return contracts.New(contracts.ErrPermissionDenied, "the service identity does not run this job type")
	}
	if err := a.CheckScope(ctx, job.Scope); err != nil {
		return err
	}
	if job.Type == ingest.JobMaintain {
		return nil
	}
	var p struct {
		SourceID string `json:"source_id"`
	}
	if err := json.Unmarshal(job.Payload, &p); err != nil || p.SourceID == "" {
		return contracts.New(contracts.ErrPermissionDenied, "the job names no source")
	}
	if a.sources.lookup(job.Scope, p.SourceID) == nil {
		return contracts.New(contracts.ErrPermissionDenied, "the source is not registered for the job's scope")
	}
	return nil
}

// ragRetryableOrDenied keeps the code of an error that means "could not decide"
// (the pool retries the attempt) and turns anything else into a denial.
func ragRetryableOrDenied(err error) error {
	if code := contracts.CodeOf(err); code != "" && code.Retryable() {
		return err
	}
	return contracts.Wrap(contracts.ErrPermissionDenied, err, "the application's package cannot be established")
}

// ---- packages ---------------------------------------------------------------

// ragActivePackage is what the registry asks of the application's active
// package. capabilities.Package has all of it.
type ragActivePackage interface {
	ApplicationID() string
	Status() policy.PackageStatus
	// ContentHash is the content address of the package: a registration was built
	// from exactly one.
	ContentHash() string
}

// ragPackageSource returns the application's active package. A package that is
// not active (quarantined, rejected, not installed, no longer verifying) is
// contracts.ErrQuarantined; any other error means the answer could not be
// established.
type ragPackageSource interface {
	ActivePackage(ctx context.Context, applicationID string) (ragActivePackage, error)
}

// ragPackageSourceFunc adapts a function to ragPackageSource.
type ragPackageSourceFunc func(ctx context.Context, applicationID string) (ragActivePackage, error)

// ActivePackage calls f.
func (f ragPackageSourceFunc) ActivePackage(ctx context.Context, applicationID string) (ragActivePackage, error) {
	return f(ctx, applicationID)
}

// ragPackagesOf is the application service as a ragPackageSource: the one source
// of packages the dialogue engine, the capability registry and the action
// coordinator already decide against.
func ragPackagesOf(apps gateway.ApplicationService) ragPackageSource {
	return ragPackageSourceFunc(func(ctx context.Context, applicationID string) (ragActivePackage, error) {
		p, err := apps.Package(ctx, applicationID)
		if err != nil {
			return nil, err
		}
		if p == nil {
			return nil, contracts.New(contracts.ErrQuarantined, "the application has no active package")
		}
		return p, nil
	})
}

// ragActive reports the application's active package. active is false (and err
// nil) for a package that is not active; err is set when that could not be
// established, and the caller must then fail closed.
func ragActive(ctx context.Context, src ragPackageSource, applicationID string) (pkg ragActivePackage, active bool, err error) {
	p, err := src.ActivePackage(ctx, applicationID)
	switch {
	case contracts.CodeOf(err) == contracts.ErrQuarantined:
		return nil, false, nil
	case err != nil:
		return nil, false, err
	case p == nil || p.Status() != policy.StatusActive || p.ApplicationID() != applicationID:
		return nil, false, nil
	}
	return p, true, nil
}

// ---- the source registry and catalog ----------------------------------------

// ragKey identifies one registration: a source id in one scope.
type ragKey struct{ app, tenant, workspace, id string }

func ragKeyOf(scope contracts.Scope, id string) ragKey {
	return ragKey{scope.ApplicationID, scope.TenantID, scope.WorkspaceID, id}
}

type ragEntry struct {
	def      source.Definition
	provider source.Provider
	// scope is the scope the source was registered under; re-audit and removal
	// queue their work under it (with the service identity).
	scope contracts.Scope
	// removing is set when the removal began: the source is no longer served.
	removing bool
	// bound is true once def is the definition built from the active package. A
	// registration read back from the durable registry at start is not bound: def
	// then holds only what the registry keeps (the ids, the declaration it came
	// from, the package hash it was last bound to), never a root or a rule, and
	// the source is not served until a re-bind (Reaudit) has rebuilt it. It is
	// still registered: a job for it is retried, not denied.
	bound bool
}

// ragSources is the registry of the sources this process serves. It is
// ingest.Registry (what a job resolves a source id to), query.SourceCatalog
// (which sources a principal may read, and in what state), and the place
// sources are registered, re-audited and removed.
//
// Authority. A source is served only while all of these hold, checked again on
// every lookup and never cached: it is registered for the exact scope (the
// application, tenant and workspace of the verified scope, Definition.CheckScope),
// the application's package is active, and the registration was built from that
// very package (Definition.ManifestHash equals the package's content hash), so a
// package that was replaced, re-audited into a different content or revoked stops
// serving its sources at once and until Reaudit rebinds them. A source that is not
// held, or fails any of these, is not returned: it reads exactly like one that does
// not exist.
//
// Every registration is also in the durable registry (internal/rag/sources),
// written before the catalogue serves it and read back by load when the process
// starts; the entries below are the catalogue's working copy of it, with the
// definitions rebuilt from the active package.
type ragSources struct {
	packages  ragPackageSource
	providers []source.Provider
	mounts    source.MountResolver
	clk       clock.Clock
	log       *slog.Logger
	// store is the durable registry.
	store *ragsources.Registry

	// reaudit finds the active package's declarations for Reconcile; nil means
	// registrations are only re-bound by an explicit Reaudit.
	reaudit ragReauditSource

	mu      sync.RWMutex
	entries map[ragKey]*ragEntry
	svc     *ingest.Service
	// retryAt holds back a re-audit of an application's package content that
	// failed, until the time it maps to.
	retryAt map[string]time.Time
	// states caches the availability and staleness label of a source for
	// statusTTL (see Sources). It never holds an authorisation.
	states    map[ragKey]ragCachedState
	statusTTL time.Duration
}

type ragCachedState struct {
	state query.SourceState
	at    time.Time
}

// ragStatusTTL is how long the availability label of a source is reused. The
// ingestion status behind it counts the source's versions, which costs about
// 1.4 microseconds a document (1.6 ms at 500 documents, 3.7 ms at 2,000), and the
// pipeline asks twice per query. What a label says follows scans, which run on
// the scale of minutes, so a few seconds of reuse costs nothing; authorisation
// is never part of it and is decided afresh on every call.
var ragStatusTTL = 5 * time.Second

var (
	_ ingest.Registry     = (*ragSources)(nil)
	_ query.SourceCatalog = (*ragSources)(nil)
)

func newRagSources(p ragPackageSource, provs []source.Provider, mounts source.MountResolver, clk clock.Clock, log *slog.Logger, store *ragsources.Registry) *ragSources {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &ragSources{packages: p, providers: provs, mounts: mounts, clk: clk, log: log, store: store, entries: map[ragKey]*ragEntry{}, retryAt: map[string]time.Time{},
		states: map[ragKey]ragCachedState{}, statusTTL: ragStatusTTL}
}

// load reads the registrations of the last run back from the durable registry,
// as registrations that are not bound to the active package yet (ragEntry.bound):
// registered, so that the jobs queued before the restart are answered and the
// re-bind knows what to rebuild, and not served until it has. It reads the whole
// deployment, once, before anything asks the catalogue (the registry's LoadAll
// says why that is the one read across scopes); a registration this process
// already holds is left as it is. A store that cannot be read, or a row that is
// not a registration, is an error: the process does not start with a catalogue
// it cannot trust.
func (r *ragSources) load(ctx context.Context) error {
	regs, err := r.store.LoadAll(ctx)
	if err != nil {
		return fmt.Errorf("rag: read the registered sources: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, reg := range regs {
		key := ragKeyOf(reg.Scope, reg.SourceID)
		if _, held := r.entries[key]; held {
			continue
		}
		r.entries[key] = &ragEntry{
			def: source.Definition{
				SourceID: reg.SourceID, PackageSourceID: reg.PackageSourceID, PackageVersion: reg.PackageVersion,
				ManifestHash: reg.ManifestHash, ApplicationID: reg.Scope.ApplicationID,
				Tenant:    source.ScopeBinding{ID: reg.BoundTenantID},
				Workspace: source.ScopeBinding{ID: reg.BoundWorkspaceID},
			},
			scope: ragCopyScope(reg.Scope), removing: reg.Removing,
		}
	}
	return nil
}

// bind gives the registry the ingestion service it asks to re-check a definition.
// It exists because the service is built from the registry.
func (r *ragSources) bind(svc *ingest.Service) {
	r.mu.Lock()
	r.svc = svc
	r.mu.Unlock()
}

// MountResolver is the operator's mounts ("rag.mounts"), for source.Binding.
func (r *ragSources) MountResolver() source.MountResolver { return r.mounts }

func (r *ragSources) count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

func (r *ragSources) providerFor(t ap.RAGSourceType) source.Provider {
	for _, p := range r.providers {
		if slices.Contains(p.Kinds(), t) {
			return p
		}
	}
	return nil
}

// lookup returns the registration of a source id in the scope, nil when there
// is none or it is being removed.
func (r *ragSources) lookup(scope contracts.Scope, id string) *ragEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e := r.entries[ragKeyOf(scope, id)]
	if e == nil || e.removing {
		return nil
	}
	cp := *e
	return &cp
}

// Resolve implements ingest.Registry. A source the registry does not hold, or
// whose package is not active or no longer the one it was built from, is
// ingest.ErrSourceGone (the job ends without touching anything); an error that
// only means the package state could not be read is returned as it is, and is
// never read as a removal.
func (r *ragSources) Resolve(ctx context.Context, scope contracts.Scope, sourceID string) (ingest.Resolved, error) {
	e := r.lookup(scope, sourceID)
	if e == nil {
		return ingest.Resolved{}, ingest.ErrSourceGone
	}
	pkg, active, err := ragActive(ctx, r.packages, e.def.ApplicationID)
	switch {
	case err != nil:
		return ingest.Resolved{}, err
	case !active:
		return ingest.Resolved{}, ingest.ErrSourceGone
	case !e.bound:
		// Registered and read back at start, not rebuilt from the package yet:
		// the job is retried once the re-bind has run, never ended as if the
		// source were gone.
		return ingest.Resolved{}, contracts.New(contracts.ErrUnavailable, "rag: the registration is being re-bound to the active package")
	case pkg.ContentHash() != e.def.ManifestHash, e.def.CheckScope(scope) != nil:
		return ingest.Resolved{}, ingest.ErrSourceGone
	}
	return ingest.Resolved{Def: e.def, Provider: e.provider}, nil
}

// Sources implements query.SourceCatalog. See ragSources for what makes a
// source readable. The state it returns is read from the ingestion service, so a
// source that could not be reached is never labelled checked.
func (r *ragSources) Sources(ctx context.Context, scope contracts.Scope, sourceIDs []string) (map[string]query.SourceState, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	out := map[string]query.SourceState{}
	if len(sourceIDs) == 0 {
		return out, nil
	}
	pkg, active, err := ragActive(ctx, r.packages, scope.ApplicationID)
	switch {
	case err != nil:
		return nil, err
	case !active:
		return out, nil
	}
	r.mu.RLock()
	svc := r.svc
	r.mu.RUnlock()
	if svc == nil {
		return nil, contracts.New(contracts.ErrInternal, "rag: the registry is not bound to the ingestion service")
	}
	for _, id := range sourceIDs {
		e := r.lookup(scope, id)
		if e == nil || !e.bound || e.def.ManifestHash != pkg.ContentHash() || e.def.CheckScope(scope) != nil {
			continue
		}
		key := ragKeyOf(scope, id)
		if st, ok := r.cachedState(key); ok {
			out[id] = st
			continue
		}
		info, err := svc.Status(ctx, scope, id)
		switch {
		case err == nil:
		case contracts.CodeOf(err) == contracts.ErrValidationFailed:
			// Registered, not scanned yet: nothing indexed is claimed as checked.
			// Not cached: the read that said so is cheap, and the first scan ends it.
			out[id] = query.SourceState{DisplayName: e.def.Title, Authorised: true}
			continue
		default:
			return nil, err
		}
		st := r.stateOf(e.def, info)
		r.storeState(key, st)
		out[id] = st
	}
	return out, nil
}

// cachedState returns the label of a source read less than statusTTL ago.
func (r *ragSources) cachedState(key ragKey) (query.SourceState, bool) {
	if r.statusTTL <= 0 {
		return query.SourceState{}, false
	}
	r.mu.RLock()
	c, ok := r.states[key]
	r.mu.RUnlock()
	if !ok || r.clk.Now().Sub(c.at) >= r.statusTTL || r.clk.Now().Before(c.at) {
		return query.SourceState{}, false
	}
	return c.state, true
}

func (r *ragSources) storeState(key ragKey, st query.SourceState) {
	if r.statusTTL <= 0 {
		return
	}
	r.mu.Lock()
	r.states[key] = ragCachedState{state: st, at: r.clk.Now()}
	r.mu.Unlock()
}

// stateOf maps the ingestion status of a source to what a citation may say.
// The declaration of a source in this package schema states no data-egress mode
// or classification, so none is copied and the evidence package carries the most
// restrictive values (query.SourceState).
func (r *ragSources) stateOf(def source.Definition, info ingest.SourceInfo) query.SourceState {
	st := query.SourceState{DisplayName: def.Title, Authorised: true}
	switch {
	case info.Reason == ingest.ReasonRemoved || info.Reason == ingest.ReasonTenantDeleted:
		st.Availability = conversation.CitationAvailabilityRemoved
	case !info.Available:
		st.Availability = conversation.CitationAvailabilityUnavailable
	default:
		st.Availability = conversation.CitationAvailabilityAvailable
		st.Staleness = conversation.CitationStalenessUnknown
		if def.Sync.Method == ap.RAGSourceSyncMethodPoll && def.Sync.Interval > 0 && !info.LastSuccess.IsZero() {
			if r.clk.Now().Sub(info.LastSuccess) <= 2*def.Sync.Interval {
				st.Staleness = conversation.CitationStalenessCurrent
			} else {
				st.Staleness = conversation.CitationStalenessStale
			}
		}
	}
	return st
}

// Register registers a source for the scope and has ingestion compare it with
// what was last scanned (ingest.Service.CheckDefinition), which queues a sync
// when the source is new or its rules changed. The definition must have been
// built from the application's active package (source.FromPackage): a
// definition of any other origin is refused. It is idempotent; registering the
// same source again replaces the definition. The registration is recorded in the
// durable registry before it is served, so a failure to record it registers
// nothing; a failure to queue the sync is returned and leaves the registration
// in place, so repeating the call retries.
func (r *ragSources) Register(ctx context.Context, scope contracts.Scope, def source.Definition) error {
	if err := def.CheckScope(scope); err != nil {
		return err
	}
	if !ids.Valid(def.SourceID, source.IDPrefix) {
		return contracts.New(contracts.ErrValidationFailed, "rag: not a source id")
	}
	prov := r.providerFor(def.Type)
	if prov == nil {
		return contracts.Newf(contracts.ErrUnavailable, "rag: no provider serves sources of type %s", string(def.Type))
	}
	pkg, active, err := ragActive(ctx, r.packages, def.ApplicationID)
	switch {
	case err != nil:
		return err
	case !active:
		return contracts.New(contracts.ErrQuarantined, "rag: the application has no active package; a source cannot be registered")
	case def.ManifestHash == "" || def.ManifestHash != pkg.ContentHash():
		return contracts.New(contracts.ErrConflict, "rag: the definition was not built from the application's active package")
	}
	// A folder or mount root is resolved now, with every refusal of the provider (a system, home, data or install
	// directory), and the operator's log shows where it really points. The path is the host's: it is logged, never
	// returned to the caller or put in the source's status, which any member of the scope can read.
	if res, ok := prov.(interface {
		ResolveRoot(source.Definition) (string, error)
	}); ok && (def.Root.Kind == source.RootFolder || def.Root.Kind == source.RootMount) {
		resolved, err := res.ResolveRoot(def)
		if err != nil && contracts.CodeOf(err) == contracts.ErrValidationFailed {
			return err
		}
		if r.log != nil {
			if err != nil {
				r.log.Warn("rag source registered; its root cannot be reached yet", slog.String("source_id", def.SourceID),
					slog.String("package_source_id", def.PackageSourceID), slog.String("root", def.Root.Path), slog.String("reason", contracts.CodeOf(err).String()))
			} else {
				r.log.Info("rag source registered", slog.String("source_id", def.SourceID),
					slog.String("package_source_id", def.PackageSourceID), slog.String("root", def.Root.Path), slog.String("resolved_root", resolved))
			}
		}
	}
	if err := r.store.Put(ctx, ragsources.Registration{
		Scope: ragCopyScope(scope), SourceID: def.SourceID, PackageSourceID: def.PackageSourceID, PackageVersion: def.PackageVersion,
		ManifestHash: def.ManifestHash, BoundTenantID: def.Tenant.ID, BoundWorkspaceID: def.Workspace.ID,
	}); err != nil {
		return err
	}
	r.mu.Lock()
	svc := r.svc
	r.entries[ragKeyOf(scope, def.SourceID)] = &ragEntry{def: def, provider: prov, scope: ragCopyScope(scope), bound: true}
	delete(r.states, ragKeyOf(scope, def.SourceID))
	r.mu.Unlock()
	return r.check(ctx, svc, scope, def.SourceID)
}

// check runs CheckDefinition for a registered source, under the service identity.
func (r *ragSources) check(ctx context.Context, svc *ingest.Service, scope contracts.Scope, id string) error {
	if svc == nil {
		return contracts.New(contracts.ErrInternal, "rag: the registry is not bound to the ingestion service")
	}
	if _, _, err := svc.CheckDefinition(ctx, scope, id, ingest.SyncOptions{ServiceIdentity: ragServiceIdentity}); err != nil {
		return err
	}
	return nil
}

// ragRebuild rebuilds a registered source's definition from a re-audited package:
// the new definition, false when the package no longer declares the source.
type ragRebuild func(old source.Definition) (source.Definition, bool, error)

// ragRebuildFrom is the rebuild of a loaded package: each source is declared
// again under its package-level name and bound as it was registered.
func ragRebuildFrom(pkg *apppkg.Package, mounts source.MountResolver) ragRebuild {
	return func(old source.Definition) (source.Definition, bool, error) {
		if _, declared := pkg.Sources[old.PackageSourceID]; !declared {
			return source.Definition{}, false, nil
		}
		def, err := source.FromPackage(pkg, old.PackageSourceID, source.Binding{
			SourceID: old.SourceID, TenantID: old.Tenant.ID, WorkspaceID: old.Workspace.ID, Mounts: mounts,
		})
		return def, true, err
	}
}

// ragSourcesDeclareFrom is the registration of a source a loaded package declares: it is
// bound to the scope's tenant and workspace the way its declaration says (a
// request-bound level names the scope's, a shared one names none, a fixed one
// takes the declaration's own), and the result still has to pass
// Definition.CheckScope for the scope, which refuses a fixed level that is
// another tenant's.
func ragSourcesDeclareFrom(pkg *apppkg.Package, mounts source.MountResolver) func(scope contracts.Scope, packageSourceID, sourceID string) (source.Definition, bool, error) {
	return func(scope contracts.Scope, packageSourceID, sourceID string) (source.Definition, bool, error) {
		decl, declared := pkg.Sources[packageSourceID]
		if !declared {
			return source.Definition{}, false, nil
		}
		b := source.Binding{SourceID: sourceID, Mounts: mounts}
		if decl.Scope.Tenant.Mode == ap.RAGSourceScopeBindingModeRequest {
			b.TenantID = scope.TenantID
		}
		if decl.Scope.Workspace.Mode == ap.RAGSourceScopeBindingModeRequest {
			b.WorkspaceID = scope.WorkspaceID
		}
		def, err := source.FromPackage(pkg, packageSourceID, b)
		return def, true, err
	}
}

// ragReauditReport counts what a Reaudit did.
type ragReauditReport struct {
	// Checked registrations of the application, Rebound to the new package,
	// Queued of those whose definition changed and a sync was queued, Removed
	// because the package no longer declares them, Failed that could not be
	// rebuilt or checked (they stay unserved: their package hash is stale).
	Checked, Rebound, Queued, Removed, Failed int
}

// Reaudit is what runs after an application's package is re-audited and active
// again: every source registered for the application is rebuilt from the new
// package and bound to it (until then it is not served: its registration names
// the old package's hash), and ingestion compares the new definition with the
// last scan's and queues a sync when the declaration's rules changed
// (ingest.Service.CheckDefinition). A source the package no longer declares is
// removed, which applies its declared deletion behaviour. The first error is
// returned after every registration has been tried.
func (r *ragSources) Reaudit(ctx context.Context, applicationID string, rebuild ragRebuild) (ragReauditReport, error) {
	return r.reauditWhere(ctx, applicationID, rebuild, func(e *ragEntry) bool { return e.def.ApplicationID == applicationID })
}

// reauditScope is Reaudit for the registrations of one scope only: what the
// /v1 API may ask for. Another tenant's registrations are neither re-bound nor,
// when the package dropped their declaration, removed by it.
func (r *ragSources) reauditScope(ctx context.Context, scope contracts.Scope, rebuild ragRebuild) (ragReauditReport, error) {
	return r.reauditWhere(ctx, scope.ApplicationID, rebuild, func(e *ragEntry) bool {
		return e.def.ApplicationID == scope.ApplicationID && e.scope.TenantID == scope.TenantID && e.scope.WorkspaceID == scope.WorkspaceID
	})
}

func (r *ragSources) reauditWhere(ctx context.Context, applicationID string, rebuild ragRebuild, selected func(*ragEntry) bool) (ragReauditReport, error) {
	var rep ragReauditReport
	pkg, active, err := ragActive(ctx, r.packages, applicationID)
	switch {
	case err != nil:
		return rep, err
	case !active:
		return rep, contracts.New(contracts.ErrQuarantined, "rag: the application has no active package; nothing is re-bound")
	}
	r.mu.RLock()
	svc := r.svc
	var todo []ragEntry
	for _, e := range r.entries {
		if selected(e) && !e.removing {
			todo = append(todo, *e)
		}
	}
	r.mu.RUnlock()
	slices.SortFunc(todo, func(a, b ragEntry) int { return strings.Compare(a.def.SourceID, b.def.SourceID) })
	if svc == nil && len(todo) > 0 {
		return rep, contracts.New(contracts.ErrInternal, "rag: the registry is not bound to the ingestion service")
	}

	var first error
	fail := func(err error) {
		rep.Failed++
		if first == nil {
			first = err
		}
	}
	for _, old := range todo {
		rep.Checked++
		def, declared, err := rebuild(old.def)
		switch {
		case err != nil:
			fail(err)
			continue
		case !declared:
			if err := r.Remove(ctx, old.scope, old.def.SourceID); err != nil {
				fail(err)
				continue
			}
			rep.Removed++
			continue
		}
		if def.SourceID != old.def.SourceID || def.ApplicationID != applicationID || def.ManifestHash != pkg.ContentHash() || def.CheckScope(old.scope) != nil {
			fail(contracts.New(contracts.ErrConflict, "rag: the rebuilt definition is not the registered source's, or not from the active package"))
			continue
		}
		prov := r.providerFor(def.Type)
		if prov == nil {
			fail(contracts.Newf(contracts.ErrUnavailable, "rag: no provider serves sources of type %s", string(def.Type)))
			continue
		}
		// The new definition is bound first, because ingestion resolves it through
		// the registry to compare it with the last scan's. If ingestion cannot
		// record the comparison (a full queue, a busy store), or the durable
		// registry cannot record the new binding, the old registration is put back:
		// the source stays unserved, because its package hash is stale (or it was
		// never rebuilt), and the next Reconcile tries again. The stored hash moves
		// only from the value this registration was read with and only after
		// ingestion accepted the definition, so a re-bind that was refused leaves
		// the durable registry as it was.
		r.mu.Lock()
		cur := r.entries[ragKeyOf(old.scope, def.SourceID)]
		var prior ragEntry
		if cur != nil && !cur.removing {
			prior = *cur
			cur.def, cur.provider, cur.bound = def, prov, true
			delete(r.states, ragKeyOf(old.scope, def.SourceID))
		} else {
			cur = nil
		}
		r.mu.Unlock()
		queued, _, err := svc.CheckDefinition(ctx, old.scope, def.SourceID, ingest.SyncOptions{ServiceIdentity: ragServiceIdentity})
		if err == nil {
			var moved bool
			if moved, err = r.store.Rebind(ctx, old.scope, def.SourceID, old.def.ManifestHash, def.ManifestHash, def.PackageVersion); err == nil && !moved {
				err = contracts.New(contracts.ErrConflict, "rag: the registration changed while it was being re-bound")
			}
		}
		if err != nil {
			if cur != nil {
				r.mu.Lock()
				if now := r.entries[ragKeyOf(old.scope, def.SourceID)]; now == cur && !now.removing {
					now.def, now.provider, now.bound = prior.def, prior.provider, prior.bound
				}
				r.mu.Unlock()
			}
			fail(err)
			continue
		}
		rep.Rebound++
		if queued {
			rep.Queued++
		}
	}
	return rep, first
}

// ragReauditSource gives the registry what it needs from an application's active
// package: its content hash and the rebuild of a registered source from it (a
// re-audit, or the re-bind of a registration read back at start), and the
// definition of a source the package declares, bound to a scope (a registration).
type ragReauditSource interface {
	Active(ctx context.Context, applicationID string) (hash string, rebuild ragRebuild, err error)
	// Declare builds the definition of the source the active package declares
	// under packageSourceID, for the scope's tenant and workspace and with the
	// core-minted sourceID. declared is false when the package declares no such
	// source. The definition carries the package's content hash; the caller
	// compares it with the hash it saw (ragSources.Register does).
	Declare(ctx context.Context, scope contracts.Scope, packageSourceID, sourceID string) (def source.Definition, declared bool, err error)
}

// ragInstallerSource reads the active package from the installer, like the
// application service does: the installed record in state active, opened with
// the full checks (signature, content hash) so a package that no longer verifies
// yields nothing.
type ragInstallerSource struct {
	inst   *apppkg.Installer
	mounts source.MountResolver
	// net, when set, learns the network the package's API sources may use each time the package is
	// opened (see open). Nil in a test that has no API source.
	net *ragServeNetwork
}

// open reads the application's active package from the installer. An
// application with no active package is contracts.ErrQuarantined.
func (s ragInstallerSource) open(ctx context.Context, applicationID string) (string, *apppkg.Package, error) {
	recs, err := s.inst.List(ctx, applicationID)
	if err != nil {
		return "", nil, err
	}
	hash := ""
	for _, r := range recs {
		if r.State == apppkg.StateActive {
			hash = r.ManifestHash
		}
	}
	if hash == "" {
		return "", nil, contracts.New(contracts.ErrQuarantined, "the application has no active package")
	}
	pkg, _, err := s.inst.Open(ctx, applicationID, hash)
	if err != nil {
		return "", nil, err
	}
	// The network of the application's API sources is the active package's: every registration and
	// every re-bind (the one at start included) passes through here, before any scan is queued.
	s.net.learn(applicationID, pkg)
	return hash, pkg, nil
}

// Active implements ragReauditSource. An application with no active package is
// contracts.ErrQuarantined.
func (s ragInstallerSource) Active(ctx context.Context, applicationID string) (string, ragRebuild, error) {
	hash, pkg, err := s.open(ctx, applicationID)
	if err != nil {
		return "", nil, err
	}
	return hash, ragRebuildFrom(pkg, s.mounts), nil
}

// Declare implements ragReauditSource.
func (s ragInstallerSource) Declare(ctx context.Context, scope contracts.Scope, packageSourceID, sourceID string) (source.Definition, bool, error) {
	_, pkg, err := s.open(ctx, scope.ApplicationID)
	if err != nil {
		return source.Definition{}, false, err
	}
	return ragSourcesDeclareFrom(pkg, s.mounts)(scope, packageSourceID, sourceID)
}

// ragReconcileEvery is how often the process looks for registrations whose
// package was replaced, and ragReauditBackoff how long a re-audit that failed
// for one package content waits before it is tried again.
var ragReconcileEvery = 30 * time.Second

const ragReauditBackoff = 5 * time.Minute

// Reconcile is the re-audit trigger of the process: for every application that
// has a registered source whose package content is no longer the active
// package's, it rebuilds the registrations from the active package (Reaudit),
// which binds them again and has ingestion compare the rules with the last
// scan's. It is cheap when nothing changed (one package lookup per application,
// from the application service's cache) and reads the package from disk only when
// the content changed. Until it succeeds the stale registrations are not served.
// A failure is logged without any package text and is not repeated for the same
// content until ragReauditBackoff has passed. The first error is returned.
func (r *ragSources) Reconcile(ctx context.Context) error {
	if r.reaudit == nil {
		return nil
	}
	r.mu.RLock()
	seen := map[string]bool{}
	for _, e := range r.entries {
		if !e.removing {
			seen[e.def.ApplicationID] = true
		}
	}
	r.mu.RUnlock()
	apps := make([]string, 0, len(seen))
	for a := range seen {
		apps = append(apps, a)
	}
	slices.Sort(apps)

	var first error
	for _, app := range apps {
		pkg, active, err := ragActive(ctx, r.packages, app)
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if !active {
			continue
		}
		hash := pkg.ContentHash()
		retryKey := app + "\x00" + hash
		r.mu.RLock()
		needs := false
		for _, e := range r.entries {
			if e.def.ApplicationID == app && !e.removing && (!e.bound || e.def.ManifestHash != hash) {
				needs = true
			}
		}
		wait := r.retryAt[retryKey]
		r.mu.RUnlock()
		if !needs || r.clk.Now().Before(wait) {
			continue
		}
		err = r.rebind(ctx, app, hash)
		r.mu.Lock()
		if err != nil {
			r.retryAt[retryKey] = r.clk.Now().Add(ragReauditBackoff)
		} else {
			delete(r.retryAt, retryKey)
		}
		r.mu.Unlock()
		if err != nil {
			r.log.Warn("rag: the sources of a re-audited package could not be re-bound; they stay unserved and are retried",
				slog.String("application", app), slog.String("code", string(contracts.CodeOf(err))))
			if first == nil {
				first = err
			}
		}
	}
	return first
}

func (r *ragSources) rebind(ctx context.Context, app, hash string) error {
	got, rebuild, err := r.reaudit.Active(ctx, app)
	switch {
	case err != nil:
		return err
	case got != hash:
		return contracts.New(contracts.ErrConflict, "rag: the active package changed while its sources were being re-bound")
	}
	rep, err := r.Reaudit(ctx, app, rebuild)
	if err == nil && rep.Failed > 0 {
		err = contracts.New(contracts.ErrConflict, "rag: some sources could not be re-bound")
	}
	return err
}

// reconcileEvery runs Reconcile on a period until ctx ends.
func (r *ragSources) reconcileEvery(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = r.Reconcile(ctx)
		}
	}
}

// Remove removes a source: it stops being served at once, then ingestion
// applies the declared deletion behaviour (ingest.Service.RemoveSource, audited).
// If that fails the source stays unserved and the call can be repeated. The
// durable registry says so first (the removal began) and forgets the source last
// (it ended), so a restart in between reads the source back as being removed:
// never served, and the removal can be finished by repeating the call.
func (r *ragSources) Remove(ctx context.Context, scope contracts.Scope, id string) error {
	key := ragKeyOf(scope, id)
	r.mu.Lock()
	e := r.entries[key]
	svc := r.svc
	wasRemoving := e != nil && e.removing
	if e != nil {
		e.removing = true
	}
	delete(r.states, key)
	r.mu.Unlock()
	if e == nil {
		return contracts.New(contracts.ErrValidationFailed, "rag: no such source").WithDetails("reason", "not_found")
	}
	if svc == nil {
		return contracts.New(contracts.ErrInternal, "rag: the registry is not bound to the ingestion service")
	}
	if _, err := r.store.MarkRemoving(ctx, scope, id); err != nil {
		// Nothing was deleted and the registry still says the source is active:
		// the source is served again rather than left unserved by a removal that
		// is not on record.
		if !wasRemoving {
			r.mu.Lock()
			if cur := r.entries[key]; cur == e {
				cur.removing = false
			}
			r.mu.Unlock()
		}
		return err
	}
	if err := svc.RemoveSource(ctx, scope, id); err != nil {
		return err
	}
	if _, err := r.store.Delete(ctx, scope, id); err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.entries, key)
	delete(r.states, key)
	r.mu.Unlock()
	return nil
}

func ragCopyScope(s contracts.Scope) contracts.Scope {
	s.Roles = slices.Clone(s.Roles)
	s.CapabilityScopes = slices.Clone(s.CapabilityScopes)
	return s
}
