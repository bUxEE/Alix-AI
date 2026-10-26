package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	mem "github.com/buxee/alix-ai/pkg/contracts/gen/memory"
)

type memoryTestFake struct {
	mu    sync.Mutex
	calls []string
	items map[string]mem.MemoryItem
}

func (f *memoryTestFake) scopeKey(scope contracts.Scope) string {
	return scope.ApplicationID + "|" + scope.TenantID + "|" + scope.WorkspaceID + "|" + scope.PrincipalID
}

func (f *memoryTestFake) List(_ context.Context, scope contracts.Scope, q MemoryListQuery) (mem.MemoryList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "List:"+f.scopeKey(scope))
	out := mem.MemoryList{Items: []mem.MemoryItem{}}
	for _, it := range f.items {
		if string(it.ApplicationID) != scope.ApplicationID || string(it.TenantID) != scope.TenantID ||
			string(it.WorkspaceID) != scope.WorkspaceID || string(it.PrincipalID) != scope.PrincipalID {
			continue
		}
		if q.Status != "" && string(it.Status) != q.Status {
			continue
		}
		out.Items = append(out.Items, it)
	}
	return out, nil
}

func (f *memoryTestFake) Create(_ context.Context, scope contracts.Scope, in mem.MemoryCreate) (mem.MemoryItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Create:"+f.scopeKey(scope))
	now := common.Timestamp(time.Now().UTC().Format(time.RFC3339))
	id := mem.MemoryID("mem_01J0000000000000000000000A")
	it := mem.MemoryItem{
		ID:            id,
		ApplicationID: common.Identifier(scope.ApplicationID),
		TenantID:      common.Identifier(scope.TenantID),
		WorkspaceID:   common.Identifier(scope.WorkspaceID),
		PrincipalID:   common.Identifier(scope.PrincipalID),
		Text:          in.Text,
		Structured:    in.Structured,
		Origin:        mem.MemoryOriginUserExplicit,
		Status:        mem.MemoryStatusActive,
		DataClass:     mem.MemoryItemDataClassInternal,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if in.DataClass != nil {
		it.DataClass = mem.MemoryItemDataClass(string(*in.DataClass))
	}
	f.items[string(id)] = it
	return it, nil
}

func (f *memoryTestFake) Patch(_ context.Context, scope contracts.Scope, id mem.MemoryID, in mem.MemoryPatch) (mem.MemoryItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Patch")
	it, ok := f.items[string(id)]
	if !ok || string(it.PrincipalID) != scope.PrincipalID || string(it.TenantID) != scope.TenantID {
		return mem.MemoryItem{}, contracts.New(contracts.ErrValidationFailed, "memory not found")
	}
	if in.Text != nil {
		it.Text = *in.Text
	}
	it.UpdatedAt = common.Timestamp(time.Now().UTC().Format(time.RFC3339))
	f.items[string(id)] = it
	return it, nil
}

func (f *memoryTestFake) Delete(_ context.Context, scope contracts.Scope, id mem.MemoryID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Delete")
	it, ok := f.items[string(id)]
	if ok && (string(it.PrincipalID) != scope.PrincipalID || string(it.TenantID) != scope.TenantID) {
		return nil // foreign scope: idempotent no-op (not_found without probing)
	}
	delete(f.items, string(id))
	return nil
}

func (f *memoryTestFake) Confirm(_ context.Context, scope contracts.Scope, id mem.MemoryID) (mem.MemoryItem, error) {
	return f.setStatus(scope, id, mem.MemoryStatusActive, "Confirm")
}

func (f *memoryTestFake) Reject(_ context.Context, scope contracts.Scope, id mem.MemoryID) (mem.MemoryItem, error) {
	return f.setStatus(scope, id, mem.MemoryStatusRejected, "Reject")
}

func (f *memoryTestFake) setStatus(scope contracts.Scope, id mem.MemoryID, st mem.MemoryStatus, call string) (mem.MemoryItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	it, ok := f.items[string(id)]
	if !ok || string(it.PrincipalID) != scope.PrincipalID || string(it.TenantID) != scope.TenantID {
		return mem.MemoryItem{}, contracts.New(contracts.ErrValidationFailed, "memory not found")
	}
	if it.Status != mem.MemoryStatusProposed {
		return mem.MemoryItem{}, contracts.New(contracts.ErrValidationFailed, "proposal expired").WithDetails("reason", "memory_proposal_expired")
	}
	it.Status = st
	it.UpdatedAt = common.Timestamp(time.Now().UTC().Format(time.RFC3339))
	f.items[string(id)] = it
	return it, nil
}

var memoryFakeSets sync.Map

func init() {
	registerTestServices("v1_memory", func(t testing.TB, e *apiEnv, s *Services) {
		f := &memoryTestFake{items: map[string]mem.MemoryItem{}}
		memoryFakeSets.Store(e, f)
		if err := Provide[MemoryService](s, f); err != nil {
			t.Fatal(err)
		}
	})
}

func memoryFakeOf(e *apiEnv) *memoryTestFake {
	v, _ := memoryFakeSets.Load(e)
	return v.(*memoryTestFake)
}

func TestMemoryRoutesMatchInventory(t *testing.T) {
	e := newAPIEnv(t)
	have := map[string]bool{}
	for _, r := range e.srv.AreaRoutes("v1_memory") {
		have[r] = true
	}
	for _, want := range []string{
		"GET /v1/memory",
		"POST /v1/memory",
		"PATCH /v1/memory/{id}",
		"DELETE /v1/memory/{id}",
		"POST /v1/memory/{id}/confirm",
		"POST /v1/memory/{id}/reject",
	} {
		if !have[want] {
			t.Errorf("missing %s", want)
		}
	}
}

func TestMemoryCRUDAndScopeIsolation(t *testing.T) {
	e := newAPIEnv(t)
	f := memoryFakeOf(e)

	created := e.call(http.MethodPost, "/v1/memory", map[string]any{"text": "prefer dark mode"})
	wantStatus(t, created, http.StatusCreated)
	var item mem.MemoryItem
	if err := json.Unmarshal(created.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	if item.Origin != mem.MemoryOriginUserExplicit || item.Status != mem.MemoryStatusActive {
		t.Fatalf("item = %+v", item)
	}

	list := e.call(http.MethodGet, "/v1/memory", nil)
	wantStatus(t, list, http.StatusOK)

	wantError(t, e.call(http.MethodGet, "/v1/memory?bad=1", nil), http.StatusBadRequest, contracts.ErrValidationFailed)

	patch := e.call(http.MethodPatch, "/v1/memory/"+string(item.ID), map[string]any{"text": "prefer light mode"})
	wantStatus(t, patch, http.StatusOK)

	// Foreign principal cannot see or mutate.
	other := e.call(http.MethodGet, "/v1/memory", nil, withPrincipal("usr_other"))
	wantStatus(t, other, http.StatusOK)
	var otherList mem.MemoryList
	_ = json.Unmarshal(other.Body.Bytes(), &otherList)
	if len(otherList.Items) != 0 {
		t.Fatalf("other principal saw %d items", len(otherList.Items))
	}

	f.mu.Lock()
	prop := f.items[string(item.ID)]
	prop.Status = mem.MemoryStatusProposed
	prop.Origin = mem.MemoryOriginAssistantProposed
	f.items[string(item.ID)] = prop
	f.mu.Unlock()

	wantStatus(t, e.call(http.MethodPost, "/v1/memory/"+string(item.ID)+"/confirm", nil), http.StatusOK)
	wantStatus(t, e.call(http.MethodDelete, "/v1/memory/"+string(item.ID), nil), http.StatusNoContent)
}

func TestMemoryRoutesUnavailableWithoutService(t *testing.T) {
	e := newAPIEnv(t)
	h := &memoryHandler{apps: e.apps, svc: nil}
	scope := e.scopeOf()
	ctx, err := identity.WithScope(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.list(rec, httptest.NewRequest(http.MethodGet, "/v1/memory", nil).WithContext(ctx))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("list = %d, want 503", rec.Code)
	}
}
