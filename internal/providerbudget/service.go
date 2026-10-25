package providerbudget

import (
	"context"
	"errors"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Fixed ID prefixes (schemas/providers shared: bud, rsv, usg).
const (
	prefixBudget      = "bud"
	prefixReservation = "rsv"
	prefixUsage       = "usg"
	prefixEnvelope    = "envl"
	prefixDecision    = "edec"
)

const (
	// DefaultReservationTTL is how long an unsettled reservation holds its amounts.
	DefaultReservationTTL = 10 * time.Minute
	// MaxReservationTTL is the longest a single call may ask to hold them.
	MaxReservationTTL = time.Hour
	// rateWindow is the sliding window of requests_per_min.
	rateWindow = time.Minute
)

// The fixed reason codes of Details["reason"].
const (
	ReasonBudgetExhausted  = "budget_exhausted"
	ReasonQuotaExhausted   = "quota_exhausted"
	ReasonConcurrencyLimit = "concurrency_limit"
	ReasonRateLimited      = "rate_limited"
	ReasonNoBudget         = "no_budget"
)

// Config is what New needs besides the database.
type Config struct {
	// Clock is the only source of time. Nil means clock.Real.
	Clock clock.Clock
	// ReservationTTL is the default timeout of a reservation; zero means DefaultReservationTTL.
	ReservationTTL time.Duration
	// LocalFallback reports whether policy names a local fallback that can serve a call to the
	// provider while its budget is exhausted. It is only a hint for the host (budget state and the
	// exhaustion error): the router applies the policy. Nil means no fallback is available.
	LocalFallback func(ctx context.Context, scope contracts.Scope, provider providers.ProviderID) bool
}

// Service is the provider budget ledger. It is safe for concurrent use; all state is in the store, so
// a new Service over the same database sees every reservation, which is how a restart or a crash
// leaves the accounting intact.
type Service struct {
	db       *store.DB
	clk      clock.Clock
	ids      *ids.Generator
	ttl      time.Duration
	fallback func(context.Context, contracts.Scope, providers.ProviderID) bool
}

// New returns a Service over db, whose migrations (module "providerbudget") must be registered.
func New(db *store.DB, cfg Config) (*Service, error) {
	if db == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "providerbudget: no database")
	}
	clk := cfg.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	ttl := cfg.ReservationTTL
	switch {
	case ttl == 0:
		ttl = DefaultReservationTTL
	case ttl < 0 || ttl > MaxReservationTTL:
		return nil, contracts.New(contracts.ErrValidationFailed, "providerbudget: the reservation timeout must be between 0 and one hour")
	}
	return &Service{db: db, clk: clk, ids: ids.NewGenerator(clk), ttl: ttl, fallback: cfg.LocalFallback}, nil
}

// ChannelForClass maps a job priority class (master plan 17.1) to the budget channel: authorization
// and commits, active turns, current RAG queries and ordinary reads are interactive; ingestion,
// embeddings, OCR, reindexing and maintenance are background and so can only draw on a background
// budget. An invalid class maps to background, the safe side.
func ChannelForClass(c jobs.Class) providers.Channel {
	if c.Valid() && c <= jobs.ClassRead {
		return providers.ChannelInteractive
	}
	return providers.ChannelBackground
}

// timestamp formats t as the RFC 3339 UTC instant of schemas/common timestamps.
func timestamp(t time.Time) common.Timestamp {
	return common.Timestamp(t.UTC().Format(time.RFC3339Nano))
}

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// window is one UTC calendar window.
type window struct {
	kind       string // "daily" or "monthly"
	start, end time.Time
}

func dayWindow(t time.Time) window {
	t = t.UTC()
	s := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return window{kind: "daily", start: s, end: s.AddDate(0, 0, 1)}
}

func monthWindow(t time.Time) window {
	t = t.UTC()
	s := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	return window{kind: "monthly", start: s, end: s.AddDate(0, 1, 0)}
}

func windowsAt(t time.Time) [2]window { return [2]window{dayWindow(t), monthWindow(t)} }

// wrapStore maps a storage failure to unavailable and keeps typed errors as they are.
func wrapStore(err error, msg string) error {
	if err == nil {
		return nil
	}
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return err
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, msg)
}

func conflict(reason, msg string) *contracts.Error {
	return contracts.New(contracts.ErrConflict, msg).WithDetails("reason", reason)
}

// ReasonOf returns the fixed reason code of err, or "".
func ReasonOf(err error) string {
	var ce *contracts.Error
	if errors.As(err, &ce) && ce != nil {
		if r, ok := ce.Details["reason"].(string); ok {
			return r
		}
	}
	return ""
}
