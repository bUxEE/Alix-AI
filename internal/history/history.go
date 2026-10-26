package history

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/blocks"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Module is the name of this package's migrations. Importing internal/sessions
// registers the "transcript" module first (a package initialises after what it
// imports), which is what the triggers and the view of the migrations need.
const Module = "history"

//go:embed migrations/*.sql
var migrations embed.FS

func init() { store.RegisterMigrations(Module, migrations) }

// Capabilities the host puts in the context token. The match is exact.
const (
	// CapRead lists the transcript segments of the caller's own sessions.
	CapRead = "history.read"
	// CapSearch searches the caller's own transcript.
	CapSearch = "history.search"
	// CapClear clears the transcript of one of the caller's own sessions.
	CapClear = "history.clear"
	// CapExport exports the caller's own transcript.
	CapExport = "history.export"
	// CapCleanup deletes every transcript turn of the workspace older than a
	// time: it reaches beyond the caller's own history, so it is its own
	// capability.
	CapCleanup = "history.cleanup"
	// CapRetention reads and changes the tenant's retention policy.
	CapRetention = "history.retention"
)

// Action names what an Authorizer is asked about.
type Action string

// The actions of the package.
const (
	ActionRead                 Action = "read"
	ActionSearch               Action = "search"
	ActionClear                Action = "clear"
	ActionExport               Action = "export"
	ActionCleanup              Action = "cleanup"
	ActionRetention            Action = "retention"
	ActionApplicationRetention Action = "application_retention"
)

// Authorizer is an extra check on top of the capability, typically a policy
// decision. A nil error lets the operation go on; any error refuses it. It can
// never grant what the capability does not: the capability is checked first.
type Authorizer func(ctx context.Context, scope contracts.Scope, action Action) error

// TranscriptStore is the part of the session store this package uses. The
// session store owns the transcript table; this package reads and deletes
// through it. *sessions.Store implements it.
type TranscriptStore interface {
	Transcript(ctx context.Context, scope contracts.Scope, afterN int64, limit int) ([]sessions.TranscriptTurn, error)
	TranscriptLen(ctx context.Context, scope contracts.Scope) (int64, error)
	ClearTranscript(ctx context.Context, scope contracts.Scope) (int64, error)
	PruneTranscript(ctx context.Context, scope contracts.Scope, cutoff time.Time) (int64, error)
}

var _ TranscriptStore = (*sessions.Store)(nil)

// Defaults of Config.
const (
	// DefaultPage is how many turns a segment page holds, and MaxPage the most it
	// may hold.
	DefaultPage = 25
	MaxPage     = 100
	// DefaultSearchLimit is how many hits a search page holds, MaxSearchLimit the
	// most, and MaxSearchOffset how deep a caller can page: BM25 scores every
	// match, so the depth is bounded.
	DefaultSearchLimit = 10
	MaxSearchLimit     = 50
	MaxSearchOffset    = 1000
	// DefaultExportPage is how many transcript rows one read of an export fetches.
	DefaultExportPage = 500
	// DefaultRetentionInterval is how often retention is applied.
	DefaultRetentionInterval = 24 * time.Hour
	// DefaultSweepBatch is how many workspaces one read of the retention sweep
	// fetches.
	DefaultSweepBatch = 200
	// MaxRetainDays bounds a retention policy (ten years).
	MaxRetainDays = 3650
)

// Config bounds the service. A zero field takes its default.
type Config struct {
	// ExportPage is the page size of an export read.
	ExportPage int
	// RetentionInterval is the period of the scheduled retention job: at most one
	// job per workspace per period is queued.
	RetentionInterval time.Duration
	// SweepBatch is how many workspaces the retention sweep reads at a time.
	SweepBatch int
}

func (c Config) withDefaults() Config {
	if c.ExportPage <= 0 {
		c.ExportPage = DefaultExportPage
	}
	if c.RetentionInterval <= 0 {
		c.RetentionInterval = DefaultRetentionInterval
	}
	if c.SweepBatch <= 0 {
		c.SweepBatch = DefaultSweepBatch
	}
	return c
}

// Quarantine is what the service needs from the upgrade's quarantine of isolated
// derived indexes (internal/upgrade.Quarantine implements it; this package does
// not import the upgrade). Has reports whether an index is isolated: its damaged
// contents were dropped and it has not been rebuilt, so it would answer with
// nothing. Search consults it and says so instead of returning an empty page.
type Quarantine interface {
	Has(name string) bool
}

// SearchIndexName is the name of the search index (history_fts, the FTS5 table of
// migrations 0001 and 0002) in the quarantine. A test pins it against the upgrade.
const SearchIndexName = "history_fts"

// Deps is everything New needs.
type Deps struct {
	// DB is the store. The modules sessions, transcript and history must be
	// registered with the registry it was opened with (importing this package
	// does that for store.Default).
	DB *store.DB
	// Transcripts is the session store.
	Transcripts TranscriptStore
	// Audit records every deletion, export and policy change. Required: nothing
	// is deleted, exported or changed without a record of it.
	Audit audit.Appender
	// Clock is the only source of time. Nil means clock.Real.
	Clock clock.Clock
	// Sanitizer sanitises the text of an assistant row before it leaves, the very
	// way the stream hub sanitises a response before it becomes token events: the
	// transcript keeps the verified text as the engine produced it, and what a host
	// renders from history must be what it would have rendered live. Nil means
	// blocks.Default().
	Sanitizer *blocks.Validator
	// Quarantine lists the derived indexes that were isolated (internal/upgrade's
	// Quarantine). While the search index is isolated Search does not run and fails
	// with unavailable: an empty page would say that nothing matched. Nil means
	// nothing is isolated.
	Quarantine Quarantine
	// Authorizer is an extra check on every operation. Nil means the capability
	// alone decides.
	Authorizer Authorizer
	// Operator authorises the application-wide retention policy, which no tenant
	// can change. Nil denies it.
	Operator Authorizer
	// Queue and DeploymentID enable the scheduled retention job. The job types are
	// registered on the queue by New; the deployment id is what the service scope
	// of the job carries.
	Queue        *jobs.Queue
	DeploymentID string
	Config       Config
	Logger       *slog.Logger
}

// MemoryExporter appends long-term memory rows onto a history export (ADR 0042).
type MemoryExporter interface {
	AppendExport(ctx context.Context, scope contracts.Scope, w io.Writer) error
}

// Service is the conversation-history API. It is safe for concurrent use.
type Service struct {
	db       *store.DB
	tr       TranscriptStore
	aud      audit.Appender
	san      *blocks.Validator
	clk      clock.Clock
	authz    Authorizer
	operator Authorizer
	quar     Quarantine
	q        *jobs.Queue
	dep      string
	cfg      Config
	log      *slog.Logger
	mem      MemoryExporter
}

// New validates the dependencies and, when a queue is given, registers the
// retention job type on it. It starts nothing.
func New(d Deps) (*Service, error) {
	missing := func(what string) (*Service, error) {
		return nil, contracts.Newf(contracts.ErrValidationFailed, "history: %s is required", what)
	}
	switch {
	case d.DB == nil:
		return missing("a store")
	case d.Transcripts == nil:
		return missing("a transcript store")
	case d.Audit == nil:
		return missing("an audit log")
	case d.Queue != nil && d.DeploymentID == "":
		return missing("a deployment id for the retention job")
	}
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.Sanitizer == nil {
		d.Sanitizer = blocks.Default()
	}
	s := &Service{
		db: d.DB, tr: d.Transcripts, aud: d.Audit, san: d.Sanitizer, clk: d.Clock, authz: d.Authorizer, operator: d.Operator, quar: d.Quarantine,
		q: d.Queue, dep: d.DeploymentID, cfg: d.Config.withDefaults(), log: d.Logger,
	}
	if s.q != nil {
		if err := s.q.Register(JobSpec()); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// authorize is the check every operation starts with: a valid scope, the exact
// capability in the token, then the optional Authorizer. Anything else is
// permission_denied, and nothing has been read or changed yet.
func (s *Service) authorize(ctx context.Context, scope contracts.Scope, action Action, capability string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if !scope.Permits(capability) {
		return contracts.Newf(contracts.ErrPermissionDenied, "history: the context does not permit %s", capability)
	}
	return s.extra(ctx, s.authz, scope, action)
}

// extra runs an additional authorizer. A nil one is no check; a refusal is
// permission_denied whatever its own code.
func (s *Service) extra(ctx context.Context, a Authorizer, scope contracts.Scope, action Action) error {
	if a == nil {
		return nil
	}
	err := a(ctx, scope, action)
	switch {
	case err == nil:
		return nil
	case contracts.CodeOf(err) == contracts.ErrPermissionDenied:
		return err
	}
	return contracts.Wrap(contracts.ErrPermissionDenied, err, "history: the operation was not authorised")
}

// wrapDB types a store failure as unavailable unless it already carries a code.
func wrapDB(err error) error {
	if err == nil {
		return nil
	}
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return err
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, "history: the store failed")
}

func toMS(t time.Time) int64    { return t.UTC().UnixMilli() }
func fromMS(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// validID reports whether s can be a session id: the grammar of the path
// identifiers of the API.
func validID(s string) bool {
	if s == "" || len(s) > contracts.MaxScopeValueLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}

func badSessionID() error {
	return contracts.New(contracts.ErrValidationFailed, "history: the session id is malformed")
}

// forSession is the scope of the caller with another of its sessions in it. The
// principal, tenant, workspace and application stay the caller's, so the session
// store's continuity check still applies to whatever is stored under that id.
func forSession(scope contracts.Scope, sessionID string) contracts.Scope {
	sc := scope.Clone()
	sc.SessionID = sessionID
	return sc
}

// checkOwner refuses a session that holds transcript rows of another principal.
// A session with no rows is empty, not forbidden. It runs before any read or
// deletion of a session by id, on top of the session store's own binding check.
func (s *Service) checkOwner(ctx context.Context, scope contracts.Scope, sessionID string) error {
	row, err := store.QueryRowScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL: "SELECT principal_id FROM transcript_turns", Where: "session_id = ?", WhereArgs: []any{sessionID}, Tail: "LIMIT 1",
	})
	if err != nil {
		return err
	}
	var owner string
	switch err := row.Scan(&owner); {
	case err == nil:
	case isNoRows(err):
		return nil
	default:
		return wrapDB(err)
	}
	if owner != scope.PrincipalID {
		return contracts.New(contracts.ErrScopeViolation, "history: the session belongs to another principal").
			WithDetails("levels", []string{string(contracts.LevelPrincipal)})
	}
	return nil
}

func (s *Service) now() time.Time { return s.clk.Now().UTC() }

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// roleAssistant is the role of a row the assistant wrote.
const roleAssistant = "assistant"

// display returns the text of a transcript row as the API may show it. A user's
// row is the user's own text, verbatim. An assistant's row is model Markdown that
// the engine stored as it verified it: the stream hub sanitises a response as a
// whole before the client sees it (raw HTML and images removed, links limited to
// the allowed schemes), so the same sanitiser runs here, and a host that renders
// history with the renderer it uses for live token events gets nothing it would
// not have got live. The stored row is not changed. When the sanitiser refuses a
// text (the live stream would have refused it too, so the user never saw it)
// nothing is shown and withheld is true: history fails closed, it never falls
// back to the raw text.
func (s *Service) display(role, text string) (shown string, withheld bool) {
	if role != roleAssistant {
		return text, false
	}
	clean, _, err := s.san.SanitizeResponse(text)
	if err != nil {
		return "", true
	}
	return clean, false
}
