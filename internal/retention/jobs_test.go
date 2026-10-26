package retention

import (
	"context"
	"testing"
	"time"
)

func TestRequestValidate(t *testing.T) {
	base := Request{ApplicationID: "app", TenantID: "ten", Reason: "r", Operator: "ops", Level: LevelTenant}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := base
	bad.Level = LevelPrincipal
	if err := bad.Validate(); err == nil {
		t.Fatal("principal without principal_id accepted")
	}
	tenantPrincipal := base
	tenantPrincipal.Level = LevelPrincipal
	tenantPrincipal.PrincipalID = "usr_a"
	tenantPrincipal.Scope = PrincipalScopeTenant
	if err := tenantPrincipal.Validate(); err != nil {
		t.Fatal(err)
	}
	appWide := Request{ApplicationID: "app", PrincipalID: "usr_a", Level: LevelPrincipal,
		Scope: PrincipalScopeApplication, Reason: "r", Operator: "ops"}
	if err := appWide.Validate(); err != nil {
		t.Fatal(err)
	}
	if appWide.TargetKey() == tenantPrincipal.TargetKey() {
		t.Fatal("tenant and application principal targets must differ")
	}
	ws := base
	ws.Level = LevelWorkspace
	if err := ws.Validate(); err == nil {
		t.Fatal("workspace without workspace_id accepted")
	}
}

func TestJobsIdempotentSameTarget(t *testing.T) {
	eng := &Engine{Clock: fixedClock{t: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}}
	j := NewJobs(eng)
	req := Request{
		ApplicationID: "app", TenantID: "ten", PrincipalID: "usr_a",
		Level: LevelPrincipal, Reason: "gdpr", Operator: "ops",
	}
	a, created, err := j.Start(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !created || a.JobID == "" {
		t.Fatalf("first start = %+v created=%v", a, created)
	}
	b, created2, err := j.Start(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if created2 || b.JobID != a.JobID {
		t.Fatalf("second start = %+v created=%v, want same job", b, created2)
	}
	got, err := j.Get(context.Background(), a.JobID)
	if err != nil || got.JobID != a.JobID {
		t.Fatalf("get: %v %+v", err, got)
	}
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }
