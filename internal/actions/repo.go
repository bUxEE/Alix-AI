package actions

import (
	"context"
	"database/sql"
	"errors"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// event builds an audit event of the action lifecycle. The data names the
// preparation and what happened to it. It never holds a field value, an effect,
// a record ID from the application or a confirmation secret: an audit event is
// a reference to what happened, not a copy of it.
func (c *Coordinator) event(t audit.EventType, r row, extra map[string]any) audit.NewEvent {
	data := map[string]any{
		"preparation_id":    r.id,
		"capability_id":     r.capabilityID,
		"risk_level":        int(r.risk),
		"confirmation_mode": string(r.mode),
		"idempotency_key":   r.key,
	}
	for k, v := range extra {
		data[k] = v
	}
	return audit.NewEvent{Type: t, Data: data}
}

// load reads one preparation of the caller's scope. A preparation that does not
// exist and one in another scope are indistinguishable (scope_violation).
func (c *Coordinator) load(ctx context.Context, q store.Reader, scope contracts.Scope, id string) (row, error) {
	srow, err := store.QueryRowScoped(ctx, q, scope, store.ScopedStmt{
		SQL:       "SELECT " + prepCols + " FROM action_preparations",
		Where:     "id = ?",
		WhereArgs: []any{id},
	})
	if err != nil {
		return row{}, err
	}
	r, err := scanRow(srow)
	if errors.Is(err, sql.ErrNoRows) {
		return row{}, errNoPreparation()
	}
	return r, err
}

// loadAny reads one preparation without a scope. It is for the service paths
// (Recover, ExpireDue), which work across tenants by nature and are never
// reachable from a tenant request.
func (c *Coordinator) loadAny(ctx context.Context, q store.Reader, id string) (row, error) {
	r, err := scanRow(q.QueryRowContext(ctx, "SELECT "+prepCols+" FROM action_preparations WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return row{}, errNoPreparation()
	}
	return r, err
}

// insert stores a new preparation. The scope columns come from the scope.
func (c *Coordinator) insert(ctx context.Context, tx *store.Tx, scope contracts.Scope, r row, fieldsJSON, preparedJSON, scopeJSON string) error {
	_, err := store.InsertScoped(ctx, tx, scope, "action_preparations",
		[]string{
			"id", "principal_id", "session_id", "scope_json", "capability_id", "state", "risk_level",
			"confirmation_mode", "confirmation_mac", "idempotency_key", "fields_json", "prepared_json",
			"snapshot_version", "data_digest", "prepared_at", "expires_at", "commit_attempts", "updated_at",
		},
		r.id, scope.PrincipalID, scope.SessionID, scopeJSON, r.capabilityID, string(r.state), int(r.risk),
		string(r.mode), r.mac, r.key, fieldsJSON, preparedJSON,
		r.snapshot, r.digest, toMS(r.preparedAt), toMS(r.expiresAt), 0, toMS(r.preparedAt))
	return err
}
