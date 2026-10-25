package apps_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/apps"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestConflictIsEqualityOfTheDomainAmongOtherApplications(t *testing.T) {
	cand := func(app, domain string) apps.Candidate {
		return apps.Candidate{ApplicationID: app, SecurityDomain: domain, Phase: apps.PhaseActivation}
	}
	tests := []struct {
		name   string
		c      apps.Candidate
		active []apps.Active
		want   string // the application it conflicts with, "" for none
	}{
		{"nothing is active", cand("b", "ops"), nil, ""},
		{"same domain", cand("b", "ops"), []apps.Active{{"a", "ops"}}, ""},
		{"another domain", cand("b", "legal"), []apps.Active{{"a", "ops"}}, "a"},
		{"undeclared shares with undeclared", cand("b", ""), []apps.Active{{"a", ""}}, ""},
		{"undeclared is not a named domain", cand("b", ""), []apps.Active{{"a", "ops"}}, "a"},
		{"a named domain is not undeclared", cand("b", "ops"), []apps.Active{{"a", ""}}, "a"},
		{"an application never conflicts with itself", cand("a", "legal"), []apps.Active{{"a", "ops"}}, ""},
		{"one differing application among several is enough", cand("d", "ops"), []apps.Active{{"a", "ops"}, {"b", "ops"}, {"c", "legal"}}, "c"},
		{"the first in the order given is named", cand("d", "x"), []apps.Active{{"a", "ops"}, {"b", "legal"}}, "a"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, conflicts := apps.Conflict(tc.c, tc.active)
			if conflicts != (tc.want != "") || got.ApplicationID != tc.want {
				t.Errorf("Conflict = %+v, %v; want a conflict with %q", got, conflicts, tc.want)
			}
		})
	}
}

// rig is a store with every module's migrations (this one's included) and an audit log.
type rig struct {
	t     *testing.T
	db    *store.DB
	log   *audit.Log
	guard *apps.Guard
}

func newRig(t *testing.T) *rig {
	t.Helper()
	db, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "alixai.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	log, err := audit.New(db, audit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	g, err := apps.NewGuard(db, log)
	if err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, db: db, log: log, guard: g}
}

func (r *rig) exec(query string, args ...any) error {
	r.t.Helper()
	return r.db.Tx(context.Background(), func(tx *store.Tx) error {
		_, err := tx.ExecContext(context.Background(), query, args...)
		return err
	})
}

// install records a quarantined package exactly as apppkg.Installer does.
func (r *rig) install(app, hash, domain string) error {
	r.t.Helper()
	return r.exec(`INSERT INTO apppkg_packages
 (application_id, manifest_hash, package_version, status, security_domain, signer_key_id, install_path,
  file_count, byte_count, warning_count, installed_at, status_changed_at, status_reason)
 VALUES (?, ?, '1.0.0', 'quarantined', ?, 'k', ?, 1, 1, 0, 1, 1, 'installed')`,
		app, strings.Repeat(hash, 64)[:64], domain, "packages/"+app)
}

// activate does what integrationaudit.Runner.Activate does to the row: supersede the application's
// own active package, then set the new one active with an audit report.
func (r *rig) activate(app, hash string) error {
	r.t.Helper()
	full := strings.Repeat(hash, 64)[:64]
	return r.db.Tx(context.Background(), func(tx *store.Tx) error {
		if _, err := tx.ExecContext(context.Background(),
			`UPDATE apppkg_packages SET status = 'superseded', status_changed_at = 2, status_reason = 'superseded'
			  WHERE application_id = ? AND status = 'active'`, app); err != nil {
			return err
		}
		_, err := tx.ExecContext(context.Background(),
			`UPDATE apppkg_packages SET status = 'active', audit_report_id = 'run_1', status_changed_at = 3, status_reason = 'activated'
			  WHERE application_id = ? AND manifest_hash = ? AND status = 'quarantined'`, app, full)
		return err
	})
}

func (r *rig) quarantine(app string) {
	r.t.Helper()
	// the way Runner.Quarantine takes an active package out of service
	if err := r.exec(`UPDATE apppkg_packages SET status = 'quarantined', audit_report_id = NULL, status_changed_at = 4
		  WHERE application_id = ? AND status = 'active'`, app); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) must(err error) {
	r.t.Helper()
	if err != nil {
		r.t.Fatal(err)
	}
}

func wantRefusal(t *testing.T, err error, phase apps.Phase) *contracts.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want a %s refusal", phase)
	}
	ce, ok := apps.AsRefusal(err)
	if !ok {
		t.Fatalf("%v is not a refusal of the security-domain rule", err)
	}
	if ce.Code != contracts.ErrConflict || ce.Details["reason"] != apps.ReasonSecurityDomain || ce.Details["phase"] != string(phase) {
		t.Fatalf("refusal = %+v, want code conflict, reason %s, phase %s", ce, apps.ReasonSecurityDomain, phase)
	}
	if !errors.Is(ce, apps.ErrSecurityDomain) {
		t.Errorf("a refusal wraps apps.ErrSecurityDomain: %v", ce)
	}
	return ce
}

// The rule as the store enforces it, through the statements the installer and the audit runner run.
func TestStoreRefusesInstallAndActivationOfAnotherDomain(t *testing.T) {
	r := newRig(t)

	// nothing is active yet: any domain installs and activates
	r.must(r.install("a", "a", "ops"))
	r.must(r.activate("a", "a"))

	// another domain cannot be installed, and not activated either
	wantRefusal(t, r.install("b", "b", "legal"), apps.PhaseInstall)
	wantRefusal(t, r.install("c", "c", ""), apps.PhaseInstall) // no declared domain is not "ops"

	// the same domain can
	r.must(r.install("d", "d", "ops"))
	r.must(r.activate("d", "d"))

	// a package installed while nothing conflicted can still be refused when it is activated: b was
	// installed (quarantined) at a time a and d were out of service
	r.quarantine("a")
	r.quarantine("d")
	r.must(r.install("b", "b", "legal"))
	r.must(r.install("a", "9", "ops")) // a second package of a, to be activated when a is back
	r.must(r.activate("a", "9"))
	wantRefusal(t, r.activate("b", "b"), apps.PhaseActivation)
	var status string
	if err := r.db.View(context.Background(), func(tx *store.Tx) error {
		return tx.QueryRowContext(context.Background(), `SELECT status FROM apppkg_packages WHERE application_id = 'b'`).Scan(&status)
	}); err != nil || status != "quarantined" {
		t.Errorf("the refused application's status = %q, %v; it stays quarantined", status, err)
	}

	// only packages that are active count: with a out of service (and d, which was quarantined above)
	// b activates
	r.quarantine("a")
	r.must(r.activate("b", "b"))
	wantRefusal(t, r.install("a", "8", "ops"), apps.PhaseInstall)
}

func TestAnApplicationNeverConflictsWithItselfAcrossVersions(t *testing.T) {
	r := newRig(t)
	r.must(r.install("a", "1", "ops"))
	r.must(r.activate("a", "1"))
	// the new version of the only active application changes its domain: nobody else is affected
	r.must(r.install("a", "2", "legal"))
	r.must(r.activate("a", "2"))
	// ...until another application is active: then a's next version must keep to that domain
	r.must(r.install("b", "b", "legal"))
	r.must(r.activate("b", "b"))
	wantRefusal(t, r.install("a", "3", "ops"), apps.PhaseInstall)
	r.must(r.install("a", "4", "legal"))
	r.must(r.activate("a", "4"))
}

func TestAsRefusalIsNotFooledByOtherErrors(t *testing.T) {
	if _, ok := apps.AsRefusal(nil); ok {
		t.Error("nil is not a refusal")
	}
	for _, err := range []error{
		errors.New("constraint failed: a package is installed quarantined"),
		contracts.New(contracts.ErrConflict, "something else"),
		contracts.New(contracts.ErrUnavailable, "security_domain_mismatch in prose is not the trigger's text"),
	} {
		if ce, ok := apps.AsRefusal(err); ok {
			t.Errorf("%v was taken for a refusal: %+v", err, ce)
		}
	}
	// a refusal survives being wrapped and joined
	wrapped := fmt.Errorf("install: %w", errors.Join(errors.New("x"), contracts.Wrap(contracts.ErrUnavailable, errors.New("SQL logic error: security_domain_mismatch(activation): ..."), "cannot activate")))
	if ce, ok := apps.AsRefusal(wrapped); !ok || ce.Details["phase"] != string(apps.PhaseActivation) {
		t.Errorf("AsRefusal(wrapped) = %+v, %v", ce, ok)
	}
}

// Guard reads what the triggers read, types the refusal and records it on the refused application's
// chain, and on no other. Check records nothing.
func TestGuardTypesAndRecordsTheRefusalOnTheRefusedChainOnly(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.must(r.install("a", "a", "ops"))
	r.must(r.activate("a", "a"))
	r.must(r.install("d", "d", "ops"))
	r.must(r.activate("d", "d"))

	count := func(app string) int {
		t.Helper()
		var n int
		if err := r.db.View(ctx, func(tx *store.Tx) error {
			return tx.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE application_id = ? AND event_type = ?`,
				app, string(apps.EventDomainRefused.Name())).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	b := apps.Candidate{ApplicationID: "b", SecurityDomain: "legal", ManifestHash: strings.Repeat("b", 64), Phase: apps.PhaseActivation}

	ce := wantRefusal(t, r.guard.Check(ctx, b), apps.PhaseActivation)
	if ce.Details["active_application_id"] != "a" || ce.Details["active_security_domain"] != "ops" || ce.Details["application_id"] != "b" {
		t.Errorf("the refusal names the wrong applications: %+v", ce.Details)
	}
	if n := count("b") + count("a") + count("d"); n != 0 {
		t.Fatalf("Check recorded %d event(s); it records nothing", n)
	}

	wantRefusal(t, r.guard.Admit(ctx, b, "root"), apps.PhaseActivation)
	if count("b") != 1 || count("a") != 0 || count("d") != 0 {
		t.Errorf("events b/a/d = %d/%d/%d, want 1/0/0: the refusal is on the refused application's chain only", count("b"), count("a"), count("d"))
	}

	// an application of the same domain is admitted, and nothing is written for it
	same := apps.Candidate{ApplicationID: "e", SecurityDomain: "ops", Phase: apps.PhaseInstall}
	if err := r.guard.Admit(ctx, same, "root"); err != nil {
		t.Errorf("an application of the same domain is refused: %v", err)
	}
	if count("e") != 0 {
		t.Error("an admission wrote an event")
	}

	// bad candidates are validation errors, not refusals and not admissions
	for _, bad := range []apps.Candidate{{SecurityDomain: "ops", Phase: apps.PhaseInstall}, {ApplicationID: "x", Phase: "later"}} {
		if err := r.guard.Check(ctx, bad); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("Check(%+v) = %v, want validation_failed", bad, err)
		}
	}
	if _, err := apps.NewGuard(nil, r.log); err == nil {
		t.Error("a guard without a store was built")
	}
	if _, err := apps.NewGuard(r.db, nil); err == nil {
		t.Error("a guard without an audit log was built")
	}
}

// The test above only means something if it fails when the rule is gone: the same scenario in a
// store whose triggers have been dropped lets the other domain in, and the guard (which reads the
// same table) is what then still says no.
func TestTheRuleIsWhatRefusesNotTheScenario(t *testing.T) {
	r := newRig(t)
	r.must(r.exec(`DROP TRIGGER apps_security_domain_on_install`))
	r.must(r.exec(`DROP TRIGGER apps_security_domain_on_activation`))

	r.must(r.install("a", "a", "ops"))
	r.must(r.activate("a", "a"))
	if err := r.install("b", "b", "legal"); err != nil {
		t.Fatalf("with the install trigger gone the install of another domain should go through: %v", err)
	}
	if err := r.activate("b", "b"); err != nil {
		t.Fatalf("with the activation trigger gone the activation of another domain should go through: %v", err)
	}
	// two domains are now active in one process: the state the rule exists to prevent
	active, err := r.guard.Others(context.Background(), "a")
	if err != nil || len(active) != 1 || active[0].SecurityDomain != "legal" {
		t.Fatalf("Others = %+v, %v", active, err)
	}
	if err := r.guard.Check(context.Background(), apps.Candidate{ApplicationID: "c", SecurityDomain: "ops", Phase: apps.PhaseInstall}); err == nil {
		t.Error("the guard admits an application into a process that already serves two domains")
	}

	// and the module's migration is what installs the triggers: a fresh store has them
	fresh := newRig(t)
	var n int
	if err := fresh.db.View(context.Background(), func(tx *store.Tx) error {
		return tx.QueryRowContext(context.Background(),
			`SELECT count(*) FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'apps_security_domain_%'`).Scan(&n)
	}); err != nil || n != 2 {
		t.Errorf("a fresh store has %d security-domain triggers, %v; want 2", n, err)
	}
}
