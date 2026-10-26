package history

import (
	"context"
	"encoding/json"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// PutTurnBlocks stores the verified rich blocks of a finished turn for later
// export. It is an internal write from the turn path (no history.* capability):
// the blocks are already sanitised by blocks.Validator. Empty blocks delete any
// prior row. Another principal's session is refused with scope_violation.
func (s *Service) PutTurnBlocks(ctx context.Context, scope contracts.Scope, sessionID, turnID string, blocks []conversation.RichBlock) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if !validID(sessionID) {
		return badSessionID()
	}
	if !validID(turnID) {
		return contracts.New(contracts.ErrValidationFailed, "history: the turn id is malformed")
	}
	if err := s.checkOwner(ctx, scope, sessionID); err != nil {
		return err
	}
	target := forSession(scope, sessionID)
	if len(blocks) == 0 {
		return s.deleteTurnBlocks(ctx, target, sessionID, turnID)
	}
	raw, err := json.Marshal(blocks)
	if err != nil {
		return contracts.Wrap(contracts.ErrInternal, err, "history: the turn blocks could not be encoded")
	}
	if err := s.deleteTurnBlocks(ctx, target, sessionID, turnID); err != nil {
		return err
	}
	_, err = store.InsertScoped(ctx, s.db, target, "history_turn_blocks",
		[]string{"principal_id", "session_id", "turn_id", "blocks_json", "created_at"},
		target.PrincipalID, sessionID, turnID, string(raw), toMS(s.now()))
	return wrapDB(err)
}

// TurnBlocks returns the verified rich blocks of a turn owned by the caller.
// Missing, erased or another principal's turn is validation_failed with reason
// not_found (exports maps it the same way). No history.read capability:
// exports.create is the gate.
func (s *Service) TurnBlocks(ctx context.Context, scope contracts.Scope, sessionID, turnID string) ([]conversation.RichBlock, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if !validID(sessionID) || !validID(turnID) {
		return nil, turnBlocksMissing()
	}
	if err := s.checkOwner(ctx, scope, sessionID); err != nil {
		if contracts.CodeOf(err) == contracts.ErrScopeViolation {
			return nil, turnBlocksMissing()
		}
		return nil, err
	}
	target := forSession(scope, sessionID)
	row, err := store.QueryRowScoped(ctx, s.db, target, store.ScopedStmt{
		SQL:       "SELECT blocks_json FROM history_turn_blocks",
		Where:     "session_id = ? AND turn_id = ? AND principal_id = ?",
		WhereArgs: []any{sessionID, turnID, scope.PrincipalID},
	})
	if err != nil {
		return nil, wrapDB(err)
	}
	var raw string
	switch err := row.Scan(&raw); {
	case err == nil:
	case isNoRows(err):
		return nil, turnBlocksMissing()
	default:
		return nil, wrapDB(err)
	}
	var blocks []conversation.RichBlock
	if err := json.Unmarshal([]byte(raw), &blocks); err != nil {
		return nil, contracts.Wrap(contracts.ErrInternal, err, "history: the turn blocks could not be decoded")
	}
	if len(blocks) == 0 {
		return nil, turnBlocksMissing()
	}
	return blocks, nil
}

func turnBlocksMissing() error {
	return contracts.New(contracts.ErrValidationFailed, "history: the turn blocks are not available").
		WithDetails("reason", "not_found")
}

func (s *Service) deleteTurnBlocks(ctx context.Context, scope contracts.Scope, sessionID, turnID string) error {
	_, err := store.ExecScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL:       "DELETE FROM history_turn_blocks",
		Where:     "session_id = ? AND turn_id = ? AND principal_id = ?",
		WhereArgs: []any{sessionID, turnID, scope.PrincipalID},
	})
	return wrapDB(err)
}

// clearSessionBlocks deletes every turn-block row of one session for the caller.
func (s *Service) clearSessionBlocks(ctx context.Context, scope contracts.Scope, sessionID string) error {
	_, err := store.ExecScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL:       "DELETE FROM history_turn_blocks",
		Where:     "session_id = ? AND principal_id = ?",
		WhereArgs: []any{sessionID, scope.PrincipalID},
	})
	return wrapDB(err)
}

// pruneTurnBlocks deletes turn-block rows of the workspace older than cutoff.
func (s *Service) pruneTurnBlocks(ctx context.Context, scope contracts.Scope, cutoff int64) error {
	_, err := store.ExecScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL: "DELETE FROM history_turn_blocks", Where: "created_at < ?", WhereArgs: []any{cutoff},
	})
	return wrapDB(err)
}
