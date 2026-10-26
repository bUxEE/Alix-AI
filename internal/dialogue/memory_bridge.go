package dialogue

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	mem "github.com/buxee/alix-ai/pkg/contracts/gen/memory"
)

// MemoryProposalDraft is a schema-validated assistant proposal (never free text).
type MemoryProposalDraft struct {
	Text       string
	Structured map[string]string
	DataClass  string
}

// MemoryRetrieved is one active item for context assembly.
type MemoryRetrieved struct {
	Item  mem.MemoryItem
	Score int
}

// MemoryStore is what the engine needs from long-term memory (ADR 0042).
// The composition root adapts internal/memory.Service to this seam.
type MemoryStore interface {
	Propose(ctx context.Context, scope contracts.Scope, draft MemoryProposalDraft) (mem.MemoryItem, error)
	RetrieveActive(ctx context.Context, scope contracts.Scope, query string) ([]MemoryRetrieved, error)
	MaxProposalsPerTurn() int
}

// SetMemory wires long-term memory into the engine. Nil clears it.
func (e *Engine) SetMemory(m MemoryStore) {
	if e == nil {
		return
	}
	if m == nil {
		e.memory.Store(nil)
		return
	}
	slot := m
	e.memory.Store(&slot)
}

func (e *Engine) memoryStore() MemoryStore {
	p := e.memory.Load()
	if p == nil {
		return nil
	}
	return *p
}

// MemoryProposalEvents are TurnEvent drafts for assistant-proposed memory items
// of this turn (status proposed). Same shape as KnowledgeCitationEvents.
func MemoryProposalEvents(res Result) []func(seq conversation.Seq) conversation.TurnEventValue {
	if len(res.MemoryProposals) == 0 {
		return nil
	}
	turnID := act.TurnID(res.TurnID)
	out := make([]func(seq conversation.Seq) conversation.TurnEventValue, 0, len(res.MemoryProposals))
	for _, it := range res.MemoryProposals {
		it := it
		out = append(out, func(seq conversation.Seq) conversation.TurnEventValue {
			return conversation.MemoryProposalEvent{TurnID: turnID, Seq: seq, Memory: it}
		})
	}
	return out
}

func draftFromProposal(mp *conversation.TaskProposalMemoryProposal) *MemoryProposalDraft {
	if mp == nil {
		return nil
	}
	text := strings.TrimSpace(string(mp.Text))
	dc := ""
	if mp.DataClass != nil {
		dc = string(*mp.DataClass)
	}
	if err := validateMemoryProposalDraft(text, mp.Structured, dc); err != nil {
		return nil
	}
	return &MemoryProposalDraft{Text: text, Structured: mp.Structured, DataClass: dc}
}

func validateMemoryProposalDraft(text string, structured map[string]string, dataClass string) error {
	n := utf8.RuneCountInString(text)
	if n < 1 || n > 4000 {
		return contracts.New(contracts.ErrValidationFailed, "dialogue: memory text length is out of bounds")
	}
	if len(structured) > 32 {
		return contracts.New(contracts.ErrValidationFailed, "dialogue: too many structured keys")
	}
	if dataClass != "" {
		switch mem.MemoryItemDataClass(dataClass) {
		case mem.MemoryItemDataClassPublic, mem.MemoryItemDataClassInternal, mem.MemoryItemDataClassConfidential, mem.MemoryItemDataClassRestricted:
		default:
			return contracts.New(contracts.ErrValidationFailed, "dialogue: memory data_class is not valid")
		}
	}
	return nil
}

func (t *turn) acceptMemoryProposal(ctx context.Context, draft *MemoryProposalDraft) {
	if draft == nil || t.e.memoryStore() == nil {
		return
	}
	limit := t.e.memoryStore().MaxProposalsPerTurn()
	if limit <= 0 {
		limit = 1
	}
	if len(t.memProps) >= limit {
		t.log("memory proposal dropped: per-turn limit", "limit", limit)
		return
	}
	it, err := t.e.memoryStore().Propose(ctx, t.scope, *draft)
	if err != nil {
		t.log("memory proposal refused", "code", string(contracts.CodeOf(err)))
		return
	}
	t.memProps = append(t.memProps, it)
}
