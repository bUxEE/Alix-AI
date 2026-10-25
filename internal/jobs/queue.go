package jobs

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Options is everything NewQueue needs besides the database.
type Options struct {
	// Config sizes the queue; zero fields take the defaults. Use FromConfig
	// for the process configuration.
	Config Config
	// Clock is the only source of time. Nil means clock.Real.
	Clock clock.Clock
	// Logger receives job lifecycle events (ids, types, classes and codes;
	// never payloads). Nil discards.
	Logger *slog.Logger
	// Guard is the enqueue admission hook (ADR 0018). Nil admits everything
	// the class depth allows.
	Guard EnqueueGuard
	// Policy orders jobs inside a class. Nil means FIFO.
	Policy DequeuePolicy
	// Jitter returns a number in [0, 1) used to spread retry delays. Nil means
	// math/rand; tests inject a fixed value.
	Jitter func() float64
}

// Queue is the durable queue over one store.DB. It is safe for concurrent use.
//
// Two groups of methods, with different scoping on purpose. The tenant-facing
// ones take a contracts.Scope and confine every statement to it: Enqueue, Get,
// Cancel and ScopedStatus. The dispatcher-facing ones work across every tenant
// because a worker serves the whole deployment: Lease, Heartbeat, Complete,
// Fail, Release, Reap, Prune and Status. They are for the Pool and the
// operations API, never for a request that carries a tenant's input; the
// lease operations still name the job's own scope in their statements.
type Queue struct {
	db     *store.DB
	clk    clock.Clock
	ids    *ids.Generator
	log    *slog.Logger
	cfg    Config
	guard  EnqueueGuard
	policy DequeuePolicy
	jitter func() float64

	mu    sync.RWMutex
	types map[string]TypeSpec

	runMu   sync.Mutex
	running map[string]*context.CancelCauseFunc

	subMu sync.Mutex
	subs  map[chan struct{}]struct{}
}

// NewQueue returns a Queue on db. The "jobs" migrations must be registered
// with the registry db was opened with (importing this package does that for
// store.Default).
func NewQueue(db *store.DB, opts Options) (*Queue, error) {
	if db == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "jobs: nil store")
	}
	if err := opts.Config.Validate(); err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "jobs: invalid configuration")
	}
	q := &Queue{
		db:      db,
		clk:     opts.Clock,
		log:     opts.Logger,
		cfg:     opts.Config.withDefaults(),
		guard:   opts.Guard,
		policy:  opts.Policy,
		jitter:  opts.Jitter,
		types:   map[string]TypeSpec{},
		running: map[string]*context.CancelCauseFunc{},
		subs:    map[chan struct{}]struct{}{},
	}
	if q.clk == nil {
		q.clk = clock.Real{}
	}
	if q.log == nil {
		q.log = slog.New(slog.DiscardHandler)
	}
	if q.jitter == nil {
		q.jitter = rand.Float64
	}
	q.ids = ids.NewGenerator(q.clk)
	return q, nil
}

// Config returns the resolved configuration (defaults applied).
func (q *Queue) Config() Config { return q.cfg }

// Register adds a job type. It can be called at any time, typically while
// wiring a module; registering a name twice is an error.
func (q *Queue) Register(spec TypeSpec) error {
	switch {
	case len(spec.Name) > maxTypeNameLen || !typeNameRE.MatchString(spec.Name):
		return contracts.Newf(contracts.ErrValidationFailed, "jobs: invalid job type name %q", spec.Name)
	case !spec.Class.Valid():
		return contracts.Newf(contracts.ErrValidationFailed, "jobs: job type %q has invalid class %d", spec.Name, spec.Class)
	case spec.MaxAttempts < 0 || spec.MaxAttempts > maxAttemptsCap:
		return contracts.Newf(contracts.ErrValidationFailed, "jobs: job type %q max attempts %d out of range", spec.Name, spec.MaxAttempts)
	case spec.BackoffBase < 0 || spec.BackoffMax < 0 || spec.BackoffBase > maxBackoff || spec.BackoffMax > maxBackoff:
		return contracts.Newf(contracts.ErrValidationFailed, "jobs: job type %q backoff out of range", spec.Name)
	}
	if spec.MaxAttempts == 0 {
		spec.MaxAttempts = q.cfg.MaxAttempts
	}
	if spec.BackoffBase == 0 {
		spec.BackoffBase = q.cfg.BackoffBase
	}
	if spec.BackoffMax == 0 {
		spec.BackoffMax = q.cfg.BackoffMax
	}
	if spec.BackoffBase > spec.BackoffMax {
		return contracts.Newf(contracts.ErrValidationFailed, "jobs: job type %q backoff base exceeds its maximum", spec.Name)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, dup := q.types[spec.Name]; dup {
		return contracts.Newf(contracts.ErrConflict, "jobs: job type %q already registered", spec.Name)
	}
	q.types[spec.Name] = spec
	return nil
}

func (q *Queue) spec(name string) (TypeSpec, bool) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	s, ok := q.types[name]
	return s, ok
}

// Enqueue adds a job under scope. The class comes from the registered type.
//
// With an idempotency key, repeating the call returns the first job with
// Duplicate set and enqueues nothing (and is not counted against depth or the
// guard); the same key with a different type or payload is a conflict, not a
// silent drop. Otherwise Enqueue refuses with resource_exhausted when the
// class already has its maximum number of waiting jobs, then asks the
// EnqueueGuard, and only then inserts, all in one transaction.
func (q *Queue) Enqueue(ctx context.Context, scope contracts.Scope, req EnqueueRequest) (EnqueueResult, error) {
	if err := scope.Validate(); err != nil {
		return EnqueueResult{}, err
	}
	spec, ok := q.spec(req.Type)
	if !ok {
		return EnqueueResult{}, contracts.Newf(contracts.ErrValidationFailed, "jobs: unknown job type %q", req.Type)
	}
	payload := bytes.TrimSpace(req.Payload)
	if len(payload) == 0 {
		payload = []byte("null")
	}
	if len(payload) > q.cfg.MaxPayloadBytes {
		return EnqueueResult{}, contracts.Newf(contracts.ErrValidationFailed,
			"jobs: payload is %d bytes, the limit is %d", len(payload), q.cfg.MaxPayloadBytes)
	}
	if !json.Valid(payload) {
		return EnqueueResult{}, contracts.New(contracts.ErrValidationFailed, "jobs: payload is not valid JSON")
	}
	// Compact so a retry that differs only in whitespace is still the same
	// request for the idempotency comparison.
	var compact bytes.Buffer
	if err := json.Compact(&compact, payload); err != nil {
		return EnqueueResult{}, contracts.Wrap(contracts.ErrValidationFailed, err, "jobs: payload is not valid JSON")
	}
	payload = compact.Bytes()
	if err := checkToken("idempotency key", req.IdempotencyKey, maxIdempotencyLen); err != nil {
		return EnqueueResult{}, err
	}
	if err := checkToken("service identity", req.ServiceIdentity, maxServiceIDLen); err != nil {
		return EnqueueResult{}, err
	}
	scopeJSON, err := json.Marshal(scope)
	if err != nil {
		return EnqueueResult{}, contracts.Wrap(contracts.ErrInternal, err, "jobs: scope cannot be encoded")
	}

	var res EnqueueResult
	attempt := func() error {
		return q.db.Tx(ctx, func(tx *store.Tx) error {
			res = EnqueueResult{} // the transaction can be retried
			now := q.clk.Now()
			runAt := req.RunAt
			if runAt.IsZero() {
				runAt = now
			}
			if !req.Deadline.IsZero() && !req.Deadline.After(runAt) {
				return contracts.New(contracts.ErrValidationFailed, "jobs: deadline is not after the time the job may run")
			}

			if req.IdempotencyKey != "" {
				existing, found, err := q.findByKey(ctx, tx, scope, req.IdempotencyKey)
				if err != nil {
					return err
				}
				if found {
					if existing.Type != req.Type || !bytes.Equal(existing.Payload, payload) {
						return contracts.New(contracts.ErrConflict, "jobs: idempotency key was already used for a different job")
					}
					res = EnqueueResult{Job: existing, Duplicate: true}
					return nil
				}
			}

			pending, err := countOne(ctx, tx, `SELECT count(*) FROM jobs WHERE state = 'pending' AND class = ?`, int(spec.Class))
			if err != nil {
				return err
			}
			depth := q.cfg.classConfig(spec.Class).MaxDepth
			if pending >= depth {
				return contracts.Newf(contracts.ErrResourceExhausted,
					"jobs: the %s queue is full (%d waiting, limit %d)", spec.Class, pending, depth).
					WithDetails("class", spec.Class.String()).
					WithDetails("depth", pending).
					WithDetails("limit", depth)
			}

			if q.guard != nil {
				gr := GuardRequest{
					Scope: scope, Type: req.Type, Class: spec.Class, Now: now,
					ClassPending: pending, ClassMaxDepth: depth,
				}
				if gr.TenantPending, gr.TenantRunning, err = q.tenantCounts(ctx, tx, scope, spec.Class); err != nil {
					return err
				}
				if err := q.guard.AllowEnqueue(ctx, gr); err != nil {
					return guardError(err)
				}
			}

			id := q.ids.New("job")
			cols := []string{"id", "type", "class", "state", "payload", "idempotency_key", "scope_json", "service_identity",
				"max_attempts", "run_at", "deadline_at", "created_at", "updated_at"}
			_, err = store.InsertScoped(ctx, tx, scope, "jobs", cols,
				id, req.Type, int(spec.Class), string(StatePending), []byte(payload), nullString(req.IdempotencyKey), string(scopeJSON), nullString(req.ServiceIdentity),
				spec.MaxAttempts, toMs(runAt), nullTime(req.Deadline), toMs(now), toMs(now))
			if err != nil {
				return err
			}
			job, err := q.getByID(ctx, tx, scope, id)
			if err != nil {
				return err
			}
			res = EnqueueResult{Job: job}
			return nil
		})
	}
	err = attempt()
	if store.IsUniqueViolation(err) && req.IdempotencyKey != "" {
		// Another process inserted the same key between our lookup and insert;
		// the second pass finds it.
		err = attempt()
	}
	if err != nil {
		return EnqueueResult{}, err
	}
	if !res.Duplicate {
		q.log.Debug("job enqueued", "job_id", res.Job.ID, "type", res.Job.Type, "class", res.Job.Class.String())
		q.notify()
	}
	return res, nil
}

// guardError keeps a guard's contracts error and turns anything else into
// internal: a guard that cannot decide must not admit the job.
func guardError(err error) error {
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return err
	}
	return contracts.Wrap(contracts.ErrInternal, err, "jobs: enqueue guard failed")
}

// Get returns one job of the caller's scope. A job that does not exist and a
// job in another scope are indistinguishable: both are scope_violation.
func (q *Queue) Get(ctx context.Context, scope contracts.Scope, id string) (Job, error) {
	return q.getByID(ctx, q.db, scope, id)
}

func (q *Queue) getByID(ctx context.Context, r store.Reader, scope contracts.Scope, id string) (Job, error) {
	row, err := store.QueryRowScoped(ctx, r, scope, store.ScopedStmt{
		SQL:       "SELECT " + jobCols + " FROM jobs",
		Where:     "id = ?",
		WhereArgs: []any{id},
	})
	if err != nil {
		return Job{}, err
	}
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, errNotInScope()
	}
	return j, err
}

func errNotInScope() error {
	return contracts.New(contracts.ErrScopeViolation, "jobs: no such job in this scope")
}

func (q *Queue) findByKey(ctx context.Context, r store.Reader, scope contracts.Scope, key string) (Job, bool, error) {
	row, err := store.QueryRowScoped(ctx, r, scope, store.ScopedStmt{
		SQL:       "SELECT " + jobCols + " FROM jobs",
		Where:     "idempotency_key = ?",
		WhereArgs: []any{key},
	})
	if err != nil {
		return Job{}, false, err
	}
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	return j, err == nil, err
}

// tenantCounts is the tenant-level (all workspaces) queued and running count
// the guard compares against a per-tenant cap.
func (q *Queue) tenantCounts(ctx context.Context, r store.Reader, scope contracts.Scope, class Class) (pending, running int, err error) {
	rows, err := r.QueryContext(ctx,
		`SELECT state, count(*) FROM jobs
		 WHERE state IN ('pending', 'running') AND application_id = ? AND tenant_id = ? AND class = ?
		 GROUP BY state`, scope.ApplicationID, scope.TenantID, int(class))
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return 0, 0, err
		}
		switch State(state) {
		case StatePending:
			pending = n
		case StateRunning:
			running = n
		}
	}
	return pending, running, rows.Err()
}

// Cancel stops a job of the caller's scope. A pending job becomes cancelled at
// once. A running job is flagged and its context cancelled; it ends cancelled
// when its handler returns (or succeeded, if the handler finished anyway). The
// returned Job is the state after the call. Cancelling a cancelled job is a
// no-op; a job that already succeeded or failed is a conflict, so a caller is
// never told a cancel worked when it did not.
func (q *Queue) Cancel(ctx context.Context, scope contracts.Scope, id string) (Job, error) {
	var out Job
	var signal bool
	err := q.db.Tx(ctx, func(tx *store.Tx) error {
		signal = false
		job, err := q.getByID(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		now := toMs(q.clk.Now())
		switch job.State {
		case StatePending:
			_, err = store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
				SQL:       "UPDATE jobs SET state = 'cancelled', finished_at = ?, updated_at = ?",
				Args:      []any{now, now},
				Where:     "id = ? AND state = 'pending'",
				WhereArgs: []any{id},
			})
		case StateRunning:
			_, err = store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
				SQL:       "UPDATE jobs SET cancel_requested = 1, updated_at = ?",
				Args:      []any{now},
				Where:     "id = ? AND state = 'running'",
				WhereArgs: []any{id},
			})
			signal = true
		case StateCancelled:
		default:
			return contracts.Newf(contracts.ErrConflict, "jobs: job already %s", job.State)
		}
		if err != nil {
			return err
		}
		out, err = q.getByID(ctx, tx, scope, id)
		return err
	})
	if err != nil {
		return Job{}, err
	}
	if signal {
		q.signalCancel(id)
	}
	return out, nil
}

// track registers the cancel function of a running job so Cancel can reach it
// in this process; the returned function removes it, unless the job has been
// tracked again since (a worker that lost its lease ends after the job was
// leased to another).
func (q *Queue) track(id string, cancel context.CancelCauseFunc) func() {
	entry := &cancel
	q.runMu.Lock()
	q.running[id] = entry
	q.runMu.Unlock()
	return func() {
		q.runMu.Lock()
		if q.running[id] == entry {
			delete(q.running, id)
		}
		q.runMu.Unlock()
	}
}

func (q *Queue) signalCancel(id string) {
	q.runMu.Lock()
	cancel := q.running[id]
	q.runMu.Unlock()
	if cancel != nil {
		(*cancel)(errJobCancelled)
	}
}

// subscribe returns a channel that receives a signal after every successful
// enqueue; a Pool uses it to wake without polling.
func (q *Queue) subscribe() (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	q.subMu.Lock()
	q.subs[ch] = struct{}{}
	q.subMu.Unlock()
	return ch, func() {
		q.subMu.Lock()
		delete(q.subs, ch)
		q.subMu.Unlock()
	}
}

func (q *Queue) notify() {
	q.subMu.Lock()
	defer q.subMu.Unlock()
	for ch := range q.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// backoff is the retry delay after the given attempt: base * 2^(attempt-1),
// capped at max, then spread over [d/2, d) so retries do not move in step.
func (q *Queue) backoff(spec TypeSpec, attempt int) time.Duration {
	base, limit := spec.BackoffBase, spec.BackoffMax
	if base == 0 {
		base = q.cfg.BackoffBase
	}
	if limit == 0 {
		limit = q.cfg.BackoffMax
	}
	d := base
	for i := 1; i < attempt && d < limit; i++ {
		d *= 2
	}
	d = min(d, limit)
	j := min(max(q.jitter(), 0), 0.999999)
	return d/2 + time.Duration(float64(d/2)*j)
}

// jobCols is the column list scanJob reads, in order.
const jobCols = `id, application_id, tenant_id, workspace_id, type, class, state, payload,
	idempotency_key, scope_json, service_identity, attempts, max_attempts, run_at, deadline_at,
	cancel_requested, error_code, error_message, created_at, started_at, finished_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanJob(r rowScanner) (Job, error) {
	var (
		j                           Job
		appID, tenantID, wsID       string
		class                       int
		state, scopeJSON            string
		payload                     []byte
		idem, svc, errCode, errMsg  sql.NullString
		runAt, createdAt            int64
		deadline, started, finished sql.NullInt64
		cancel                      int
	)
	if err := r.Scan(&j.ID, &appID, &tenantID, &wsID, &j.Type, &class, &state, &payload,
		&idem, &scopeJSON, &svc, &j.Attempts, &j.MaxAttempts, &runAt, &deadline,
		&cancel, &errCode, &errMsg, &createdAt, &started, &finished); err != nil {
		return Job{}, err
	}
	if err := json.Unmarshal([]byte(scopeJSON), &j.Scope); err != nil {
		return Job{}, contracts.Wrap(contracts.ErrInternal, errCorruptRow, "jobs: stored scope is unreadable")
	}
	// The indexed columns and the stored scope must agree; a row where they do
	// not is corrupt or tampered with, and is never run.
	if j.Scope.ApplicationID != appID || j.Scope.TenantID != tenantID || j.Scope.WorkspaceID != wsID {
		return Job{}, contracts.Wrap(contracts.ErrInternal, errCorruptRow, "jobs: stored scope disagrees with the row's scope columns").WithDetails("job_id", j.ID)
	}
	j.Class = Class(class)
	j.State = State(state)
	j.Payload = payload
	j.IdempotencyKey = idem.String
	j.ServiceIdentity = svc.String
	j.RunAt = fromMs(runAt)
	if deadline.Valid {
		j.Deadline = fromMs(deadline.Int64)
	}
	j.CancelRequested = cancel != 0
	j.ErrorCode = contracts.ErrCode(errCode.String)
	j.ErrorMessage = errMsg.String
	j.CreatedAt = fromMs(createdAt)
	if started.Valid {
		j.StartedAt = fromMs(started.Int64)
	}
	if finished.Valid {
		j.FinishedAt = fromMs(finished.Int64)
	}
	return j, nil
}

func toMs(t time.Time) int64    { return t.UnixMilli() }
func fromMs(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return toMs(t)
}

func countOne(ctx context.Context, r store.Reader, query string, args ...any) (int, error) {
	var n int
	if err := r.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// checkToken validates an identifier-like caller string: empty is allowed,
// otherwise valid UTF-8 without control characters, at most max bytes.
func checkToken(what, s string, max int) error {
	if s == "" {
		return nil
	}
	if len(s) > max || !utf8.ValidString(s) || strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return contracts.Newf(contracts.ErrValidationFailed, "jobs: %s is not valid (control characters, bad UTF-8, or over %d bytes)", what, max)
	}
	return nil
}

// truncateMessage keeps a stored error message short.
func truncateMessage(s string) string {
	if len(s) <= maxMessageLen {
		return s
	}
	s = s[:maxMessageLen]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

var (
	errJobCancelled = errors.New("jobs: job cancelled")
	// errCorruptRow marks a row whose stored scope cannot be trusted.
	errCorruptRow = errors.New("jobs: corrupt row")
)

// placeholders returns "?, ?, ?" for n values.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
