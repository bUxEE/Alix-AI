package sessions

import (
	"context"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// scopeCaps is a valid scope with the given capability scopes.
func scopeCaps(tenant, principal, session string, caps ...string) contracts.Scope {
	sc := scopeOf(tenant, principal, session)
	sc.CapabilityScopes = caps
	return sc
}

// inWorkspace returns sc moved to another workspace of the same tenant.
func inWorkspace(sc contracts.Scope, ws string) contracts.Scope {
	sc = sc.Clone()
	sc.WorkspaceID = ws
	return sc
}

// seed opens the session of sc, lets mutate change its state and saves it.
func seed(t *testing.T, s *Store, sc contracts.Scope, mutate func(*State)) Session {
	t.Helper()
	ctx := context.Background()
	sess, err := s.Open(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&sess.State)
	saved, err := s.Save(ctx, sess, nil)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

// reopen reads the session of sc again.
func reopen(t *testing.T, s *Store, sc contracts.Scope) Session {
	t.Helper()
	sess, err := s.Open(context.Background(), sc)
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// invCall is one call to the coordinator's Invalidate.
type invCall struct {
	scope  contracts.Scope
	id     string
	reason act.PreparedActionInvalidationConditionsItem
}

// scopedInvalidator records every Invalidate with the scope it was called with,
// and can be told to fail.
type scopedInvalidator struct {
	mu    sync.Mutex
	calls []invCall
	err   error
}

func (r *scopedInvalidator) Invalidate(_ context.Context, sc contracts.Scope, id string, reason act.PreparedActionInvalidationConditionsItem) (contracts.PreparedAction, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, invCall{scope: sc, id: id, reason: reason})
	return contracts.PreparedAction{}, r.err
}

func (r *scopedInvalidator) ids() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.calls {
		out = append(out, c.id)
	}
	return out
}

func countRows(t *testing.T, db *store.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
