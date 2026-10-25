package native

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// preparedFrom builds the PreparedAction the coordinator would hold for a
// successful Prepare result.
func preparedFrom(res actions.DeterministicResult) actions.PreparedAction {
	p := res.Preparation
	return actions.PreparedAction{
		PreparationID:          actions.PreparationID(ids.New("prep")),
		CapabilityID:           res.CapabilityID,
		State:                  actions.PreparedActionStateCommitting,
		Summary:                p.Summary,
		Effects:                p.Effects,
		RiskLevel:              p.DeclaredRiskLevel,
		DataVersions:           p.DataVersions,
		PreparedAt:             "2026-10-09T08:00:00Z",
		ExpiresAt:              "2026-10-09T08:05:00Z",
		InvalidationConditions: []actions.PreparedActionInvalidationConditionsItem{actions.PreparedActionInvalidationConditionsItemDataVersionChanged},
		Confirmation:           actions.ConfirmationRequirement{Mode: actions.ConfirmationRequirementModeNone, Bindings: []actions.ConfirmationRequirementBindingsItem{}},
		IdempotencyKey:         actions.IdempotencyKey(ids.New("idem")),
	}
}

// The adapter and the Go bridge speak the same protocol: every operation of the
// contract goes through (the prepared action survives the bridge's strict
// decoding), the host sees exactly the identity of the call, an operation the
// host exposes and the package does not map is reported to the audit and cannot be
// called, and the audit's provisioning operations are reachable only when the
// adapter configuration approves them and the host declares them.
func TestAdapterOverTheGoBridge(t *testing.T) {
	for _, network := range []string{NetworkUnix, NetworkTCP} {
		t.Run(network, func(t *testing.T) {
			b := startBridge(t, network)
			cfg := fixtureConfig(t, network, b.addr)
			cfg.Endpoints[0].Provisioning = []string{"seed", "cleanup"}
			a := newAdapter(t, cfg)
			ctx := context.Background()
			sc := testScope()

			d, err := a.Describe(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if d.Transport != Transport || d.ApplicationVersion != "1.0.0" || len(d.Capabilities) != 2 {
				t.Errorf("description = %+v", d)
			}
			if diff := cmp.Diff([]string{"admin_dump"}, d.Unmapped); diff != "" {
				t.Errorf("unmapped (-want +got):\n%s", diff)
			}
			if h, err := a.Health(ctx); err != nil || h.Status != adapters.HealthOK {
				t.Errorf("health = %+v, %v", h, err)
			}
			in, err := a.Inspect(ctx, sc, adapters.InspectRequest{})
			if err != nil || len(in.Unverified()) != len(adapters.AllAspects()) {
				t.Errorf("a bridge that exposes nothing for inspection leaves every aspect unverified: %+v, %v", in, err)
			}

			if _, err := a.Manage(ctx, testEP, "seed", map[string]any{"name": "t"}); err != nil {
				t.Fatalf("an approved provisioning operation: %v", err)
			}
			for _, name := range []string{"cleanup", "drop_database", "items_get"} {
				if _, err := a.Manage(ctx, testEP, name, nil); err == nil {
					t.Errorf("Manage(%q) succeeded: it is not an approved and declared management operation", name)
				}
			}
			if _, err := a.Manage(ctx, "other", "seed", nil); err == nil {
				t.Error("Manage on an unknown endpoint succeeded")
			}

			w := wrapped(t, a)
			res, err := w.Read(ctx, sc, adapters.ReadRequest{CapabilityID: fake.ReadCapability, Fields: map[string]any{"item_id": "item_1"}})
			if err != nil || res.Status != actions.DeterministicResultStatusOk {
				t.Fatalf("read: %+v, %v", res, err)
			}
			calls := b.app.Calls()
			want := adapters.IdentityFrom(sc)
			if diff := cmp.Diff(want, calls[len(calls)-1].Identity); diff != "" {
				t.Errorf("the host received a different identity (-want +got):\n%s", diff)
			}

			fields := map[string]any{"name": "Gadget"}
			if v, err := w.Validate(ctx, sc, adapters.ValidateRequest{CapabilityID: fake.ActionCapability, Fields: fields}); err != nil || v.Status != actions.DeterministicResultStatusOk {
				t.Fatalf("validate: %+v, %v", v, err)
			}
			prep, err := w.Prepare(ctx, sc, adapters.PrepareRequest{CapabilityID: fake.ActionCapability, Fields: fields})
			if err != nil || prep.Preparation == nil {
				t.Fatalf("prepare: %+v, %v", prep, err)
			}
			p := preparedFrom(prep)
			req := adapters.CommitRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: p.IdempotencyKey, Prepared: p}
			first, err := w.Commit(ctx, sc, req)
			if err != nil || first.Status != actions.DeterministicResultStatusOk || len(first.Identifiers) != 1 {
				t.Fatalf("commit: %+v, %v", first, err)
			}
			again, err := w.Commit(ctx, sc, req)
			if err != nil || cmp.Diff(first, again) != "" || b.app.Effects() != 1 {
				t.Fatalf("a repeated commit: %v, %d effects; want the original result and one effect", err, b.app.Effects())
			}
			st, err := w.Status(ctx, sc, adapters.StatusRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: p.IdempotencyKey})
			if err != nil || st.Status != actions.DeterministicResultStatusOk {
				t.Fatalf("status: %+v, %v", st, err)
			}

			// A method the package does not map is not callable by any request.
			before := b.handled.Load()
			if _, err := w.Read(ctx, sc, adapters.ReadRequest{CapabilityID: "admin.dump", Fields: map[string]any{}}); err == nil {
				t.Error("an unmapped capability was called")
			}
			if b.handled.Load() != before {
				t.Error("a call reached the host for a method nothing maps")
			}
		})
	}
}

// validCommit is a commit request the guard accepts, from a real preparation by
// the in-memory fake adapter.
func validCommit(t testing.TB) adapters.CommitRequest {
	t.Helper()
	fa := fake.NewAdapter(fake.NewApp())
	res, err := fa.Prepare(context.Background(), testScope(), adapters.PrepareRequest{CapabilityID: fake.ActionCapability, Fields: map[string]any{"name": "Widget"}})
	if err != nil || res.Preparation == nil {
		t.Fatalf("prepare: %+v, %v", res, err)
	}
	p := preparedFrom(res)
	return adapters.CommitRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: p.IdempotencyKey, Prepared: p}
}
