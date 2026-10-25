package actions

import (
	"context"
	"embed"
	"io"
	"log/slog"
	"time"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

//go:embed migrations/*.sql
var migrations embed.FS

func init() { store.RegisterMigrations("actions", migrations) }

// ID prefixes. They match the typedId grammar of schemas/common/ids (2 to 8
// lower-case letters) and the patterns of schemas/actions/shared.
const (
	preparationPrefix = "prep"
	receiptPrefix     = "rcpt"
	idempotencyPrefix = "idem"
)

// Defaults and limits of Config.
const (
	// DefaultMaxPreparationTTL is the platform limit on how long a preparation
	// stays valid: ten minutes. The host's valid_for_seconds can only shorten it.
	DefaultMaxPreparationTTL = 10 * time.Minute
	// MaxPreparationTTLLimit is the largest platform limit an operator may set.
	MaxPreparationTTLLimit = 24 * time.Hour
	// DefaultSettleTimeout bounds the work done after a commit was dispatched
	// (the Status call and the transaction that records the outcome). It runs
	// on a context detached from the caller's, so a client that disconnects
	// cannot strand a dispatched commit in the committing state.
	DefaultSettleTimeout = 30 * time.Second
	// DefaultCommitStaleAfter is how old a committing row must be before Recover
	// treats it as abandoned: longer than the longest adapter call
	// (adapters.MaxCallTimeout) plus the settle budget.
	DefaultCommitStaleAfter = adapters.MaxCallTimeout + DefaultSettleTimeout
)

// Config tunes the coordinator. The zero value takes the defaults.
type Config struct {
	// MaxPreparationTTL is the platform limit on the life of a preparation.
	MaxPreparationTTL time.Duration
	// SettleTimeout bounds the post-dispatch Status call and outcome write.
	SettleTimeout time.Duration
	// CommitStaleAfter is the age at which Recover resolves a committing row.
	CommitStaleAfter time.Duration
}

func (c Config) withDefaults() (Config, error) {
	if c.MaxPreparationTTL == 0 {
		c.MaxPreparationTTL = DefaultMaxPreparationTTL
	}
	if c.SettleTimeout == 0 {
		c.SettleTimeout = DefaultSettleTimeout
	}
	if c.CommitStaleAfter == 0 {
		c.CommitStaleAfter = DefaultCommitStaleAfter
	}
	switch {
	case c.MaxPreparationTTL < time.Second || c.MaxPreparationTTL > MaxPreparationTTLLimit:
		return c, contracts.New(contracts.ErrValidationFailed, "actions: MaxPreparationTTL must be between one second and 24 hours")
	case c.SettleTimeout < time.Second || c.SettleTimeout > 5*time.Minute:
		return c, contracts.New(contracts.ErrValidationFailed, "actions: SettleTimeout must be between one second and five minutes")
	case c.CommitStaleAfter < time.Second:
		return c, contracts.New(contracts.ErrValidationFailed, "actions: CommitStaleAfter must be at least one second")
	}
	return c, nil
}

// PackageSource resolves the application package decisions are made against.
// The package loader and capability registry implement it. A source that
// cannot produce an active package returns an error or a package that is not
// active; either way nothing is prepared or committed.
type PackageSource interface {
	ActivePackage(ctx context.Context, applicationID string) (policy.ActivePackage, error)
}

// PackageSourceFunc adapts a function to PackageSource.
type PackageSourceFunc func(ctx context.Context, applicationID string) (policy.ActivePackage, error)

// ActivePackage calls f.
func (f PackageSourceFunc) ActivePackage(ctx context.Context, applicationID string) (policy.ActivePackage, error) {
	return f(ctx, applicationID)
}

// AdapterSource resolves the adapter of an application. The adapter MUST be the
// adapters.Wrap wrapper around a transport: the coordinator relies on its
// scope check, time and concurrency bounds, result re-validation and mapping of
// ambiguous commit failures to uncertain_outcome, and re-checks every result
// itself as defence in depth.
type AdapterSource interface {
	Adapter(ctx context.Context, applicationID string) (adapters.AppAdapter, error)
}

// AdapterSourceFunc adapts a function to AdapterSource.
type AdapterSourceFunc func(ctx context.Context, applicationID string) (adapters.AppAdapter, error)

// Adapter calls f.
func (f AdapterSourceFunc) Adapter(ctx context.Context, applicationID string) (adapters.AppAdapter, error) {
	return f(ctx, applicationID)
}

// Deps is what a Coordinator is built from.
type Deps struct {
	// DB is the store. The "actions" and "audit" migrations must be registered
	// with the registry it was opened with (importing this package and
	// internal/audit does that for store.Default).
	DB *store.DB
	// Policy decides at prepare and again at commit. It should be built with a
	// sink that writes to the audit log, so every decision is recorded.
	Policy *policy.Gateway
	// Audit receives the action lifecycle events. The "committing" event is
	// appended in the same transaction as the state change and before the
	// adapter is called; if the append fails the commit does not happen.
	Audit audit.Appender
	// Packages and Adapters resolve the application's package and adapter.
	Packages PackageSource
	Adapters AdapterSource
	// Clock is the only source of time. Nil means clock.Real.
	Clock clock.Clock
	// Config tunes the coordinator; the zero value takes the defaults.
	Config Config
	// Logger receives state transitions and refusals: identifiers and codes
	// only, never field values or confirmation secrets. Nil discards.
	Logger *slog.Logger
}

// Coordinator is the prepare / confirm / commit coordinator (master plan 3
// rule 8). It is safe for concurrent use.
type Coordinator struct {
	db       *store.DB
	policy   *policy.Gateway
	audit    audit.Appender
	packages PackageSource
	adapters AdapterSource
	clk      clock.Clock
	ids      *ids.Generator
	cfg      Config
	log      *slog.Logger
}

// New builds a Coordinator. Every dependency except the clock and logger is
// required: a coordinator without a policy gateway, an audit log or an adapter
// source could only fail open, so it cannot be built.
func New(d Deps) (*Coordinator, error) {
	switch {
	case d.DB == nil:
		return nil, contracts.New(contracts.ErrValidationFailed, "actions: Deps.DB is required")
	case d.Policy == nil:
		return nil, contracts.New(contracts.ErrValidationFailed, "actions: Deps.Policy is required")
	case d.Audit == nil:
		return nil, contracts.New(contracts.ErrValidationFailed, "actions: Deps.Audit is required")
	case d.Packages == nil:
		return nil, contracts.New(contracts.ErrValidationFailed, "actions: Deps.Packages is required")
	case d.Adapters == nil:
		return nil, contracts.New(contracts.ErrValidationFailed, "actions: Deps.Adapters is required")
	}
	cfg, err := d.Config.withDefaults()
	if err != nil {
		return nil, err
	}
	clk := d.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	lg := d.Logger
	if lg == nil {
		lg = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Coordinator{
		db: d.DB, policy: d.Policy, audit: d.Audit, packages: d.Packages, adapters: d.Adapters,
		clk: clk, ids: ids.NewGenerator(clk), cfg: cfg, log: lg,
	}, nil
}
