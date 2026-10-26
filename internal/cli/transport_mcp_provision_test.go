package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	mcptransport "github.com/buxee/alix-ai/adapters/mcp"
	mcpfixture "github.com/buxee/alix-ai/adapters/mcp/testdata/fixture"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/integrationaudit/probes"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// ---- the clinic reference application, started as a subprocess --------------

const mcpProvisionClinicDir = "../../examples/reference-apps/clinic-booking"

const mcpProvisionClinicApp = "clinic-booking"

// mcpProvisionClinic is a running clinic application in test support mode.
type mcpProvisionClinic struct {
	// pid is the process this test started and is the only one it ever stops.
	pid     int
	baseURL string
	mcpURL  string
}

type mcpProvisionStats struct {
	Calls  map[string]int `json:"calls"`
	Totals map[string]int `json:"totals"`
}

// mcpProvisionStartClinic starts the real clinic application (TypeScript, the official MCP
// SDK) with its test support surface on a free loopback port. It is skipped only where
// node or the application's dependencies are missing; it is not behind an environment
// variable, because the acceptance of the bridge is that it runs against the real thing.
func mcpProvisionStartClinic(t *testing.T) *mcpProvisionClinic {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the clinic reference application cannot be started")
	}
	appDir, err := filepath.Abs(mcpProvisionClinicDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(appDir, "node_modules")); err != nil {
		t.Skip("the clinic application is not installed (pnpm install --ignore-workspace in " + appDir + ")")
	}
	cmd := exec.Command(node, "src/main.ts")
	cmd.Dir = appDir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CLINIC_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "CLINIC_PORT=0", "CLINIC_TEST_SUPPORT=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid // the process this test started: the only one it stops
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	lines := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		if sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
		_, _ = io.Copy(io.Discard, out)
	}()
	select {
	case line, ok := <-lines:
		var ev struct{ Event, URL, MCP string }
		if !ok || json.Unmarshal([]byte(line), &ev) != nil || ev.Event != "listening" || ev.MCP == "" || ev.URL == "" {
			t.Fatalf("the clinic application did not report where it listens: %q", line)
		}
		return &mcpProvisionClinic{pid: pid, baseURL: ev.URL, mcpURL: ev.MCP}
	case <-time.After(30 * time.Second):
		t.Fatal("the clinic application did not start")
	}
	return nil
}

// stats reads the application's own counters: what each tool was called how often, and
// how many rows of each kind exist.
func (c *mcpProvisionClinic) stats(t testing.TB) mcpProvisionStats {
	t.Helper()
	cl := &http.Client{Timeout: 10 * time.Second}
	resp, err := cl.Get(c.baseURL + "/test/stats")
	if err != nil {
		t.Fatalf("the application's counters: %v", err)
	}
	defer resp.Body.Close()
	var s mcpProvisionStats
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatalf("the application's counters: %v", err)
	}
	return s
}

// ---- the audit's input for the clinic package -------------------------------

// mcpProvisionLoadClinic loads the clinic package (its signature is not checked here: the
// loader's own tests do that) and returns it with its absolute directory. credential_ref is
// cleared so provision unit tests that do not provision application_credentials can build
// the adapter (production DR/e2e keep the package ref and provision the secret).
func mcpProvisionLoadClinic(t testing.TB) (*apppkg.Package, string) {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join(mcpProvisionClinicDir, "package"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := apppkg.Load(context.Background(), apppkg.FromDir(dir), apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Fatalf("the clinic package does not load: %+v", res.Findings)
	}
	for id, ep := range res.Package.Manifest.Endpoints {
		v, ok := ep.Value.(ap.ManifestMCPEndpoint)
		if !ok || v.CredentialRef == nil {
			continue
		}
		v.CredentialRef = nil
		res.Package.Manifest.Endpoints[id] = ap.ManifestEndpoint{Value: v}
	}
	return res.Package, dir
}

// mcpProvisionTestRegistry is the transport registry the adapter of a test is built through;
// nil is the process's own, the one serve and audit use.
var mcpProvisionTestRegistry *transportsRegistry

// mcpProvisionInputFor builds what the audit hands its checks for the package, with the
// MCP endpoint pointed at url: the adapter the registry builds (the wrapped MCP adapter,
// exactly as audit run holds it) and what the runner asks of it first.
func mcpProvisionInputFor(t testing.TB, pkg *apppkg.Package, dir, url string) *integrationaudit.Input {
	t.Helper()
	for id, ep := range pkg.Manifest.Endpoints {
		if v, ok := ep.Value.(ap.ManifestMCPEndpoint); ok {
			v.URL = ap.HTTPURL(url)
			pkg.Manifest.Endpoints[id] = ap.ManifestEndpoint{Value: v}
		}
	}
	dataDir := filepath.Dir(dir)
	was := mcpProvisionDataDir
	mcpProvisionDataDir = func() string { return dataDir }
	t.Cleanup(func() { mcpProvisionDataDir = was })

	rec := apppkg.Record{
		ApplicationID: string(pkg.Manifest.Application.ID), ManifestHash: "h1", State: apppkg.StateQuarantined, Path: filepath.Base(dir),
	}
	ctx := context.Background()
	ad, _, err := transportsSource{registry: mcpProvisionTestRegistry, dataDir: dataDir}.build(ctx, rec.ApplicationID, pkg, rec)
	if err != nil {
		t.Fatalf("the adapter of the package: %v", err)
	}
	t.Cleanup(func() { transportsClose(ad) })

	in := &integrationaudit.Input{
		Package: pkg, Record: rec, CoreVersion: apppkg.DevCoreVersion, Authenticity: apppkg.AuthVerified,
		Adapter: ad, Clock: clock.Real{},
		Scope: contracts.Scope{
			DeploymentID: "dep_audit", ApplicationID: rec.ApplicationID, TenantID: "alixai-audit", WorkspaceID: "alixai-audit",
			PrincipalID: "alixai-audit", Roles: []string{"auditor"}, CapabilityScopes: []string{"audit.inspect"}, SessionID: "audit-test",
		},
	}
	// what the runner asks of the adapter before any check runs
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if d, err := ad.Describe(cctx); err == nil {
		in.Description = &d
	}
	if h, err := ad.Health(cctx); err == nil {
		in.Health = &h
	}
	if i, err := ad.Inspect(cctx, in.Scope, adapters.InspectRequest{}); err == nil {
		in.Inspection = &i
	}
	return in
}

// mcpProvisionRun runs the checks of the given sections (the audit's runtime probes) with
// the options and returns their outcomes by section.
func mcpProvisionRun(t testing.TB, in *integrationaudit.Input, opts probes.Options, sections ...int) map[int]*integrationaudit.Outcome {
	t.Helper()
	if opts.ScratchDir == "" {
		opts.ScratchDir = t.TempDir()
	}
	want := map[int]bool{}
	for _, s := range sections {
		want[s] = true
	}
	out := map[int]*integrationaudit.Outcome{}
	for _, c := range probes.Checks(opts) {
		if !want[c.Section()] {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		o, err := c.Run(ctx, in)
		cancel()
		if err != nil {
			t.Fatalf("section %d (%s) did not run: %v", c.Section(), c.ID(), err)
		}
		out[c.Section()] = o
	}
	return out
}

// mcpProvisionDescribe prints an outcome the way a failing test needs to read it.
func mcpProvisionDescribe(o *integrationaudit.Outcome) string {
	var b strings.Builder
	for _, v := range o.Verdicts {
		fmt.Fprintf(&b, "  verdict %s %s: %s\n", v.CapabilityID, v.Status, v.Reason)
	}
	for _, u := range o.Unverified {
		fmt.Fprintf(&b, "  unverified %s [%s]: %s\n", u.Subject, u.Capability, u.Reason)
	}
	for _, f := range o.Findings {
		fmt.Fprintf(&b, "  finding %s %s [%s]: %s\n", f.Severity, f.CheckID, f.Capability, f.Message)
	}
	covered := append([]string(nil), o.Covered...)
	sort.Strings(covered)
	fmt.Fprintf(&b, "  covered: %v\n", covered)
	return b.String()
}

// ---- a provisioner for a test, and the tenants of the clinic suite -----------

const mcpProvisionSuffix = "0123456789"

func mcpProvisionCtx(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func mcpProvisionMustSource(t testing.TB, in *integrationaudit.Input) probes.Provisioner {
	t.Helper()
	p, err := mcpProvisionSource(mcpProvisionCtx(t), in)
	if err != nil {
		t.Fatalf("the provisioning source: %v", err)
	}
	if p == nil {
		t.Fatal("the provisioning source offers no provisioner")
	}
	return p
}

// mcpProvisionTenantOf is a tenant of the clinic suite, with the identifiers the audit
// would generate (probes: tnt_<ref>_<suffix>, ws_<ref>_<suffix>, usr_<ref>_<suffix>,
// "-" as "_"), for the principals of the first site.
func mcpProvisionTenantOf(suffix string) probes.Tenant {
	ws := "ws_site_a1_" + suffix
	p := func(ref, role string) probes.Principal {
		return probes.Principal{Ref: strings.ReplaceAll(ref, "_", "-"), ID: "usr_" + ref + "_" + suffix, WorkspaceID: ws, Roles: []string{role}}
	}
	return probes.Tenant{
		ID:         "tnt_group_a_" + suffix,
		Workspaces: []string{ws, "ws_site_a2_" + suffix},
		Principals: []probes.Principal{
			p("reception_a1", "receptionist"), p("doctor_a1", "doctor"), p("patient_a1", "patient"), p("patient_a1b", "patient"),
		},
	}
}

func mcpProvisionSeedOf(suffix string) probes.Seed {
	ws := "ws_site_a1_" + suffix
	return probes.Seed{
		TenantID: "tnt_group_a_" + suffix, DataFile: "tests/seed/clinic.json",
		Records: []probes.Record{
			{Ref: "doctor-rec-a1", EntityType: "doctor", WorkspaceID: ws},
			{Ref: "patient-rec-a1", EntityType: "patient", WorkspaceID: ws},
			{Ref: "slot-a1", EntityType: "slot", WorkspaceID: ws},
			{Ref: "appt-a1", EntityType: "appointment", WorkspaceID: ws},
		},
	}
}

// ---- 1. the real clinic application ------------------------------------------

// provisioningText is every sentence of an outcome that says why something was not probed.
func mcpProvisionReasons(o *integrationaudit.Outcome) []string {
	var out []string
	for _, v := range o.Verdicts {
		out = append(out, v.Reason)
	}
	for _, u := range o.Unverified {
		out = append(out, u.Reason)
	}
	for _, f := range o.Findings {
		out = append(out, f.Message)
	}
	return out
}

func mcpProvisionHas(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// The audit's runtime probes (sections 6, 8 and 10) run against the real clinic
// application through the provisioner this file registers: the disposable tenants of the
// package's suite are created, seeded and removed through its test_* tools, the probes run
// through the capability tools, and no capability is recorded as not probed for want of a
// disposable tenant. The checks use the default source, so this also proves the init
// registration.
func TestMCPProvisionerRunsFixturesAgainstTheClinicApp(t *testing.T) {
	clinic := mcpProvisionStartClinic(t)
	pkg, dir := mcpProvisionLoadClinic(t)
	in := mcpProvisionInputFor(t, pkg, dir, clinic.mcpURL)
	before := clinic.stats(t)

	outs := mcpProvisionRun(t, in, probes.Options{}, 6, 8, 10)

	after := clinic.stats(t)
	for _, tool := range []string{"test_create_tenant", "test_seed", "test_cleanup"} {
		if after.Calls[tool] == 0 {
			t.Errorf("the application was never asked to %s: %v", tool, after.Calls)
		}
	}
	if after.Calls["test_cleanup"] < after.Calls["test_create_tenant"] {
		t.Errorf("%d tenants created and %d cleanups: a tenant was left behind", after.Calls["test_create_tenant"], after.Calls["test_cleanup"])
	}
	if fmt.Sprint(after.Totals) != fmt.Sprint(before.Totals) {
		t.Errorf("the application holds rows the audit created: before %v, after %v", before.Totals, after.Totals)
	}

	// what the audit could not probe must not be for want of a disposable tenant
	notProvisioning := []string{"provisioner", "creates a disposable tenant", "create_tenant for", "seed for fixture"}
	for s, o := range outs {
		for _, reason := range mcpProvisionReasons(o) {
			for _, bad := range notProvisioning {
				if strings.Contains(reason, bad) {
					t.Errorf("section %d still says the disposable tenant could not be reached: %q", s, reason)
				}
			}
		}
		for _, f := range o.Findings {
			if f.CheckID == "probes.tenant_left_behind" {
				t.Errorf("section %d: a disposable tenant was left behind: %s", s, f.Message)
			}
		}
		passed := 0
		for _, p := range o.Probes {
			if p.Passed {
				passed++
			}
		}
		if passed == 0 {
			t.Errorf("section %d ran no probe that passed:\n%s", s, mcpProvisionDescribe(o))
		}
	}

	// every capability the application can serve is probed in the sections that are about it
	covered := func(section int) []string { return outs[section].Covered }
	for section, want := range map[int][]string{
		integrationaudit.SectionScope:         {"appointments.book", "appointments.cancel", "availability.list", "patients.find"},
		integrationaudit.SectionSafeExecution: {"appointments.book", "appointments.cancel"},
		integrationaudit.SectionRuntimeSmoke:  {"appointments.book", "availability.list"},
	} {
		for _, id := range want {
			if !mcpProvisionHas(covered(section), id) {
				t.Errorf("section %d did not probe %s:\n%s", section, id, mcpProvisionDescribe(outs[section]))
			}
		}
	}

	// patients.update_contact is probed too: the probes fill the one-of field its rule
	// contact_change_present asks for, so the clinic no longer answers validation_failed
	for _, section := range []int{integrationaudit.SectionScope, integrationaudit.SectionSafeExecution} {
		if !mcpProvisionHas(covered(section), "patients.update_contact") {
			t.Errorf("section %d did not probe patients.update_contact:\n%s", section, mcpProvisionDescribe(outs[section]))
		}
	}
	// and the application was asked to change records: the change-record test tool
	if after.Calls["test_change_record"] == 0 {
		t.Errorf("the application was never asked to change a record: %v", after.Calls)
	}
}

// What the audit finds in the real clinic application once the probes and the application
// agree (M1-27). patients.update_contact used to be the capability that showed it: the probes
// left its optional fields out, the clinic's rule contact_change_present asks for a phone number
// or an email address, prepare answered validation_failed, and sections 6 and 8 reported two
// critical scope.allow_refused findings and a critical unprobed capability. The probes fill the
// one-of field now, the application has a change-record tool and a fault tool the audit finds by
// name, and the suite declares the code the platform's coordinator really answers when a declined
// preparation is committed. What is left unverified is what no provisioning can fix and says so.
func TestClinicAuditHasNoCriticalUnverifiedFindingsForUpdateContact(t *testing.T) {
	clinic := mcpProvisionStartClinic(t)
	pkg, dir := mcpProvisionLoadClinic(t)
	in := mcpProvisionInputFor(t, pkg, dir, clinic.mcpURL)
	before := clinic.stats(t)

	const updateContact = "patients.update_contact"
	sections := []int{
		integrationaudit.SectionDeterministic, integrationaudit.SectionScope, integrationaudit.SectionSafeExecution,
		integrationaudit.SectionDegradation, integrationaudit.SectionRuntimeSmoke,
	}
	outs := mcpProvisionRun(t, in, probes.Options{}, sections...)

	for _, section := range sections {
		o := outs[section]
		if o == nil {
			t.Fatalf("section %d did not run", section)
		}
		for _, f := range o.Findings {
			if f.Severity == contracts.SeverityCritical {
				t.Errorf("section %d has a critical finding: %s [%s]: %s\n%s", section, f.CheckID, f.Capability, f.Message, mcpProvisionDescribe(o))
			}
			if f.Capability == updateContact {
				t.Errorf("section %d has a finding for %s: %s %s: %s", section, updateContact, f.Severity, f.CheckID, f.Message)
			}
		}
		for _, u := range o.Unverified {
			if u.Capability == updateContact {
				t.Errorf("section %d leaves %s unverified (%s): %s", section, updateContact, u.Subject, u.Reason)
			}
		}
		for _, v := range o.Verdicts {
			if v.CapabilityID == updateContact && v.Status == integrationaudit.CapabilityUnverified {
				t.Errorf("section %d has an unverified verdict for %s: %s", section, updateContact, v.Reason)
			}
		}
		// every capability the model is offered is probed in the sections that are about it
		if section == integrationaudit.SectionRuntimeSmoke {
			continue
		}
		if !mcpProvisionHas(o.Covered, updateContact) {
			t.Errorf("section %d did not probe %s:\n%s", section, updateContact, mcpProvisionDescribe(o))
		}
	}

	// the application can change a record on request, so Validate noticing a change is shown for
	// the consequential capabilities, and it can be made to fail, so its typed failures are shown
	safe := outs[integrationaudit.SectionSafeExecution]
	for _, id := range []string{"appointments.book", "appointments.cancel", updateContact} {
		found := false
		for _, p := range safe.Probes {
			if p.Capability == id && p.Subject == "validate notices a changed record" {
				found = true
				if !p.Passed {
					t.Errorf("%s: Validate did not notice a changed record: %+v", id, p)
				}
			}
		}
		if !found {
			t.Errorf("%s: the audit did not change a record to see whether Validate notices it:\n%s", id, mcpProvisionDescribe(safe))
		}
	}
	for _, section := range sections {
		for _, u := range outs[section].Unverified {
			if u.Subject == "safety:validate_notices_change" || strings.HasPrefix(u.Subject, "degradation:") {
				t.Errorf("section %d: %s [%s] is unverified although the application offers the test tool for it: %s", section, u.Subject, u.Capability, u.Reason)
			}
		}
	}
	for _, f := range safe.Findings {
		if f.CheckID == "safety.error_code_differs" {
			t.Errorf("the suite and the platform disagree about a safety probe: %s", f.Message)
		}
	}

	// What stays unverified is what provisioning cannot fix: the audit events of the dialogue
	// controller are written by a layer the audit does not run, and the RAG probes are another
	// task's. Anything else showing up here is new and has to be looked at.
	known := map[string]bool{
		"audit_event:dialogue.turn (smoke-text)":   true,
		"smoke_path:smoke-rag":                     true,
		"smoke_path:smoke-rag-no-evidence":         true,
		"scope_probe:reception-retrieves-policies": true,
		"scope_probe:patient-retrieves-policies":   true,
	}
	for _, section := range sections {
		for _, u := range outs[section].Unverified {
			if !known[u.Subject] {
				t.Errorf("section %d: %s [%s] is unverified: %s", section, u.Subject, u.Capability, u.Reason)
			}
		}
	}

	if after := clinic.stats(t); fmt.Sprint(after.Totals) != fmt.Sprint(before.Totals) {
		t.Errorf("the application holds rows the audit created: before %v, after %v", before.Totals, after.Totals)
	}
	if clinic.stats(t).Calls["test_change_record"] == 0 || clinic.stats(t).Calls["test_inject_fault"] == 0 {
		t.Errorf("the application was not asked to change a record and to fail: %v", clinic.stats(t).Calls)
	}
}

// ---- 2. only the audit's own tenants -------------------------------------------

// A provisioner acts only on the tenants it asked for. A second provisioner of the same
// audit (the audit builds one for every check) is refused a tenant the first one
// created, and so is a tenant nobody created, and the application is not even asked: an
// application that does not guard its own tenants is never given the chance. A create
// the application refused (the tenant exists) is not adopted either, so the cleanup
// that follows a failed create can never remove somebody else's tenant.
func TestMCPProvisionerRefusesTenantsTheAuditDidNotCreate(t *testing.T) {
	clinic := mcpProvisionStartClinic(t)
	pkg, dir := mcpProvisionLoadClinic(t)
	in := mcpProvisionInputFor(t, pkg, dir, clinic.mcpURL)
	ctx := mcpProvisionCtx(t)
	baseline := clinic.stats(t)

	mine, other := mcpProvisionMustSource(t, in), mcpProvisionMustSource(t, in)
	tenant := mcpProvisionTenantOf("aaaaaaaaaa")
	if err := mine.CreateTenant(ctx, tenant); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if _, err := mine.Seed(ctx, mcpProvisionSeedOf("aaaaaaaaaa")); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	asked := func() (seeds, cleanups int) {
		c := clinic.stats(t).Calls
		return c["test_seed"], c["test_cleanup"]
	}
	seeds0, cleanups0 := asked()

	stranger := mcpProvisionSeedOf("aaaaaaaaaa")
	for name, call := range map[string]func() error{
		"seed of a tenant another provisioner created": func() error { _, err := other.Seed(ctx, stranger); return err },
		"cleanup of a tenant another provisioner created": func() error {
			_, err := other.Cleanup(ctx, tenant.ID)
			return err
		},
		"seed of a tenant nobody created": func() error {
			s := mcpProvisionSeedOf("bbbbbbbbbb")
			_, err := mine.Seed(ctx, s)
			return err
		},
		"cleanup of a tenant nobody created": func() error {
			_, err := mine.Cleanup(ctx, "tnt_group_a_bbbbbbbbbb")
			return err
		},
		"cleanup of the application's own demonstration tenant": func() error {
			_, err := mine.Cleanup(ctx, "tnt_demo")
			return err
		},
	} {
		err := call()
		if contracts.CodeOf(err) != contracts.ErrPermissionDenied || mcpProvisionReasonOf(err) != mcpProvisionReasonNotOurs {
			t.Errorf("%s: %v (code %q, reason %q), want permission_denied", name, err, contracts.CodeOf(err), mcpProvisionReasonOf(err))
		}
	}
	if seeds, cleanups := asked(); seeds != seeds0 || cleanups != cleanups0 {
		t.Errorf("a refused call reached the application: test_seed %d -> %d, test_cleanup %d -> %d", seeds0, seeds, cleanups0, cleanups)
	}

	// a tenant the other provisioner owns, which this one asks to create: the application
	// refuses (it exists), and that tenant stays the other's
	foreign := mcpProvisionTenantOf("cccccccccc")
	if err := other.CreateTenant(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if err := mine.CreateTenant(ctx, foreign); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Fatalf("a second create of an existing tenant: %v, want conflict", err)
	}
	_, cleanups1 := asked()
	if _, err := mine.Cleanup(ctx, foreign.ID); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("cleanup of a tenant whose creation the application refused: %v, want permission_denied", err)
	}
	if _, cleanups := asked(); cleanups != cleanups1 {
		t.Error("the cleanup of a tenant this provisioner did not create reached the application")
	}
	rep, err := other.Cleanup(ctx, foreign.ID)
	if err != nil || !rep.Existed || rep.Left() != 0 {
		t.Errorf("the owner's cleanup of the tenant = %+v, %v: it must still exist and be removed", rep, err)
	}

	rep, err = mine.Cleanup(ctx, tenant.ID)
	if err != nil || !rep.Existed || rep.Left() != 0 {
		t.Errorf("the owner's cleanup = %+v, %v", rep, err)
	}
	if after := clinic.stats(t); fmt.Sprint(after.Totals) != fmt.Sprint(baseline.Totals) {
		t.Errorf("rows are left: before %v, after %v", baseline.Totals, after.Totals)
	}
}

// ---- 3. every method can be called again ---------------------------------------

func TestMCPProvisionerCleanupIsIdempotent(t *testing.T) {
	clinic := mcpProvisionStartClinic(t)
	pkg, dir := mcpProvisionLoadClinic(t)
	in := mcpProvisionInputFor(t, pkg, dir, clinic.mcpURL)
	ctx := mcpProvisionCtx(t)
	baseline := clinic.stats(t)

	p := mcpProvisionMustSource(t, in)
	tenant := mcpProvisionTenantOf(mcpProvisionSuffix)
	if err := p.CreateTenant(ctx, tenant); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	// a second create of a tenant that exists is a typed conflict and harms nothing
	if err := p.CreateTenant(ctx, tenant); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Errorf("a second CreateTenant: %v, want conflict", err)
	}
	seed, err := p.Seed(ctx, mcpProvisionSeedOf(mcpProvisionSuffix))
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	for _, r := range mcpProvisionSeedOf(mcpProvisionSuffix).Records {
		if seed.Entities[r.Ref] == "" {
			t.Errorf("no identifier for %s: %v", r.Ref, seed.Entities)
		}
	}
	if clinic.stats(t).Totals["tenants"] != 1 {
		t.Fatalf("the application does not hold the tenant: %v", clinic.stats(t).Totals)
	}

	first, err := p.Cleanup(ctx, tenant.ID)
	if err != nil || !first.Existed || first.Left() != 0 {
		t.Fatalf("the first cleanup = %+v, %v: want Existed true and nothing left", first, err)
	}
	for i := 2; i <= 3; i++ {
		again, err := p.Cleanup(ctx, tenant.ID)
		if err != nil || again.Existed || again.Left() != 0 {
			t.Errorf("cleanup number %d = %+v, %v: want Existed false and nothing left", i, again, err)
		}
	}
	if after := clinic.stats(t); fmt.Sprint(after.Totals) != fmt.Sprint(baseline.Totals) {
		t.Errorf("rows are left: before %v, after %v", baseline.Totals, after.Totals)
	}
	// nothing is seeded into a tenant that is gone, and nothing is sent to try
	seeds := clinic.stats(t).Calls["test_seed"]
	if _, err := p.Seed(ctx, mcpProvisionSeedOf(mcpProvisionSuffix)); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("Seed after Cleanup: %v, want permission_denied", err)
	}
	if clinic.stats(t).Calls["test_seed"] != seeds {
		t.Error("a seed of a removed tenant reached the application")
	}
	// the same identifiers can be created again once the tenant is gone
	if err := p.CreateTenant(ctx, tenant); err != nil {
		t.Errorf("CreateTenant after Cleanup: %v", err)
	}
	if rep, err := p.Cleanup(ctx, tenant.ID); err != nil || !rep.Existed || rep.Left() != 0 {
		t.Errorf("the last cleanup = %+v, %v", rep, err)
	}
}

// ---- 4. a server without provisioning tools --------------------------------------

// mcpProvisionFake is the application behind the provisioning tools of a fixture server:
// it keeps tenants in memory and says what it was asked.
type mcpProvisionFake struct {
	mu      sync.Mutex
	tenants map[string]bool
	creates []map[string]any
	seeds   []map[string]any
	changes []map[string]any
	faults  []map[string]any
	// loseCreateReply applies the next create and then answers with a tool error.
	loseCreateReply bool
}

const mcpProvisionCanary = "CANARY-TEXT-THE-SERVER-CHOSE"

func mcpProvisionArgs(raw json.RawMessage) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

func mcpProvisionResult(body map[string]any) mcpfixture.Result {
	b, _ := json.Marshal(body)
	return mcpfixture.Result{Structured: body, Text: string(b)}
}

func mcpProvisionRejected(code string) mcpfixture.Result {
	return mcpProvisionResult(map[string]any{"status": "rejected", "errors": []any{map[string]any{"code": code, "message": mcpProvisionCanary}}})
}

func (f *mcpProvisionFake) create(_ context.Context, raw json.RawMessage) mcpfixture.Result {
	args := mcpProvisionArgs(raw)
	id, _ := args["tenant_id"].(string)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, args)
	if f.tenants[id] {
		return mcpProvisionRejected("conflict")
	}
	f.tenants[id] = true
	if f.loseCreateReply {
		f.loseCreateReply = false
		return mcpfixture.Result{Text: mcpProvisionCanary, IsError: true}
	}
	return mcpProvisionResult(map[string]any{"status": "ok", "tenant_id": id, "workspaces": args["workspaces"], "principals": args["principals"]})
}

func (f *mcpProvisionFake) seed(_ context.Context, raw json.RawMessage) mcpfixture.Result {
	args := mcpProvisionArgs(raw)
	id, _ := args["tenant_id"].(string)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seeds = append(f.seeds, args)
	if !f.tenants[id] {
		return mcpProvisionRejected("scope_violation")
	}
	var out []any
	recs, _ := args["records"].([]any)
	for _, r := range recs {
		rec, _ := r.(map[string]any)
		ref, _ := rec["ref"].(string)
		out = append(out, map[string]any{"ref": ref, "entity_type": rec["entity_type"], "entity_id": "ent_" + ref, "workspace_id": rec["workspace_id"]})
	}
	return mcpProvisionResult(map[string]any{"status": "ok", "tenant_id": id, "records": out})
}

func (f *mcpProvisionFake) cleanup(_ context.Context, raw json.RawMessage) mcpfixture.Result {
	args := mcpProvisionArgs(raw)
	id, _ := args["tenant_id"].(string)
	f.mu.Lock()
	defer f.mu.Unlock()
	existed := f.tenants[id]
	delete(f.tenants, id)
	return mcpProvisionResult(map[string]any{
		"status": "ok", "tenant_id": id, "existed": existed,
		"removed": map[string]any{"tenants": 0}, "remaining": map[string]any{"tenants": 0, "patients": 0},
	})
}

func (f *mcpProvisionFake) change(_ context.Context, raw json.RawMessage) mcpfixture.Result {
	args := mcpProvisionArgs(raw)
	id, _ := args["tenant_id"].(string)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changes = append(f.changes, args)
	if !f.tenants[id] {
		return mcpProvisionRejected("scope_violation")
	}
	return mcpProvisionResult(map[string]any{
		"status": "ok", "tenant_id": id, "entity_type": args["entity_type"], "entity_id": args["entity_id"], "changed": true,
	})
}

func (f *mcpProvisionFake) fault(_ context.Context, raw json.RawMessage) mcpfixture.Result {
	args := mcpProvisionArgs(raw)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = append(f.faults, args)
	return mcpProvisionResult(map[string]any{"status": "ok", "armed": true})
}

// mcpProvisionWorld is the clinic package audited against a fixture MCP server that offers
// the tools of the package, and, when asked, the provisioning tools behind a fake application.
type mcpProvisionWorld struct {
	pkg *apppkg.Package
	dir string
	in  *integrationaudit.Input
	srv *mcpfixture.HTTPServer
	app *mcpProvisionFake
}

// mcpProvisionFixture serves every tool the clinic package maps and approves, each
// answering with a well-formed result; the test_* tools only when provisioning is true.
// dirOf, when set, names the install directory the transport reads the adapter
// configuration and the seed data from (a copy of the package).
func mcpProvisionFixture(t *testing.T, provisioning bool, dirOf func(dir string) string, edit ...func(pkg *apppkg.Package)) *mcpProvisionWorld {
	t.Helper()
	pkg, dir := mcpProvisionLoadClinic(t)
	raw, err := os.ReadFile(filepath.Join(dir, "schemas", "adapters", "clinic-mcp.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	ac, err := mcptransport.ParseAdapterConfig(raw, "clinic-mcp")
	if err != nil {
		t.Fatal(err)
	}
	srv := mcpfixture.New(fake.NewApp())
	for _, n := range srv.Names() {
		if n != mcpfixture.ToolDescribe && n != mcpfixture.ToolHealth && n != mcpfixture.ToolInspect {
			srv.Remove(n)
		}
	}
	app := &mcpProvisionFake{tenants: map[string]bool{}}
	generic := func(context.Context, json.RawMessage) mcpfixture.Result {
		return mcpfixture.FromBody([]byte(`{"status":"ok","identifiers":[],"values":[],"errors":[],"allowed_next_actions":[],"freshness":{"as_of":"2026-10-09T08:00:00Z","version":"v1"}}`), "")
	}
	provisioningTools := map[string]mcpfixture.Handler{
		"test_create_tenant": app.create, "test_seed": app.seed, "test_cleanup": app.cleanup,
		mcpProvisionChangeTool: app.change, mcpProvisionFaultTool: app.fault,
	}
	for _, n := range ac.Tools.Allow {
		tool := mcpfixture.Tool{
			Name:   n,
			Input:  map[string]any{"type": "object", "properties": map[string]any{"identity": map[string]any{"type": "object"}}},
			Output: map[string]any{"type": "object", "properties": map[string]any{"status": map[string]any{"type": "string"}}},
		}
		if h, ok := provisioningTools[n]; ok {
			if provisioning {
				tool.Input = map[string]any{"type": "object", "properties": map[string]any{"tenant_id": map[string]any{"type": "string"}}}
				tool.Output = nil
				srv.Add(tool, h)
			}
			continue
		}
		srv.Add(tool, generic)
	}
	hs := mcpfixture.StartHTTP(t, srv)
	for _, e := range edit {
		e(pkg)
	}
	use := dir
	if dirOf != nil {
		use = dirOf(dir)
	}
	return &mcpProvisionWorld{pkg: pkg, dir: use, in: mcpProvisionInputFor(t, pkg, use, hs.URL), srv: hs, app: app}
}

// mcpProvisionCopyPackage copies the clinic package to <temp>/package and lets edit change
// the adapter configuration of its endpoint. Only the transport reads the copy; the
// package the audit loaded is the untouched original.
func mcpProvisionCopyPackage(t *testing.T, dir string, edit func(adapterConfig string) string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "package")
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if filepath.ToSlash(rel) == "schemas/adapters/clinic-mcp.yaml" {
			b = []byte(edit(string(b)))
		}
		return os.WriteFile(target, b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// A server that does not offer the provisioning tools (the application is not in test
// mode) leaves the probes exactly as they were before this bridge: no provisioner, every
// capability not probed, critical where it changes data and a warning where it only
// reads, and nothing is sent to the application to try.
func TestNoProvisioningToolsLeavesChecksUnverified(t *testing.T) {
	w := mcpProvisionFixture(t, false, nil)
	ctx := mcpProvisionCtx(t)
	if p, err := mcpProvisionSource(ctx, w.in); p != nil || err != nil {
		t.Fatalf("a server without provisioning tools gave a provisioner: %v, %v", p, err)
	}
	callsBefore := w.srv.ToolCalls("")

	sections := []int{integrationaudit.SectionScope, integrationaudit.SectionSafeExecution, integrationaudit.SectionRuntimeSmoke}
	got := mcpProvisionRun(t, w.in, probes.Options{}, sections...)
	none := func(context.Context, *integrationaudit.Input) (probes.Provisioner, error) { return nil, nil }
	was := mcpProvisionRun(t, w.in, probes.Options{Source: none}, sections...)
	if n := w.srv.ToolCalls(""); n != callsBefore {
		t.Errorf("the probes called %d tools of an application they cannot give a disposable tenant", n-callsBefore)
	}

	wantCritical := map[int][]string{
		integrationaudit.SectionScope:         {"appointments.book", "appointments.cancel", "patients.update_contact"},
		integrationaudit.SectionSafeExecution: {"appointments.book", "appointments.cancel", "patients.update_contact"},
		integrationaudit.SectionRuntimeSmoke:  {"appointments.book"},
	}
	wantWarning := map[int][]string{
		integrationaudit.SectionScope:        {"availability.list", "patients.find"},
		integrationaudit.SectionRuntimeSmoke: {"availability.list"},
	}
	for _, s := range sections {
		if diff := cmp.Diff(mcpProvisionDescribe(was[s]), mcpProvisionDescribe(got[s])); diff != "" {
			t.Errorf("section %d differs from the audit of an application with no provisioner (-before +now):\n%s", s, diff)
		}
		o := got[s]
		if len(o.Covered) != 0 {
			t.Errorf("section %d covered %v without a disposable tenant", s, o.Covered)
		}
		unverified := map[string]bool{}
		for _, v := range o.Verdicts {
			if v.Status == integrationaudit.CapabilityUnverified {
				unverified[v.CapabilityID] = true
			}
		}
		severity := func(id string) (check string, sev contracts.Severity) {
			for _, f := range o.Findings {
				if f.Capability == id && strings.HasPrefix(f.CheckID, "probes.capability_not_probed") {
					return f.CheckID, f.Severity
				}
			}
			return "", ""
		}
		for _, id := range wantCritical[s] {
			if check, sev := severity(id); check != "probes.capability_not_probed" || sev != contracts.SeverityCritical || !unverified[id] {
				t.Errorf("section %d, %s: finding %q severity %q, unverified %v: want critical and unverified", s, id, check, sev, unverified[id])
			}
		}
		for _, id := range wantWarning[s] {
			if check, sev := severity(id); check != "probes.capability_not_probed_minor" || sev != contracts.SeverityWarning || !unverified[id] {
				t.Errorf("section %d, %s: finding %q severity %q, unverified %v: want a warning and unverified", s, id, check, sev, unverified[id])
			}
		}
		if reasons := strings.Join(mcpProvisionReasons(o), "\n"); !strings.Contains(reasons, "offers no operation that creates a disposable tenant") {
			t.Errorf("section %d does not say why:\n%s", s, reasons)
		}
	}
}

// Tools the server offers and the package does not approve (tools.allow) are not
// provisioning tools: the source fails with a typed error, the probes record the
// capabilities as not probed, and nothing is called.
func TestMCPProvisionerRefusesProvisioningToolsThePackageDoesNotApprove(t *testing.T) {
	w := mcpProvisionFixture(t, true, func(dir string) string {
		return mcpProvisionCopyPackage(t, dir, func(cfg string) string {
			var keep []string
			for _, line := range strings.Split(cfg, "\n") {
				if !strings.Contains(line, "test_") {
					keep = append(keep, line)
				}
			}
			return strings.Join(keep, "\n")
		})
	})
	p, err := mcpProvisionSource(mcpProvisionCtx(t), w.in)
	if p != nil || contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("an unapproved provisioning tool: %v, %v, want a validation_failed error", p, err)
	}
	outs := mcpProvisionRun(t, w.in, probes.Options{}, integrationaudit.SectionSafeExecution)
	if o := outs[integrationaudit.SectionSafeExecution]; len(o.Covered) != 0 {
		t.Errorf("probed %v without an approved provisioning tool:\n%s", o.Covered, mcpProvisionDescribe(o))
	}
	for _, tool := range []string{"test_create_tenant", "test_seed", "test_cleanup"} {
		if n := w.srv.ToolCalls(tool); n != 0 {
			t.Errorf("%s was called %d times although the package does not approve it", tool, n)
		}
	}
}

// ---- 5. what the bridge sends, and what it will not trust or send ------------------

// The bridge sends the audit's own identifiers and the seed data file's members, and nothing
// the audit owns can be set by the file: a record's workspace and principal are the
// generated identifiers, and the logical references of the file never reach the application.
func TestMCPProvisionerSendsTheSeedDataWithTheAuditsIdentifiers(t *testing.T) {
	w := mcpProvisionFixture(t, true, nil)
	ctx := mcpProvisionCtx(t)
	p := mcpProvisionMustSource(t, w.in)

	tenant := mcpProvisionTenantOf(mcpProvisionSuffix)
	tenant.Principals[1].Service = true
	if err := p.CreateTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if len(w.app.creates) != 1 {
		t.Fatalf("%d creates", len(w.app.creates))
	}
	wantCreate := map[string]any{
		"tenant_id": tenant.ID,
		"workspaces": []any{
			map[string]any{"workspace_id": "ws_site_a1_" + mcpProvisionSuffix},
			map[string]any{"workspace_id": "ws_site_a2_" + mcpProvisionSuffix},
		},
		"principals": []any{
			map[string]any{"principal_id": "usr_reception_a1_" + mcpProvisionSuffix, "workspace_id": "ws_site_a1_" + mcpProvisionSuffix, "roles": []any{"receptionist"}},
			map[string]any{"principal_id": "usr_doctor_a1_" + mcpProvisionSuffix, "workspace_id": "ws_site_a1_" + mcpProvisionSuffix, "roles": []any{"doctor"}, "service": true},
			map[string]any{"principal_id": "usr_patient_a1_" + mcpProvisionSuffix, "workspace_id": "ws_site_a1_" + mcpProvisionSuffix, "roles": []any{"patient"}},
			map[string]any{"principal_id": "usr_patient_a1b_" + mcpProvisionSuffix, "workspace_id": "ws_site_a1_" + mcpProvisionSuffix, "roles": []any{"patient"}},
		},
	}
	if diff := cmp.Diff(wantCreate, w.app.creates[0]); diff != "" {
		t.Errorf("create_tenant arguments (-want +got):\n%s", diff)
	}

	seed := mcpProvisionSeedOf(mcpProvisionSuffix)
	res, err := p.Seed(ctx, seed)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"doctor-rec-a1": "ent_doctor-rec-a1", "patient-rec-a1": "ent_patient-rec-a1", "slot-a1": "ent_slot-a1", "appt-a1": "ent_appt-a1"}
	if diff := cmp.Diff(want, res.Entities); diff != "" {
		t.Errorf("entities (-want +got):\n%s", diff)
	}
	ws := "ws_site_a1_" + mcpProvisionSuffix
	wantRecords := map[string]map[string]any{
		"doctor-rec-a1":  {"ref": "doctor-rec-a1", "entity_type": "doctor", "workspace_id": ws, "principal_id": "usr_doctor_a1_" + mcpProvisionSuffix, "name": "Dr. Anna Verdi"},
		"patient-rec-a1": {"ref": "patient-rec-a1", "entity_type": "patient", "workspace_id": ws, "principal_id": "usr_patient_a1_" + mcpProvisionSuffix, "given_name": "Maria", "family_name": "Rossi"},
		"slot-a1":        {"ref": "slot-a1", "entity_type": "slot", "workspace_id": ws, "principal_id": "usr_doctor_a1_" + mcpProvisionSuffix},
		"appt-a1":        {"ref": "appt-a1", "entity_type": "appointment", "workspace_id": ws, "principal_id": "usr_patient_a1_" + mcpProvisionSuffix, "reference": "APT-7K3Q9X"},
	}
	if len(w.app.seeds) != 1 || w.app.seeds[0]["tenant_id"] != tenant.ID {
		t.Fatalf("seed calls: %v", w.app.seeds)
	}
	sent, _ := w.app.seeds[0]["records"].([]any)
	if len(sent) != len(wantRecords) {
		t.Fatalf("%d records sent, want %d", len(sent), len(wantRecords))
	}
	for _, r := range sent {
		rec, _ := r.(map[string]any)
		ref, _ := rec["ref"].(string)
		if diff := cmp.Diff(wantRecords[ref], rec); diff != "" {
			t.Errorf("record %s (-want +got):\n%s", ref, diff)
		}
	}
}

// Whatever cannot be sent safely fails before anything is sent.
func TestMCPProvisionerFailsASeedClosedBeforeSendingAnything(t *testing.T) {
	w := mcpProvisionFixture(t, true, nil)
	ctx := mcpProvisionCtx(t)
	p := mcpProvisionMustSource(t, w.in)
	tenant := mcpProvisionTenantOf(mcpProvisionSuffix)
	if err := p.CreateTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	// a tenant created without the principals the seed data file names
	bare := mcpProvisionTenantOf("dddddddddd")
	bare.Principals = bare.Principals[:1]
	if err := p.CreateTenant(ctx, bare); err != nil {
		t.Fatal(err)
	}

	withFile := func(rel string) probes.Seed {
		s := mcpProvisionSeedOf(mcpProvisionSuffix)
		s.DataFile = rel
		return s
	}
	record := func(mod func(*probes.Seed)) probes.Seed {
		s := mcpProvisionSeedOf(mcpProvisionSuffix)
		mod(&s)
		return s
	}
	for name, seed := range map[string]probes.Seed{
		"a principal the tenant was not created with":     mcpProvisionSeedOf("dddddddddd"),
		"a path that climbs out of the package":           withFile("tests/../../package.json"),
		"a path outside tests/":                           withFile("package.json"),
		"an absolute path":                                withFile(filepath.ToSlash(filepath.Join(w.dir, "tests", "seed", "clinic.json"))),
		"a path that is not clean":                        withFile("tests//seed/clinic.json"),
		"a file that is not there":                        withFile("tests/seed/none.json"),
		"a directory":                                     withFile("tests/seed"),
		"a workspace of another tenant":                   record(func(s *probes.Seed) { s.Records[0].WorkspaceID = "ws_site_a1_ffffffffff" }),
		"a record listed twice":                           record(func(s *probes.Seed) { s.Records = append(s.Records, s.Records[0]) }),
		"a record with no entity type":                    record(func(s *probes.Seed) { s.Records[0].EntityType = "" }),
		"an entity type the seed data file does not give": record(func(s *probes.Seed) { s.Records[0].EntityType = "slot" }),
		"no records":                                      record(func(s *probes.Seed) { s.Records = nil }),
		"a reference that is not an identifier":           record(func(s *probes.Seed) { s.Records[0].Ref = "a b" }),
	} {
		before := w.srv.ToolCalls("test_seed")
		_, err := p.Seed(ctx, seed)
		if contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("%s: %v, want validation_failed", name, err)
		}
		if w.srv.ToolCalls("test_seed") != before {
			t.Errorf("%s: the seed reached the application", name)
		}
	}
	// the same refusals for a tenant that is badly formed
	for name, tn := range map[string]probes.Tenant{
		"an identifier with a space":       {ID: "tnt a", Workspaces: []string{"ws_x"}},
		"no workspace":                     {ID: "tnt_x"},
		"a repeated workspace":             {ID: "tnt_x", Workspaces: []string{"ws_x", "ws_x"}},
		"a principal in no workspace":      {ID: "tnt_x", Workspaces: []string{"ws_x"}, Principals: []probes.Principal{{ID: "usr_x", WorkspaceID: "ws_y", Roles: []string{"r"}}}},
		"a principal repeated":             {ID: "tnt_x", Workspaces: []string{"ws_x"}, Principals: []probes.Principal{{ID: "usr_x", WorkspaceID: "ws_x"}, {ID: "usr_x", WorkspaceID: "ws_x"}}},
		"a role that is not an identifier": {ID: "tnt_x", Workspaces: []string{"ws_x"}, Principals: []probes.Principal{{ID: "usr_x", WorkspaceID: "ws_x", Roles: []string{"a b"}}}},
	} {
		before := w.srv.ToolCalls("test_create_tenant")
		if err := p.CreateTenant(ctx, tn); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("%s: %v, want validation_failed", name, err)
		}
		if w.srv.ToolCalls("test_create_tenant") != before {
			t.Errorf("%s: the create reached the application", name)
		}
	}
}

// A create whose reply was lost may have been applied: the tenant is the audit's to clean
// up, and not yet its to seed. A create the application refused is nobody's but the
// application's.
func TestMCPProvisionerOwnsATenantWhoseCreateReplyWasLost(t *testing.T) {
	w := mcpProvisionFixture(t, true, nil)
	ctx := mcpProvisionCtx(t)
	p := mcpProvisionMustSource(t, w.in)
	tenant := mcpProvisionTenantOf(mcpProvisionSuffix)

	w.app.loseCreateReply = true
	err := p.CreateTenant(ctx, tenant)
	if contracts.CodeOf(err) != contracts.ErrUnavailable || mcpProvisionReasonOf(err) != mcpProvisionReasonToolError {
		t.Fatalf("a create whose reply was a tool error: %v, want unavailable (tool_error)", err)
	}
	if strings.Contains(err.Error(), mcpProvisionCanary) {
		t.Errorf("text the server chose reached an error: %v", err)
	}
	if _, err := p.Seed(ctx, mcpProvisionSeedOf(mcpProvisionSuffix)); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("Seed of a tenant whose creation is not confirmed: %v, want permission_denied", err)
	}
	rep, err := p.Cleanup(ctx, tenant.ID)
	if err != nil || !rep.Existed || rep.Left() != 0 {
		t.Errorf("Cleanup of the tenant whose reply was lost = %+v, %v: want it found and removed", rep, err)
	}
	if w.app.tenants[tenant.ID] {
		t.Error("the tenant is still there")
	}
}

func TestMCPProvisionerDoesNotAdoptATenantTheApplicationRefusedToCreate(t *testing.T) {
	w := mcpProvisionFixture(t, true, nil)
	ctx := mcpProvisionCtx(t)
	p := mcpProvisionMustSource(t, w.in)
	tenant := mcpProvisionTenantOf(mcpProvisionSuffix)
	w.app.tenants[tenant.ID] = true // somebody else's, with the same identifier

	err := p.CreateTenant(ctx, tenant)
	if contracts.CodeOf(err) != contracts.ErrConflict || strings.Contains(err.Error(), mcpProvisionCanary) {
		t.Fatalf("CreateTenant of a tenant that exists: %v, want a conflict that carries no server text", err)
	}
	if _, err := p.Cleanup(ctx, tenant.ID); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("Cleanup of a tenant the application refused to create: %v, want permission_denied", err)
	}
	if w.srv.ToolCalls("test_cleanup") != 0 || !w.app.tenants[tenant.ID] {
		t.Error("somebody else's tenant was touched")
	}
}

// What the application says is held to the shape of the contract, and a word of it never
// reaches an error.
func TestMCPProvisionerDoesNotTrustWhatTheApplicationSays(t *testing.T) {
	w := mcpProvisionFixture(t, true, nil)
	ctx := mcpProvisionCtx(t)
	p := mcpProvisionMustSource(t, w.in)
	tenant := mcpProvisionTenantOf(mcpProvisionSuffix)
	if err := p.CreateTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	other := "tnt_group_a_eeeeeeeeee"
	huge := strings.Repeat("x", 2<<20)

	cleanup := func(body mcpfixture.Result) error {
		w.srv.Handle("test_cleanup", func(context.Context, json.RawMessage) mcpfixture.Result { return body })
		_, err := p.Cleanup(ctx, tenant.ID)
		return err
	}
	ok := func(extra map[string]any) mcpfixture.Result {
		body := map[string]any{"status": "ok", "tenant_id": tenant.ID, "existed": true, "remaining": map[string]any{"patients": 0}}
		for k, v := range extra {
			if v == nil {
				delete(body, k)
				continue
			}
			body[k] = v
		}
		return mcpProvisionResult(body)
	}
	for name, c := range map[string]struct {
		body   mcpfixture.Result
		code   contracts.ErrCode
		reason string
	}{
		"text only":                             {mcpfixture.Result{Text: `{"status":"ok","tenant_id":"` + tenant.ID + `","existed":true,"remaining":{}}`}, contracts.ErrSchemaInvalid, mcpProvisionReasonTextOnly},
		"a tool error":                          {mcpfixture.Result{Text: mcpProvisionCanary, IsError: true}, contracts.ErrUnavailable, mcpProvisionReasonToolError},
		"a reply for another tenant":            {ok(map[string]any{"tenant_id": other}), contracts.ErrSchemaInvalid, ""},
		"no statement of what existed":          {ok(map[string]any{"existed": nil}), contracts.ErrSchemaInvalid, ""},
		"no statement of what remains":          {ok(map[string]any{"remaining": nil}), contracts.ErrSchemaInvalid, ""},
		"a negative count":                      {ok(map[string]any{"remaining": map[string]any{"patients": -1}}), contracts.ErrSchemaInvalid, ""},
		"a status that is neither ok nor not":   {ok(map[string]any{"status": "queued"}), contracts.ErrSchemaInvalid, ""},
		"a refusal with a code of the platform": {mcpProvisionRejected("scope_violation"), contracts.ErrScopeViolation, mcpProvisionReasonRejected},
		"a refusal with a code of its own":      {mcpProvisionRejected("no-such-code"), contracts.ErrValidationFailed, mcpProvisionReasonRejected},
		"a reply over the bound":                {ok(map[string]any{"note": huge}), contracts.ErrSchemaInvalid, mcpProvisionReasonTooLarge},
		"a reply far over the bound":            {ok(map[string]any{"note": strings.Repeat("y", 6<<20)}), contracts.ErrSchemaInvalid, mcpProvisionReasonTooLarge},
	} {
		err := cleanup(c.body)
		if contracts.CodeOf(err) != c.code || (c.reason != "" && mcpProvisionReasonOf(err) != c.reason) {
			t.Errorf("%s: %v (code %q, reason %q), want %s %s", name, err, contracts.CodeOf(err), mcpProvisionReasonOf(err), c.code, c.reason)
		}
		if err != nil && strings.Contains(err.Error(), mcpProvisionCanary) {
			t.Errorf("%s: text the server chose reached an error: %v", name, err)
		}
	}

	// a seed that answers with a record nobody asked for, or one without an identifier
	for name, records := range map[string][]any{
		"a record nobody asked for": {map[string]any{"ref": "unasked", "entity_id": "x"}},
		"no identifier":             {map[string]any{"ref": "slot-a1", "entity_id": ""}},
	} {
		w.srv.Handle("test_seed", func(context.Context, json.RawMessage) mcpfixture.Result {
			return mcpProvisionResult(map[string]any{"status": "ok", "tenant_id": tenant.ID, "records": records})
		})
		if _, err := p.Seed(ctx, mcpProvisionSeedOf(mcpProvisionSuffix)); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
			t.Errorf("a seed reply with %s: %v, want schema_invalid", name, err)
		}
	}
}

// ---- 6. the adapter's own checks ------------------------------------------------------

// mcpProvisionChange rewrites the input schema of a tool of the fixture server: a rug pull.
func mcpProvisionChange(w *mcpProvisionWorld, tool string) {
	w.srv.Redefine(tool, func(tl *mcpfixture.Tool) {
		tl.Input = map[string]any{"type": "object", "properties": map[string]any{"changed": map[string]any{"type": "string"}}}
	})
}

// A tool that changed after it was approved is refused as the adapter refuses it
// (quarantined, tool_schema_drift), and nothing is sent: a provisioning tool against the
// hash this client first saw, a mapped tool against the pin of the audit's adapter.
func TestMCPProvisionerRefusesAChangedTool(t *testing.T) {
	for name, tool := range map[string]string{
		"a provisioning tool": "test_cleanup",
		"a mapped tool":       "patients_find",
	} {
		t.Run(name, func(t *testing.T) {
			w := mcpProvisionFixture(t, true, nil)
			ctx := mcpProvisionCtx(t)
			p := mcpProvisionMustSource(t, w.in)
			tenant := mcpProvisionTenantOf(mcpProvisionSuffix)
			if err := p.CreateTenant(ctx, tenant); err != nil {
				t.Fatal(err)
			}
			mcpProvisionChange(w, tool)
			_, err := p.Cleanup(ctx, tenant.ID)
			if contracts.CodeOf(err) != contracts.ErrQuarantined || mcpProvisionReasonOf(err) != mcpProvisionReasonDrift {
				t.Fatalf("Cleanup after %s changed: %v, want quarantined (%s)", name, err, mcpProvisionReasonDrift)
			}
			if w.srv.ToolCalls("test_cleanup") != 0 || !w.app.tenants[tenant.ID] {
				t.Error("a call was sent to a tool that changed")
			}
		})
	}
}

// An adapter that has found drift provisions nothing: the audit is over until the
// application is audited again.
func TestMCPProvisionerRefusesAnAdapterThatFoundDrift(t *testing.T) {
	w := mcpProvisionFixture(t, true, nil)
	mcpProvisionChange(w, "patients_find")
	if _, err := w.in.Adapter.Describe(mcpProvisionCtx(t)); err != nil { // lists the tools again, and finds it
		t.Fatal(err)
	}
	p, err := mcpProvisionSource(mcpProvisionCtx(t), w.in)
	if p != nil || contracts.CodeOf(err) != contracts.ErrQuarantined {
		t.Errorf("the source after drift: %v, %v, want a quarantined error", p, err)
	}
}

// mcpProvisionMappedTools lists the tools a package's capabilities map: the set the
// manifest's contract_sha256 is taken over.
func mcpProvisionMappedTools(pkg *apppkg.Package) []string {
	set := map[string]bool{}
	bind := func(b *ap.CapabilityOperationBinding) {
		if b != nil && b.MCPTool != nil {
			set[string(*b.MCPTool)] = true
		}
	}
	for _, c := range pkg.Capabilities {
		bind(c.Operations.Read)
		bind(c.Operations.Validate)
		bind(c.Operations.Prepare)
		bind(c.Operations.Commit)
		bind(c.Operations.Status)
		for _, et := range c.EntityTypes {
			bind(et.Resolve)
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// mcpProvisionPinContract sets the manifest's contract_sha256 of the package's mcp endpoint.
func mcpProvisionPinContract(hash string) func(pkg *apppkg.Package) {
	return func(pkg *apppkg.Package) {
		for id, ep := range pkg.Manifest.Endpoints {
			if v, ok := ep.Value.(ap.ManifestMCPEndpoint); ok {
				h := ap.SHA256(hash)
				v.ContractSHA256 = &h
				pkg.Manifest.Endpoints[id] = ap.ManifestEndpoint{Value: v}
			}
		}
	}
}

// The manifest's contract_sha256 is checked against the tools the package maps by the
// adapter, which lists the server's tools before every call: a listing that hashes as the
// manifest approved provisions, and one that does not is refused as quarantined before a
// provisioning tool is called.
func TestMCPProvisionerChecksTheContractHash(t *testing.T) {
	// the fixture serves every mapped tool with this input schema
	served := map[string]any{"type": "object", "properties": map[string]any{"identity": map[string]any{"type": "object"}}}
	probe := mcpProvisionFixture(t, true, nil)
	var listed []mcptransport.ContractTool
	for _, n := range mcpProvisionMappedTools(probe.pkg) {
		listed = append(listed, mcptransport.ContractTool{Name: n, InputSchema: served})
	}
	good, err := mcptransport.ContractSHA256(listed)
	if err != nil {
		t.Fatal(err)
	}

	w := mcpProvisionFixture(t, true, nil, mcpProvisionPinContract(good))
	p := mcpProvisionMustSource(t, w.in)
	if err := p.CreateTenant(mcpProvisionCtx(t), mcpProvisionTenantOf(mcpProvisionSuffix)); err != nil {
		t.Errorf("a listing that hashes as the manifest approved: %v", err)
	}

	w = mcpProvisionFixture(t, true, nil, mcpProvisionPinContract(strings.Repeat("0", 64)))
	got, err := mcpProvisionSource(mcpProvisionCtx(t), w.in)
	if got != nil || contracts.CodeOf(err) != contracts.ErrQuarantined || mcpProvisionReasonOf(err) != mcpProvisionReasonDrift {
		t.Errorf("a listing that does not hash as the manifest approved: %v, %v, want a quarantined error", got, err)
	}
	for _, tool := range []string{"test_create_tenant", "test_seed", "test_cleanup", mcpProvisionChangeTool} {
		if n := w.srv.ToolCalls(tool); n != 0 {
			t.Errorf("%s was called %d times on a server whose tools do not hash as approved", tool, n)
		}
	}
}

// mcpProvisionObserved collects the drift the transport's observers are told of.
var mcpProvisionObserved struct {
	sync.Mutex
	drifts []mcptransport.Drift
	apps   []string
	once   sync.Once
}

func mcpProvisionObserve() {
	mcpProvisionObserved.once.Do(func() {
		RegisterMCPDriftObserver(func(app string, d mcptransport.Drift) {
			mcpProvisionObserved.Lock()
			defer mcpProvisionObserved.Unlock()
			mcpProvisionObserved.drifts = append(mcpProvisionObserved.drifts, d)
			mcpProvisionObserved.apps = append(mcpProvisionObserved.apps, app)
		})
	})
}

// The bridge has no client of its own: every call goes through the audited adapter's
// approved-tool call, so it is held to the adapter's own pins, drift flag and observers, on
// the adapter's own session. What shows it: the test tools are pinned in the adapter, a
// change to one of them raises the adapter's re-audit flag and reaches its drift observers,
// and all the calls of a run share one MCP session where a client of its own opened one
// for every call.
func TestMCPProvisionerUsesTheAdapterApprovedToolCall(t *testing.T) {
	mcpProvisionObserve()

	w := mcpProvisionFixture(t, true, nil)
	audited := w.in.Adapter.(transportsClosing).inner.(*mcptransport.Adapter)
	testTools := []string{"test_create_tenant", "test_seed", "test_cleanup", mcpProvisionChangeTool, mcpProvisionFaultTool}
	pinOf := func(tool string) string { return audited.Pins()["clinic-mcp/"+tool] }
	for _, tool := range testTools {
		if pinOf(tool) != "" {
			t.Fatalf("%s is pinned before the bridge looked at it", tool)
		}
	}
	mapped := audited.Pins()
	if len(mapped) < 3 {
		t.Fatalf("the audit's adapter holds %d pins of the mapped tools: %v", len(mapped), mapped)
	}

	ctx := mcpProvisionCtx(t)
	p := mcpProvisionMustSource(t, w.in)
	for _, tool := range testTools {
		if len(pinOf(tool)) != 64 {
			t.Errorf("%s was not pinned by the adapter when the bridge first saw it: %q", tool, pinOf(tool))
		}
	}
	for k, want := range mapped {
		if audited.Pins()[k] != want {
			t.Errorf("the pin of %s changed", k)
		}
	}

	tenant := mcpProvisionTenantOf(mcpProvisionSuffix)
	if err := p.CreateTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Seed(ctx, mcpProvisionSeedOf(mcpProvisionSuffix)); err != nil {
		t.Fatal(err)
	}
	if rep, err := p.Cleanup(ctx, tenant.ID); err != nil || rep.Left() != 0 {
		t.Fatalf("Cleanup = %+v, %v", rep, err)
	}
	sessions := 0
	for _, e := range w.srv.Log() {
		if e.Method == "initialize" {
			sessions++
		}
	}
	if sessions != 1 {
		t.Errorf("%d MCP sessions were opened for the audit's calls and the bridge's own, want the adapter's one", sessions)
	}

	// a test tool that changes is the adapter's drift: its flag, its list, its observers
	if audited.ReauditRequired() {
		t.Fatal("the re-audit flag is up before anything changed")
	}
	again := mcpProvisionTenantOf("1111111111")
	if err := p.CreateTenant(ctx, again); err != nil {
		t.Fatal(err)
	}
	mcpProvisionChange(w, "test_cleanup")
	_, err := p.Cleanup(ctx, again.ID)
	if contracts.CodeOf(err) != contracts.ErrQuarantined || mcpProvisionReasonOf(err) != mcpProvisionReasonDrift {
		t.Fatalf("Cleanup after test_cleanup changed: %v, want quarantined (%s)", err, mcpProvisionReasonDrift)
	}
	if !audited.ReauditRequired() {
		t.Error("the adapter's re-audit flag is not up: the refusal did not come from the adapter")
	}
	found := false
	for _, d := range audited.Drifts() {
		found = found || (d.Tool == "test_cleanup" && d.Kind == mcptransport.DriftSchemaChanged && d.Endpoint == "clinic-mcp")
	}
	if !found {
		t.Errorf("the adapter recorded no schema drift of test_cleanup: %+v", audited.Drifts())
	}
	mcpProvisionObserved.Lock()
	observed := false
	for i, d := range mcpProvisionObserved.drifts {
		observed = observed || (d.Tool == "test_cleanup" && mcpProvisionObserved.apps[i] == mcpProvisionClinicApp)
	}
	mcpProvisionObserved.Unlock()
	if !observed {
		t.Error("the drift of a test tool did not reach the drift observers of the transport")
	}
	// and once the adapter has found drift the bridge provisions nothing at all
	creates := w.srv.ToolCalls("test_create_tenant")
	if err := p.CreateTenant(ctx, mcpProvisionTenantOf("2222222222")); contracts.CodeOf(err) != contracts.ErrQuarantined {
		t.Errorf("CreateTenant on an adapter that found drift: %v, want quarantined", err)
	}
	if n := w.srv.ToolCalls("test_create_tenant"); n != creates {
		t.Errorf("%d creates reached the application after the adapter found drift", n-creates)
	}
	if n := w.srv.ToolCalls("test_cleanup"); n != 1 {
		t.Errorf("test_cleanup was called %d times, want only the cleanup before it changed", n)
	}
}

// without removes the lines of the named tools from the adapter configuration of a copy of the package.
func mcpProvisionWithout(t *testing.T, tools ...string) func(dir string) string {
	return func(dir string) string {
		return mcpProvisionCopyPackage(t, dir, func(cfg string) string {
			var keep []string
			for _, line := range strings.Split(cfg, "\n") {
				drop := false
				for _, tool := range tools {
					drop = drop || strings.TrimSpace(line) == "- "+tool
				}
				if !drop {
					keep = append(keep, line)
				}
			}
			return strings.Join(keep, "\n")
		})
	}
}

// The change-record and the fault tools are conventions: the bridge offers each ability
// only when the package approves the tool, no capability maps it and the server lists it,
// and a provisioner never claims one it lacks.
func TestMCPProvisionerOffersOnlyTheAbilitiesTheApplicationHas(t *testing.T) {
	type abilities struct{ change, fault bool }
	has := func(p probes.Provisioner) abilities {
		_, change := p.(probes.RecordChanger)
		_, fault := p.(probes.FaultInjector)
		return abilities{change, fault}
	}

	t.Run("the clinic package approves both tools and the server offers both", func(t *testing.T) {
		w := mcpProvisionFixture(t, true, nil)
		if got := has(mcpProvisionMustSource(t, w.in)); got != (abilities{change: true, fault: true}) {
			t.Errorf("abilities = %+v, want both", got)
		}
	})
	t.Run("a tool the server offers and the package does not approve is no ability, and is not even pinned", func(t *testing.T) {
		w := mcpProvisionFixture(t, true, mcpProvisionWithout(t, mcpProvisionFaultTool))
		if got := has(mcpProvisionMustSource(t, w.in)); got != (abilities{change: true}) {
			t.Errorf("abilities = %+v, want only RecordChanger: the server also lists %s but the package does not approve it", got, mcpProvisionFaultTool)
		}
		audited := w.in.Adapter.(transportsClosing).inner.(*mcptransport.Adapter)
		if pin := audited.Pins()["clinic-mcp/"+mcpProvisionFaultTool]; pin != "" {
			t.Errorf("the tool the package does not approve was pinned: the bridge looked at a tool it must ignore")
		}
	})
	t.Run("the other tool alone", func(t *testing.T) {
		w := mcpProvisionFixture(t, true, mcpProvisionWithout(t, mcpProvisionChangeTool))
		if got := has(mcpProvisionMustSource(t, w.in)); got != (abilities{fault: true}) {
			t.Errorf("abilities = %+v, want only FaultInjector", got)
		}
	})
	t.Run("an approved tool the server does not list is no ability", func(t *testing.T) {
		w := mcpProvisionFixture(t, true, nil)
		w.srv.Remove(mcpProvisionChangeTool)
		if got := has(mcpProvisionMustSource(t, w.in)); got != (abilities{fault: true}) {
			t.Errorf("abilities = %+v, want only FaultInjector", got)
		}
		// a tool the adapter has seen and the server then stops listing is drift, not an absence
		w.srv.Remove(mcpProvisionFaultTool)
		if p, err := mcpProvisionSource(mcpProvisionCtx(t), w.in); p != nil || contracts.CodeOf(err) != contracts.ErrQuarantined {
			t.Errorf("a tool that was listed and is gone: %v, %v, want a quarantined error", p, err)
		}
	})
	t.Run("no approved tool is listed", func(t *testing.T) {
		w := mcpProvisionFixture(t, true, nil)
		w.srv.Remove(mcpProvisionChangeTool)
		w.srv.Remove(mcpProvisionFaultTool)
		if got := has(mcpProvisionMustSource(t, w.in)); got != (abilities{}) {
			t.Errorf("abilities = %+v, want none", got)
		}
	})
	t.Run("a tool a capability maps is no ability: the adapter contract owns it", func(t *testing.T) {
		w := mcpProvisionFixture(t, true, nil, func(pkg *apppkg.Package) {
			c := pkg.Capabilities["availability.list"]
			tool := ap.OperationName(mcpProvisionChangeTool)
			c.Operations.Validate = &ap.CapabilityOperationBinding{Endpoint: "clinic-mcp", MCPTool: &tool}
			pkg.Capabilities["availability.list"] = c
		})
		if got := has(mcpProvisionMustSource(t, w.in)); got != (abilities{fault: true}) {
			t.Errorf("abilities = %+v, want only FaultInjector: a tool a capability maps must not be offered as a record changer", got)
		}
	})
	t.Run("neither tool approved", func(t *testing.T) {
		w := mcpProvisionFixture(t, true, mcpProvisionWithout(t, mcpProvisionChangeTool, mcpProvisionFaultTool))
		if got := has(mcpProvisionMustSource(t, w.in)); got != (abilities{}) {
			t.Errorf("abilities = %+v, want none", got)
		}
	})
}

func TestMCPProvisionerChangesRecordsOfItsOwnTenantsOnly(t *testing.T) {
	w := mcpProvisionFixture(t, true, nil)
	ctx := mcpProvisionCtx(t)
	prov := mcpProvisionMustSource(t, w.in)
	p, ok := prov.(probes.RecordChanger)
	if !ok {
		t.Fatal("the provisioner of an application that offers the change tool is not a record changer")
	}
	tenant := mcpProvisionTenantOf(mcpProvisionSuffix)
	if err := prov.CreateTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	seeded, err := prov.Seed(ctx, mcpProvisionSeedOf(mcpProvisionSuffix))
	if err != nil {
		t.Fatal(err)
	}
	id := seeded.Entities["patient-rec-a1"]

	if err := p.ChangeRecord(ctx, tenant.ID, "patient", id); err != nil {
		t.Fatalf("ChangeRecord: %v", err)
	}
	want := map[string]any{"tenant_id": tenant.ID, "entity_type": "patient", "entity_id": id}
	if len(w.app.changes) != 1 || cmp.Diff(want, w.app.changes[0]) != "" {
		t.Fatalf("the application was asked %v, want %v", w.app.changes, want)
	}

	// a tenant nobody created is not touched
	if err := p.ChangeRecord(ctx, "tnt_group_a_bbbbbbbbbb", "patient", id); contracts.CodeOf(err) != contracts.ErrPermissionDenied || mcpProvisionReasonOf(err) != mcpProvisionReasonNotOurs {
		t.Errorf("ChangeRecord of a tenant nobody created: %v, want permission_denied", err)
	}
	for name, args := range map[string][2]string{
		"an entity type that is not one": {"pa tient", id},
		"an identifier that is not one":  {"patient", "a\nb"},
		"no identifier":                  {"patient", ""},
	} {
		before := len(w.app.changes)
		if err := p.ChangeRecord(ctx, tenant.ID, args[0], args[1]); contracts.CodeOf(err) != contracts.ErrValidationFailed || len(w.app.changes) != before {
			t.Errorf("%s: %v (%d requests)", name, err, len(w.app.changes)-before)
		}
	}

	// an application that does not say it changed the record asked for has shown nothing
	reply := func(extra map[string]any) mcpfixture.Result {
		body := map[string]any{"status": "ok", "tenant_id": tenant.ID, "entity_type": "patient", "entity_id": id, "changed": true}
		for k, v := range extra {
			if v == nil {
				delete(body, k)
				continue
			}
			body[k] = v
		}
		return mcpProvisionResult(body)
	}
	for name, c := range map[string]struct {
		body mcpfixture.Result
		code contracts.ErrCode
	}{
		"changed false":           {reply(map[string]any{"changed": false}), contracts.ErrSchemaInvalid},
		"changed missing":         {reply(map[string]any{"changed": nil}), contracts.ErrSchemaInvalid},
		"another record":          {reply(map[string]any{"entity_id": "pat_other"}), contracts.ErrSchemaInvalid},
		"another entity type":     {reply(map[string]any{"entity_type": "slot"}), contracts.ErrSchemaInvalid},
		"another tenant":          {reply(map[string]any{"tenant_id": "tnt_other"}), contracts.ErrSchemaInvalid},
		"a refusal of its own":    {mcpProvisionRejected("scope_violation"), contracts.ErrScopeViolation},
		"a refusal of no meaning": {mcpProvisionRejected("no-such-code"), contracts.ErrValidationFailed},
	} {
		w.srv.Handle(mcpProvisionChangeTool, func(context.Context, json.RawMessage) mcpfixture.Result { return c.body })
		err := p.ChangeRecord(ctx, tenant.ID, "patient", id)
		if contracts.CodeOf(err) != c.code {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
		if err != nil && strings.Contains(err.Error(), mcpProvisionCanary) {
			t.Errorf("%s: text the server chose reached an error: %v", name, err)
		}
	}
}

func TestMCPProvisionerArmsFaultsOnlyForItsOwnTenants(t *testing.T) {
	w := mcpProvisionFixture(t, true, nil)
	ctx := mcpProvisionCtx(t)
	prov := mcpProvisionMustSource(t, w.in)
	p, ok := prov.(probes.FaultInjector)
	if !ok {
		t.Fatal("the provisioner of an application that offers the fault tool is not a fault injector")
	}
	fault := probes.Fault{Operation: adapters.OpCommit, CapabilityID: "appointments.book", Kind: probes.FaultReplyLost}

	// before any tenant exists there is nothing to confine the failure to
	if err := p.InjectFault(ctx, fault); contracts.CodeOf(err) != contracts.ErrPermissionDenied || len(w.app.faults) != 0 {
		t.Fatalf("a fault with no tenant of the audit: %v (%d requests), want permission_denied and nothing sent", err, len(w.app.faults))
	}
	a, b := mcpProvisionTenantOf("aaaaaaaaaa"), mcpProvisionTenantOf("bbbbbbbbbb")
	for _, tn := range []probes.Tenant{a, b} {
		if err := prov.CreateTenant(ctx, tn); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.InjectFault(ctx, fault); err != nil {
		t.Fatalf("InjectFault: %v", err)
	}
	want := map[string]any{
		"operation": "commit", "capability_id": "appointments.book", "kind": "reply_lost", "tenant_ids": []any{a.ID, b.ID},
	}
	if len(w.app.faults) != 1 || cmp.Diff(want, w.app.faults[0]) != "" {
		t.Fatalf("the application was asked %v, want %v", w.app.faults, want)
	}
	// a tenant that is gone is no longer one a failure is armed for
	if rep, err := prov.Cleanup(ctx, a.ID); err != nil || rep.Left() != 0 {
		t.Fatal(rep, err)
	}
	if err := p.InjectFault(ctx, probes.Fault{Operation: adapters.OpRead, CapabilityID: "availability.list", Kind: probes.FaultDependencyDown}); err != nil {
		t.Fatal(err)
	}
	if got := w.app.faults[1]["tenant_ids"]; cmp.Diff([]any{b.ID}, got) != "" {
		t.Errorf("tenant_ids = %v, want only the tenant that is still there", got)
	}

	before := len(w.app.faults)
	for name, f := range map[string]probes.Fault{
		"a kind that is not one":       {Operation: adapters.OpCommit, CapabilityID: "appointments.book", Kind: "melt"},
		"no operation":                 {CapabilityID: "appointments.book", Kind: probes.FaultReplyLost},
		"a capability that is not one": {Operation: adapters.OpCommit, CapabilityID: "a b", Kind: probes.FaultReplyLost},
	} {
		if err := p.InjectFault(ctx, f); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("%s: %v, want validation_failed", name, err)
		}
	}
	if len(w.app.faults) != before {
		t.Error("a malformed fault reached the application")
	}
	for name, body := range map[string]mcpfixture.Result{
		"armed false":   mcpProvisionResult(map[string]any{"status": "ok", "armed": false}),
		"armed missing": mcpProvisionResult(map[string]any{"status": "ok"}),
		"a refusal":     mcpProvisionRejected("permission_denied"),
	} {
		w.srv.Handle(mcpProvisionFaultTool, func(context.Context, json.RawMessage) mcpfixture.Result { return body })
		if err := p.InjectFault(ctx, fault); err == nil {
			t.Errorf("%s: a failure the application did not say it armed was reported as armed", name)
		}
	}
}

// A seed finds the principal a record names by the reference the suite gave it, whatever
// the audit's identifier for it looks like.
func TestMCPProvisionerResolvesSeedPrincipalsByReference(t *testing.T) {
	w := mcpProvisionFixture(t, true, nil)
	ctx := mcpProvisionCtx(t)
	p := mcpProvisionMustSource(t, w.in)
	ws := "ws_site_a1_" + mcpProvisionSuffix
	tenant := probes.Tenant{
		ID: "tnt_group_a_" + mcpProvisionSuffix, Workspaces: []string{ws},
		Principals: []probes.Principal{
			{Ref: "patient-a1", ID: "P-0001", WorkspaceID: ws, Roles: []string{"patient"}},
			{Ref: "doctor-a1", ID: "D-0001", WorkspaceID: ws, Roles: []string{"doctor"}},
			{ID: "N-0001", WorkspaceID: ws, Roles: []string{"patient"}}, // no reference: nobody can name it
		},
	}
	if err := p.CreateTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	for _, pr := range w.app.creates[0]["principals"].([]any) {
		if _, has := pr.(map[string]any)["ref"]; has {
			t.Errorf("the audit's reference reached the application: %v", pr)
		}
	}
	seed := probes.Seed{TenantID: tenant.ID, DataFile: "tests/seed/clinic.json", Records: []probes.Record{
		{Ref: "patient-rec-a1", EntityType: "patient", WorkspaceID: ws},
		{Ref: "appt-a1", EntityType: "appointment", WorkspaceID: ws},
	}}
	if _, err := p.Seed(ctx, seed); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	got := map[string]any{}
	for _, r := range w.app.seeds[0]["records"].([]any) {
		rec := r.(map[string]any)
		got[rec["ref"].(string)] = rec["principal_id"]
	}
	if got["patient-rec-a1"] != "P-0001" || got["appt-a1"] != "P-0001" {
		t.Errorf("principal_id of the seeded records = %v, want the identifiers the tenant was created with, found by reference", got)
	}

	// a tenant without the references the seed data names fails the seed closed
	bare := tenant
	bare.ID = "tnt_group_a_cccccccccc"
	bare.Principals = []probes.Principal{{ID: "usr_patient_a1_cccccccccc", WorkspaceID: ws, Roles: []string{"patient"}}}
	if err := p.CreateTenant(ctx, bare); err != nil {
		t.Fatal(err)
	}
	seed.TenantID = bare.ID
	seeds := w.srv.ToolCalls("test_seed")
	if _, err := p.Seed(ctx, seed); contracts.CodeOf(err) != contracts.ErrValidationFailed || w.srv.ToolCalls("test_seed") != seeds {
		t.Errorf("a seed naming a principal that carries no reference: %v, want validation_failed with nothing sent", err)
	}

	// a reference two principals share is ambiguous: the tenant is not created
	twice := tenant
	twice.ID = "tnt_group_a_dddddddddd"
	twice.Principals = []probes.Principal{
		{Ref: "patient-a1", ID: "P-0002", WorkspaceID: ws}, {Ref: "patient-a1", ID: "P-0003", WorkspaceID: ws},
	}
	creates := w.srv.ToolCalls("test_create_tenant")
	if err := p.CreateTenant(ctx, twice); contracts.CodeOf(err) != contracts.ErrValidationFailed || w.srv.ToolCalls("test_create_tenant") != creates {
		t.Errorf("a repeated principal reference: %v, want validation_failed with nothing sent", err)
	}
}

// The install directory is the one the transport opened the package from, not what the
// configuration says: a harness that audits with a configuration registry of its own, so
// that config.Core() names some other data directory, still finds its package. Where the
// transport did not record it (a registry that was not wired through this process's) the
// configuration is the fallback, and with neither the seed fails typed while everything
// that needs no file still works.
func TestMCPProvisionerFindsTheInstallDirectoryWithoutTheConfiguration(t *testing.T) {
	setDataDir := func(t *testing.T, f func() string) {
		was := mcpProvisionDataDir
		mcpProvisionDataDir = f
		t.Cleanup(func() { mcpProvisionDataDir = was })
	}
	seedOf := func(t *testing.T, p probes.Provisioner) error {
		ctx := mcpProvisionCtx(t)
		if err := p.CreateTenant(ctx, mcpProvisionTenantOf(mcpProvisionSuffix)); err != nil {
			t.Fatalf("CreateTenant: %v", err)
		}
		_, err := p.Seed(ctx, mcpProvisionSeedOf(mcpProvisionSuffix))
		return err
	}

	t.Run("the directory the transport opened the package from wins over the configuration", func(t *testing.T) {
		for name, dataDir := range map[string]string{"no data directory": "", "another data directory": t.TempDir()} {
			w := mcpProvisionFixture(t, true, nil)
			setDataDir(t, func() string { return dataDir })
			if err := seedOf(t, mcpProvisionMustSource(t, w.in)); err != nil {
				t.Errorf("%s: Seed = %v, want the seed data file found where the package was opened from", name, err)
			}
		}
	})

	// an adapter built through a registry that does not record where it opened a package
	unrecorded := func(t *testing.T) {
		// an earlier test opened this package (same application and manifest hash) through the
		// process's registry, and the transport remembers where
		mcpProvisionInstallDirs.Delete(mcpProvisionPackageKey{mcpProvisionClinicApp, "h1"})
		reg := newTransportsRegistry()
		reg.register(TransportKindMCP, mcpTransportOpen)
		mcpProvisionTestRegistry = reg
		t.Cleanup(func() { mcpProvisionTestRegistry = nil })
	}
	t.Run("the configuration is the fallback", func(t *testing.T) {
		unrecorded(t)
		w := mcpProvisionFixture(t, true, nil)
		setDataDir(t, func() string { return filepath.Dir(w.dir) })
		if err := seedOf(t, mcpProvisionMustSource(t, w.in)); err != nil {
			t.Errorf("Seed = %v, want the data directory of the configuration to be used", err)
		}
	})
	t.Run("with neither the seed fails typed and the rest works", func(t *testing.T) {
		unrecorded(t)
		w := mcpProvisionFixture(t, true, nil)
		setDataDir(t, func() string { return "" })
		p := mcpProvisionMustSource(t, w.in)
		err := seedOf(t, p)
		if contracts.CodeOf(err) != contracts.ErrUnavailable || strings.Contains(err.Error(), w.dir) {
			t.Errorf("Seed = %v, want an unavailable error that does not name a path", err)
		}
		if w.srv.ToolCalls("test_seed") != 0 {
			t.Error("a seed whose data file cannot be read reached the application")
		}
		if rep, err := p.Cleanup(mcpProvisionCtx(t), mcpProvisionTenantOf(mcpProvisionSuffix).ID); err != nil || !rep.Existed {
			t.Errorf("Cleanup = %+v, %v", rep, err)
		}
	})
}

// A transport registry has the MCP factory the bridge wraps: a registry without one is a
// programming error found when the bridge starts.
func TestMCPProvisionBridgeNeedsTheMCPTransport(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("a registry without the MCP transport was wrapped without a word")
		}
	}()
	mcpProvisionRecordInstallDirs(newTransportsRegistry())
}

// An application that is down is unavailable, typed, and nothing is adopted: a create that
// never left is not a tenant to clean up.
func TestMCPProvisionerReportsAnUnreachableApplicationAsUnavailable(t *testing.T) {
	w := mcpProvisionFixture(t, true, nil)
	ctx := mcpProvisionCtx(t)
	p := mcpProvisionMustSource(t, w.in)
	w.srv.Down()
	tenant := mcpProvisionTenantOf(mcpProvisionSuffix)
	err := p.CreateTenant(ctx, tenant)
	if contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Fatalf("CreateTenant of an application that is down: %v, want unavailable", err)
	}
	if _, err := p.Cleanup(ctx, tenant.ID); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("Cleanup of a tenant whose create never left: %v, want permission_denied", err)
	}
	if got, err := mcpProvisionSource(ctx, w.in); got != nil || contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Errorf("the source of an application that is down: %v, %v, want an unavailable error", got, err)
	}
	// a caller that gave up is told so with its own error
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	w.srv.Up(t)
	if err := p.CreateTenant(cancelled, tenant); !errors.Is(err, context.Canceled) {
		t.Errorf("CreateTenant on a cancelled context: %v, want context.Canceled", err)
	}
}

// The source claims nothing that is not an audit of the MCP adapter this transport built:
// an error from it would stop the probes of every other transport.
func TestMCPProvisionSourceLeavesOtherAdaptersAlone(t *testing.T) {
	w := mcpProvisionFixture(t, true, nil)
	app := fake.NewAdapter(fake.NewApp())
	cases := map[string]*integrationaudit.Input{
		"no input":                   nil,
		"no package":                 {Adapter: w.in.Adapter},
		"no adapter":                 {Package: w.pkg, Record: w.in.Record},
		"an adapter of another kind": {Package: w.pkg, Record: w.in.Record, Adapter: app},
		"a wrapper around another":   {Package: w.pkg, Record: w.in.Record, Adapter: transportsClosing{AppAdapter: app, inner: app}},
	}
	for name, in := range cases {
		if p, err := mcpProvisionSource(mcpProvisionCtx(t), in); p != nil || err != nil {
			t.Errorf("%s: %v, %v, want no provisioner and no error", name, p, err)
		}
	}
}
