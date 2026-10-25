package ingest

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"sync"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/parse"
	"github.com/buxee/alix-ai/internal/rag/parse/wire"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// ErrSourceGone is what a Registry returns for a source it does not (or no
// longer) hold. A job for such a source ends without touching anything: a
// registry that merely failed to answer must never be read as "the source was
// removed" (that would delete an index on a glitch), and a removal is an
// explicit call (Service.RemoveSource).
var ErrSourceGone = contracts.New(contracts.ErrStale, "the source is not registered")

// Resolved is a registered source: its current definition and the provider
// that reads it.
type Resolved struct {
	Def      source.Definition
	Provider source.Provider
}

// Registry resolves a source ID to its current definition. The registration
// layer decides whether the package may serve (an activated, audited package),
// so a source of a quarantined or deactivated package is simply not resolved.
type Registry interface {
	// Resolve returns the source's current definition and provider, or an error
	// wrapping ErrSourceGone. The scope is the job's initiating scope.
	Resolve(ctx context.Context, scope contracts.Scope, sourceID string) (Resolved, error)
}

// RegistryFunc adapts a function to Registry.
type RegistryFunc func(ctx context.Context, scope contracts.Scope, sourceID string) (Resolved, error)

// Resolve calls f.
func (f RegistryFunc) Resolve(ctx context.Context, scope contracts.Scope, sourceID string) (Resolved, error) {
	return f(ctx, scope, sourceID)
}

// Parser is the sandboxed document parser. *parse.Runner (through
// NewRunnerParser) is the production implementation; tests use a stand-in so
// that they do not start a process per document.
type Parser interface {
	// Parse extracts text and structure from one document.
	Parse(ctx context.Context, format wire.Format, doc []byte) (*parse.Result, error)
	// Probe starts a worker and reports the confinement it got, without parsing.
	Probe(ctx context.Context) (parse.SandboxReport, error)
	// Component is the parser identity and version recorded in every index
	// generation: a different one is a new generation.
	Component() rag.ComponentVersion
}

type runnerParser struct{ r *parse.Runner }

// NewRunnerParser adapts a parse.Runner to Parser.
func NewRunnerParser(r *parse.Runner) Parser { return runnerParser{r: r} }

func (p runnerParser) Parse(ctx context.Context, f wire.Format, doc []byte) (*parse.Result, error) {
	return p.r.Parse(ctx, f, doc)
}

func (p runnerParser) Probe(ctx context.Context) (parse.SandboxReport, error) { return p.r.Probe(ctx) }

func (runnerParser) Component() rag.ComponentVersion { return parse.Component() }

// DiskPressure is the disk-pressure hook (master plan 18: "pause ingestion
// before interactive or transactional storage is endangered"). The resource
// module provides the production implementation (free space of the data
// directory against a floor); this package only asks.
type DiskPressure interface {
	// UnderPressure reports whether ingestion must stop writing now. An error
	// means the answer is unknown, and unknown is treated as pressure: ingestion
	// fails closed rather than fill a disk it could not measure.
	UnderPressure(ctx context.Context) (bool, error)
}

// DiskPressureFunc adapts a function to DiskPressure.
type DiskPressureFunc func(ctx context.Context) (bool, error)

// UnderPressure calls f.
func (f DiskPressureFunc) UnderPressure(ctx context.Context) (bool, error) { return f(ctx) }

// OperatorAction names what an operator is asked to authorise.
type OperatorAction string

const (
	// ActionConfirmMassDeletion is the confirmation of a held mass deletion.
	ActionConfirmMassDeletion OperatorAction = "confirm_mass_deletion"
	// ActionDeleteTenant is the deletion of a tenant's ingestion data.
	ActionDeleteTenant OperatorAction = "delete_tenant"
)

// OperatorAuthorizer decides whether the caller may take an operator action. It
// is a deterministic hook into the policy layer; with none configured every
// operator action is denied (fail closed), so a held mass deletion can only be
// confirmed by a deployment that wired its authorisation.
type OperatorAuthorizer interface {
	AuthorizeOperator(ctx context.Context, scope contracts.Scope, action OperatorAction) error
}

// OperatorAuthorizerFunc adapts a function to OperatorAuthorizer.
type OperatorAuthorizerFunc func(ctx context.Context, scope contracts.Scope, action OperatorAction) error

// AuthorizeOperator calls f.
func (f OperatorAuthorizerFunc) AuthorizeOperator(ctx context.Context, scope contracts.Scope, action OperatorAction) error {
	return f(ctx, scope, action)
}

// Embedding configures queued embeddings. Without it the pipeline indexes text
// only: search is full-text, and the generation records no embedder.
type Embedding struct {
	// Embedder is the embedding runtime role.
	Embedder providers.Embedder
	// Model is the model identifier the runtime reports and the vector index
	// records (vector.PipelineOptions.Model).
	Model string
	// Component is the embedder identity recorded in an index generation: a
	// changed one is a new generation. Name is a lower-case component name
	// (letters, digits, '.', '_', '-'), which a model identifier containing a
	// colon is not, so it is given separately.
	Component rag.ComponentVersion
	// BatchSize and Profile pass through to vector.PipelineOptions.
	BatchSize int
	Profile   string
}

var componentNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func (e Embedding) validate() error {
	switch {
	case e.Embedder == nil:
		return contracts.New(contracts.ErrValidationFailed, "ingest: embedding has no embedder")
	case e.Model == "":
		return contracts.New(contracts.ErrValidationFailed, "ingest: embedding has no model")
	case !componentNameRE.MatchString(e.Component.Name):
		return contracts.Newf(contracts.ErrValidationFailed, "ingest: embedder component name %q is not a valid component name", e.Component.Name)
	case e.Component.Version == "" || len(e.Component.Version) > 128:
		return contracts.New(contracts.ErrValidationFailed, "ingest: embedder component needs a version of at most 128 bytes")
	}
	return nil
}

// Deps is everything New needs.
type Deps struct {
	// DB is the store; the modules jobs, ragindex, vector (when embedding) and
	// ragingest must be registered (importing the packages does it).
	DB    *store.DB
	Clock clock.Clock
	Queue *jobs.Queue
	Index *index.Index
	// Vectors is the vector index. Required with Embedding; optional otherwise
	// (given, it is still kept in step with deletions).
	Vectors   vector.VectorIndexProvider
	Embedding *Embedding

	Registry Registry
	Parser   Parser
	Chunker  *chunk.Chunker
	// Charge charges the ingestion volume (bytes a sync reads into new versions) in the transaction that
	// writes the versions. Nil means the charger bound to the store (BindCharger), and with none, no charge.
	Charge Charger
	// Audit records mass-deletion decisions and source and tenant deletions.
	// Required: a refused or confirmed deletion that cannot be recorded does not
	// proceed.
	Audit audit.Appender
	// Pause is the disk-pressure gate. The composition root creates it first
	// (NewPause), installs Pause.Policy on the job queue (jobs.Options.Policy),
	// and hands the same value here, so the queue stops leasing ingestion work
	// under pressure and the stages check again when they start. Nil builds one
	// from Pressure, which still pauses the stages but cannot be installed on a
	// queue that already exists.
	Pause *Pause
	// Pressure is the hook the gate asks when Pause is nil. Nil means never.
	Pressure DiskPressure
	// Operator authorises operator actions. Nil denies them.
	Operator OperatorAuthorizer

	Config Config
	Logger *slog.Logger
}

// Service is the ingestion pipeline of master plan 12.3 over internal/jobs. It
// is safe for concurrent use.
type Service struct {
	db       *store.DB
	clk      clock.Clock
	q        *jobs.Queue
	ix       *index.Index
	vec      vector.VectorIndexProvider
	emb      *Embedding
	pipe     *vector.Pipeline
	reg      Registry
	parser   Parser
	chunker  *chunk.Chunker
	aud      audit.Appender
	operator OperatorAuthorizer
	charge   Charger
	cfg      Config
	log      *slog.Logger
	ids      *ids.Generator
	pause    *Pause
	chunks   *chunkReader

	locks  sync.Map // source key -> *sync.Mutex: the write lock
	slocks sync.Map // source key -> *sync.Mutex: one scan or removal at a time

	readyMu sync.Mutex
	ready   *Readiness
}

// New validates the dependencies and registers the job types on the queue. It
// starts nothing: Start probes the parser, Handle attaches the handlers to a
// Pool.
func New(d Deps) (*Service, error) {
	missing := func(what string) (*Service, error) {
		return nil, contracts.Newf(contracts.ErrValidationFailed, "ingest: %s is required", what)
	}
	switch {
	case d.DB == nil:
		return missing("a store")
	case d.Queue == nil:
		return missing("a job queue")
	case d.Index == nil:
		return missing("an index")
	case d.Registry == nil:
		return missing("a source registry")
	case d.Parser == nil:
		return missing("a parser")
	case d.Chunker == nil:
		return missing("a chunker")
	case d.Audit == nil:
		return missing("an audit log")
	}
	if err := d.Config.Validate(); err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "ingest: invalid configuration")
	}
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	s := &Service{
		db: d.DB, clk: d.Clock, q: d.Queue, ix: d.Index, vec: d.Vectors, emb: d.Embedding,
		reg: d.Registry, parser: d.Parser, chunker: d.Chunker, aud: d.Audit, operator: d.Operator, charge: d.Charge,
		cfg: d.Config.withDefaults(), log: d.Logger, ids: ids.NewGenerator(d.Clock),
	}
	s.pause = d.Pause
	if s.pause == nil {
		s.pause = NewPause(d.Pressure, d.Clock, s.cfg.PressureRecheck, s.log)
	}
	s.chunks = &chunkReader{db: d.DB}
	if d.Embedding != nil {
		if err := d.Embedding.validate(); err != nil {
			return nil, err
		}
		if d.Vectors == nil {
			return missing("a vector index (embedding is configured)")
		}
		p, err := vector.NewPipeline(vector.PipelineOptions{
			Index: d.Vectors, Embedder: d.Embedding.Embedder, Source: s.chunks, Model: d.Embedding.Model,
			BatchSize: d.Embedding.BatchSize, Profile: d.Embedding.Profile, Logger: d.Logger,
		})
		if err != nil {
			return nil, err
		}
		s.pipe = p
	}
	specs := JobSpecs()
	if s.pipe != nil {
		// Embedding is class 5 work fixed by the vector package's own spec.
		specs = append(specs, vector.EmbedJobSpec())
	}
	for _, spec := range specs {
		if err := d.Queue.Register(spec); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// components are the component versions this build of the pipeline would
// record in a new generation (master plan 12.3: parser, OCR, chunker and
// embedder versions are recorded). There is no OCR engine, so none is recorded.
func (s *Service) components() index.Components {
	c := index.Components{Parser: s.parser.Component(), Chunker: s.chunker.Component()}
	if s.emb != nil {
		e := s.emb.Component
		c.Embedder = &e
	}
	return c
}

func sameComponent(a, b *rag.ComponentVersion) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// sameComponents reports whether two sets name the same component versions.
func sameComponents(a, b index.Components) bool {
	return a.Parser == b.Parser && a.Chunker == b.Chunker && sameComponent(a.OCR, b.OCR) && sameComponent(a.Embedder, b.Embedder)
}

// lock is the write lock of one source inside this process. It is held while a
// scan is written and its deletions applied, while an index stage checks that
// its version is still current and writes its document, and while a generation
// is activated, so the three never interleave (a document cannot be written
// between "recorded as deleted" and "hidden"). It is only ever held for
// database work, never across a directory walk or a parse. Across processes the
// snapshot sequence number guards the snapshot; running two ingestion
// processes over one database is not supported.
func (s *Service) lock(scope contracts.Scope, sourceID string) func() {
	return lockOf(&s.locks, scope, sourceID)
}

// syncLock lets one scan, removal or confirmation of a source run at a time.
// It is taken before the write lock, never after it.
func (s *Service) syncLock(scope contracts.Scope, sourceID string) func() {
	return lockOf(&s.slocks, scope, sourceID)
}

func lockOf(m *sync.Map, scope contracts.Scope, sourceID string) func() {
	k := scope.ApplicationID + "\x00" + scope.TenantID + "\x00" + scope.WorkspaceID + "\x00" + sourceID
	l, _ := m.LoadOrStore(k, &sync.Mutex{})
	mu := l.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func isGone(err error) bool { return errors.Is(err, ErrSourceGone) }

// errNoSuchSource is what a caller gets for a source id its scope does not
// hold; a source of another scope is reported exactly like one that does not
// exist.
func errNoSuchSource() error {
	return contracts.New(contracts.ErrValidationFailed, "ingest: no such source").WithDetails("reason", "not_found")
}
