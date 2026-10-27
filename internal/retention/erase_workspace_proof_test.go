package retention

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/memory"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	mem "github.com/buxee/alix-ai/pkg/contracts/gen/memory"
)

const (
	proofApp = "app_demo"
	proofDep = "dep_test"
	tenT     = "ten_t"
	tenU     = "ten_u"
	wsA      = "wsp_a"
	wsB      = "wsp_b"
	prinP    = "usr_p"
	prinQ    = "usr_q"
)

func TestPrincipalWorkspaceErasureProof(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC))
	db := storetest.Open(t, nil, func(so *store.Options) { so.Clock = clk })
	ts := trust.NewStore(db, clk)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const signer = "retention-proof-key"
	if _, err := ts.RegisterKey(ctx, trust.PlatformApplicationID, signer, pub, "proof", trust.WithPurpose(trust.PurposeAuditCheckpoint)); err != nil {
		t.Fatal(err)
	}
	alog, err := audit.New(db, audit.Options{
		Clock: clk, Registry: audit.DefaultRegistry,
		Signing: &trust.SigningKey{KeyID: signer, Key: priv}, Verifier: trust.NewVerifier(ts, clk),
	})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sessions.New(db, clk, sessions.Config{})
	if err != nil {
		t.Fatal(err)
	}
	memSvc, err := memory.New(memory.Deps{DB: db, Audit: alog, Clock: clk, Config: memory.Config{
		Enabled: true, MaxItemsPerScope: 50, MaxRetrieve: 8, MaxProposalsPerTurn: 1, MaxProposalsPerDay: 20, ProposalTTL: time.Hour,
	}})
	if err != nil {
		t.Fatal(err)
	}

	seedSession := func(ten, ws, prin, sid, text string) {
		t.Helper()
		sc := contracts.Scope{
			DeploymentID: proofDep, ApplicationID: proofApp, TenantID: ten, WorkspaceID: ws,
			PrincipalID: prin, SessionID: sid, Roles: []string{"member"},
			CapabilityScopes: []string{memory.CapRead, memory.CapWrite, memory.CapDelete},
		}
		s, err := sess.Open(ctx, sc)
		if err != nil {
			t.Fatal(err)
		}
		s.State.Goal = &sessions.Goal{CapabilityID: "items.create", Phase: "open", Slots: map[string]sessions.Slot{}, StartedAt: clk.Now(), UpdatedAt: clk.Now(), ExpiresAt: clk.Now().Add(time.Hour)}
		if _, err := sess.Save(ctx, s, &sessions.TurnRecord{TurnID: "trn_" + sid, User: text, Assistant: "ok"}); err != nil {
			t.Fatal(err)
		}
		if _, err := memSvc.Create(ctx, sc, mem.MemoryCreate{Text: mem.MemoryText("memory " + text)}); err != nil {
			t.Fatal(err)
		}
		if _, err := alog.Append(ctx, sc, audit.NewEvent{Type: audit.EventSettingsChanged, Data: map[string]any{"note": text}}); err != nil {
			t.Fatal(err)
		}
	}

	seedSession(tenT, wsA, prinP, "ses_pa", "p-in-a")
	seedSession(tenT, wsB, prinP, "ses_pb", "p-in-b")
	seedSession(tenT, wsA, prinQ, "ses_qa", "q-in-a")
	seedSession(tenU, wsA, prinP, "ses_pu", "p-in-u")

	credsCalls := 0
	eng := &Engine{
		DB: db, Log: alog, Clock: clk,
		PrincipalLLM: func(ctx context.Context, application, principal string) (int, int, error) {
			credsCalls++
			return 1, 1, nil
		},
	}
	jobs := NewJobs(eng)
	req := Request{
		ApplicationID: proofApp, TenantID: tenT, WorkspaceID: wsA, PrincipalID: prinP,
		Level: LevelPrincipal, Scope: PrincipalScopeWorkspace, Reason: "left site", Operator: "usr_admin",
	}
	accepted, created, err := jobs.Start(ctx, req)
	if err != nil || !created {
		t.Fatalf("start: %v created=%v", err, created)
	}
	// Wait for the in-process job (Jobs run synchronously when Engine has no queue).
	deadline := time.Now().Add(5 * time.Second)
	var st *Job
	for time.Now().Before(deadline) {
		st, err = jobs.Get(ctx, accepted.JobID)
		if err != nil {
			t.Fatal(err)
		}
		if st.Status == JobCompleted || st.Status == JobFailed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st == nil || st.Status != JobCompleted {
		t.Fatalf("job = %+v", st)
	}
	if st.Scope != PrincipalScopeWorkspace || st.WorkspaceID != wsA || st.PrincipalID != prinP {
		t.Fatalf("job status scope = %+v", st)
	}
	if credsCalls != 0 {
		t.Fatalf("workspace erase must not touch credentials/bindings (calls=%d)", credsCalls)
	}

	countHist := func(ten, ws, prin string) int {
		t.Helper()
		out, err := sessions.CountHistory(ctx, db, sessions.EraseSpec{
			ApplicationID: proofApp, TenantID: ten, WorkspaceID: ws, PrincipalID: prin, Now: clk.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return out.Sessions
	}
	countMem := func(ten, ws, prin string) int {
		t.Helper()
		out, err := memory.CountMemory(ctx, db, memory.EraseSpec{
			ApplicationID: proofApp, TenantID: ten, WorkspaceID: ws, PrincipalID: prin,
		})
		if err != nil {
			t.Fatal(err)
		}
		return out.Items
	}

	if n := countHist(tenT, wsA, prinP); n != 0 {
		t.Fatalf("P in A still has %d sessions", n)
	}
	if n := countMem(tenT, wsA, prinP); n != 0 {
		t.Fatalf("P in A still has %d memory", n)
	}
	if n := countHist(tenT, wsB, prinP); n == 0 {
		t.Fatal("P in B was erased")
	}
	if n := countMem(tenT, wsB, prinP); n == 0 {
		t.Fatal("P memory in B was erased")
	}
	if n := countHist(tenT, wsA, prinQ); n == 0 {
		t.Fatal("Q in A was erased")
	}
	if n := countHist(tenU, wsA, prinP); n == 0 {
		t.Fatal("P in tenant U was erased")
	}

	// Tombstone / erasure record on the tenant chain.
	var erasedN int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE application_id = ? AND tenant_id = ? AND data = ?`,
		proofApp, tenT, audit.ErasedMarker).Scan(&erasedN); err != nil {
		t.Fatal(err)
	}
	if erasedN == 0 {
		t.Fatal("expected erased audit payloads (tombstones) on tenant T")
	}
	var recN int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE application_id = ? AND tenant_id = ? AND event_type = ?`,
		proofApp, tenT, string(audit.EventAuditErased)).Scan(&recN); err != nil {
		t.Fatal(err)
	}
	if recN == 0 {
		t.Fatal("expected an audit.erased record of the erasure")
	}

	// Idempotent: same TargetKey returns the same job, no extra deletes.
	again, created2, err := jobs.Start(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if created2 && again.JobID != accepted.JobID {
		// After completion a new job may start; either same or a no-op second run.
		st2, err := jobs.Get(ctx, again.JobID)
		if err != nil {
			t.Fatal(err)
		}
		for time.Now().Before(deadline) && st2.Status != JobCompleted && st2.Status != JobFailed {
			time.Sleep(20 * time.Millisecond)
			st2, _ = jobs.Get(ctx, again.JobID)
		}
		if st2.Counts.SessionsRemoved != 0 || st2.Counts.MemoryItemsRemoved != 0 {
			t.Fatalf("second erase deleted more: %+v", st2.Counts)
		}
	}
	if n := countHist(tenT, wsB, prinP); n == 0 {
		t.Fatal("second run touched workspace B")
	}
}

func TestWorkspaceLevelLeavesOtherWorkspaces(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC))
	db := storetest.Open(t, nil, func(so *store.Options) { so.Clock = clk })
	ts := trust.NewStore(db, clk)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const signer = "ws-level-key"
	if _, err := ts.RegisterKey(ctx, trust.PlatformApplicationID, signer, pub, "proof", trust.WithPurpose(trust.PurposeAuditCheckpoint)); err != nil {
		t.Fatal(err)
	}
	alog, err := audit.New(db, audit.Options{
		Clock: clk, Registry: audit.DefaultRegistry,
		Signing: &trust.SigningKey{KeyID: signer, Key: priv}, Verifier: trust.NewVerifier(ts, clk),
	})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sessions.New(db, clk, sessions.Config{})
	if err != nil {
		t.Fatal(err)
	}
	memSvc, err := memory.New(memory.Deps{DB: db, Audit: alog, Clock: clk, Config: memory.Config{
		Enabled: true, MaxItemsPerScope: 50, MaxRetrieve: 8, MaxProposalsPerTurn: 1, MaxProposalsPerDay: 20, ProposalTTL: time.Hour,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, ws := range []string{wsA, wsB} {
		sc := contracts.Scope{
			DeploymentID: proofDep, ApplicationID: proofApp, TenantID: tenT, WorkspaceID: ws,
			PrincipalID: prinP, SessionID: "ses_" + ws, Roles: []string{"member"},
			CapabilityScopes: []string{memory.CapRead, memory.CapWrite},
		}
		s, err := sess.Open(ctx, sc)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sess.Save(ctx, s, &sessions.TurnRecord{TurnID: "trn_" + ws, User: "hi", Assistant: "ok"}); err != nil {
			t.Fatal(err)
		}
		if _, err := memSvc.Create(ctx, sc, mem.MemoryCreate{Text: mem.MemoryText("m-" + ws)}); err != nil {
			t.Fatal(err)
		}
	}
	eng := &Engine{DB: db, Log: alog, Clock: clk}
	counts, err := eng.Erase(ctx, Request{
		ApplicationID: proofApp, TenantID: tenT, WorkspaceID: wsA,
		Level: LevelWorkspace, Reason: "close site", Operator: "ops",
	})
	if err != nil {
		t.Fatal(err)
	}
	if counts.SessionsRemoved == 0 && counts.MemoryItemsRemoved == 0 {
		t.Fatalf("expected removals in A: %+v", counts)
	}
	leftSess, err := sessions.CountHistory(ctx, db, sessions.EraseSpec{
		ApplicationID: proofApp, TenantID: tenT, WorkspaceID: wsB, Now: clk.Now(),
	})
	if err != nil || leftSess.Sessions == 0 {
		t.Fatalf("workspace B history gone: %+v %v", leftSess, err)
	}
	leftMem, err := memory.CountMemory(ctx, db, memory.EraseSpec{
		ApplicationID: proofApp, TenantID: tenT, WorkspaceID: wsB,
	})
	if err != nil || leftMem.Items == 0 {
		t.Fatalf("workspace B memory gone: %+v %v", leftMem, err)
	}
}
