package history

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

func TestPutAndTurnBlocks(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	aliceSc := scopeOf(testApp, tenA, ws1, alice, "ses_1")
	turnID := e.say(aliceSc, "hi", "here is a table")

	table := loadRich(t, "table.json")
	if err := e.svc.PutTurnBlocks(ctx, aliceSc, "ses_1", turnID, []conversation.RichBlock{table}); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.TurnBlocks(ctx, aliceSc, "ses_1", turnID)
	if err != nil || len(got) != 1 {
		t.Fatalf("got=%v err=%v", got, err)
	}
	if _, ok := got[0].Value.(conversation.TableBlock); !ok {
		t.Fatalf("want table, got %T", got[0].Value)
	}

	bobSc := scopeOf(testApp, tenA, ws1, bob, "ses_1")
	_, err = e.svc.TurnBlocks(ctx, bobSc, "ses_1", turnID)
	if contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("other principal: %v", err)
	}

	if _, err := e.svc.ClearSession(ctx, aliceSc, "ses_1"); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.TurnBlocks(ctx, aliceSc, "ses_1", turnID)
	if contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("after erase: %v", err)
	}
}

func loadRich(t *testing.T, name string) conversation.RichBlock {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", "conversation", "examples", "rich-block", "valid", name))
	if err != nil {
		t.Fatal(err)
	}
	var b conversation.RichBlock
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	return b
}
