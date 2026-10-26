package memory

import (
	"context"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// EraseSpec says whose memory items are erased (ADR 0036).
type EraseSpec struct {
	ApplicationID string
	TenantID      string
	PrincipalID   string // empty → whole tenant
}

// EraseOutcome is what Erase / Count found.
type EraseOutcome struct {
	Items int
	More  bool
}

var eraseBatch = 1000

func (s EraseSpec) validate() error {
	if s.ApplicationID == "" || s.TenantID == "" {
		return contracts.New(contracts.ErrScopeMissing, "memory: an erasure names an application and a tenant")
	}
	return nil
}

func (s EraseSpec) where() (string, []any) {
	w, args := "application_id = ? AND tenant_id = ?", []any{s.ApplicationID, s.TenantID}
	if s.PrincipalID != "" {
		w += " AND principal_id = ?"
		args = append(args, s.PrincipalID)
	}
	return w, args
}

// CountMemory counts items matching spec.
func CountMemory(ctx context.Context, q store.Reader, spec EraseSpec) (EraseOutcome, error) {
	var out EraseOutcome
	if err := spec.validate(); err != nil {
		return out, err
	}
	w, args := spec.where()
	if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM memory_items WHERE "+w, args...).Scan(&out.Items); err != nil {
		return EraseOutcome{}, wrapDB(err)
	}
	return out, nil
}

// EraseMemory deletes one batch of matching items inside tx.
func EraseMemory(ctx context.Context, tx *store.Tx, spec EraseSpec) (EraseOutcome, error) {
	var out EraseOutcome
	if err := spec.validate(); err != nil {
		return out, err
	}
	w, args := spec.where()
	args = append(args, eraseBatch)
	res, err := tx.ExecContext(ctx, "DELETE FROM memory_items WHERE id IN (SELECT id FROM memory_items WHERE "+w+" LIMIT ?)", args...)
	if err != nil {
		return out, wrapDB(err)
	}
	n, _ := res.RowsAffected()
	out.Items = int(n)
	out.More = n >= int64(eraseBatch)
	return out, nil
}
