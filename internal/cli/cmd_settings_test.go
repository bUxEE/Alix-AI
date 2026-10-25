package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/history"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/settings"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/tools/devkeys"
)

// The settings area is provided by a serve provider of its own, registered from
// init() like every later area, and serve refuses to start without it (every serve
// test in this package passes only because the provider is wired).
func TestSettingsServeProviderIsRegistered(t *testing.T) {
	names, byName := registeredServeProviders()
	found := false
	for _, n := range names {
		found = found || n == "settings"
	}
	if !found || byName["settings"] == nil {
		t.Fatalf("serve providers = %v: the settings provider is not registered", names)
	}
}

// The provider-health read-out is the registry's roles and nothing else: no runtime
// name, no version, no reason.
func TestSettingsProvidersReportRolesOnly(t *testing.T) {
	reg := providers.NewRegistry(clock.NewFake(settingsNow))
	runtime := pfake.New()
	if err := reg.Register(providers.RoleController, runtime); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetHealth(providers.RoleController, providers.HealthDegraded, "LocalAI at http://127.0.0.1:8080 is slow"); err != nil {
		t.Fatal(err)
	}
	h, err := settingsProviders{reg: reg}.ProviderHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(h)
	for _, banned := range []string{runtime.Name(), "LocalAI", "127.0.0.1", "slow", "http"} {
		if banned != "" && strings.Contains(string(wire), banned) {
			t.Errorf("the provider health carries %q: %s", banned, wire)
		}
	}
	var controller *settings.ProviderRole
	for i := range h.Roles {
		if h.Roles[i].Role == "controller" {
			controller = &h.Roles[i]
		}
	}
	if controller == nil || !controller.Configured || controller.Status != "degraded" || controller.Effect == "" {
		t.Errorf("controller = %+v", controller)
	}
	if h.Status == "ok" || len(h.Roles) < 2 {
		t.Errorf("health = %+v", h)
	}
}

var settingsNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

// The integration-audit read-out reads the real stored, signed reports: the same
// ones `alixai audit run` wrote. Warnings are open until an authorised operator
// accepts them, and a report that does not verify is not reported at all.
func TestSettingsIntegrationAuditReadsTheStoredReports(t *testing.T) {
	auditChecksProbesBuiltinChecksOnly(t)
	transportsSwapDefault(t)
	e := newOpsEnv(t, "integration_audit:\n  accept_operators: [alice]\n")
	signerPriv, signerPub, _ := e.genKey("audit-signer")
	e.mustRun("trust", "add-key", "-app", trust.PlatformApplicationID, "-key-id", "audit-signer", "-public-key-file", signerPub, "-operator", "root")
	e.mustRun("trust", "add-key", "-app", "demo-library", "-key-id", devkeys.KeyID, "-public-key", devkeys.PublicKeyBase64(), "-allow-test-keys", "-operator", "root")

	open := func() (*settingsReports, func()) {
		db, err := store.Open(context.Background(), store.Options{Path: filepath.Join(e.dir, "data", store.DefaultFileName)})
		if err != nil {
			t.Fatal(err)
		}
		log, err := audit.New(db, audit.Options{})
		if err != nil {
			t.Fatal(err)
		}
		r, err := newSettingsReports(serveDeps{DB: db, Clock: clock.Real{}, DataDir: filepath.Join(e.dir, "data"), Audit: log})
		if err != nil {
			t.Fatal(err)
		}
		return r, func() { _ = db.Close() }
	}
	ctx := context.Background()

	// Nothing installed.
	r, closeDB := open()
	st, err := r.IntegrationAudit(ctx, "demo-library")
	closeDB()
	if err != nil || st.State != "not_installed" || st.Audited || st.Findings == nil || len(st.Findings) != 0 {
		t.Fatalf("before the install: %+v, %v", st, err)
	}

	// Installed, never audited.
	var inst installOut
	decode(t, e.mustRun("package", "install", "-operator", "root", filepath.Join(fixtures, "valid-minimal")).stdout, &inst)
	r, closeDB = open()
	st, err = r.IntegrationAudit(ctx, "demo-library")
	closeDB()
	if err != nil || st.State != "quarantined" || st.Audited || st.ManifestHash != inst.ManifestHash || st.PackageVersion == "" {
		t.Fatalf("after the install: %+v, %v", st, err)
	}

	// Audited: warnings the operator must accept, shown as open.
	var first runOut
	res := e.run("audit", "run", "-signing-key", "audit-signer="+signerPriv, "-operator", "root", "demo-library")
	decode(t, res.stdout, &first)
	r, closeDB = open()
	st, err = r.IntegrationAudit(ctx, "demo-library")
	closeDB()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Audited || st.RunID != first.RunID || st.Outcome != "needs_acceptance" || st.Counts.Critical != 0 || st.Counts.Warnings < 2 || st.State != "quarantined" {
		t.Fatalf("after the audit: %+v", st)
	}
	warnings := 0
	for _, f := range st.Findings {
		if f.Severity == "warning" {
			warnings++
			if f.Status != settings.FindingOpen || f.Key == "" || f.Message == "" || f.Remediation == "" {
				t.Errorf("a warning nobody accepted: %+v", f)
			}
		}
	}
	if warnings != st.Counts.Warnings {
		t.Errorf("%d warnings listed, %d counted", warnings, st.Counts.Warnings)
	}

	// The operator accepts them: the read-out says so, and nothing else moved.
	for _, f := range first.Findings {
		if f.Severity == "warning" {
			e.mustRun("audit", "accept-warning", "-finding", f.Key, "-operator", "alice", "-reason", "reviewed by the operator", "demo-library")
		}
	}
	r, closeDB = open()
	st, err = r.IntegrationAudit(ctx, "demo-library")
	closeDB()
	if err != nil || st.State != "quarantined" || st.Outcome != "needs_acceptance" {
		t.Fatalf("after the acceptances: %+v, %v", st, err)
	}
	for _, f := range st.Findings {
		if f.Severity == "warning" && f.Status != settings.FindingAccepted {
			t.Errorf("an accepted warning is %q", f.Status)
		}
	}

	// Another application's findings are not this application's.
	r, closeDB = open()
	st, err = r.IntegrationAudit(ctx, "other-app")
	closeDB()
	if err != nil || st.State != "not_installed" || len(st.Findings) != 0 || st.ApplicationID != "other-app" {
		t.Errorf("another application: %+v, %v", st, err)
	}

	// A report that was altered after it was signed is not reported at all.
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(e.dir, "data", store.DefaultFileName)})
	if err != nil {
		t.Fatal(err)
	}
	// The table refuses an UPDATE, as it should; an attacker with the file is not
	// bound by a trigger, so the test removes it first.
	if _, err := db.ExecContext(ctx, `DROP TRIGGER integrationaudit_runs_no_update`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	res2, err := db.ExecContext(ctx, `UPDATE integrationaudit_runs SET document = replace(document, '"outcome":"needs_acceptance"', '"outcome":"clear"') WHERE application_id = 'demo-library'`)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if n, _ := res2.RowsAffected(); n == 0 {
		_ = db.Close()
		t.Fatal("the report was not altered: the test would prove nothing")
	}
	_ = db.Close()
	r, closeDB = open()
	_, err = r.IntegrationAudit(ctx, "demo-library")
	closeDB()
	if err == nil || contracts.CodeOf(err) != contracts.ErrInternal {
		t.Errorf("an altered report was read: %v", err)
	}
}

// The operator's settings section reaches the service serve builds: a settings
// block in the configuration file is read into the registered section, and the
// provider builds the service over it (not over the defaults).
func TestSettingsConfigReachesTheServiceServeBuilds(t *testing.T) {
	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	cfg, _ := serveConfig(t, f, "settings:\n  approved_profiles: [lite, balanced]\n  default_profile: balanced\n  approved_voices: [it_female_1]\n  max_attachment_mb: 8\n  max_history_retention_days: 30\n  default_history_retention_days: 14\n")

	var got settings.Limits
	var found bool
	errStop := errors.New("the probe has what it needs")
	registerServeProvider("zz_settings_probe", func(_ context.Context, d serveDeps) error {
		svc, ok := gateway.Lookup[gateway.SettingsService](d.Services)
		if !ok {
			return errors.New("the settings provider did not provide its service")
		}
		real, ok := svc.(*settings.Service)
		if !ok {
			return errors.New("the settings service is not the module's")
		}
		got, found = real.Limits(), true
		return errStop
	})
	t.Cleanup(func() {
		serveProvidersMu.Lock()
		delete(serveProviders, "zz_settings_probe")
		serveProvidersMu.Unlock()
		*settings.ConfigSection() = settings.DefaultConfig()
	})

	var out, errOut syncBuf
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	env := serveEnv(t, &out, &errOut)
	settings.RegisterSection(env.Config) // the registry of a test; the process's own, config.Default, has it from init
	err := runServe(ctx, env, []string{"-config", cfg})
	if !found || !errors.Is(err, errStop) && (err == nil || !strings.Contains(err.Error(), errStop.Error())) {
		t.Fatalf("serve = %v, found = %t\nstdout:\n%s\nstderr:\n%s", err, found, out.String(), errOut.String())
	}
	if fmt.Sprint(got.ApprovedProfiles) != "[lite balanced]" || got.DefaultProfile != "balanced" || fmt.Sprint(got.ApprovedVoices) != "[it_female_1]" ||
		got.MaxAttachmentMB != 8 || got.MaxHistoryRetentionDays != 30 || got.DefaultHistoryRetentionDays != 14 {
		t.Errorf("the service was built over %+v, not over the operator's section", got)
	}
}

// A settings block that contradicts itself stops serve before anything listens.
func TestSettingsConfigContradictionStopsServe(t *testing.T) {
	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	cfg, _ := serveConfig(t, f, "settings:\n  approved_profiles: [lite]\n  default_profile: balanced\n")
	t.Cleanup(func() { *settings.ConfigSection() = settings.DefaultConfig() })
	var out, errOut syncBuf
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	env := serveEnv(t, &out, &errOut)
	settings.RegisterSection(env.Config)
	err := runServe(ctx, env, []string{"-config", cfg})
	if err == nil || !strings.Contains(err.Error(), "default_profile") || strings.Contains(out.String(), "listening on") {
		t.Fatalf("serve = %v\n%s", err, out.String())
	}
}

// The retention a settings document states is history's, so serve builds the settings service over
// the history service (M3-21): the history provider runs after the RAG stack, whose queue and pool
// its job shares, and before the settings provider, which needs the service it provides.
func TestSettingsServeStoresRetentionInHistory(t *testing.T) {
	names, _ := registeredServeProviders()
	at := func(n string) int {
		for i, name := range names {
			if name == n {
				return i
			}
		}
		return -1
	}
	if !(at("rag") >= 0 && at("rag") < at(historyRetentionProviderName) && at(historyRetentionProviderName) < at("settings")) {
		t.Fatalf("the providers run in the order %v: want rag, then %s, then settings", names, historyRetentionProviderName)
	}

	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	cfg, _ := serveConfig(t, f, identityYAML)
	var delegated, haveHistory, haveStack bool
	errStop := errors.New("the probe has what it needs")
	registerServeProvider("zz_settings_retention_probe", func(_ context.Context, d serveDeps) error {
		svc, _ := gateway.Lookup[gateway.SettingsService](d.Services)
		if real, ok := svc.(*settings.Service); ok {
			delegated = real.RetentionDelegated()
		}
		_, haveHistory = gateway.Lookup[*history.Service](d.Services)
		_, haveStack = gateway.Lookup[*ragStack](d.Services)
		return errStop
	})
	t.Cleanup(func() {
		serveProvidersMu.Lock()
		delete(serveProviders, "zz_settings_retention_probe")
		serveProvidersMu.Unlock()
		*settings.ConfigSection() = settings.DefaultConfig()
	})
	var out, errOut syncBuf
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	env := serveEnv(t, &out, &errOut)
	settings.RegisterSection(env.Config)
	err := runServe(ctx, env, []string{"-config", cfg})
	if err == nil || !strings.Contains(err.Error(), errStop.Error()) {
		t.Fatalf("serve = %v\nstderr:\n%s", err, errOut.String())
	}
	if !delegated || !haveHistory || !haveStack {
		t.Errorf("settings delegates retention = %t, history provided = %t, RAG stack provided = %t; want all three", delegated, haveHistory, haveStack)
	}
}
