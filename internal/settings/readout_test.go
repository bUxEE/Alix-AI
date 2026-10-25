package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestSettingsCapabilitiesView(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sc := ada()

	view, err := f.svc.Capabilities(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range view.Capabilities {
		ids = append(ids, c.ID)
		if !c.Enabled || len(c.DisabledBy) != 0 || c.ExtraConfirmation {
			t.Errorf("%s: %+v", c.ID, c)
		}
	}
	if strings.Join(ids, ",") != "items.create,items.delete,items.get" {
		t.Errorf("capabilities = %v: the view is the gateway's exposure (admin.purge is admin-only)", ids)
	}
	byID := f.capabilityStates(sc)
	if byID[capGet].RiskLevel != 0 || byID[capCreate].RiskLevel != 2 || byID[capDelete].RiskLevel != 3 {
		t.Errorf("risk levels: %+v", byID)
	}
	if !byID[capCreate].RequiresConfirmation || byID[capGet].RequiresConfirmation || !byID[capDelete].RequiresStepUp {
		t.Errorf("requirements: %+v", byID)
	}
	if strings.Join(byID[capCreate].Fields, ",") != "name,quantity" {
		t.Errorf("fields = %v", byID[capCreate].Fields)
	}
	if view.SettingsAccess.User != (LevelAccess{true, true}) || view.SettingsAccess.Tenant != (LevelAccess{true, true}) || view.SettingsAccess.Deployment != (LevelAccess{true, true}) {
		t.Errorf("settings access = %+v", view.SettingsAccess)
	}

	// Levels switch capabilities off, and the view says which level did.
	f.put(sc, LevelDeployment, 0, `{"capabilities":{"disabled":["items.delete"]}}`)
	f.put(sc, LevelTenant, 0, `{"capabilities":{"disabled":["items.delete","items.create"]}}`)
	f.put(sc, LevelUser, 0, `{"capabilities":{"disabled":["items.get"]}}`)
	byID = f.capabilityStates(sc)
	if got := byID[capDelete]; got.Enabled || fmt.Sprint(got.DisabledBy) != "[deployment tenant]" {
		t.Errorf("items.delete = %+v", got)
	}
	if got := byID[capCreate]; got.Enabled || fmt.Sprint(got.DisabledBy) != "[tenant]" {
		t.Errorf("items.create = %+v", got)
	}
	if got := byID[capGet]; got.Enabled || fmt.Sprint(got.DisabledBy) != "[user]" {
		t.Errorf("items.get = %+v", got)
	}
	// Switching off removes nothing from the list: the gateway's exposure is the list.
	if len(byID) != 3 {
		t.Errorf("%d capabilities, want 3", len(byID))
	}
	// A visitor sees only what the host policy grants a visitor.
	visitor := ada().Clone()
	visitor.Roles = []string{"visitor"}
	if got := f.capabilityStates(visitor); len(got) != 1 || got[capGet].ID == "" {
		t.Errorf("a visitor sees %v", got)
	}
}

func TestSettingsProfiles(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sc := ada()
	list, err := f.svc.Profiles(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(list.Profiles); got != "[{balanced false false} {lite true true}]" {
		t.Errorf("profiles = %s", got)
	}
	f.put(sc, LevelUser, 0, `{"model_profile":"balanced"}`)
	list, _ = f.svc.Profiles(ctx, sc)
	if got := fmt.Sprint(list.Profiles); got != "[{balanced false true} {lite true false}]" {
		t.Errorf("after selecting: %s", got)
	}
	// The list is the operator's, whatever any document says.
	if len(list.Profiles) != 2 {
		t.Errorf("%d profiles", len(list.Profiles))
	}
}

func TestSettingsIntegrationAuditReadout(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sc := withScopes(ada(), CapabilityDeploymentRead)
	f.findings.byApp[tApp] = AuditStatus{
		ApplicationID: tApp, State: "quarantined", PackageVersion: "1.2.0", ManifestHash: strings.Repeat("a", 64), Audited: true, RunID: "run_01", Outcome: "needs_acceptance",
		Counts: AuditCounts{Warnings: 1, Info: 1},
		Findings: []AuditFinding{
			{Key: "0123456789abcdef", CheckID: "audit.section_not_run", Severity: "warning", Message: "a section was not run", Remediation: "run it", Status: FindingOpen},
			{Key: "fedcba9876543210", CheckID: "docs.ok", Severity: "info", Message: "fine", Remediation: "none", Status: FindingInformational},
		},
	}
	st, err := f.svc.IntegrationAudit(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "quarantined" || st.Outcome != "needs_acceptance" || len(st.Findings) != 2 || st.Findings[0].Status != FindingOpen || st.Counts.Warnings != 1 {
		t.Errorf("status = %+v", st)
	}
	// An application that was never audited reports so, with an empty list (never null).
	other := scopeOf(tOther, tTenantA, tWsp1, tUser1, CapabilityDeploymentRead)
	st, err = f.svc.IntegrationAudit(ctx, other)
	if err != nil || st.Audited || st.State != "not_installed" || st.Findings == nil {
		t.Errorf("unaudited: %+v, %v", st, err)
	}
	if wire, _ := json.Marshal(st); !strings.Contains(string(wire), `"findings":[]`) {
		t.Errorf("findings is not an empty list: %s", wire)
	}
	// A source error is typed and a plain one is unavailable, never an internal text.
	f.findings.err = fmt.Errorf("sql: database is locked at /var/lib/alixai/alixai.db")
	_, err = f.svc.IntegrationAudit(ctx, sc)
	wantCode(t, err, contracts.ErrUnavailable)
	var ce *contracts.Error
	asErr(err, &ce)
	if strings.Contains(ce.Message, "/var/lib") {
		t.Errorf("the message carries the cause: %s", ce.Message)
	}
	// No source wired: unavailable, not empty.
	g := newFixture(t, func(d *Deps) { d.Findings, d.Providers = nil, nil })
	_, err = g.svc.IntegrationAudit(ctx, sc)
	wantCode(t, err, contracts.ErrUnavailable)
	_, err = g.svc.ProviderHealth(ctx, sc)
	wantCode(t, err, contracts.ErrUnavailable)
}

func TestSettingsNewRefusesUnsafeWiring(t *testing.T) {
	f := newFixture(t)
	deps := func(mutate func(*Deps)) error {
		d := Deps{DB: f.db, Clock: f.clk, Audit: f.rec, Policy: f.gw, Packages: f.packages, Limits: DefaultLimits()}
		mutate(&d)
		_, err := New(d)
		return err
	}
	for name, mutate := range map[string]func(*Deps){
		"no database":          func(d *Deps) { d.DB = nil },
		"no audit log":         func(d *Deps) { d.Audit = nil },
		"no policy gateway":    func(d *Deps) { d.Policy = nil },
		"no package source":    func(d *Deps) { d.Packages = nil },
		"no approved profile":  func(d *Deps) { d.Limits.ApprovedProfiles = nil },
		"unapproved default":   func(d *Deps) { d.Limits.DefaultProfile = "balanced" },
		"a retention of zero":  func(d *Deps) { d.Limits.MaxHistoryRetentionDays = 0 },
		"a default over a cap": func(d *Deps) { d.Limits.DefaultHistoryRetentionDays = 999 },
		"no attachment size":   func(d *Deps) { d.Limits.MaxAttachmentMB = 0 },
	} {
		if err := deps(mutate); err == nil {
			t.Errorf("%s: New succeeded", name)
		} else if code(err) != contracts.ErrValidationFailed {
			t.Errorf("%s: code %s", name, code(err))
		}
	}
	if err := deps(func(*Deps) {}); err != nil {
		t.Errorf("valid wiring: %v", err)
	}
	// The service holds its own copy of the limits.
	d := Deps{DB: f.db, Audit: f.rec, Policy: f.gw, Packages: f.packages, Limits: DefaultLimits()}
	svc, _ := New(d)
	d.Limits.ApprovedProfiles[0] = "tampered"
	if svc.limits.ApprovedProfiles[0] != "lite" {
		t.Error("the service shares the caller's slice of approved profiles")
	}
	// ...and hands out copies too.
	got := svc.Limits()
	got.ApprovedProfiles[0] = "tampered"
	if svc.Limits().ApprovedProfiles[0] != "lite" || got.DefaultProfile != "lite" {
		t.Error("Limits shares the service's slice")
	}
}

func TestSettingsConfig(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("the defaults are invalid: %v", err)
	}
	bad := map[string]func(*Config){
		"no profiles":                  func(c *Config) { c.ApprovedProfiles = nil },
		"a url as a profile":           func(c *Config) { c.ApprovedProfiles = []string{"http://x"} },
		"an upper-case profile":        func(c *Config) { c.ApprovedProfiles = []string{"Lite"}; c.DefaultProfile = "Lite" },
		"a default that is not listed": func(c *Config) { c.DefaultProfile = "balanced" },
		"a path as a voice":            func(c *Config) { c.ApprovedVoices = []string{"/voices/x"} },
		"too many profiles": func(c *Config) {
			c.ApprovedProfiles = nil
			for i := 0; i < 40; i++ {
				c.ApprovedProfiles = append(c.ApprovedProfiles, fmt.Sprintf("p%d", i))
			}
			c.DefaultProfile = "p1"
		},
		"retention above the hard bound": func(c *Config) { c.MaxHistoryRetentionDays = 99999 },
		"a default above the ceiling":    func(c *Config) { c.DefaultHistoryRetentionDays = 400 },
		"an attachment ceiling of zero":  func(c *Config) { c.MaxAttachmentMB = 0 },
		"an attachment ceiling too high": func(c *Config) { c.MaxAttachmentMB = 4096 },
	}
	for name, mutate := range bad {
		c := DefaultConfig()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The section is registered under its name and an operator can override it.
	found := false
	for _, s := range config.Default.Sections() {
		found = found || s == SectionName
	}
	if !found {
		t.Fatalf("section %q is not registered (have %v)", SectionName, config.Default.Sections())
	}
	r := config.NewRegistry()
	c := DefaultConfig()
	r.Register(SectionName, &c)
	dir := t.TempDir()
	path := dir + "/alixai.yaml"
	if err := writeFile(path, "settings:\n  approved_profiles: [lite, balanced]\n  default_profile: balanced\n  max_attachment_mb: 8\n"); err != nil {
		t.Fatal(err)
	}
	if err := r.Load(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := c.Limits(); fmt.Sprint(got.ApprovedProfiles) != "[lite balanced]" || got.DefaultProfile != "balanced" || got.MaxAttachmentMB != 8 || got.MaxHistoryRetentionDays != 365 {
		t.Errorf("limits = %+v", got)
	}
	// A contradictory file stops the load: nothing half-understood runs.
	if err := writeFile(path, "settings:\n  default_profile: nonexistent\n"); err != nil {
		t.Fatal(err)
	}
	if err := r.Load(path); err == nil {
		t.Error("an unapproved default profile was accepted by the loader")
	}
	// CurrentConfig hands out copies.
	cur := CurrentConfig()
	cur.ApprovedProfiles[0] = "tampered"
	if CurrentConfig().ApprovedProfiles[0] != "lite" {
		t.Error("CurrentConfig shares its slice with the registered section")
	}
}
