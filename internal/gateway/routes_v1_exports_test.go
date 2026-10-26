package gateway

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/internal/exports"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

type exportsTestSvc struct {
	src  *exports.MemorySource
	last exports.Request
}

func (f *exportsTestSvc) Create(_ context.Context, scope contracts.Scope, sessionID string, req exports.Request) (conversation.FileRef, error) {
	f.last = req
	if !scope.Permits(exports.CapCreate) {
		return conversation.FileRef{}, contracts.Newf(contracts.ErrPermissionDenied, "exports: the context does not permit %s", exports.CapCreate)
	}
	blocks, err := f.src.Blocks(context.Background(), scope, sessionID, req.TurnID)
	if err != nil {
		return conversation.FileRef{}, err
	}
	if len(blocks) == 0 {
		return conversation.FileRef{}, contracts.New(contracts.ErrValidationFailed, "exports: the turn is not available for export").
			WithDetails("reason", "not_found")
	}
	return conversation.FileRef{
		FileID: "fil_01ARZ3NDEKTSV4RRFFQ69G5FAV", Name: "export.csv", MimeType: "text/csv",
		SizeBytes: 12, Owner: common.Identifier(scope.PrincipalID), Origin: conversation.FileRefOriginGenerated,
		Permissions: []conversation.FileRefPermissionsItem{conversation.FileRefPermissionsItemDownload},
		Previewable: true, ExpiresAt: "2026-10-26T19:00:00.000Z",
	}, nil
}

var exportsFakeSets sync.Map

func init() {
	registerTestServices("v1_exports", func(t testing.TB, e *apiEnv, s *Services) {
		f := &exportsTestSvc{src: exports.NewMemorySource()}
		exportsFakeSets.Store(e, f)
		if err := Provide[ExportsService](s, f); err != nil {
			t.Fatal(err)
		}
	})
}

func exportsFakeOf(e *apiEnv) *exportsTestSvc {
	v, _ := exportsFakeSets.Load(e)
	return v.(*exportsTestSvc)
}

func TestExportsCreate(t *testing.T) {
	e := newAPIEnv(t)
	fake := exportsFakeOf(e)
	scope := e.scopeOf()
	fake.src.Remember(scope, scope.SessionID, "trn_01ARZ3NDEKTSV4RRFFQ69G5FAV", []conversation.RichBlock{
		{Value: conversation.MarkdownBlock{Text: "hi"}},
	})

	rec := e.call(http.MethodPost, "/v1/sessions/"+scope.SessionID+"/exports", map[string]any{
		"turn_id": "trn_01ARZ3NDEKTSV4RRFFQ69G5FAV", "format": "html",
	}, withScopes(exports.CapCreate))
	wantStatus(t, rec, http.StatusCreated)
	if fake.last.Format != exports.FormatHTML {
		t.Fatalf("format=%s", fake.last.Format)
	}

	rec = e.call(http.MethodPost, "/v1/sessions/"+scope.SessionID+"/exports", map[string]any{
		"turn_id": "trn_01ARZ3NDEKTSV4RRFFQ69G5FAV", "format": "csv",
	})
	wantError(t, rec, http.StatusForbidden, contracts.ErrPermissionDenied)

	rec = e.call(http.MethodPost, "/v1/sessions/"+scope.SessionID+"/exports", map[string]any{
		"turn_id": "trn_01MISSINGTURN000000000000", "format": "md",
	}, withScopes(exports.CapCreate))
	wantError(t, rec, http.StatusNotFound, contracts.ErrValidationFailed)
}

func TestExportsRoutesMatchInventory(t *testing.T) {
	e := newAPIEnv(t)
	have := map[string]bool{}
	for _, r := range e.srv.AreaRoutes("v1_exports") {
		have[r] = true
	}
	if !have["POST /v1/sessions/{session_id}/exports"] {
		t.Errorf("missing POST /v1/sessions/{session_id}/exports; have %v", have)
	}
}
