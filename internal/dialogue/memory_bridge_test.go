package dialogue

import (
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	mem "github.com/buxee/alix-ai/pkg/contracts/gen/memory"
)

func TestParseProposalAcceptsMemoryProposal(t *testing.T) {
	raw := []byte(`{"capability_id":null,"fields":{},"entity_refs":[],"intent":"help","uncertainty":{"overall":1},"memory_proposal":{"text":"likes tea"}}`)
	tp, err := parseProposal(raw, "trc_01J0000000000000000000000A")
	if err != nil {
		t.Fatal(err)
	}
	draft := draftFromProposal(tp.MemoryProposal)
	if draft == nil || draft.Text != "likes tea" {
		t.Fatalf("draft = %+v", draft)
	}
}

func TestParseProposalRejectsUnknownMemoryMember(t *testing.T) {
	raw := []byte(`{"capability_id":null,"fields":{},"entity_refs":[],"intent":"help","uncertainty":{"overall":1},"memory_proposal":{"text":"likes tea","secret":"x"}}`)
	if _, err := parseProposal(raw, "trc_01J0000000000000000000000A"); err == nil {
		t.Fatal("expected schema_invalid for unknown memory_proposal member")
	}
}

func TestMemoryProposalEventsShape(t *testing.T) {
	it := mem.MemoryItem{
		ID: "mem_01J0000000000000000000000A", Text: "likes tea",
		ApplicationID: "app", TenantID: "ten", WorkspaceID: "ws", PrincipalID: "usr",
		Origin: mem.MemoryOriginAssistantProposed, Status: mem.MemoryStatusProposed,
		DataClass: mem.MemoryItemDataClassInternal,
		CreatedAt: common.Timestamp("2026-10-26T12:00:00Z"), UpdatedAt: common.Timestamp("2026-10-26T12:00:00Z"),
	}
	res := Result{TurnID: "trn_01J0000000000000000000000A", MemoryProposals: []mem.MemoryItem{it}}
	evs := MemoryProposalEvents(res)
	if len(evs) != 1 {
		t.Fatalf("events = %d", len(evs))
	}
	v := evs[0](1)
	mp, ok := v.(conversation.MemoryProposalEvent)
	if !ok || mp.Memory.ID != it.ID || mp.Memory.Status != mem.MemoryStatusProposed {
		t.Fatalf("event = %#v", v)
	}
}
