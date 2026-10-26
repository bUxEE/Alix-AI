package cli

import (
	"context"
	"io"

	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/history"
	"github.com/buxee/alix-ai/internal/memory"
	"github.com/buxee/alix-ai/pkg/contracts"
	mem "github.com/buxee/alix-ai/pkg/contracts/gen/memory"
)

// memoryProvideName sorts after rag_history so SetMemoryExport finds the history service.
const memoryProvideName = "rag_memory"

func init() { registerServeProvider(memoryProvideName, memoryProvide) }

func memoryProvide(_ context.Context, d serveDeps) error {
	cfg := memory.Live().WithDefaults()
	if !cfg.Enabled {
		if d.Log != nil {
			d.Log.Info("memory: long-term memory is disabled (memory.enabled=false); routes answer unavailable")
		}
		return nil
	}
	svc, err := memory.New(memory.Deps{DB: d.DB, Audit: d.Audit, Clock: d.Clock, Config: cfg, Logger: d.Log})
	if err != nil {
		return err
	}
	if err := gateway.Provide[gateway.MemoryService](d.Services, memoryGateway{svc}); err != nil {
		return err
	}
	if eng, ok := gateway.Lookup[*dialogue.Engine](d.Services); ok {
		eng.SetMemory(dialogueMemoryAdapt{svc})
	}
	if hist, ok := gateway.Lookup[*history.Service](d.Services); ok {
		hist.SetMemoryExport(memoryExportAdapt{svc})
	}
	return nil
}

// dialogueMemoryAdapt implements dialogue.MemoryStore over *memory.Service.
type dialogueMemoryAdapt struct{ s *memory.Service }

func (a dialogueMemoryAdapt) Propose(ctx context.Context, scope contracts.Scope, draft dialogue.MemoryProposalDraft) (mem.MemoryItem, error) {
	return a.s.Propose(ctx, scope, memory.ProposalDraft{Text: draft.Text, Structured: draft.Structured, DataClass: draft.DataClass})
}
func (a dialogueMemoryAdapt) RetrieveActive(ctx context.Context, scope contracts.Scope, query string) ([]dialogue.MemoryRetrieved, error) {
	got, err := a.s.RetrieveActive(ctx, scope, query)
	if err != nil {
		return nil, err
	}
	out := make([]dialogue.MemoryRetrieved, len(got))
	for i, r := range got {
		out[i] = dialogue.MemoryRetrieved{Item: r.Item, Score: r.Score}
	}
	return out, nil
}
func (a dialogueMemoryAdapt) MaxProposalsPerTurn() int { return a.s.MaxProposalsPerTurn() }

type memoryGateway struct{ s *memory.Service }

func (g memoryGateway) List(ctx context.Context, scope contracts.Scope, q gateway.MemoryListQuery) (mem.MemoryList, error) {
	return g.s.List(ctx, scope, memory.ListQuery{Q: q.Q, Status: q.Status, Cursor: q.Cursor, Limit: q.Limit})
}
func (g memoryGateway) Create(ctx context.Context, scope contracts.Scope, in mem.MemoryCreate) (mem.MemoryItem, error) {
	return g.s.Create(ctx, scope, in)
}
func (g memoryGateway) Patch(ctx context.Context, scope contracts.Scope, id mem.MemoryID, in mem.MemoryPatch) (mem.MemoryItem, error) {
	return g.s.Patch(ctx, scope, id, in)
}
func (g memoryGateway) Delete(ctx context.Context, scope contracts.Scope, id mem.MemoryID) error {
	return g.s.Delete(ctx, scope, id)
}
func (g memoryGateway) Confirm(ctx context.Context, scope contracts.Scope, id mem.MemoryID) (mem.MemoryItem, error) {
	return g.s.Confirm(ctx, scope, id)
}
func (g memoryGateway) Reject(ctx context.Context, scope contracts.Scope, id mem.MemoryID) (mem.MemoryItem, error) {
	return g.s.Reject(ctx, scope, id)
}

type memoryExportAdapt struct{ s *memory.Service }

func (a memoryExportAdapt) AppendExport(ctx context.Context, scope contracts.Scope, w io.Writer) error {
	_, err := a.s.AppendExport(ctx, scope, w)
	return err
}
