package agui

import (
	"encoding/json"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	mem "github.com/buxee/alix-ai/pkg/contracts/gen/memory"
)

func TestMemoryProposalMapsToCustomEvent(t *testing.T) {
	it := mem.MemoryItem{
		ID: "mem_01J0000000000000000000000A", Text: "likes tea",
		ApplicationID: "app", TenantID: "ten", WorkspaceID: "ws", PrincipalID: "usr",
		Origin: mem.MemoryOriginAssistantProposed, Status: mem.MemoryStatusProposed,
		DataClass: mem.MemoryItemDataClassInternal,
		CreatedAt: common.Timestamp("2026-10-26T12:00:00Z"), UpdatedAt: common.Timestamp("2026-10-26T12:00:00Z"),
	}
	raw, err := json.Marshal(conversation.MemoryProposalEvent{
		TurnID: testTurn, Seq: 0, Memory: it,
	})
	if err != nil {
		t.Fatal(err)
	}
	var ev conversation.TurnEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	m, err := NewMapper(newRC())
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Map(ev)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range got {
		if c, ok := e.Event.(CustomEvent); ok && c.Name == MemoryProposalName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("mapped = %#v", got)
	}
}
