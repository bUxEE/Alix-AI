package quotas

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func init() { store.RegisterMigrations("quotas", migrations) }

// tenantKey is what every quota is keyed by: the application AND the tenant of a
// verified scope. A struct, not a joined string, so ("ab","c") and ("a","bc")
// can never collide, and the same tenant id under two applications is two
// tenants.
type tenantKey struct{ application, tenant string }

// tenantState is the in-memory state of one tenant: its bucket and its open
// session leases. It is a few hundred bytes (ADR 0018 has the measurement).
type tenantState struct {
	bucket   bucket
	leases   map[uint64]held
	slots    int64
	lastSeen time.Time
}

type held struct {
	expires time.Time
	n       int64
}

// purge frees the slots whose lease expired.
func (s *tenantState) purge(now time.Time) {
	for id, h := range s.leases {
		if !now.Before(h.expires) {
			s.slots -= h.n
			delete(s.leases, id)
		}
	}
}

// Options are what New needs besides the database.
type Options struct {
	// Config is the "quotas" section; zero fields take the defaults.
	Config Config
	// Clock is the only source of time. Nil means clock.Real.
	Clock clock.Clock
}

// Quotas is the per-tenant accounting of ADR 0018. It is safe for concurrent use.
type Quotas struct {
	db    *store.DB
	clk   clock.Clock
	ttl   time.Duration
	sweep time.Duration

	base    Limits
	apps    map[string]Limits
	tenants map[tenantKey]Limits

	mu        sync.Mutex
	state     map[tenantKey]*tenantState
	lastSweep time.Time
	nextLease uint64

	admitted [numDims]atomic.Uint64
	rejected [numDims]atomic.Uint64
}

const numDims = 4

func dimIndex(d Dimension) int {
	switch d {
	case DimRequestsPerMinute:
		return 0
	case DimConcurrentSessions:
		return 1
	case DimIngestBytesPerDay:
		return 2
	}
	return 3
}

// New builds a Quotas over the store. It validates the configuration and
// resolves every override once: a tenant override inherits from its
// application's, which inherits from Defaults, which inherit from the profile.
func New(db *store.DB, opts Options) (*Quotas, error) {
	if db == nil {
		return nil, errors.New("quotas: a store is required")
	}
	cfg := opts.Config.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	clk := opts.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	profile, _ := ProfileLimits(cfg.Profile)
	q := &Quotas{
		db: db, clk: clk, ttl: cfg.LeaseTTL, sweep: max(cfg.IdleEvictAfter/2, time.Second),
		base:    cfg.Defaults.over(profile),
		apps:    map[string]Limits{},
		tenants: map[tenantKey]Limits{},
		state:   map[tenantKey]*tenantState{},
	}
	for _, o := range cfg.Overrides {
		if o.TenantID == "" {
			q.apps[o.ApplicationID] = o.Limits.over(q.base)
		}
	}
	for _, o := range cfg.Overrides {
		if o.TenantID == "" {
			continue
		}
		parent, ok := q.apps[o.ApplicationID]
		if !ok {
			parent = q.base
		}
		q.tenants[tenantKey{o.ApplicationID, o.TenantID}] = o.Limits.over(parent)
	}
	return q, nil
}

func (q *Quotas) limitsFor(k tenantKey) Limits {
	if l, ok := q.tenants[k]; ok {
		return l
	}
	if l, ok := q.apps[k.application]; ok {
		return l
	}
	return q.base
}

// Limits returns the effective limits of the scope's tenant.
func (q *Quotas) Limits(scope contracts.Scope) (Limits, error) {
	if err := scope.Validate(); err != nil {
		return Limits{}, err
	}
	l := q.limitsFor(tenantKey{scope.ApplicationID, scope.TenantID})
	l.Burst = l.burst()
	return l, nil
}

// Lease is a held share of a concurrency quota. Release it when the session
// ends; Renew it while the session lives (an unrenewed lease expires after
// Config.LeaseTTL, so a holder that never releases cannot leak a slot). The
// lease Admit returns for the other dimensions holds nothing and its methods do
// nothing. A nil *Lease is safe to use.
type Lease struct {
	q   *Quotas
	key tenantKey
	id  uint64
}

// noLease is returned for dimensions that hold no slot, so the hot path
// allocates nothing.
var noLease = &Lease{}

// Release frees the slots. It is idempotent: releasing twice, or after the
// lease expired and its slot was reclaimed, changes nothing, so a counter can
// never go below zero.
func (l *Lease) Release() {
	if l == nil || l.q == nil {
		return
	}
	l.q.mu.Lock()
	defer l.q.mu.Unlock()
	st := l.q.state[l.key]
	if st == nil {
		return
	}
	if h, ok := st.leases[l.id]; ok {
		st.slots -= h.n
		delete(st.leases, l.id)
	}
}

// Renew extends the lease by Config.LeaseTTL. A lease that already expired is
// not revived (another session may hold its slot now): Renew returns an
// expired error and the holder must treat the session as no longer admitted.
func (l *Lease) Renew() error {
	if l == nil || l.q == nil {
		return nil
	}
	l.q.mu.Lock()
	defer l.q.mu.Unlock()
	now := l.q.clk.Now()
	st := l.q.state[l.key]
	if st != nil {
		if h, ok := st.leases[l.id]; ok && now.Before(h.expires) {
			h.expires = now.Add(l.q.ttl)
			st.leases[l.id] = h
			return nil
		}
	}
	return contracts.New(contracts.ErrExpired, "quotas: the session lease expired")
}

// Admit asks for amount units of a dimension on behalf of the tenant of scope,
// and either takes them or refuses with a typed error and takes nothing.
//
//	requests_per_minute  takes amount tokens from the bucket; the lease is empty
//	concurrent_sessions  takes amount slots; release the lease when the session ends
//	ingest_bytes_per_day charges amount to today's counter (one short write transaction)
//	storage_bytes        charges amount to the gauge (Refund gives it back)
//
// Refusals are rate_limited (the bucket; retryable, with a retry_after_ms
// detail) or resource_exhausted (everything else; names the dimension, the
// limit and, for a daily counter, when the window resets). The returned lease
// is nil whenever the error is not. An invalid scope is refused with its own
// code (scope_missing or scope_violation): there is no default tenant.
//
// Admit is for the start of work, before a prepare or any step that begins
// something. It must not be called to refuse a commit the user has already
// confirmed (ADR 0018). Callers that write the data in a transaction of their
// own use ChargeTx for the volume dimensions, so the data and its charge
// commit or roll back together.
func (q *Quotas) Admit(ctx context.Context, scope contracts.Scope, dim Dimension, amount int64) (*Lease, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := checkAmount(dim, amount); err != nil {
		return nil, err
	}
	key := tenantKey{scope.ApplicationID, scope.TenantID}
	lim := q.limitsFor(key)
	var (
		lease = noLease
		err   error
	)
	switch dim {
	case DimRequestsPerMinute:
		err = q.take(key, lim, amount)
	case DimConcurrentSessions:
		lease, err = q.acquire(key, lim, amount)
	default:
		err = q.db.Tx(ctx, func(tx *store.Tx) error { return q.charge(ctx, tx, key, dim, amount) })
	}
	if err != nil {
		q.rejected[dimIndex(dim)].Add(1)
		return nil, err
	}
	q.admitted[dimIndex(dim)].Add(1)
	return lease, nil
}

func checkAmount(dim Dimension, amount int64) error {
	switch {
	case !dim.valid():
		return contracts.Newf(contracts.ErrValidationFailed, "quotas: unknown dimension %q", string(dim))
	case amount < 1 || amount > MaxAmount:
		return contracts.Newf(contracts.ErrValidationFailed, "quotas: an amount must be between 1 and %d, got %d", MaxAmount, amount).
			WithDetails("dimension", string(dim))
	}
	return nil
}

// stateLocked returns the tenant's state, creating it, and forgets idle tenants
// now and then. The caller holds q.mu.
func (q *Quotas) stateLocked(k tenantKey, now time.Time) *tenantState {
	if now.Sub(q.lastSweep) >= q.sweep {
		q.sweepLocked(now)
	}
	st := q.state[k]
	if st == nil {
		st = &tenantState{}
		q.state[k] = st
	}
	st.lastSeen = now
	return st
}

// sweepLocked forgets the tenants that hold nothing: no live lease and a bucket
// that is already full again, so the state a fresh tenant would get is the same
// and nobody gains tokens by being forgotten. A tenant that is still waiting
// for tokens, or still has a session open, is kept.
func (q *Quotas) sweepLocked(now time.Time) {
	q.lastSweep = now
	idle := 2 * q.sweep
	for k, st := range q.state {
		if now.Sub(st.lastSeen) < idle {
			continue
		}
		st.purge(now)
		if st.slots > 0 {
			continue
		}
		lim := q.limitsFor(k)
		if lim.RequestsPerMinute > 0 && !st.bucket.full(now, lim.burst(), lim.RequestsPerMinute) {
			continue
		}
		delete(q.state, k)
	}
}

// take removes tokens from the tenant's bucket.
func (q *Quotas) take(k tenantKey, lim Limits, n int64) error {
	rate := lim.RequestsPerMinute
	if rate == Unlimited {
		return nil
	}
	burst := lim.burst()
	if n > burst {
		return contracts.Newf(contracts.ErrResourceExhausted,
			"quotas: a request of %d is larger than the burst %d and can never be admitted", n, burst).
			WithDetails("dimension", string(DimRequestsPerMinute)).
			WithDetails("limit", burst).
			WithDetails("requested", n)
	}
	q.mu.Lock()
	now := q.clk.Now()
	ok, wait := q.stateLocked(k, now).bucket.take(now, burst, rate, n)
	q.mu.Unlock()
	if ok {
		return nil
	}
	return contracts.New(contracts.ErrRateLimited, "quotas: the tenant is over its request rate").
		WithDetails("dimension", string(DimRequestsPerMinute)).
		WithDetails("limit", rate).
		WithDetails("retry_after_ms", (wait + time.Millisecond - 1).Milliseconds())
}

// acquire takes session slots.
func (q *Quotas) acquire(k tenantKey, lim Limits, n int64) (*Lease, error) {
	limit := lim.ConcurrentSessions
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.clk.Now()
	st := q.stateLocked(k, now)
	st.purge(now)
	if limit != Unlimited && st.slots > limit-n {
		return nil, contracts.New(contracts.ErrResourceExhausted, "quotas: the tenant has all its concurrent sessions open").
			WithDetails("dimension", string(DimConcurrentSessions)).
			WithDetails("limit", limit).
			WithDetails("used", st.slots).
			WithDetails("requested", n)
	}
	q.nextLease++
	if st.leases == nil {
		st.leases = map[uint64]held{}
	}
	st.leases[q.nextLease] = held{expires: now.Add(q.ttl), n: n}
	st.slots += n
	return &Lease{q: q, key: k, id: q.nextLease}, nil
}

// ChargeTx charges amount to a persisted dimension (ingest_bytes_per_day or
// storage_bytes) inside the caller's write transaction, which is the one that
// writes the data the amount describes: when the quota is exhausted the error
// aborts that transaction, and the data is not written; when the data write
// fails later, the charge rolls back with it. It returns resource_exhausted,
// naming the dimension, the limit, what is used and, for the daily counter,
// when the window resets, and charges nothing.
func (q *Quotas) ChargeTx(ctx context.Context, tx *store.Tx, scope contracts.Scope, dim Dimension, amount int64) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if err := checkAmount(dim, amount); err != nil {
		return err
	}
	if !dim.persisted() {
		return contracts.Newf(contracts.ErrValidationFailed, "quotas: %s is not a persisted dimension; use Admit", string(dim))
	}
	return q.charge(ctx, tx, tenantKey{scope.ApplicationID, scope.TenantID}, dim, amount)
}

// Check reports whether amount of a persisted dimension would be admitted now,
// and charges nothing: it is the cheap early refusal in front of work whose real
// charge is made later, in the data's own transaction with ChargeTx (an upload
// that is over its quota is turned away before a byte is read). It is advisory
// by design. The answer can be stale by the time the data is written, so it is
// never the check that lets a write through: ChargeTx is, on the single writer,
// and a Check that passes is no promise that ChargeTx will.
func (q *Quotas) Check(ctx context.Context, scope contracts.Scope, dim Dimension, amount int64) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if err := checkAmount(dim, amount); err != nil {
		return err
	}
	if !dim.persisted() {
		return contracts.Newf(contracts.ErrValidationFailed, "quotas: %s is not a persisted dimension; use Admit", string(dim))
	}
	k := tenantKey{scope.ApplicationID, scope.TenantID}
	now := q.clk.Now()
	limit := q.limitsFor(k).of(dim)
	if limit == Unlimited {
		return nil
	}
	var used int64
	err := q.db.View(ctx, func(tx *store.Tx) error {
		switch err := tx.QueryRowContext(ctx, selectUsed, k.application, k.tenant, string(dim), windowStart(dim, now)).Scan(&used); {
		case errors.Is(err, sql.ErrNoRows):
			used = 0
		case err != nil:
			return err
		}
		return nil
	})
	if err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "quotas: the counter cannot be read")
	}
	if used > limit-amount {
		q.rejected[dimIndex(dim)].Add(1)
		return exhausted(dim, limit, used, amount, now)
	}
	return nil
}

// windowStart is the start of the dimension's current window in unix
// milliseconds: the UTC day for the daily counter, 0 for the gauge.
func windowStart(dim Dimension, now time.Time) int64 {
	if dim == DimIngestBytesPerDay {
		return now.UTC().Truncate(24 * time.Hour).UnixMilli()
	}
	return 0
}

// The statements name all three scope columns and the dimension, always from a
// tenantKey that came out of a validated Scope. workspace_id is the empty string
// on every row: quotas belong to a tenant, not to one of its workspaces.
const (
	selectUsed = `SELECT used FROM quota_counters
		WHERE application_id = ? AND tenant_id = ? AND workspace_id = '' AND dimension = ? AND window_start = ?`
	upsertUsed = `INSERT INTO quota_counters (application_id, tenant_id, workspace_id, dimension, window_start, used, updated_at)
		VALUES (?, ?, '', ?, ?, ?, ?)
		ON CONFLICT (application_id, tenant_id, workspace_id, dimension, window_start)
		DO UPDATE SET used = excluded.used, updated_at = excluded.updated_at`
)

func (q *Quotas) charge(ctx context.Context, tx *store.Tx, k tenantKey, dim Dimension, amount int64) error {
	now := q.clk.Now()
	ws := windowStart(dim, now)
	var used int64
	switch err := tx.QueryRowContext(ctx, selectUsed, k.application, k.tenant, string(dim), ws).Scan(&used); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return contracts.Wrap(contracts.ErrUnavailable, err, "quotas: the counter cannot be read")
	}
	limit := q.limitsFor(k).of(dim)
	if limit != Unlimited && used > limit-amount {
		return exhausted(dim, limit, used, amount, now)
	}
	if used > MaxAmount-amount {
		return contracts.New(contracts.ErrResourceExhausted, "quotas: the counter is at its ceiling").
			WithDetails("dimension", string(dim))
	}
	if _, err := tx.ExecContext(ctx, upsertUsed, k.application, k.tenant, string(dim), ws, used+amount, now.UnixMilli()); err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "quotas: the counter cannot be written")
	}
	return nil
}

func exhausted(dim Dimension, limit, used, amount int64, now time.Time) error {
	e := contracts.Newf(contracts.ErrResourceExhausted, "quotas: the tenant's %s quota is used up", string(dim)).
		WithDetails("dimension", string(dim)).
		WithDetails("limit", limit).
		WithDetails("used", used).
		WithDetails("requested", amount)
	if dim == DimIngestBytesPerDay {
		e.WithDetails("resets_at", now.UTC().Truncate(24*time.Hour).Add(24*time.Hour).Format(time.RFC3339))
	}
	return e
}

// Refund gives back amount of a persisted dimension: bytes that were charged
// but not stored (the ingestion failed after Admit), or storage that was
// deleted. The counter stops at zero, never below, and a refund of a daily
// counter applies to today's window. Refund is not a quota check and cannot be
// refused for quota.
func (q *Quotas) Refund(ctx context.Context, scope contracts.Scope, dim Dimension, amount int64) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if err := checkAmount(dim, amount); err != nil {
		return err
	}
	if !dim.persisted() {
		return contracts.Newf(contracts.ErrValidationFailed, "quotas: %s is not a persisted dimension", string(dim))
	}
	now := q.clk.Now()
	_, err := q.db.ExecContext(ctx, `UPDATE quota_counters SET used = max(used - ?, 0), updated_at = ?
		WHERE application_id = ? AND tenant_id = ? AND workspace_id = '' AND dimension = ? AND window_start = ?`,
		amount, now.UnixMilli(), scope.ApplicationID, scope.TenantID, string(dim), windowStart(dim, now))
	if err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "quotas: the counter cannot be written")
	}
	return nil
}

// SetStorage replaces the storage gauge with a measured value: the maintenance
// job that reconciles the gauge against the sizes of what the tenant holds
// (class 6) calls it, so a drift in either direction does not last.
func (q *Quotas) SetStorage(ctx context.Context, scope contracts.Scope, bytes int64) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if bytes < 0 || bytes > MaxAmount {
		return contracts.Newf(contracts.ErrValidationFailed, "quotas: storage must be between 0 and %d, got %d", MaxAmount, bytes)
	}
	_, err := q.db.ExecContext(ctx, upsertUsed, scope.ApplicationID, scope.TenantID, string(DimStorageBytes), int64(0), bytes, q.clk.Now().UnixMilli())
	if err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "quotas: the counter cannot be written")
	}
	return nil
}

// Prune deletes the daily counters of windows that ended before the last keep
// whole days (the gauge is never touched). It is deployment-wide maintenance for
// the dispatcher side, like jobs.Prune: never reachable from a tenant request.
// It returns how many rows it deleted.
func (q *Quotas) Prune(ctx context.Context, keepDays int) (int64, error) {
	if keepDays < 1 {
		return 0, contracts.New(contracts.ErrValidationFailed, "quotas: keep at least one day")
	}
	cutoff := q.clk.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -(keepDays - 1)).UnixMilli()
	res, err := q.db.ExecContext(ctx, `DELETE FROM quota_counters WHERE dimension = ? AND window_start < ?`,
		string(DimIngestBytesPerDay), cutoff)
	if err != nil {
		return 0, contracts.Wrap(contracts.ErrUnavailable, err, "quotas: the counters cannot be pruned")
	}
	return res.RowsAffected()
}

// Usage is one dimension of a tenant's report.
type Usage struct {
	Dimension Dimension
	// Limit is the effective limit; Unlimited when there is none.
	Limit int64
	// Used is what the tenant consumed (the bucket's spent tokens, the slots held,
	// today's bytes, the bytes stored).
	Used int64
	// Remaining is what can still be taken now, never negative; -1 when unlimited.
	// For requests_per_minute it is the tokens in the bucket (the burst is its
	// capacity), for the others Limit-Used.
	Remaining int64
	// ResetsAt is when a daily counter starts again; zero for the others.
	ResetsAt time.Time
}

// Usage reports the scope's own tenant, and only that tenant: the key comes from
// the verified scope, so there is no way to ask about another one.
func (q *Quotas) Usage(ctx context.Context, scope contracts.Scope) ([]Usage, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	k := tenantKey{scope.ApplicationID, scope.TenantID}
	lim := q.limitsFor(k)
	now := q.clk.Now()
	out := make([]Usage, 0, numDims)
	add := func(d Dimension, used int64, reset time.Time) {
		u := Usage{Dimension: d, Limit: lim.of(d), Used: used, ResetsAt: reset, Remaining: Unlimited}
		if u.Limit != Unlimited {
			u.Remaining = max(0, u.Limit-used)
		}
		out = append(out, u)
	}

	q.mu.Lock()
	var slots int64
	burst := lim.burst()
	tokens := burst // a tenant with no state has a full bucket
	if st := q.state[k]; st != nil {
		st.purge(now)
		slots = st.slots
		if lim.RequestsPerMinute > 0 {
			tokens = st.bucket.available(now, burst, lim.RequestsPerMinute)
		}
	}
	q.mu.Unlock()
	add(DimRequestsPerMinute, burst-tokens, time.Time{})
	if lim.RequestsPerMinute != Unlimited {
		out[0].Remaining = tokens
	} else {
		out[0].Used = 0
	}
	add(DimConcurrentSessions, slots, time.Time{})

	var day, stored int64
	err := q.db.View(ctx, func(tx *store.Tx) error {
		for _, c := range []struct {
			dim Dimension
			out *int64
		}{{DimIngestBytesPerDay, &day}, {DimStorageBytes, &stored}} {
			switch err := tx.QueryRowContext(ctx, selectUsed, k.application, k.tenant, string(c.dim), windowStart(c.dim, now)).Scan(c.out); {
			case errors.Is(err, sql.ErrNoRows):
				*c.out = 0
			case err != nil:
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "quotas: the counters cannot be read")
	}
	add(DimIngestBytesPerDay, day, now.UTC().Truncate(24*time.Hour).Add(24*time.Hour))
	add(DimStorageBytes, stored, time.Time{})
	return out, nil
}

// Stats are local counters of what Admit decided; nothing leaves the process.
type Stats struct {
	// TrackedTenants is how many tenants hold in-memory state right now.
	TrackedTenants int
	Admitted       map[Dimension]uint64
	Rejected       map[Dimension]uint64
}

// Stats returns the counters.
func (q *Quotas) Stats() Stats {
	q.mu.Lock()
	n := len(q.state)
	q.mu.Unlock()
	s := Stats{TrackedTenants: n, Admitted: map[Dimension]uint64{}, Rejected: map[Dimension]uint64{}}
	for _, d := range Dimensions() {
		s.Admitted[d] = q.admitted[dimIndex(d)].Load()
		s.Rejected[d] = q.rejected[dimIndex(d)].Load()
	}
	return s
}

// String names the effective limits, for logs: never tenant data.
func (l Limits) String() string {
	return fmt.Sprintf("rpm=%d burst=%d sessions=%d ingest/day=%d storage=%d",
		l.RequestsPerMinute, l.burst(), l.ConcurrentSessions, l.IngestBytesPerDay, l.StorageBytes)
}
