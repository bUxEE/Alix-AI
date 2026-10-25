package jobs

import (
	"context"
	"time"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// ClassStatus is the observable state of one priority class.
type ClassStatus struct {
	Class Class  `json:"class"`
	Name  string `json:"name"`
	// Workers and MaxDepth are the configured limits.
	Workers  int `json:"workers"`
	MaxDepth int `json:"max_depth"`

	// Pending counts every waiting job, including retries still in backoff.
	Pending   int `json:"pending"`
	Running   int `json:"running"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`

	// OldestReadyAgeMs is how long the longest-waiting ready job has been
	// eligible to run, in milliseconds; zero when nothing is ready. A large
	// value on a class with free workers means starvation. (Deployment-wide
	// Status only.)
	OldestReadyAgeMs int64 `json:"oldest_ready_age_ms"`
}

// Status is counts per class and state, for the operations API.
type Status struct {
	At time.Time `json:"at"`
	// Classes always lists all six classes, most urgent first.
	Classes []ClassStatus `json:"classes"`
}

func (q *Queue) newStatus() Status {
	st := Status{At: q.clk.Now(), Classes: make([]ClassStatus, 0, numClasses)}
	for _, c := range Classes() {
		cc := q.cfg.classConfig(c)
		st.Classes = append(st.Classes, ClassStatus{Class: c, Name: c.String(), Workers: cc.Workers, MaxDepth: cc.MaxDepth})
	}
	return st
}

func (st *Status) add(class Class, state State, n int) {
	if !class.Valid() {
		return
	}
	cs := &st.Classes[int(class)-1]
	switch state {
	case StatePending:
		cs.Pending += n
	case StateRunning:
		cs.Running += n
	case StateSucceeded:
		cs.Succeeded += n
	case StateFailed:
		cs.Failed += n
	case StateCancelled:
		cs.Cancelled += n
	}
}

// Status returns deployment-wide counts per class and state. It aggregates
// across every tenant and exposes no tenant data, but it is still an operator
// view: the route that serves it must be an operations route, not a tenant
// one (tenants use ScopedStatus). It reads through the covering partial
// indexes, so its cost follows the live and retained rows, which Prune bounds.
func (q *Queue) Status(ctx context.Context) (Status, error) {
	st := q.newStatus()
	rows, err := q.db.QueryContext(ctx,
		`SELECT class, 'pending', count(*) FROM jobs WHERE state = 'pending' GROUP BY class
		 UNION ALL
		 SELECT class, 'running', count(*) FROM jobs WHERE state = 'running' GROUP BY class
		 UNION ALL
		 SELECT class, state, count(*) FROM jobs WHERE state IN ('succeeded', 'failed', 'cancelled') GROUP BY class, state`)
	if err != nil {
		return Status{}, err
	}
	for rows.Next() {
		var class, n int
		var state string
		if err := rows.Scan(&class, &state, &n); err != nil {
			rows.Close()
			return Status{}, err
		}
		st.add(Class(class), State(state), n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Status{}, err
	}
	rows.Close()

	nowMs := toMs(st.At)
	oldest, err := q.db.QueryContext(ctx,
		`SELECT class, min(run_at) FROM jobs WHERE state = 'pending' AND run_at <= ? GROUP BY class`, nowMs)
	if err != nil {
		return Status{}, err
	}
	defer oldest.Close()
	for oldest.Next() {
		var class int
		var runAt int64
		if err := oldest.Scan(&class, &runAt); err != nil {
			return Status{}, err
		}
		if Class(class).Valid() {
			st.Classes[class-1].OldestReadyAgeMs = nowMs - runAt
		}
	}
	return st, oldest.Err()
}

// ScopedStatus is Status confined to one application, tenant and workspace.
// A tenant may read this for itself; it reveals nothing about anyone else.
// It reads every row of the scope (finished jobs included, until pruned), so
// call it on demand, not on a timer.
func (q *Queue) ScopedStatus(ctx context.Context, scope contracts.Scope) (Status, error) {
	st := q.newStatus()
	rows, err := store.QueryScoped(ctx, q.db, scope, store.ScopedStmt{
		SQL:  "SELECT class, state, count(*) FROM jobs",
		Tail: "GROUP BY class, state",
	})
	if err != nil {
		return Status{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var class, n int
		var state string
		if err := rows.Scan(&class, &state, &n); err != nil {
			return Status{}, err
		}
		st.add(Class(class), State(state), n)
	}
	return st, rows.Err()
}
