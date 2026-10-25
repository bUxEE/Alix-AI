package fake_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

func scope(tenant string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep_1", ApplicationID: testApp, TenantID: tenant, WorkspaceID: "wks_1", PrincipalID: "usr_1",
		Roles: []string{"member"}, CapabilityScopes: []string{fake.ReadCapability, fake.ActionCapability}, SessionID: "ses_1",
	}
}

// prep prepares an item and returns the PreparedAction with a fresh key.
func prep(t *testing.T, a adapters.AppAdapter, sc contracts.Scope, name string) actions.PreparedAction {
	t.Helper()
	res, err := a.Prepare(context.Background(), sc, adapters.PrepareRequest{CapabilityID: fake.ActionCapability, Fields: map[string]any{"name": name}})
	if err != nil || res.Preparation == nil {
		t.Fatalf("prepare %q: %+v, %v", name, res, err)
	}
	return actions.PreparedAction{
		PreparationID: actions.PreparationID(ids.New("prep")), CapabilityID: fake.ActionCapability, State: actions.PreparedActionStateCommitting,
		Summary: res.Preparation.Summary, Effects: res.Preparation.Effects, RiskLevel: res.Preparation.DeclaredRiskLevel,
		DataVersions: res.Preparation.DataVersions, PreparedAt: "2026-10-09T08:00:00Z", ExpiresAt: "2026-10-09T08:05:00Z",
		InvalidationConditions: []actions.PreparedActionInvalidationConditionsItem{actions.PreparedActionInvalidationConditionsItemDataVersionChanged},
		Confirmation:           actions.ConfirmationRequirement{Mode: actions.ConfirmationRequirementModeNone, Bindings: []actions.ConfirmationRequirementBindingsItem{}},
		IdempotencyKey:         actions.IdempotencyKey(ids.New("idem")),
	}
}

func doCommit(a adapters.AppAdapter, sc contracts.Scope, p actions.PreparedAction) (actions.DeterministicResult, error) {
	return a.Commit(context.Background(), sc, adapters.CommitRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: p.IdempotencyKey, Prepared: p})
}

func wrapped(t *testing.T, app *fake.App) adapters.AppAdapter {
	t.Helper()
	a, err := adapters.Wrap(fake.NewAdapter(app, fake.WithMaxResponseBytes(limit)), adapters.Options{ApplicationID: testApp})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestAppHostRules pins the behaviours of the fake host that other tasks'
// tests rely on: staleness, key reuse, the non-idempotent mode.
func TestAppHostRules(t *testing.T) {
	t.Run("a preparation goes stale when the data changes", func(t *testing.T) {
		app := fake.NewApp()
		a := wrapped(t, app)
		sc := scope("ten_a")
		p1 := prep(t, a, sc, "First")
		p2 := prep(t, a, sc, "Second") // prepared against the same version
		if res, err := doCommit(a, sc, p1); err != nil || res.Status != actions.DeterministicResultStatusOk {
			t.Fatalf("first commit: %+v, %v", res, err)
		}
		res, err := doCommit(a, sc, p2)
		if err != nil || res.Status != actions.DeterministicResultStatusRejected || len(res.Errors) != 1 || res.Errors[0].Code != "stale" {
			t.Fatalf("a stale preparation: %+v, %v; want rejected with stale", res, err)
		}
		if app.Effects() != 1 {
			t.Fatalf("%d effects", app.Effects())
		}
	})

	t.Run("a key reused for a different action is a conflict, not a replay", func(t *testing.T) {
		app := fake.NewApp()
		a := wrapped(t, app)
		sc := scope("ten_a")
		p1 := prep(t, a, sc, "First")
		if _, err := doCommit(a, sc, p1); err != nil {
			t.Fatal(err)
		}
		other := prep(t, a, sc, "Other")
		other.IdempotencyKey = p1.IdempotencyKey
		res, err := doCommit(a, sc, other)
		if err != nil || res.Status != actions.DeterministicResultStatusRejected || res.Errors[0].Code != "conflict" {
			t.Fatalf("got %+v, %v; want rejected with conflict", res, err)
		}
		if app.Effects() != 1 {
			t.Fatalf("%d effects", app.Effects())
		}
	})

	t.Run("the same key in another tenant is another action", func(t *testing.T) {
		app := fake.NewApp()
		a := wrapped(t, app)
		pA := prep(t, a, scope("ten_a"), "Same")
		if _, err := doCommit(a, scope("ten_a"), pA); err != nil {
			t.Fatal(err)
		}
		pB := prep(t, a, scope("ten_b"), "Same")
		pB.IdempotencyKey = pA.IdempotencyKey
		if res, err := doCommit(a, scope("ten_b"), pB); err != nil || res.Status != actions.DeterministicResultStatusOk || app.Effects() != 2 {
			t.Fatalf("got %+v, %v, %d effects", res, err, app.Effects())
		}
	})

	t.Run("a non-idempotent host applies every commit it receives", func(t *testing.T) {
		app := fake.NewApp(fake.WithNonIdempotentCommit())
		a := wrapped(t, app)
		sc := scope("ten_a")
		p := prep(t, a, sc, "Twice")
		for i := 0; i < 3; i++ {
			if res, err := doCommit(a, sc, p); err != nil || res.Status != actions.DeterministicResultStatusOk {
				t.Fatalf("commit %d: %+v, %v", i, res, err)
			}
		}
		if app.Effects() != 3 {
			t.Fatalf("%d effects, want 3", app.Effects())
		}
	})

	t.Run("an incomplete prepared action is refused", func(t *testing.T) {
		app := fake.NewApp()
		a := wrapped(t, app)
		sc := scope("ten_a")
		p := prep(t, a, sc, "x")
		p.Effects = nil
		res, err := doCommit(a, sc, p)
		if err != nil || res.Status != actions.DeterministicResultStatusRejected || res.Errors[0].Code != "validation_failed" || app.Effects() != 0 {
			t.Fatalf("got %+v, %v, %d effects", res, err, app.Effects())
		}
	})

	t.Run("unknown capability and operation", func(t *testing.T) {
		app := fake.NewApp()
		a, _ := adapters.Wrap(fake.NewAdapter(app), adapters.Options{ApplicationID: testApp})
		sc := scope("ten_a")
		sc.CapabilityScopes = append(sc.CapabilityScopes, "other.capability")
		res, err := a.Read(context.Background(), sc, adapters.ReadRequest{CapabilityID: "other.capability"})
		if err != nil || res.Status != actions.DeterministicResultStatusRejected || res.Errors[0].Code != "unknown_capability" {
			t.Fatalf("got %+v, %v", res, err)
		}
		// The read capability has no commit.
		res, err = a.Status(context.Background(), sc, adapters.StatusRequest{CapabilityID: fake.ReadCapability, IdempotencyKey: actions.IdempotencyKey(ids.New("idem"))})
		if err != nil || res.Status != actions.DeterministicResultStatusRejected || res.Errors[0].Code != "unknown_capability" {
			t.Fatalf("got %+v, %v", res, err)
		}
	})

	t.Run("seeded items are readable only in their own scope", func(t *testing.T) {
		app := fake.NewApp()
		app.Seed("ten_a", "wks_1", "item_seed", "Seed")
		a := wrapped(t, app)
		read := func(tenant string) actions.DeterministicResult {
			res, err := a.Read(context.Background(), scope(tenant), adapters.ReadRequest{CapabilityID: fake.ReadCapability, Fields: map[string]any{"item_id": "item_seed"}})
			if err != nil {
				t.Fatal(err)
			}
			return res
		}
		if read("ten_a").Status != actions.DeterministicResultStatusOk || read("ten_b").Status != actions.DeterministicResultStatusRejected {
			t.Fatal("tenant isolation of seeded items")
		}
		if diff := cmp.Diff(map[string]string{"item_seed": "Seed"}, app.Items("ten_a", "wks_1")); diff != "" {
			t.Fatal(diff)
		}
		if len(app.Items("ten_b", "wks_1")) != 0 {
			t.Fatal("items leaked into another tenant")
		}
	})
}

// TestHostRepliesCarryOnlyHostMembers: the host never names the operation or
// capability; those are the core's, and a fixture wire must not need them.
func TestHostRepliesCarryOnlyHostMembers(t *testing.T) {
	app := fake.NewApp()
	id := adapters.IdentityFrom(scope("ten_a"))
	for _, req := range []fake.Request{
		{Operation: adapters.OpRead, Identity: id, CapabilityID: fake.ReadCapability, Fields: map[string]any{"item_id": "nope"}},
		{Operation: adapters.OpValidate, Identity: id, CapabilityID: fake.ActionCapability, Fields: map[string]any{"name": "x"}},
		{Operation: adapters.OpPrepare, Identity: id, CapabilityID: fake.ActionCapability, Fields: map[string]any{"name": "x"}},
	} {
		reply := app.Handle(context.Background(), req)
		if reply.Action != fake.Deliver {
			t.Fatalf("%s: action %d", req.Operation, reply.Action)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(reply.Body, &m); err != nil {
			t.Fatalf("%s: %v", req.Operation, err)
		}
		if _, ok := m["operation"]; ok {
			t.Errorf("%s: the host named the operation", req.Operation)
		}
		if _, ok := m["capability_id"]; ok {
			t.Errorf("%s: the host named the capability", req.Operation)
		}
	}
}

// TestFaults pins what each injected fault does on the wire and to the host
// state, because other tasks' tests build scenarios from them.
func TestFaults(t *testing.T) {
	sc := scope("ten_a")
	run := func(t *testing.T, f fake.Fault, op adapters.Operation) (res actions.DeterministicResult, err error, app *fake.App, p actions.PreparedAction) {
		t.Helper()
		app = fake.NewApp(fake.WithOversizeBytes(4 * limit))
		a := wrapped(t, app)
		p = prep(t, a, sc, "Widget")
		app.Inject(op, f)
		switch op {
		case adapters.OpCommit:
			res, err = doCommit(a, sc, p)
		case adapters.OpRead:
			res, err = a.Read(context.Background(), sc, adapters.ReadRequest{CapabilityID: fake.ReadCapability, Fields: map[string]any{"item_id": "x"}})
		}
		return res, err, app, p
	}

	t.Run("reply damage leaves a commit applied", func(t *testing.T) {
		for _, f := range []fake.Fault{fake.FaultMalformed, fake.FaultPartial, fake.FaultTextOnly, fake.FaultGeneric, fake.FaultWrongCapability, fake.FaultWrongKey, fake.FaultOversize, fake.FaultDropReply} {
			_, err, app, _ := run(t, f, adapters.OpCommit)
			if contracts.CodeOf(err) != contracts.ErrUncertainOutcome {
				t.Errorf("%s: %v, want uncertain_outcome", f, err)
			}
			if app.Effects() != 1 {
				t.Errorf("%s: %d effects, want 1 (the host processed the commit)", f, app.Effects())
			}
		}
	})

	t.Run("a lost request or a hang leaves a commit unapplied", func(t *testing.T) {
		for _, f := range []fake.Fault{fake.FaultDropRequest, fake.FaultHang} {
			app := fake.NewApp()
			a := wrapped(t, app)
			p := prep(t, a, sc, "Widget")
			app.Inject(adapters.OpCommit, f)
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			_, err := a.Commit(ctx, sc, adapters.CommitRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: p.IdempotencyKey, Prepared: p})
			cancel()
			if contracts.CodeOf(err) != contracts.ErrUncertainOutcome || app.Effects() != 0 {
				t.Errorf("%s: %v, %d effects", f, err, app.Effects())
			}
		}
	})

	t.Run("deny and dependency_down replace the execution", func(t *testing.T) {
		for f, want := range map[fake.Fault]actions.DeterministicResultStatus{
			fake.FaultDeny: actions.DeterministicResultStatusRejected, fake.FaultDependencyDown: actions.DeterministicResultStatusUnavailable,
		} {
			res, err, app, _ := run(t, f, adapters.OpCommit)
			if err != nil || res.Status != want || app.Effects() != 0 {
				t.Errorf("%s: %+v, %v, %d effects", f, res, err, app.Effects())
			}
		}
	})

	t.Run("a fault fires once, in injection order, and can be cleared", func(t *testing.T) {
		app := fake.NewApp()
		a := wrapped(t, app)
		read := func() (actions.DeterministicResult, error) {
			return a.Read(context.Background(), sc, adapters.ReadRequest{CapabilityID: fake.ReadCapability, Fields: map[string]any{"item_id": "x"}})
		}
		app.Inject(adapters.OpRead, fake.FaultDeny)
		app.Inject(adapters.OpRead, fake.FaultDependencyDown)
		app.Inject(adapters.OpRead, fake.FaultTextOnly)
		r1, e1 := read()
		r2, e2 := read()
		_, e3 := read()
		r4, e4 := read()
		if e1 != nil || r1.Errors[0].Code != "permission_denied" || e2 != nil || r2.Status != actions.DeterministicResultStatusUnavailable || contracts.CodeOf(e3) != contracts.ErrSchemaInvalid || e4 != nil || r4.Errors[0].Code != "validation_failed" {
			t.Fatalf("sequence: %+v %v | %+v %v | %v | %+v %v", r1, e1, r2, e2, e3, r4, e4)
		}
		app.Inject(adapters.OpRead, fake.FaultDeny)
		app.ClearFaults()
		if r, err := read(); err != nil || r.Errors[0].Code != "validation_failed" {
			t.Fatalf("a cleared fault fired: %+v, %v", r, err)
		}
		// A fault is per operation.
		app.Inject(adapters.OpCommit, fake.FaultDeny)
		if r, err := read(); err != nil || r.Errors[0].Code != "validation_failed" {
			t.Fatalf("a commit fault fired on a read: %+v, %v", r, err)
		}
	})

	t.Run("AllFaults is complete and distinct", func(t *testing.T) {
		seen := map[fake.Fault]bool{}
		for _, f := range fake.AllFaults() {
			if f == "" || seen[f] {
				t.Errorf("fault %q is empty or listed twice", f)
			}
			seen[f] = true
		}
		if len(seen) != 13 {
			t.Errorf("%d faults", len(seen))
		}
	})
}

// TestAdapterTransportBehaviour: the fake adapter classifies wire conditions
// the way adapters.AppAdapter requires of a transport.
func TestAdapterTransportBehaviour(t *testing.T) {
	sc := scope("ten_a")
	t.Run("refused is unavailable and the host never saw the request", func(t *testing.T) {
		app := fake.NewApp()
		app.SetAvailable(false)
		ad := fake.NewAdapter(app)
		_, err := ad.Read(context.Background(), sc, adapters.ReadRequest{CapabilityID: fake.ReadCapability})
		if contracts.CodeOf(err) != contracts.ErrUnavailable || len(app.Calls()) != 0 {
			t.Fatalf("%v, %d calls", err, len(app.Calls()))
		}
	})
	t.Run("a hang ends with the context's error", func(t *testing.T) {
		app := fake.NewApp()
		app.Inject(adapters.OpRead, fake.FaultHang)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		_, err := fake.NewAdapter(app).Read(ctx, sc, adapters.ReadRequest{CapabilityID: fake.ReadCapability})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("a context that is done sends nothing", func(t *testing.T) {
		app := fake.NewApp()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := fake.NewAdapter(app).Read(ctx, sc, adapters.ReadRequest{CapabilityID: fake.ReadCapability})
		if !errors.Is(err, context.Canceled) || len(app.Calls()) != 0 {
			t.Fatalf("%v, %d calls", err, len(app.Calls()))
		}
	})
	t.Run("a dropped reply is unavailable for a read and uncertain for a commit", func(t *testing.T) {
		app := fake.NewApp()
		ad := fake.NewAdapter(app)
		app.Inject(adapters.OpRead, fake.FaultDropReply)
		if _, err := ad.Read(context.Background(), sc, adapters.ReadRequest{CapabilityID: fake.ReadCapability}); contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Fatalf("read: %v", err)
		}
	})
	t.Run("the adapter sends only the identity", func(t *testing.T) {
		app := fake.NewApp()
		if _, err := fake.NewAdapter(app).Read(context.Background(), sc, adapters.ReadRequest{CapabilityID: fake.ReadCapability}); err != nil {
			t.Fatal(err)
		}
		c := app.Calls()
		if len(c) != 1 || cmp.Diff(adapters.IdentityFrom(sc), c[0].Identity) != "" || c[0].Seq != 1 {
			t.Fatalf("calls = %+v", c)
		}
	})
	t.Run("describe reports what the host is configured with", func(t *testing.T) {
		app := fake.NewApp(fake.WithApplicationVersion("3.1.4"), fake.WithUnmapped("admin_reset"))
		d, err := fake.NewAdapter(app).Describe(context.Background())
		if err != nil || d.Transport != fake.Transport || d.ApplicationVersion != "3.1.4" || len(d.Unmapped) != 1 || d.Unmapped[0] != "admin_reset" {
			t.Fatalf("%+v, %v", d, err)
		}
		app.SetContractVersions(1, 2)
		if d, err := fake.NewAdapter(app).Describe(context.Background()); err != nil || len(d.ContractVersions) != 2 {
			t.Fatalf("%+v, %v", d, err)
		}
	})
}
