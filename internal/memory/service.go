package memory

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	mem "github.com/buxee/alix-ai/pkg/contracts/gen/memory"
)

// ListQuery filters List.
type ListQuery struct {
	Q      string
	Status string
	Cursor string
	Limit  int
}

// List returns a page of items in the token scope.
func (s *Service) List(ctx context.Context, scope contracts.Scope, q ListQuery) (mem.MemoryList, error) {
	if err := s.authorize(ctx, scope, ActionRead, CapRead); err != nil {
		return mem.MemoryList{}, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	where := "principal_id = ?"
	args := []any{scope.PrincipalID}
	if st := strings.TrimSpace(q.Status); st != "" {
		if st != "proposed" && st != "active" && st != "rejected" {
			return mem.MemoryList{}, contracts.New(contracts.ErrValidationFailed, "memory: status must be proposed, active or rejected")
		}
		where += " AND status = ?"
		args = append(args, st)
	}
	if needle := clipQuery(q.Q); needle != "" {
		where += " AND lower(text) LIKE ?"
		args = append(args, "%"+strings.ToLower(needle)+"%")
	}
	afterID, afterUpdated, err := decodeCursor(q.Cursor)
	if err != nil {
		return mem.MemoryList{}, err
	}
	if afterID != "" {
		where += " AND (updated_at < ? OR (updated_at = ? AND id < ?))"
		args = append(args, afterUpdated, afterUpdated, afterID)
	}
	rows, err := store.QueryScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL:   "SELECT " + itemCols + " FROM memory_items",
		Where: where, WhereArgs: args,
		Tail: "ORDER BY updated_at DESC, id DESC LIMIT ?", TailArgs: []any{limit + 1},
	})
	if err != nil {
		return mem.MemoryList{}, wrapDB(err)
	}
	defer rows.Close()
	out := mem.MemoryList{Items: []mem.MemoryItem{}}
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return mem.MemoryList{}, wrapDB(err)
		}
		out.Items = append(out.Items, r.item())
	}
	if err := rows.Err(); err != nil {
		return mem.MemoryList{}, wrapDB(err)
	}
	if len(out.Items) > limit {
		last := out.Items[limit-1]
		out.Items = out.Items[:limit]
		cur := encodeCursor(string(last.ID), last.UpdatedAt)
		out.NextCursor = &cur
	}
	return out, nil
}

// Create stores an active item on the user's explicit host request.
func (s *Service) Create(ctx context.Context, scope contracts.Scope, in mem.MemoryCreate) (mem.MemoryItem, error) {
	if err := s.authorize(ctx, scope, ActionWrite, CapWrite); err != nil {
		return mem.MemoryItem{}, err
	}
	if err := validateText(string(in.Text)); err != nil {
		return mem.MemoryItem{}, err
	}
	structured, err := encodeStructured(in.Structured)
	if err != nil {
		return mem.MemoryItem{}, err
	}
	exp, err := parseTS(in.ExpiresAt)
	if err != nil {
		return mem.MemoryItem{}, err
	}
	n, err := s.countPrincipal(ctx, scope)
	if err != nil {
		return mem.MemoryItem{}, err
	}
	if n >= s.cfg.MaxItemsPerScope {
		return mem.MemoryItem{}, contracts.Newf(contracts.ErrValidationFailed, "memory: at most %d items per scope", s.cfg.MaxItemsPerScope)
	}
	now := s.clk.Now().UTC()
	id := s.ids.New(IDPrefix)
	class := dataClassOf(in.DataClass)
	var expMS any
	if !exp.IsZero() {
		expMS = toMS(exp)
	}
	if err := s.audit(ctx, scope, EventCreated, map[string]any{
		"id": id, "origin": string(mem.MemoryOriginUserExplicit), "status": string(mem.MemoryStatusActive), "data_class": string(class),
	}); err != nil {
		return mem.MemoryItem{}, err
	}
	_, err = store.InsertScoped(ctx, s.db, scope, "memory_items",
		[]string{"id", "principal_id", "text", "structured_json", "origin", "status", "data_class", "expires_at", "created_at", "updated_at"},
		id, scope.PrincipalID, string(in.Text), structured,
		string(mem.MemoryOriginUserExplicit), string(mem.MemoryStatusActive), string(class),
		expMS, toMS(now), toMS(now),
	)
	if err != nil {
		return mem.MemoryItem{}, wrapDB(err)
	}
	r, err := s.get(ctx, scope, id)
	if err != nil {
		return mem.MemoryItem{}, err
	}
	return r.item(), nil
}

// Patch edits an item in the token scope.
func (s *Service) Patch(ctx context.Context, scope contracts.Scope, id mem.MemoryID, in mem.MemoryPatch) (mem.MemoryItem, error) {
	if err := s.authorize(ctx, scope, ActionWrite, CapWrite); err != nil {
		return mem.MemoryItem{}, err
	}
	r, err := s.get(ctx, scope, string(id))
	if err != nil {
		return mem.MemoryItem{}, err
	}
	text, structured, class := r.text, r.structured, r.class
	exp := r.expires
	if in.Text != nil {
		if err := validateText(string(*in.Text)); err != nil {
			return mem.MemoryItem{}, err
		}
		text = string(*in.Text)
	}
	if in.Structured != nil {
		structured, err = encodeStructured(in.Structured)
		if err != nil {
			return mem.MemoryItem{}, err
		}
	}
	if in.DataClass != nil {
		c := mem.MemoryItemDataClass(string(*in.DataClass))
		if !c.Valid() {
			return mem.MemoryItem{}, contracts.New(contracts.ErrValidationFailed, "memory: data_class is not valid")
		}
		class = string(c)
	}
	if in.ExpiresAt != nil {
		t, err := parseTS(in.ExpiresAt)
		if err != nil {
			return mem.MemoryItem{}, err
		}
		if t.IsZero() {
			exp = sql.NullInt64{}
		} else {
			exp = sql.NullInt64{Int64: toMS(t), Valid: true}
		}
	}
	now := toMS(s.clk.Now().UTC())
	var expMS any
	if exp.Valid {
		expMS = exp.Int64
	}
	if err := s.audit(ctx, scope, EventUpdated, map[string]any{"id": string(id)}); err != nil {
		return mem.MemoryItem{}, err
	}
	_, err = store.ExecScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL:   "UPDATE memory_items SET text = ?, structured_json = ?, data_class = ?, expires_at = ?, updated_at = ?",
		Args:  []any{text, structured, class, expMS, now},
		Where: "id = ? AND principal_id = ?", WhereArgs: []any{string(id), scope.PrincipalID},
	})
	if err != nil {
		return mem.MemoryItem{}, wrapDB(err)
	}
	got, err := s.get(ctx, scope, string(id))
	if err != nil {
		return mem.MemoryItem{}, err
	}
	return got.item(), nil
}

// Delete removes an item. Foreign or absent ids are an idempotent success.
func (s *Service) Delete(ctx context.Context, scope contracts.Scope, id mem.MemoryID) error {
	if err := s.authorize(ctx, scope, ActionDelete, CapDelete); err != nil {
		return err
	}
	_, err := s.get(ctx, scope, string(id))
	if err != nil {
		if contracts.CodeOf(err) == contracts.ErrValidationFailed {
			return nil // not found / foreign: same as absent
		}
		return err
	}
	if err := s.audit(ctx, scope, EventDeleted, map[string]any{"id": string(id)}); err != nil {
		return err
	}
	_, err = store.ExecScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL:   "DELETE FROM memory_items",
		Where: "id = ? AND principal_id = ?", WhereArgs: []any{string(id), scope.PrincipalID},
	})
	return wrapDB(err)
}

// Confirm activates a proposed item. Expired or non-proposed → memory_proposal_expired.
func (s *Service) Confirm(ctx context.Context, scope contracts.Scope, id mem.MemoryID) (mem.MemoryItem, error) {
	return s.decide(ctx, scope, id, mem.MemoryStatusActive, EventConfirmed)
}

// Reject marks a proposed item rejected.
func (s *Service) Reject(ctx context.Context, scope contracts.Scope, id mem.MemoryID) (mem.MemoryItem, error) {
	return s.decide(ctx, scope, id, mem.MemoryStatusRejected, EventRejected)
}

func (s *Service) decide(ctx context.Context, scope contracts.Scope, id mem.MemoryID, st mem.MemoryStatus, ev audit.EventType) (mem.MemoryItem, error) {
	if err := s.authorize(ctx, scope, ActionConfirm, CapConfirm); err != nil {
		return mem.MemoryItem{}, err
	}
	r, err := s.get(ctx, scope, string(id))
	if err != nil {
		return mem.MemoryItem{}, err
	}
	if r.status != string(mem.MemoryStatusProposed) || r.origin != string(mem.MemoryOriginAssistantProposed) {
		return mem.MemoryItem{}, proposalExpired()
	}
	if r.expires.Valid && !fromMS(r.expires.Int64).After(s.clk.Now().UTC()) {
		return mem.MemoryItem{}, proposalExpired()
	}
	now := toMS(s.clk.Now().UTC())
	if err := s.audit(ctx, scope, ev, map[string]any{"id": string(id), "status": string(st)}); err != nil {
		return mem.MemoryItem{}, err
	}
	_, err = store.ExecScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL:   "UPDATE memory_items SET status = ?, updated_at = ?",
		Args:  []any{string(st), now},
		Where: "id = ? AND principal_id = ?", WhereArgs: []any{string(id), scope.PrincipalID},
	})
	if err != nil {
		return mem.MemoryItem{}, wrapDB(err)
	}
	got, err := s.get(ctx, scope, string(id))
	if err != nil {
		return mem.MemoryItem{}, err
	}
	return got.item(), nil
}

type cursorPayload struct {
	ID        string `json:"id"`
	UpdatedAt string `json:"u"`
}

func encodeCursor(id string, updated common.Timestamp) string {
	b, _ := json.Marshal(cursorPayload{ID: id, UpdatedAt: string(updated)})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(cur string) (id string, updatedMS int64, err error) {
	if strings.TrimSpace(cur) == "" {
		return "", 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cur)
	if err != nil {
		return "", 0, contracts.New(contracts.ErrValidationFailed, "memory: cursor is not valid")
	}
	var p cursorPayload
	if err := json.Unmarshal(raw, &p); err != nil || p.ID == "" || p.UpdatedAt == "" {
		return "", 0, contracts.New(contracts.ErrValidationFailed, "memory: cursor is not valid")
	}
	t, err := time.Parse(time.RFC3339Nano, p.UpdatedAt)
	if err != nil {
		t, err = time.Parse(time.RFC3339, p.UpdatedAt)
	}
	if err != nil {
		return "", 0, contracts.New(contracts.ErrValidationFailed, "memory: cursor is not valid")
	}
	return p.ID, toMS(t), nil
}
