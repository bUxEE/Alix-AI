package exports

import (
	"context"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/history"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/stream"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// TestDurableExportAfterHubEviction: HistorySource serves exports after the
// stream hub forgets the turn; another principal and a cleared session are
// not_found.
func TestDurableExportAfterHubEviction(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 26, 18, 0, 0, 0, time.UTC)
	clk := clock.NewFake(t0)
	db := storetest.Open(t, nil)
	alog, err := audit.New(db, audit.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sessions.New(db, clk, sessions.Config{})
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(history.Deps{DB: db, Transcripts: sess, Audit: alog, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}

	scope := contracts.Scope{
		DeploymentID: "dep_1", ApplicationID: "app_1", TenantID: "ten_1", WorkspaceID: "ws_1",
		PrincipalID: "usr_alice", SessionID: "ses_1", Roles: []string{"member"},
		CapabilityScopes: []string{CapCreate, history.CapClear},
	}
	s, err := sess.Open(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	turnID := "trn_01ARZ3NDEKTSV4RRFFQ69G5FAV"
	if _, err := sess.Save(ctx, s, &sessions.TurnRecord{TurnID: turnID, User: "report", Act: "answer", Assistant: "here"}); err != nil {
		t.Fatal(err)
	}

	blocks := []conversation.RichBlock{mustBlock(t, "table.json"), mustBlock(t, "chart-bar.json")}
	if err := hist.PutTurnBlocks(ctx, scope, scope.SessionID, turnID, blocks); err != nil {
		t.Fatal(err)
	}

	hub := stream.NewHub(stream.Config{Clock: clk, Retention: time.Minute})
	turn, err := hub.Begin(scope, turnID)
	if err != nil {
		t.Fatal(err)
	}
	if err := turn.PublishResponse("report ready"); err != nil {
		t.Fatal(err)
	}
	if err := turn.Publish(conversation.TurnEvent{Value: conversation.DoneEvent{
		TurnID: act.TurnID(turnID), Seq: conversation.Seq(turn.NextSeq()),
		FinishReason: conversation.DoneEventFinishReasonCompleted,
	}}); err != nil {
		t.Fatal(err)
	}

	clk.Advance(2 * time.Minute)
	hub.Sweep()
	if _, err := hub.Lookup(scope, turnID); err == nil {
		t.Fatal("hub still holds the turn after retention")
	}

	files := &memFiles{}
	svc, err := New(Options{
		Source: MultiSource{HistorySource{Store: hist}, HubSource{Hub: hub}},
		Files:  files,
		Clock:  clk,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, format := range []Format{FormatCSV, FormatXLSX, FormatHTML, FormatMD} {
		ref, err := svc.Create(ctx, scope, scope.SessionID, Request{TurnID: turnID, Format: format})
		if err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if ref.Origin != conversation.FileRefOriginGenerated {
			t.Fatalf("%s origin=%s", format, ref.Origin)
		}
	}

	other := scope
	other.PrincipalID = "usr_bob"
	other.CapabilityScopes = []string{CapCreate}
	_, err = svc.Create(ctx, other, other.SessionID, Request{TurnID: turnID, Format: FormatCSV})
	if !IsNotFound(err) {
		t.Fatalf("other principal: %v", err)
	}

	if _, err := hist.ClearSession(ctx, scope, scope.SessionID); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Create(ctx, scope, scope.SessionID, Request{TurnID: turnID, Format: FormatCSV})
	if !IsNotFound(err) {
		t.Fatalf("after erase: %v", err)
	}
}
