package sdkgo_test

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/sdk-go/sdktest"
)

func TestExportsCreate(t *testing.T) {
	fake := sdktest.New()
	t.Cleanup(fake.Close)
	client, err := fake.Client(sdktest.Claims{SessionID: "ses-1", CapabilityScopes: []string{"exports.create"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	got, err := client.Exports().Create(ctx, "ses-1", conversation.ExportRequest{
		TurnID: "trn_01ARZ3NDEKTSV4RRFFQ69G5FAV", Format: conversation.ExportRequestFormatCsv,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != conversation.FileRefOriginGenerated {
		t.Fatalf("origin=%s", got.Origin)
	}
	_, err = client.Exports().Create(ctx, "ses-1", conversation.ExportRequest{
		TurnID: "trn_01ARZ3NDEKTSV4RRFFQ69G5FA0", Format: conversation.ExportRequestFormatHtml,
	})
	if contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("missing turn: %v", err)
	}
	denied, err := fake.Client(sdktest.Claims{SessionID: "ses-1", CapabilityScopes: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = denied.Exports().Create(ctx, "ses-1", conversation.ExportRequest{
		TurnID: "trn_01ARZ3NDEKTSV4RRFFQ69G5FAV", Format: conversation.ExportRequestFormatMd,
	})
	if contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Fatalf("no capability: %v", err)
	}
}
