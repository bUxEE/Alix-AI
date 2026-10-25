package m1

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/cli"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/tools/devkeys"
)

// refusal is what `alixai package install` prints for a package it refuses.
type refusal struct {
	Installed bool   `json:"installed"`
	Reason    string `json:"reason"`
	Findings  []struct {
		CheckID  string `json:"check_id"`
		Severity string `json:"severity"`
		Path     string `json:"path"`
		Message  string `json:"message"`
	} `json:"findings"`
}

// servesNothing holds the application to being served to nobody: the API reports it in the given
// status, every request that would reach it is refused as quarantined, and neither the application
// nor the model is touched.
func servesNothing(t *testing.T, r *rig, dep *deployment, c *convo, status string) {
	t.Helper()
	before := dep.clinic.stats(t)
	calls := r.model.Calls()
	if got, _, reply := c.application(); got != status {
		t.Errorf("the application is %q (%s), want %q", got, reply, status)
	}
	if got := c.open(); got.Status != http.StatusForbidden || got.code() != contracts.ErrQuarantined {
		t.Errorf("a session of an application in status %q: %s", status, got)
	}
	if got := c.call(http.MethodPost, c.turnsPath(), map[string]any{"text": "Please book Alma Gatefield", "language": "en"}); got.code() != contracts.ErrQuarantined {
		t.Errorf("a turn for an application in status %q: %s", status, got)
	}
	if got := c.confirmAction("prep_01M000000000000000000000AA", "cfm_x"); got.code() != contracts.ErrQuarantined {
		t.Errorf("a confirmation for an application in status %q: %s", status, got)
	}
	after := dep.clinic.stats(t)
	if appCalls(after) != appCalls(before) || after.commits() != before.commits() {
		t.Errorf("the application was asked something for a package that is not active: %s", after.callsSince(before))
	}
	if r.model.Calls() != calls {
		t.Errorf("the model was called for a package that is not active")
	}
}

// A package that has been tampered with never reaches the active state, by any road the operator
// has: a package edited after it was signed, one signed by a stranger or with a forged signature,
// one that is unsigned or whose signature has run out, an installed copy edited before the audit,
// and a package whose publisher's key was revoked after it was installed. Each is refused or
// blocked with the finding that says why, and in none does the application serve a request, get
// asked a thing, or see the model. The untouched package installs without complaint, so the refusals
// are not an artefact of the fixture.
// SEC-057
func TestM1TamperedPackageNeverActivates(t *testing.T) {
	clinic := startClinic(t)
	op := newOperator(t)
	dep := &deployment{clinic: clinic, op: op}
	w := seedWorld(t, clinic, "tp")
	r := newRig(t, dep, withTemplateOnly())
	good := signedClinicPackage(t, clinic)
	op.addIssuerKey(good)
	rc := r.convo(t, w, w.Reception)
	start := clinic.stats(t)

	_, strangerKey, err := trust.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	signedBy := func(key trust.SigningKey) func(t tb) string {
		return func(t tb) string {
			dir := signedClinicPackage(t, clinic)
			signPackage(t, dir, key)
			return dir
		}
	}
	cases := []struct {
		name   string
		build  func(t tb) string
		checks []string // finding check ids that must be among the critical ones
		sub    string   // a phrase the findings must say
	}{
		{
			name: "edited after it was signed: the confirmation requirement removed from the booking",
			build: func(t tb) string {
				dir := signedClinicPackage(t, clinic)
				editFile(t, filepath.Join(dir, "capabilities", "appointments-book.yaml"), func(s string) string {
					s = strings.Replace(s, "risk_level: 2", "risk_level: 0", 1)
					return strings.Replace(s, "confirmation:\n  mode: explicit", "confirmation:\n  mode: none", 1)
				})
				return dir
			},
			checks: []string{"signature.tampered"}, sub: `modified "capabilities/appointments-book.yaml"`,
		},
		{
			name: "a capability file added after it was signed",
			build: func(t tb) string {
				dir := signedClinicPackage(t, clinic)
				if err := os.WriteFile(filepath.Join(dir, "capabilities", "export-everything.yaml"), []byte("id: export-everything\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return dir
			},
			checks: []string{"signature.tampered"}, sub: `added "capabilities/export-everything.yaml"`,
		},
		{
			name: "unsigned",
			build: func(t tb) string {
				dir := signedClinicPackage(t, clinic)
				if err := os.Remove(filepath.Join(dir, "signatures", "signature-manifest.json")); err != nil {
					t.Fatal(err)
				}
				return dir
			},
			checks: []string{"signature.missing"}, sub: "unsigned",
		},
		{
			name:   "signed by a key the operator never registered",
			build:  signedBy(trust.SigningKey{KeyID: strangerKeyID, Key: strangerKey}),
			checks: []string{"signature.invalid"}, sub: "key_not_listed",
		},
		{
			name:   "signed under the publisher's key id by another private key",
			build:  signedBy(trust.SigningKey{KeyID: devkeys.KeyID, Key: strangerKey}),
			checks: []string{"signature.invalid"}, sub: "bad_signature",
		},
		{
			name: "signed once, with signatures that have run out",
			build: func(t tb) string {
				dir := copyTree(t, clinicPkgDir)
				retarget(t, dir, clinic.port)
				if _, err := trust.SignPackageDir(dir, trust.SignOptions{
					PackageIdentity: trust.PackageIdentity{ApplicationID: clinicApplication, PackageVersion: "1.0.0"},
					Keys:            []trust.SigningKey{devkeys.SigningKey()},
					Now:             time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC),
				}); err != nil {
					t.Fatal(err)
				}
				return dir
			},
			checks: []string{"signature.invalid"}, sub: "signature_expired",
		},
	}
	for _, c := range cases {
		t.Run("refused at install: "+c.name, func(t *testing.T) {
			res := op.with(t).run("package", "install", "-operator", "root", c.build(t))
			if res.Code != cli.ExitError {
				t.Fatalf("install exit %d, want a refusal\n%s\n%s", res.Code, res.Stdout, res.Stderr)
			}
			var out refusal
			decodeOut(t, "package install", res.Stdout, &out)
			if out.Installed {
				t.Fatalf("the package was installed: %s", res.Stdout)
			}
			for _, want := range c.checks {
				found := false
				for _, f := range out.Findings {
					if f.CheckID == want && f.Severity == "critical" && strings.Contains(f.Message, c.sub) {
						found = true
					}
				}
				if !found {
					t.Errorf("no critical finding %s mentioning %q:\n%s", want, c.sub, res.Stdout)
				}
			}
			// nothing was recorded, so there is nothing to activate
			if act, _ := op.with(t).audit(); act.Activated {
				t.Errorf("an audit activated a package that was refused at install")
			}
			servesNothing(t, r, dep, rc, "not_installed")
		})
	}

	// the control: the untouched package installs, into quarantine, and activates nothing by itself
	var installed installResult
	t.Run("the untouched package installs into quarantine and serves nothing yet", func(t *testing.T) {
		installed = op.with(t).install(good)
		servesNothing(t, r, dep, rc, "quarantined")
		if !contains(eventTypes(r.applicationEvents()), "package.installed") {
			t.Errorf("the install is not in the audit log: %v", eventTypes(r.applicationEvents()))
		}
	})
	if installed.ManifestHash == "" {
		t.Fatal("the control package was not installed; the rest of this test depends on it")
	}

	t.Run("an installed copy edited before the audit is blocked and stays in quarantine", func(t *testing.T) {
		copyFile := filepath.Join(op.dataDir, filepath.FromSlash(installed.Path), "capabilities", "appointments-book.yaml")
		original, err := os.ReadFile(copyFile)
		if err != nil {
			t.Fatal(err)
		}
		editFile(t, copyFile, func(s string) string { return strings.Replace(s, "risk_level: 2", "risk_level: 0", 1) })
		defer func() {
			if err := os.WriteFile(copyFile, original, 0o600); err != nil {
				t.Errorf("restore the installed copy: %v", err)
			}
		}()
		run, res := op.with(t).audit()
		if res.Code == cli.ExitOK || run.Activated || run.Outcome != "blocked" {
			t.Fatalf("the audit of an edited copy: exit %d\n%s\n%s", res.Code, run, res.Stderr)
		}
		if !hasFinding(run, "signature.tampered", "critical") {
			t.Errorf("the audit did not find the tampering:\n%s", run)
		}
		servesNothing(t, r, dep, rc, "quarantined")
	})

	t.Run("a revoked publisher key blocks the package it signed", func(t *testing.T) {
		op := op.with(t)
		op.must("trust", "revoke-key", "-app", clinicApplication, "-key-id", devkeys.KeyID, "-reason", "the gate revokes the publisher", "-operator", "root")
		run, res := op.audit()
		if res.Code == cli.ExitOK || run.Activated || run.Outcome != "blocked" {
			t.Fatalf("the audit of a package whose key was revoked: exit %d\n%s\n%s", res.Code, run, res.Stderr)
		}
		if !hasFinding(run, "signature.invalid", "critical") {
			t.Errorf("the audit did not refuse the signature:\n%s", run)
		}
		servesNothing(t, r, dep, rc, "quarantined")
	})

	// in the whole test nothing was activated, and the application heard of none of it
	for _, e := range r.applicationEvents() {
		if e.Type == "package.activated" {
			t.Errorf("a package was activated: %v", eventTypes(r.applicationEvents()))
		}
	}
	end := clinic.stats(t)
	if appCalls(end) != appCalls(start) || end.commits() != start.commits() || end.totalsSince(start, "appointments") != 0 {
		t.Errorf("the application was asked something: %s", end.callsSince(start))
	}
	if r.model.Calls() != 0 {
		t.Errorf("the model was called %d times", r.model.Calls())
	}
}

func hasFinding(run auditRun, checkID, severity string) bool {
	for _, f := range run.Findings {
		if f.CheckID == checkID && f.Severity == severity {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
