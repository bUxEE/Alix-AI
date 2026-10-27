package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

type opsErasureFake struct {
	mu        sync.Mutex
	operators map[string]bool // deployment operators by principal
	tenants   map[string]bool // "tenant/principal" tenant admins
	jobs      map[string]ErasureJobStatus
	targets   map[string]string
	deny      bool
}

func (f *opsErasureFake) Authorize(_ context.Context, scope contracts.Scope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deny {
		return contracts.New(contracts.ErrPermissionDenied, "not an operator")
	}
	if f.operators[scope.PrincipalID] {
		return nil
	}
	if f.tenants[scope.TenantID+"/"+scope.PrincipalID] {
		return nil
	}
	return contracts.New(contracts.ErrPermissionDenied, "not an operator")
}

func (f *opsErasureFake) Start(_ context.Context, scope contracts.Scope, req ErasureRequest) (ErasureJobAccepted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tenant := req.TenantID
	if tenant == "" {
		tenant = scope.TenantID
	}
	scopeKind := req.Scope
	if req.Level == ErasureLevelPrincipal && scopeKind == "" {
		scopeKind = ErasureScopeTenant
	}
	key := string(req.Level) + "|" + string(scopeKind) + "|" + scope.ApplicationID + "|" + tenant + "|" + req.WorkspaceID + "|" + req.PrincipalID
	if id, ok := f.targets[key]; ok {
		j := f.jobs[id]
		return ErasureJobAccepted{JobID: j.JobID, Status: j.Status}, nil
	}
	id := "erj_01J0000000000000000000000A"
	if len(f.jobs) > 0 {
		id = "erj_01J0000000000000000000000B"
	}
	st := ErasureJobStatus{
		JobID: id, Status: "queued", Level: req.Level, Scope: scopeKind, TenantID: tenant,
		WorkspaceID: req.WorkspaceID, PrincipalID: req.PrincipalID,
	}
	f.jobs[id] = st
	f.targets[key] = id
	return ErasureJobAccepted{JobID: id, Status: "queued"}, nil
}

func (f *opsErasureFake) Status(_ context.Context, _ contracts.Scope, jobID string) (ErasureJobStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[jobID]
	if !ok {
		return ErasureJobStatus{}, contracts.New(contracts.ErrValidationFailed, "unknown").WithDetails("reason", "not_found")
	}
	j.Status = "completed"
	now := time.Now().UTC()
	j.FinishedAt = &now
	j.Counts = &ErasureCounts{AuditEventsErased: 1, SessionsRemoved: 1}
	f.jobs[jobID] = j
	return j, nil
}

func init() {
	registerTestServices("v1_ops_erasure", func(t testing.TB, e *apiEnv, s *Services) {
		f := &opsErasureFake{
			operators: map[string]bool{e.scopeOf().PrincipalID: true},
			jobs:      map[string]ErasureJobStatus{},
			targets:   map[string]string{},
		}
		opsErasureFakes.Store(e, f)
		if err := Provide[ErasureService](s, f); err != nil {
			t.Fatal(err)
		}
	})
}

var opsErasureFakes sync.Map

func opsErasureFakeOf(e *apiEnv) *opsErasureFake {
	v, _ := opsErasureFakes.Load(e)
	return v.(*opsErasureFake)
}

func TestOpsErasureRoutesMatchInventory(t *testing.T) {
	e := newAPIEnv(t)
	have := map[string]bool{}
	for _, r := range e.srv.AreaRoutes("v1_ops_erasure") {
		have[r] = true
	}
	for _, r := range []string{"POST /v1/ops/erasure", "GET /v1/ops/erasure/{job_id}"} {
		if !have[r] {
			t.Errorf("missing %s (have %v)", r, e.srv.AreaRoutes("v1_ops_erasure"))
		}
	}
}

func TestOpsErasureStartAndStatus(t *testing.T) {
	e := newAPIEnv(t)
	_ = opsErasureFakeOf(e)

	rec := e.call(http.MethodPost, "/v1/ops/erasure", map[string]any{
		"level": "principal", "principal_id": "usr_alice", "reason": "gdpr request",
	})
	wantStatus(t, rec, http.StatusAccepted)
	var accepted ErasureJobAccepted
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted.JobID == "" || accepted.Status == "" {
		t.Fatalf("accepted = %+v", accepted)
	}

	// Idempotent: same target returns the running/queued job.
	rec2 := e.call(http.MethodPost, "/v1/ops/erasure", map[string]any{
		"level": "principal", "principal_id": "usr_alice", "reason": "gdpr request again",
	})
	wantStatus(t, rec2, http.StatusAccepted)
	var again ErasureJobAccepted
	_ = json.Unmarshal(rec2.Body.Bytes(), &again)
	if again.JobID != accepted.JobID {
		t.Fatalf("idempotent job = %s, want %s", again.JobID, accepted.JobID)
	}

	st := e.call(http.MethodGet, "/v1/ops/erasure/"+accepted.JobID, nil)
	wantStatus(t, st, http.StatusOK)
	var body ErasureJobStatus
	if err := json.Unmarshal(st.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "completed" || body.Counts == nil || body.Counts.AuditEventsErased != 1 {
		t.Fatalf("status = %+v", body)
	}
	wantError(t, e.call(http.MethodGet, "/v1/ops/erasure/erj_01J0000000000000000000000Z", nil), http.StatusNotFound, contracts.ErrValidationFailed)
}

func TestOpsErasureRequiresOperator(t *testing.T) {
	e := newAPIEnv(t)
	f := opsErasureFakeOf(e)
	f.deny = true
	wantError(t, e.call(http.MethodPost, "/v1/ops/erasure", map[string]any{"level": "tenant"}), http.StatusForbidden, contracts.ErrPermissionDenied)
}

func TestOpsErasureWorkspaceAuthz(t *testing.T) {
	e := newAPIEnv(t)
	f := opsErasureFakeOf(e)
	f.mu.Lock()
	f.operators = map[string]bool{}
	f.tenants = map[string]bool{
		apiTenant + "/usr_admin_t": true,
		"ten_other/usr_admin_u":    true,
	}
	f.mu.Unlock()

	body := map[string]any{
		"level": "principal", "scope": "workspace", "principal_id": "usr_alice",
		"workspace_id": apiWsp, "reason": "left site",
	}
	wantStatus(t, e.call(http.MethodPost, "/v1/ops/erasure", body, withPrincipal("usr_admin_t")), http.StatusAccepted)
	// Admin of another tenant keeps the target tenant in scope — Authorize denies.
	wantError(t, e.call(http.MethodPost, "/v1/ops/erasure", body, withPrincipal("usr_admin_u")),
		http.StatusForbidden, contracts.ErrPermissionDenied)
	wantError(t, e.call(http.MethodPost, "/v1/ops/erasure", body, withPrincipal("usr_plain")),
		http.StatusForbidden, contracts.ErrPermissionDenied)
}
