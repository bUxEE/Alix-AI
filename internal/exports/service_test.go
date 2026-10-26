package exports

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/files"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

type memFiles struct {
	last files.GeneratedInput
	body []byte
}

func (m *memFiles) PutGenerated(_ context.Context, _ contracts.Scope, in files.GeneratedInput) (conversation.FileRef, error) {
	m.last = in
	b, err := io.ReadAll(in.Content)
	if err != nil {
		return conversation.FileRef{}, err
	}
	m.body = b
	return conversation.FileRef{
		FileID: "fil_01ARZ3NDEKTSV4RRFFQ69G5FAV", Name: in.Name, MimeType: in.DeclaredMIME,
		SizeBytes: int64(len(b)), Owner: "usr_1", Origin: conversation.FileRefOriginGenerated,
		Permissions: []conversation.FileRefPermissionsItem{conversation.FileRefPermissionsItemRead, conversation.FileRefPermissionsItemDownload},
		Previewable: true, ExpiresAt: "2026-10-26T19:00:00.000Z",
	}, nil
}

func testScope() contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep_1", ApplicationID: "app_1", TenantID: "ten_1", WorkspaceID: "ws_1",
		PrincipalID: "usr_1", SessionID: "ses_1", Roles: []string{"member"},
		CapabilityScopes: []string{CapCreate},
	}
}

func TestCreateCSVAndScope(t *testing.T) {
	src := NewMemorySource()
	scope := testScope()
	table := mustBlock(t, "table.json")
	src.Remember(scope, "ses_1", "trn_01ARZ3NDEKTSV4RRFFQ69G5FAV", []conversation.RichBlock{table})
	store := &memFiles{}
	svc, err := New(Options{Source: src, Files: store, Clock: clock.NewFake(time.Date(2026, 10, 26, 18, 0, 0, 0, time.UTC))})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := svc.Create(context.Background(), scope, "ses_1", Request{
		TurnID: "trn_01ARZ3NDEKTSV4RRFFQ69G5FAV", Format: FormatCSV,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ref.Origin != conversation.FileRefOriginGenerated || store.last.DeclaredMIME != "text/csv" {
		t.Fatalf("ref=%+v mime=%s", ref, store.last.DeclaredMIME)
	}
	if !bytes.Contains(store.body, []byte("Start")) {
		t.Fatalf("csv body=%s", store.body)
	}

	other := scope
	other.TenantID = "ten_other"
	other.CapabilityScopes = []string{CapCreate}
	_, err = svc.Create(context.Background(), other, "ses_1", Request{
		TurnID: "trn_01ARZ3NDEKTSV4RRFFQ69G5FAV", Format: FormatCSV,
	})
	if !IsNotFound(err) {
		t.Fatalf("other tenant: %v", err)
	}

	noCap := scope
	noCap.CapabilityScopes = []string{"history.read"}
	_, err = svc.Create(context.Background(), noCap, "ses_1", Request{
		TurnID: "trn_01ARZ3NDEKTSV4RRFFQ69G5FAV", Format: FormatCSV,
	})
	if contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Fatalf("no cap: %v", err)
	}

	_, err = svc.Create(context.Background(), scope, "ses_1", Request{
		TurnID: "trn_01ARZ3NDEKTSV4RRFFQ69G5FAV", Format: FormatCSV,
		BlockIDs: []string{"missing"},
	})
	if !IsNotFound(err) {
		t.Fatalf("missing block: %v", err)
	}

	mdOnly := NewMemorySource()
	mdOnly.Remember(scope, "ses_1", "trn_01ARZ3NDEKTSV4RRFFQ69G5FAV", []conversation.RichBlock{
		{Value: conversation.MarkdownBlock{Text: "only text"}},
	})
	svc2, _ := New(Options{Source: mdOnly, Files: store, Clock: clock.NewFake(time.Date(2026, 10, 26, 18, 0, 0, 0, time.UTC))})
	_, err = svc2.Create(context.Background(), scope, "ses_1", Request{
		TurnID: "trn_01ARZ3NDEKTSV4RRFFQ69G5FAV", Format: FormatCSV,
	})
	if contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("csv without table: %v", err)
	}
}

func TestLimits(t *testing.T) {
	src := NewMemorySource()
	scope := testScope()
	rows := make([][]any, 5)
	for i := range rows {
		rows[i] = []any{"a"}
	}
	src.Remember(scope, "ses_1", "trn_01ARZ3NDEKTSV4RRFFQ69G5FAV", []conversation.RichBlock{
		{Value: conversation.TableBlock{
			Columns: []conversation.TableColumn{{Key: "a", Label: "A"}},
			Rows:    rows,
		}},
	})
	cfg := DefaultConfig()
	cfg.MaxRows = 2
	store := &memFiles{}
	svc, err := New(Options{Source: src, Files: store, Config: &cfg, Clock: clock.NewFake(time.Date(2026, 10, 26, 18, 0, 0, 0, time.UTC))})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Create(context.Background(), scope, "ses_1", Request{
		TurnID: "trn_01ARZ3NDEKTSV4RRFFQ69G5FAV", Format: FormatCSV,
	})
	if contracts.CodeOf(err) != contracts.ErrResourceExhausted {
		t.Fatalf("want resource_exhausted, got %v", err)
	}
}
