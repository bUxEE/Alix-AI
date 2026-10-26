package exports

import (
	"context"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// TurnBlocksStore reads durable verified rich blocks of a turn (history.Service).
type TurnBlocksStore interface {
	TurnBlocks(ctx context.Context, scope contracts.Scope, sessionID, turnID string) ([]conversation.RichBlock, error)
}

// HistorySource loads verified blocks from the durable history store.
type HistorySource struct {
	Store TurnBlocksStore
}

// Blocks implements BlockSource. Scope is the caller's; another principal,
// erased or expired turn is not_found.
func (h HistorySource) Blocks(ctx context.Context, scope contracts.Scope, sessionID, turnID string) ([]IdentifiedBlock, error) {
	if h.Store == nil {
		return nil, notFound("turn", "the turn is not available for export")
	}
	if sessionID != "" && scope.SessionID != "" && sessionID != scope.SessionID {
		return nil, notFound("turn", "the turn is not available for export")
	}
	blocks, err := h.Store.TurnBlocks(ctx, scope, sessionID, turnID)
	if err != nil {
		if IsNotFound(err) || contracts.CodeOf(err) == contracts.ErrScopeViolation {
			return nil, notFound("turn", "the turn is not available for export")
		}
		return nil, err
	}
	if len(blocks) == 0 {
		return nil, notFound("turn", "the turn has no verified blocks to export")
	}
	return identify(turnID, blocks), nil
}
