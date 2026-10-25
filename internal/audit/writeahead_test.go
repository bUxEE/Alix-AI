package audit

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// coordinator is a stand-in for internal/actions: it moves a preparation to
// "committing" and calls the adapter only if the audit event of that step was
// committed with the state change. Its shape is the one the action
// coordinator is expected to use (SEC-087, gap G10).
type coordinator struct {
	h       *harness
	adapter func() error
	calls   int
}

func (c *coordinator) commit(a Appender, ev NewEvent) error {
	loc, err := ForScope(s1)
	if err != nil {
		return err
	}
	err = c.h.db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO coord (id, state) VALUES ('prep_1', 'committing')`); err != nil {
			return err
		}
		_, err := a.AppendTx(ctx, tx, loc, ev)
		return err
	})
	if err != nil {
		return err // the state never became "committing": nothing to call
	}
	c.calls++
	return c.adapter()
}

// SEC-087: the "committing" event is written with the state change, before
// the adapter's Commit is called, and a failed append aborts the commit.
func TestSecWriteAheadAppendFailureAbortsCommit(t *testing.T) {
	setup := func(t *testing.T) (*harness, *coordinator) {
		h := newHarness(t)
		h.exec(`CREATE TABLE coord (id TEXT PRIMARY KEY, state TEXT NOT NULL) STRICT`)
		c := &coordinator{h: h}
		c.adapter = func() error { return nil }
		return h, c
	}
	committing := NewEvent{Type: EventActionCommitting, Data: map[string]any{"preparation_id": "prep_1"}}

	t.Run("the event is committed before the adapter is called", func(t *testing.T) {
		h, c := setup(t)
		var seenAtCall int
		c.adapter = func() error {
			seenAtCall = h.count(`SELECT COUNT(*) FROM audit_events WHERE event_type = 'action.committing'`)
			return nil
		}
		if err := c.commit(h.log, committing); err != nil {
			t.Fatal(err)
		}
		if seenAtCall != 1 {
			t.Errorf("the adapter ran with %d committing events recorded, want 1", seenAtCall)
		}
		if n := h.count(`SELECT COUNT(*) FROM coord WHERE state = 'committing'`); n != 1 {
			t.Errorf("state rows = %d", n)
		}
		h.mustVerify(key1)
	})

	t.Run("an event that is refused aborts the state change and the adapter", func(t *testing.T) {
		h, c := setup(t)
		bad := NewEvent{Type: EventActionCommitting, Data: map[string]any{"missing": "preparation_id"}}
		c.adapter = func() error { t.Fatal("the adapter was called without an audit record"); return nil }
		err := c.commit(h.log, bad)
		if contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Fatalf("err = %v", err)
		}
		if n := h.count(`SELECT COUNT(*) FROM coord`); n != 0 {
			t.Errorf("the state change survived the failed append (%d rows): the two are not atomic", n)
		}
		if n := h.count(`SELECT COUNT(*) FROM audit_events`); n != 0 {
			t.Errorf("%d events", n)
		}
	})

	t.Run("a store that fails the insert aborts the state change and the adapter", func(t *testing.T) {
		h, c := setup(t)
		// A trigger standing in for a full disk, an I/O error or a locked table.
		h.exec(`CREATE TRIGGER audit_insert_fails BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
		c.adapter = func() error { t.Fatal("the adapter was called without an audit record"); return nil }
		err := c.commit(h.log, committing)
		if err == nil {
			t.Fatal("the commit went ahead although the audit append failed")
		}
		if got := contracts.CodeOf(err); got != contracts.ErrUnavailable {
			t.Errorf("code = %s, want unavailable (%v)", got, err)
		}
		if n := h.count(`SELECT COUNT(*) FROM coord`); n != 0 {
			t.Errorf("state rows = %d, want 0", n)
		}
		if c.calls != 0 {
			t.Errorf("adapter called %d times", c.calls)
		}
	})

	t.Run("an Appender that is down aborts the commit", func(t *testing.T) {
		h, c := setup(t)
		c.adapter = func() error { t.Fatal("the adapter was called without an audit record"); return nil }
		if err := c.commit(failingAppender{}, committing); err == nil {
			t.Fatal("the commit went ahead")
		}
		if n := h.count(`SELECT COUNT(*) FROM coord`); n != 0 {
			t.Errorf("state rows = %d", n)
		}
	})

	t.Run("a failed append in its own transaction writes nothing and says so", func(t *testing.T) {
		h := newHarness(t)
		h.exec(`CREATE TRIGGER audit_insert_fails BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
		if _, err := h.log.Append(ctx, s1, settings(1)); contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Errorf("err = %v", err)
		}
		if _, err := h.log.Append(ctx, s1, settings(2)); err == nil {
			t.Error("a second append succeeded")
		}
	})

	t.Run("several events in one transaction chain in order, and roll back together", func(t *testing.T) {
		h, _ := setup(t)
		loc, _ := ForScope(s1)
		err := h.db.Tx(ctx, func(tx *store.Tx) error {
			for i := 1; i <= 3; i++ {
				if _, err := h.log.AppendTx(ctx, tx, loc, settings(i)); err != nil {
					return err
				}
			}
			return errRollback
		})
		if err != errRollback {
			t.Fatalf("err = %v", err)
		}
		if n := h.count(`SELECT COUNT(*) FROM audit_events`); n != 0 {
			t.Errorf("%d events survived a rolled-back transaction", n)
		}
		err = h.db.Tx(ctx, func(tx *store.Tx) error {
			for i := 1; i <= 3; i++ {
				e, err := h.log.AppendTx(ctx, tx, loc, settings(i))
				if err != nil {
					return err
				}
				if e.ChainSeq != int64(i) {
					t.Errorf("event %d at position %d: events of one transaction must chain", i, e.ChainSeq)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if rep := h.mustVerify(key1); rep.Events != 3 {
			t.Errorf("report = %+v", rep)
		}
	})

	t.Run("AppendTx needs a transaction", func(t *testing.T) {
		h, _ := setup(t)
		loc, _ := ForScope(s1)
		if _, err := h.log.AppendTx(context.Background(), nil, loc, settings(1)); err == nil {
			t.Error("AppendTx(nil tx) succeeded")
		}
	})
}

type rollbackError struct{}

func (rollbackError) Error() string { return "roll back" }

var errRollback error = rollbackError{}
