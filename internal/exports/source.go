package exports

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/buxee/alix-ai/internal/stream"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// contracts is used for Scope in BlockSource.

// BlockSource loads the verified rich blocks of a turn in the caller's scope.
type BlockSource interface {
	Blocks(ctx context.Context, scope contracts.Scope, sessionID, turnID string) ([]IdentifiedBlock, error)
}

// MemorySource is an in-process archive of verified blocks, filled by Remember
// (tests and composition roots that record as BlockEvents are published).
type MemorySource struct {
	mu    sync.RWMutex
	byKey map[string][]IdentifiedBlock
}

// NewMemorySource returns an empty archive.
func NewMemorySource() *MemorySource {
	return &MemorySource{byKey: map[string][]IdentifiedBlock{}}
}

func memKey(scope contracts.Scope, sessionID, turnID string) string {
	return scope.ApplicationID + "|" + scope.TenantID + "|" + scope.WorkspaceID + "|" + scope.PrincipalID + "|" + sessionID + "|" + turnID
}

// Remember stores the verified blocks of a turn for later export.
func (m *MemorySource) Remember(scope contracts.Scope, sessionID, turnID string, blocks []conversation.RichBlock) {
	if m == nil {
		return
	}
	key := memKey(scope, sessionID, turnID)
	m.mu.Lock()
	m.byKey[key] = identify(turnID, append([]conversation.RichBlock(nil), blocks...))
	m.mu.Unlock()
}

// Blocks implements BlockSource.
func (m *MemorySource) Blocks(_ context.Context, scope contracts.Scope, sessionID, turnID string) ([]IdentifiedBlock, error) {
	if m == nil {
		return nil, notFound("turn", "the turn is not available for export")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	blocks, ok := m.byKey[memKey(scope, sessionID, turnID)]
	if !ok {
		return nil, notFound("turn", "the turn is not available for export")
	}
	out := make([]IdentifiedBlock, len(blocks))
	copy(out, blocks)
	return out, nil
}

// HubSource reads verified BlockEvents from the stream hub while the turn is retained.
type HubSource struct {
	Hub *stream.Hub
}

// Blocks implements BlockSource.
func (h HubSource) Blocks(ctx context.Context, scope contracts.Scope, sessionID, turnID string) ([]IdentifiedBlock, error) {
	if h.Hub == nil {
		return nil, notFound("turn", "the turn is not available for export")
	}
	if sessionID != "" && scope.SessionID != "" && sessionID != scope.SessionID {
		return nil, notFound("turn", "the turn is not available for export")
	}
	t, err := h.Hub.Lookup(scope, turnID)
	if err != nil {
		return nil, notFound("turn", "the turn is not available for export")
	}
	sub, err := t.Subscribe(stream.FromStart)
	if err != nil {
		if errors.Is(err, stream.ErrCursorExpired) || errors.Is(err, stream.ErrNotVisible) {
			return nil, notFound("turn", "the turn is not available for export")
		}
		return nil, err
	}
	defer sub.Close()
	var blocks []conversation.RichBlock
	for {
		f, err := sub.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if f.Type != "block" {
			continue
		}
		var ev conversation.TurnEvent
		if err := json.Unmarshal(f.Data, &ev); err != nil {
			return nil, err
		}
		be, ok := ev.Value.(conversation.BlockEvent)
		if !ok {
			continue
		}
		blocks = append(blocks, be.Block)
	}
	if len(blocks) == 0 {
		return nil, notFound("turn", "the turn has no verified blocks to export")
	}
	return identify(turnID, blocks), nil
}

// MultiSource tries sources in order; the first that finds the turn wins.
// not_found from one source continues to the next; other errors stop.
type MultiSource []BlockSource

// Blocks implements BlockSource.
func (m MultiSource) Blocks(ctx context.Context, scope contracts.Scope, sessionID, turnID string) ([]IdentifiedBlock, error) {
	var last error
	for _, s := range m {
		if s == nil {
			continue
		}
		blocks, err := s.Blocks(ctx, scope, sessionID, turnID)
		if err == nil {
			return blocks, nil
		}
		if IsNotFound(err) {
			last = err
			continue
		}
		return nil, err
	}
	if last != nil {
		return nil, last
	}
	return nil, notFound("turn", "the turn is not available for export")
}
