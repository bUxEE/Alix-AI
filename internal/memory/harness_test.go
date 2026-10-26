package memory

import (
	"context"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	mem "github.com/buxee/alix-ai/pkg/contracts/gen/memory"
)

const (
	testApp = "app_demo"
	tenA    = "ten_a"
	tenB    = "ten_b"
	ws1     = "wsp_1"
	ws2     = "wsp_2"
	alice   = "usr_alice"
	bob     = "usr_bob"
	testDep = "dep_test"
)

var t0 = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

var userCaps = []string{CapRead, CapWrite, CapDelete, CapConfirm, CapExport}

type env struct {
	t    *testing.T
	db   *store.DB
	clk  *clock.Fake
	svc  *Service
	alog *audit.Log
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	db := storetest.Open(t, nil, func(so *store.Options) { so.Clock = clk })
	alog, err := audit.New(db, audit.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(Deps{DB: db, Audit: alog, Clock: clk, Config: Config{
		Enabled: true, MaxItemsPerScope: 50, MaxRetrieve: 8,
		MaxProposalsPerTurn: 1, MaxProposalsPerDay: 20, ProposalTTL: time.Hour,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, db: db, clk: clk, svc: svc, alog: alog}
}

func scopeOf(app, ten, ws, prin string, caps ...string) contracts.Scope {
	if len(caps) == 0 {
		caps = userCaps
	}
	return contracts.Scope{
		DeploymentID: testDep, ApplicationID: app, TenantID: ten, WorkspaceID: ws, PrincipalID: prin,
		SessionID: "ses_01J00000000000000000000001", Roles: []string{"member"},
		CapabilityScopes: append([]string(nil), caps...),
	}
}

func (e *env) create(sc contracts.Scope, text string) mem.MemoryItem {
	e.t.Helper()
	it, err := e.svc.Create(context.Background(), sc, mem.MemoryCreate{Text: mem.MemoryText(text)})
	if err != nil {
		e.t.Fatalf("Create: %v", err)
	}
	return it
}
