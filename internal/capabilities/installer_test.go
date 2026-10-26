package capabilities

import (
	"context"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/tools/devkeys"
)

// TestRegistryOverTheRealInstaller runs the registry and router over the real
// installer and the signed demo-library example package: the package is
// installed quarantined (nothing is served), the integration audit's activation
// is simulated with the same UPDATE the audit will issue (nothing in apppkg can
// do it), and from then on the registry serves the typed capabilities and
// terminology of the real package. (SEC-060: quarantined exposes nothing.)
//
// SEC-060
func TestRegistryOverTheRealInstaller(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))
	db := storetest.Open(t, nil) // the default registry holds the apppkg and trust modules
	ts := trust.NewStore(db, clk, trust.AllowTestOnlyKeys())
	if _, err := ts.RegisterKey(ctx, "demo-library", devkeys.KeyID, devkeys.PublicKey(), "test"); err != nil {
		t.Fatal(err)
	}
	in, err := apppkg.NewInstaller(apppkg.InstallerOptions{
		DB: db, Clock: clk, DataDir: t.TempDir(), Verifier: trust.NewVerifier(ts, clk), CoreVersion: apppkg.DevCoreVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := in.Install(ctx, apppkg.FromDir("../../examples/packages/valid-minimal"))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if inst.Record.State != apppkg.StateQuarantined {
		t.Fatalf("installed %s, want quarantined", inst.Record.State)
	}

	reg := NewRegistry(InstallerSource(in), WithClock(clk), WithMaxAge(time.Minute))
	rt, err := NewRouter(reg, policy.NewGateway(clk))
	if err != nil {
		t.Fatal(err)
	}
	member := scopeFor(appLib, "member", []string{"catalog.find_title", "reservations.create"})
	visitor := scopeFor(appLib, "visitor", []string{"catalog.find_title", "reservations.create"})

	// Quarantined: nothing is served.
	if _, err := rt.Route(ctx, reqFor(member, "I'd like to reserve a book")); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Fatalf("a quarantined package was routed: err = %v", err)
	}

	// The audit activates it.
	if _, err := db.ExecContext(ctx,
		`UPDATE apppkg_packages SET status = 'active', audit_report_id = 'rpt_test', status_changed_at = ? WHERE application_id = ? AND manifest_hash = ?`,
		clk.Now().UnixMilli(), appLib, inst.Record.ManifestHash); err != nil {
		t.Fatal(err)
	}
	// The registry notices on its next re-check (or at once, on Refresh).
	if err := reg.Refresh(ctx, appLib); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	info, err := reg.Info(ctx, appLib)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(info.Capabilities, []string{"catalog.find_title", "reservations.create"}) ||
		info.ContentHash != inst.Record.ManifestHash || !equalStrings(info.Languages, []string{"en", "it"}) {
		t.Fatalf("Info = %+v", info)
	}

	for _, tc := range []struct {
		name  string
		scope contracts.Scope
		text  string
		lang  string
		want  []string
	}{
		{"English, by the package's terminology", member, "I want to put a hold on a work", "en", []string{"reservations.create", "catalog.find_title"}},
		{"Italian, by the package's terminology", member, "Vorrei una prenotazione", "it", []string{"reservations.create"}},
		{"a visitor is granted search only", visitor, "I want to put a hold on a work", "en", []string{"catalog.find_title"}},
		{"nothing in the request", member, "What is the capital of Australia?", "en", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := mustRoute(t, rt, reqFor(tc.scope, tc.text, inLanguage(tc.lang)))
			if !equalStrings(res.IDs(), tc.want) {
				t.Fatalf("shortlist = %v, want %v (%s)", res.IDs(), tc.want, res.Outcome)
			}
			if tc.want == nil && res.Outcome != OutcomeOutOfScope {
				t.Fatalf("outcome = %s", res.Outcome)
			}
		})
	}

	// The package is superseded: after the re-check interval nothing is served.
	if _, err := db.ExecContext(ctx, `UPDATE apppkg_packages SET status = 'superseded', status_changed_at = ? WHERE application_id = ? AND manifest_hash = ?`,
		clk.Now().UnixMilli(), appLib, inst.Record.ManifestHash); err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Minute)
	if _, err := rt.Route(ctx, reqFor(member, "I'd like to reserve a book")); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Fatalf("a superseded package was still routed: err = %v", err)
	}
}
