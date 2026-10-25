package jobs

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The methods in this file are the dispatcher side of the queue. They work
// across every tenant because a worker serves the whole deployment, so Lease,
// Reap, Prune and the readiness checks are unscoped on purpose. Everything
// that acts on one job names that job's own scope as well as its id and lease
// token.

// Lease takes ready jobs for a worker.
//
// The most urgent requested class with ready work is served first (priority),
// then the DequeuePolicy orders inside it (FIFO by default). The yield rule
// applies first: a class numbered Config.BackgroundFrom or higher leases
// nothing while a job of a class up to Config.InteractiveUpTo is waiting. A
// job whose deadline has passed is failed with timeout and never returned.
// Leasing counts an attempt.
//
// When nothing is ready Lease reads through the reader pool and takes no
// write lock, so an idle poll is cheap. It returns nil, not an error.
func (q *Queue) Lease(ctx context.Context, req LeaseRequest) ([]Leased, error) {
	classes, err := normaliseClasses(req.Classes)
	if err != nil {
		return nil, err
	}
	for _, t := range req.Types {
		if t == "" {
			return nil, contracts.New(contracts.ErrValidationFailed, "jobs: empty job type in lease request")
		}
	}
	limit := min(max(req.Max, 1), maxLeaseBatch)
	ttl := req.TTL
	if ttl == 0 {
		ttl = q.cfg.LeaseTTL
	}
	if ttl < minLeaseTTL || ttl > maxLeaseTTL {
		return nil, contracts.Newf(contracts.ErrValidationFailed, "jobs: lease ttl %s out of range", ttl)
	}

	ready, err := q.hasReady(ctx, q.db, classes, req.Types, toMs(q.clk.Now()))
	if err != nil || !ready {
		return nil, err
	}

	var out []Leased
	err = q.db.Tx(ctx, func(tx *store.Tx) error {
		out = out[:0] // the transaction can be retried
		now := q.clk.Now()
		nowMs := toMs(now)
		if _, err := q.expireDeadlines(ctx, tx, nowMs); err != nil {
			return err
		}
		var yield leaseYield // per attempt: the transaction can be retried
		for len(out) < limit {
			j, ok, err := q.leaseOne(ctx, tx, classes, req.Types, now, ttl, &yield)
			if err != nil {
				return err
			}
			if !ok {
				break
			}
			out = append(out, j)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, l := range out {
		q.log.Debug("job leased", "job_id", l.ID, "type", l.Type, "class", l.Class.String(), "attempt", l.Attempts)
	}
	return out, nil
}

func normaliseClasses(in []Class) ([]Class, error) {
	if len(in) == 0 {
		return nil, contracts.New(contracts.ErrValidationFailed, "jobs: lease request names no class")
	}
	out := slices.Clone(in)
	slices.Sort(out)
	out = slices.Compact(out)
	for _, c := range out {
		if !c.Valid() {
			return nil, contracts.Newf(contracts.ErrValidationFailed, "jobs: invalid class %d", c)
		}
	}
	return out, nil
}

// typeFilter is " AND type IN (...)" for a non-empty list.
func typeFilter(types []string) (string, []any) {
	if len(types) == 0 {
		return "", nil
	}
	args := make([]any, len(types))
	for i, t := range types {
		args[i] = t
	}
	return " AND type IN (" + placeholders(len(types)) + ")", args
}

// yielding reports whether an interactive job is waiting, which stops the
// background classes from leasing (master plan 17.1). Waiting means pending
// and ready; a retry still inside its backoff is not waiting for a worker.
func (q *Queue) yielding(ctx context.Context, r store.Reader, nowMs int64) (bool, error) {
	var one int
	err := r.QueryRowContext(ctx,
		`SELECT 1 FROM jobs WHERE state = 'pending' AND class <= ? AND run_at <= ? LIMIT 1`,
		q.cfg.InteractiveUpTo, nowMs).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// hasReady reports, without locking, whether Lease would find work.
func (q *Queue) hasReady(ctx context.Context, r store.Reader, classes []Class, types []string, nowMs int64) (bool, error) {
	filter, fargs := typeFilter(types)
	yield, yieldKnown := false, false
	for _, c := range classes {
		if q.cfg.isBackground(c) {
			if !yieldKnown {
				var err error
				if yield, err = q.yielding(ctx, r, nowMs); err != nil {
					return false, err
				}
				yieldKnown = true
			}
			if yield {
				continue
			}
		}
		var one int
		err := r.QueryRowContext(ctx,
			`SELECT 1 FROM jobs WHERE state = 'pending' AND class = ? AND run_at <= ?`+filter+` LIMIT 1`,
			append([]any{int(c), nowMs}, fargs...)...).Scan(&one)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
	}
	return false, nil
}

// leaseYield remembers, inside one lease transaction, whether an interactive job
// is waiting (yielding), so it is asked once and not once per background class
// and job. Only the transaction itself changes rows meanwhile, and only a lease
// or a quarantine in an interactive class can change the answer: reaching such
// a class forgets it.
type leaseYield struct {
	known, yield bool
}

// leaseOne leases the next job inside the lease transaction.
func (q *Queue) leaseOne(ctx context.Context, tx *store.Tx, classes []Class, types []string, now time.Time, ttl time.Duration, yield *leaseYield) (Leased, bool, error) {
	nowMs := toMs(now)
	filter, fargs := typeFilter(types)
	for _, c := range classes {
		if q.cfg.isBackground(c) {
			if !yield.known {
				y, err := q.yielding(ctx, tx, nowMs)
				if err != nil {
					return Leased{}, false, err
				}
				yield.known, yield.yield = true, y
			}
			if yield.yield {
				continue
			}
		}
		if int(c) <= q.cfg.InteractiveUpTo {
			yield.known = false // picking in this class may lease or quarantine a job that counts as waiting
		}
		seq, job, ok, err := q.pickJob(ctx, tx, c, filter, fargs, nowMs)
		if err != nil {
			return Leased{}, false, err
		}
		if !ok {
			continue
		}
		token := q.ids.New("lease")
		until := now.Add(ttl)
		res, err := tx.ExecContext(ctx,
			`UPDATE jobs SET state = 'running', attempts = attempts + 1, lease_token = ?, lease_until = ?,
			 started_at = COALESCE(started_at, ?), updated_at = ? WHERE seq = ? AND state = 'pending'`,
			token, toMs(until), nowMs, nowMs, seq)
		if err != nil {
			return Leased{}, false, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return Leased{}, false, contracts.New(contracts.ErrInternal, "jobs: leased job vanished inside its own transaction")
		}
		job.State = StateRunning
		job.Attempts++
		if job.StartedAt.IsZero() {
			job.StartedAt = fromMs(nowMs)
		}
		return Leased{Job: job, Token: token, Until: until}, true, nil
	}
	return Leased{}, false, nil
}

// pickJob returns the next ready job of one class: the head of the class when
// there is no policy, otherwise the policy's choice among the oldest ready
// job of each tenant.
func (q *Queue) pickJob(ctx context.Context, tx *store.Tx, class Class, filter string, fargs []any, nowMs int64) (seq int64, job Job, ok bool, err error) {
	base := []any{int(class), nowMs}
	if q.policy == nil {
		// A row whose stored scope disagrees with its scope columns is never run;
		// it is failed so it cannot block the jobs behind it.
		for range maxQuarantinePerLease {
			row := tx.QueryRowContext(ctx,
				`SELECT seq, `+jobCols+` FROM jobs WHERE state = 'pending' AND class = ? AND run_at <= ?`+filter+` ORDER BY seq LIMIT 1`,
				append(base, fargs...)...)
			job, err = scanJob(&prefixScanner{r: row, pre: []any{&seq}})
			switch {
			case errors.Is(err, sql.ErrNoRows):
				return 0, Job{}, false, nil
			case errors.Is(err, errCorruptRow):
				if err := q.quarantine(ctx, tx, seq, nowMs); err != nil {
					return 0, Job{}, false, err
				}
				continue
			}
			return seq, job, err == nil, err
		}
		return 0, Job{}, false, nil
	}

	// One candidate per tenant: the lowest seq (oldest) ready job of each
	// application and tenant in the class, so a flood by one tenant cannot hide
	// everyone else from the policy.
	args := append(base, fargs...)
	args = append(args, q.cfg.PolicyWindow)
	rows, err := tx.QueryContext(ctx,
		`SELECT seq, id, application_id, tenant_id, workspace_id, type, class, created_at, attempts FROM jobs
		 WHERE seq IN (SELECT min(seq) FROM jobs WHERE state = 'pending' AND class = ? AND run_at <= ?`+filter+`
		               GROUP BY application_id, tenant_id)
		 ORDER BY seq LIMIT ?`, args...)
	if err != nil {
		return 0, Job{}, false, err
	}
	var cands []Candidate
	for rows.Next() {
		var c Candidate
		var cl int
		var created int64
		if err := rows.Scan(&c.Seq, &c.ID, &c.ApplicationID, &c.TenantID, &c.WorkspaceID, &c.Type, &cl, &created, &c.Attempts); err != nil {
			rows.Close()
			return 0, Job{}, false, err
		}
		c.Class, c.EnqueuedAt = Class(cl), fromMs(created)
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, Job{}, false, err
	}
	rows.Close()
	if len(cands) == 0 {
		return 0, Job{}, false, nil
	}
	i := q.policy.Pick(cands)
	if i < 0 || i >= len(cands) {
		return 0, Job{}, false, nil
	}
	job, err = scanJob(tx.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE seq = ?`, cands[i].Seq))
	if errors.Is(err, errCorruptRow) {
		return 0, Job{}, false, q.quarantine(ctx, tx, cands[i].Seq, nowMs)
	}
	if err != nil {
		return 0, Job{}, false, err
	}
	return cands[i].Seq, job, true, nil
}

// maxQuarantinePerLease bounds how many corrupt rows one lease call skips.
const maxQuarantinePerLease = 16

// quarantine fails a pending row that cannot be trusted.
func (q *Queue) quarantine(ctx context.Context, tx *store.Tx, seq, nowMs int64) error {
	q.log.Error("job row failed its scope integrity check and was quarantined", "seq", seq)
	_, err := tx.ExecContext(ctx,
		`UPDATE jobs SET state = 'failed', error_code = 'internal', error_message = ?, finished_at = ?, updated_at = ?
		 WHERE seq = ? AND state = 'pending'`,
		"stored scope disagrees with the row's scope columns", nowMs, nowMs, seq)
	return err
}

// prefixScanner scans extra leading columns before the ones scanJob reads.
type prefixScanner struct {
	r   rowScanner
	pre []any
}

func (p *prefixScanner) Scan(dest ...any) error {
	return p.r.Scan(append(slices.Clone(p.pre), dest...)...)
}

// expireDeadlines fails every waiting job whose deadline has passed.
func (q *Queue) expireDeadlines(ctx context.Context, tx *store.Tx, nowMs int64) (int, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE jobs SET state = 'failed', error_code = 'timeout', error_message = ?, finished_at = ?, updated_at = ?
		 WHERE state = 'pending' AND deadline_at IS NOT NULL AND deadline_at <= ?`,
		"deadline passed before the job started", nowMs, nowMs, nowMs)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// HeartbeatResult is what a heartbeat learned.
type HeartbeatResult struct {
	// Until is the new lease expiry.
	Until time.Time
	// CancelRequested is true when the job was asked to cancel (possibly by
	// another process); the worker should cancel its handler.
	CancelRequested bool
}

// Heartbeat extends a lease by Config.LeaseTTL. It fails with a lease-lost
// error (see IsLeaseLost) when the worker no longer holds the job; the worker
// must then stop and write nothing.
func (q *Queue) Heartbeat(ctx context.Context, l Leased) (HeartbeatResult, error) {
	var res HeartbeatResult
	err := q.db.Tx(ctx, func(tx *store.Tx) error {
		now := q.clk.Now()
		until := now.Add(q.cfg.LeaseTTL)
		row, err := store.QueryRowScoped(ctx, tx, l.Scope, store.ScopedStmt{
			SQL:       "UPDATE jobs SET lease_until = ?, updated_at = ?",
			Args:      []any{toMs(until), toMs(now)},
			Where:     "id = ? AND lease_token = ? AND state = 'running'",
			WhereArgs: []any{l.ID, l.Token},
			Tail:      "RETURNING cancel_requested",
		})
		if err != nil {
			return err
		}
		var cancel int
		if err := row.Scan(&cancel); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return leaseLost()
			}
			return err
		}
		res = HeartbeatResult{Until: until, CancelRequested: cancel != 0}
		return nil
	})
	return res, err
}

// Complete marks a leased job succeeded. A job that was asked to cancel but
// finished anyway is succeeded: the work was done.
func (q *Queue) Complete(ctx context.Context, l Leased) error {
	return q.db.Tx(ctx, func(tx *store.Tx) error {
		now := toMs(q.clk.Now())
		res, err := store.ExecScoped(ctx, tx, l.Scope, store.ScopedStmt{
			SQL: `UPDATE jobs SET state = 'succeeded', error_code = NULL, error_message = NULL,
			      lease_token = NULL, lease_until = NULL, finished_at = ?, updated_at = ?`,
			Args:      []any{now, now},
			Where:     "id = ? AND lease_token = ? AND state = 'running'",
			WhereArgs: []any{l.ID, l.Token},
		})
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return leaseLost()
		}
		return nil
	})
}

// Fail records that an attempt failed and decides what happens next:
//
//   - the job was asked to cancel: it becomes cancelled;
//   - the error is not retryable (a non-retryable code, or Permanent), or the
//     type's attempts are used up: it becomes failed with the error's code;
//   - otherwise it returns to pending after an exponential, jittered backoff,
//     unless the retry would start after the job's deadline, in which case it
//     fails with timeout.
//
// Only the error's contracts code and message are stored, never the text of a
// wrapped cause, so an internal error cannot leak through a job record.
func (q *Queue) Fail(ctx context.Context, l Leased, cause error) (Outcome, error) {
	code, retry, msg := classify(cause)
	var out Outcome
	err := q.db.Tx(ctx, func(tx *store.Tx) error {
		out = Outcome{}
		now := q.clk.Now()
		nowMs := toMs(now)
		row, err := store.QueryRowScoped(ctx, tx, l.Scope, store.ScopedStmt{
			SQL:       "SELECT attempts, max_attempts, deadline_at, cancel_requested FROM jobs",
			Where:     "id = ? AND lease_token = ? AND state = 'running'",
			WhereArgs: []any{l.ID, l.Token},
		})
		if err != nil {
			return err
		}
		var attempts, maxAttempts, cancel int
		var deadline sql.NullInt64
		if err := row.Scan(&attempts, &maxAttempts, &deadline, &cancel); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return leaseLost()
			}
			return err
		}

		state, errCode, errMsg := StateFailed, string(code), msg
		var next time.Time
		switch {
		case cancel != 0:
			state, errCode, errMsg = StateCancelled, "", ""
		case retry && attempts < maxAttempts:
			spec, _ := q.spec(l.Type)
			next = now.Add(q.backoff(spec, attempts))
			if deadline.Valid && toMs(next) >= deadline.Int64 {
				state, errCode, errMsg = StateFailed, string(contracts.ErrTimeout), "the retry would start after the job's deadline"
				next = time.Time{}
			} else {
				state = StatePending
			}
		case retry:
			errMsg = truncateMessage(msg + " (attempts exhausted)")
		}

		var set string
		var args []any
		if state == StatePending {
			set = `state = 'pending', run_at = ?, lease_token = NULL, lease_until = NULL, error_code = ?, error_message = ?, updated_at = ?`
			args = []any{toMs(next), errCode, errMsg, nowMs}
		} else {
			set = `state = ?, error_code = ?, error_message = ?, lease_token = NULL, lease_until = NULL, finished_at = ?, updated_at = ?`
			args = []any{string(state), nullString(errCode), nullString(errMsg), nowMs, nowMs}
		}
		res, err := store.ExecScoped(ctx, tx, l.Scope, store.ScopedStmt{
			SQL:       "UPDATE jobs SET " + set,
			Args:      args,
			Where:     "id = ? AND lease_token = ? AND state = 'running'",
			WhereArgs: []any{l.ID, l.Token},
		})
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return leaseLost()
		}
		out = Outcome{State: state, RunAt: next, Code: contracts.ErrCode(errCode)}
		return nil
	})
	if err != nil {
		return Outcome{}, err
	}
	q.log.Info("job attempt failed", "job_id", l.ID, "type", l.Type, "class", l.Class.String(),
		"attempt", l.Attempts, "outcome", string(out.State), "code", string(out.Code))
	return out, nil
}

// classify turns a handler error into a stored code, a retry decision and a
// safe message.
func classify(cause error) (code contracts.ErrCode, retry bool, msg string) {
	var ce *contracts.Error
	switch {
	case cause == nil:
		return contracts.ErrInternal, false, "job failed without an error"
	case errors.As(cause, &ce) && ce != nil:
		code, msg = ce.Code, ce.Message
	default:
		code, msg = contracts.ErrInternal, "job handler returned an error"
	}
	if msg == "" {
		msg = string(code)
	}
	return code, code.Retryable() && !IsPermanent(cause), truncateMessage(msg)
}

// Release hands a leased job back without counting the attempt: the Pool uses
// it when it stops mid-run. A job that had been asked to cancel becomes
// cancelled instead.
func (q *Queue) Release(ctx context.Context, l Leased) error {
	return q.db.Tx(ctx, func(tx *store.Tx) error {
		now := toMs(q.clk.Now())
		where, wargs := "id = ? AND lease_token = ? AND state = 'running'", []any{l.ID, l.Token}
		if _, err := store.ExecScoped(ctx, tx, l.Scope, store.ScopedStmt{
			SQL:       "UPDATE jobs SET state = 'cancelled', lease_token = NULL, lease_until = NULL, finished_at = ?, updated_at = ?",
			Args:      []any{now, now},
			Where:     where + " AND cancel_requested = 1",
			WhereArgs: wargs,
		}); err != nil {
			return err
		}
		// A job cancelled above no longer matches here, which is fine.
		_, err := store.ExecScoped(ctx, tx, l.Scope, store.ScopedStmt{
			SQL:       "UPDATE jobs SET state = 'pending', attempts = max(attempts - 1, 0), run_at = ?, lease_token = NULL, lease_until = NULL, updated_at = ?",
			Args:      []any{now, now},
			Where:     where,
			WhereArgs: wargs,
		})
		return err
	})
}

// Reap is the maintenance pass: it returns jobs whose lease expired (the
// worker crashed or stopped heartbeating) to pending, and fails waiting jobs
// whose deadline passed with timeout. A job whose lease expired is not
// simply requeued when it was asked to cancel (cancelled), when its deadline
// passed (timeout), or when it was on its last attempt (failed with
// uncertain_outcome: the handler may have done its work, and requeueing a
// job that keeps killing its worker would loop forever).
//
// It reads first and takes the write lock only when there is something to do.
func (q *Queue) Reap(ctx context.Context) (Reaped, error) {
	nowMs := toMs(q.clk.Now())
	var due int
	err := q.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM jobs WHERE state = 'running' AND lease_until <= ?)
		     OR EXISTS (SELECT 1 FROM jobs WHERE state = 'pending' AND deadline_at IS NOT NULL AND deadline_at <= ?)`,
		nowMs, nowMs).Scan(&due)
	if err != nil || due == 0 {
		return Reaped{}, err
	}

	var out Reaped
	err = q.db.Tx(ctx, func(tx *store.Tx) error {
		out = Reaped{}
		nowMs := toMs(q.clk.Now())
		n, err := q.expireDeadlines(ctx, tx, nowMs)
		if err != nil {
			return err
		}
		out.TimedOut = n

		const expired = `state = 'running' AND lease_until <= ?`
		step := func(set, extra string, args ...any) (int, error) {
			res, err := tx.ExecContext(ctx, "UPDATE jobs SET "+set+" WHERE "+expired+extra, args...)
			if err != nil {
				return 0, err
			}
			n, _ := res.RowsAffected()
			return int(n), nil
		}
		const clear = `lease_token = NULL, lease_until = NULL, updated_at = ?`
		if out.Cancelled, err = step(`state = 'cancelled', finished_at = ?, `+clear,
			` AND cancel_requested = 1`, nowMs, nowMs, nowMs); err != nil {
			return err
		}
		n, err = step(`state = 'failed', error_code = 'timeout', error_message = 'deadline passed while the job was running', finished_at = ?, `+clear,
			` AND deadline_at IS NOT NULL AND deadline_at <= ?`, nowMs, nowMs, nowMs, nowMs)
		if err != nil {
			return err
		}
		out.TimedOut += n
		if out.Exhausted, err = step(`state = 'failed', error_code = 'uncertain_outcome', error_message = 'lease expired on the last attempt; the outcome is unknown', finished_at = ?, `+clear,
			` AND attempts >= max_attempts`, nowMs, nowMs, nowMs); err != nil {
			return err
		}
		out.Requeued, err = step(`state = 'pending', run_at = ?, `+clear, ``, nowMs, nowMs, nowMs)
		return err
	})
	if err != nil {
		return Reaped{}, err
	}
	if out.Total() > 0 {
		q.log.Warn("job maintenance pass", "requeued", out.Requeued, "timed_out", out.TimedOut,
			"exhausted", out.Exhausted, "cancelled", out.Cancelled)
	}
	return out, nil
}

// Prune deletes finished jobs older than Config.RetainFinished, at most
// pruneBatch per call so the writer is never held long; call it again while
// it returns the batch size. Deleting a job also releases its idempotency key.
func (q *Queue) Prune(ctx context.Context) (int, error) {
	cutoff := toMs(q.clk.Now().Add(-q.cfg.RetainFinished))
	var n int
	err := q.db.Tx(ctx, func(tx *store.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM jobs WHERE seq IN (
			     SELECT seq FROM jobs WHERE state IN ('succeeded', 'failed', 'cancelled') AND finished_at <= ?
			     ORDER BY finished_at LIMIT ?)`, cutoff, pruneBatch)
		if err != nil {
			return err
		}
		c, _ := res.RowsAffected()
		n = int(c)
		return nil
	})
	return n, err
}

// cancelRequested reads the cancel flag of a job a worker holds.
func (q *Queue) cancelRequested(ctx context.Context, l Leased) (bool, error) {
	row, err := store.QueryRowScoped(ctx, q.db, l.Scope, store.ScopedStmt{
		SQL:       "SELECT cancel_requested FROM jobs",
		Where:     "id = ? AND lease_token = ? AND state = 'running'",
		WhereArgs: []any{l.ID, l.Token},
	})
	if err != nil {
		return false, err
	}
	var c int
	if err := row.Scan(&c); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, leaseLost()
		}
		return false, err
	}
	return c != 0, nil
}
