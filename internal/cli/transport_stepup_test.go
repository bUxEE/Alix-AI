package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	mcptransport "github.com/buxee/alix-ai/adapters/mcp"
	mcpfixture "github.com/buxee/alix-ai/adapters/mcp/testdata/fixture"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/integrationaudit/probes"
	"github.com/buxee/alix-ai/pkg/bridge"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

// The step-up completers of the three provisioning bridges (master plan 4.5 check 8, level 3
// capabilities): over the real transports, against the two reference applications that have a
// level 3 capability. The ledger (Go, native) records an approval that the commit which follows
// applies once; facility-maintenance (Python, OpenAPI) applies the effect in its own UI, so a
// commit through the adapter stays refused. The audit must accept both, and must say so only when
// it could complete the step-up itself.

const (
	// transportStepUpFacilityDir is the facility-maintenance reference application.
	transportStepUpFacilityDir = "../../examples/reference-apps/facility-maintenance"
	// transportStepUpEffectSubject is what the audit's probe of the effect after the step-up says it showed.
	transportStepUpEffectSubject = "the effect is there after the application's own step-up"
	// transportStepUpCompletion is the subject of the unverified item recorded when it cannot complete one.
	transportStepUpCompletion = "safety:step_up_completion"
)

// transportStepUpPythonOK reports whether an interpreter can run the facility application.
func transportStepUpPythonOK(py string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, py, "-c", "import fastapi, uvicorn, yaml").Run() == nil
}

// transportStepUpPython finds an interpreter that has the facility application's dependencies: the one
// named by ALIXAI_FACILITY_PYTHON, the application's own virtual environment, the same in the main
// checkout when this is a task worktree, and python on PATH. "" when there is none.
func transportStepUpPython(appDir string) string {
	var candidates []string
	if v := os.Getenv("ALIXAI_FACILITY_PYTHON"); v != "" {
		candidates = append(candidates, v)
	}
	venvPython := func(dir string) string {
		if runtime.GOOS == "windows" {
			return filepath.Join(dir, ".venv", "Scripts", "python.exe")
		}
		return filepath.Join(dir, ".venv", "bin", "python")
	}
	candidates = append(candidates, venvPython(appDir))
	// a task worktree (<repo>/.worktrees/<task>/...) has no virtual environment of its own
	slash := filepath.ToSlash(appDir)
	if before, after, ok := strings.Cut(slash, "/.worktrees/"); ok {
		if _, rest, ok := strings.Cut(after, "/"); ok {
			candidates = append(candidates, venvPython(filepath.FromSlash(before+"/"+rest)))
		}
	}
	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			candidates = append(candidates, p)
		}
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err != nil && !strings.ContainsAny(c, `/\`) {
			continue
		}
		if transportStepUpPythonOK(c) {
			return c
		}
	}
	return ""
}

// transportStepUpFacility is the facility-maintenance application, running as a subprocess in test
// support mode.
type transportStepUpFacility struct {
	// pid is the process this test started and is the only one it ever stops.
	pid     int
	baseURL string
}

// transportStepUpStartFacility starts the real facility application (Python, FastAPI) with its test
// support surface on a free loopback port. It is skipped only where no interpreter has the
// application's dependencies.
func transportStepUpStartFacility(t *testing.T) *transportStepUpFacility {
	t.Helper()
	appDir, err := filepath.Abs(transportStepUpFacilityDir)
	if err != nil {
		t.Fatal(err)
	}
	py := transportStepUpPython(appDir)
	if py == "" {
		t.Skip("no python with fastapi and uvicorn is available: the facility-maintenance reference application cannot be started (set ALIXAI_FACILITY_PYTHON)")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	cmd := exec.Command(py, "-m", "facility_maintenance")
	cmd.Dir = appDir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "FACILITY_") && !strings.HasPrefix(kv, "PYTHONPATH=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	// the code of this checkout, whatever the interpreter's own installation points at
	cmd.Env = append(cmd.Env, "PYTHONPATH="+filepath.Join(appDir, "src"), "PYTHONUNBUFFERED=1",
		"FACILITY_TEST_SUPPORT=1", "FACILITY_PORT="+strconv.Itoa(port))
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid // the process this test started: the only one it stops
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		// A virtual environment's python.exe on Windows is a launcher whose child is the interpreter, so the
		// process tree of the PID this test started is stopped (never a process found by name).
		if runtime.GOOS == "windows" {
			_ = exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
		} else {
			_ = cmd.Process.Kill()
		}
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Errorf("the facility application (pid %d) did not stop", pid)
		}
		if c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second); err == nil {
			_ = c.Close()
			t.Errorf("something still listens on the facility application's port %d after it was stopped", port)
		}
	})
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
		}
		_, _ = io.Copy(io.Discard, out)
	}()

	base := "http://127.0.0.1:" + strconv.Itoa(port)
	deadline := time.Now().Add(60 * time.Second)
	poll := &http.Client{Timeout: 2 * time.Second}
	for {
		resp, err := poll.Get(base + "/v1/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return &transportStepUpFacility{pid: pid, baseURL: base}
			}
		}
		select {
		case <-done:
			t.Fatal("the facility application stopped before it listened")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the facility application did not start")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// transportStepUpFacilityInput is what the audit hands its checks for the facility package, with
// the OpenAPI endpoint pointed at the running application: the adapter the process's registry
// builds (the wrapped OpenAPI adapter, exactly as audit run holds it) and what the runner asks of it
// first. The package is the shipped one, read from its directory.
func transportStepUpFacilityInput(t testing.TB, baseURL string, dirOf func(dir string) string, edit ...func(pkg *apppkg.Package)) *integrationaudit.Input {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join(transportStepUpFacilityDir, "package"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := apppkg.Load(context.Background(), apppkg.FromDir(dir), apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Fatalf("the facility package does not load: %+v", res.Findings)
	}
	pkg := res.Package
	found := false
	for id, ep := range pkg.Manifest.Endpoints {
		if v, ok := ep.Value.(ap.ManifestOpenAPIEndpoint); ok {
			v.BaseURL = ap.HTTPURL(baseURL)
			pkg.Manifest.Endpoints[id] = ap.ManifestEndpoint{Value: v}
			found = true
		}
	}
	if !found {
		t.Fatal("the facility package has no openapi endpoint")
	}
	for _, e := range edit {
		e(pkg)
	}
	use := dir
	if dirOf != nil {
		use = dirOf(dir)
	}
	dataDir := filepath.Dir(use)
	was := openapiProvisionDataDir
	openapiProvisionDataDir = func() string { return dataDir }
	t.Cleanup(func() { openapiProvisionDataDir = was })

	rec := apppkg.Record{
		ApplicationID: string(pkg.Manifest.Application.ID), ManifestHash: "h1", State: apppkg.StateQuarantined, Path: filepath.Base(use),
	}
	ctx := context.Background()
	ad, _, err := transportsSource{dataDir: dataDir}.build(ctx, rec.ApplicationID, pkg, rec)
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

// transportStepUpDescribe prints an outcome the way a failing test needs to read it.
func transportStepUpDescribe(o *integrationaudit.Outcome) string {
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
	for _, p := range o.Probes {
		fmt.Fprintf(&b, "  probe %s [%s] %s: expected %q, observed %q, passed %v\n", p.Kind, p.Capability, p.Subject, p.Expected, p.Observed, p.Passed)
	}
	return b.String()
}

// transportStepUpSafety runs the audit's safe-execution check with the default provisioner source
// (the one the transports registered) and returns its outcome.
func transportStepUpSafety(t testing.TB, in *integrationaudit.Input) *integrationaudit.Outcome {
	t.Helper()
	return nativeProvisionRun(t, in, probes.Options{}, integrationaudit.SectionSafeExecution)[integrationaudit.SectionSafeExecution]
}

// transportStepUpExpect asserts what the audit said about the step-up of one capability: either it
// completed it and showed that the effect is there only afterwards, or it says it could not and
// records the completion as unverified, and in both cases there is no critical finding about it.
func transportStepUpExpect(t *testing.T, o *integrationaudit.Outcome, capability string, completed bool) {
	t.Helper()
	describe := transportStepUpDescribe(o)
	for _, f := range o.Findings {
		if f.Severity == contracts.SeverityCritical && (f.Capability == capability || strings.Contains(f.CheckID, "step_up")) {
			t.Errorf("a critical finding about the step-up of %s: %s %s\n%s", capability, f.CheckID, f.Message, describe)
		}
	}
	unverified := 0
	for _, u := range o.Unverified {
		if u.Subject == transportStepUpCompletion && u.Capability == capability {
			unverified++
		}
	}
	shown := 0
	for _, p := range o.Probes {
		if p.Capability == capability && p.Subject == transportStepUpEffectSubject {
			if !p.Passed {
				t.Errorf("the effect after the step-up of %s was not shown:\n%s", capability, describe)
			}
			shown++
		}
	}
	switch {
	case completed && (unverified != 0 || shown != 1):
		t.Errorf("%s: the step-up completion should be verified (unverified %d, effect shown %d):\n%s", capability, unverified, shown, describe)
	case !completed && (unverified != 1 || shown != 0):
		t.Errorf("%s: the step-up completion should be unverified, and nothing shown (unverified %d, effect shown %d):\n%s", capability, unverified, shown, describe)
	}
}

// The real audit, over the real transports, against the real reference applications with a level 3
// capability: AlixAI's own composition root completes the step-up (the OpenAPI and native
// provisioning bridges implement probes.StepUpCompleter where the package approves the
// application's approval operation), so the audit shows that the effect happens only after it, for
// both designs, instead of recording the completion as unverified. Without the approval operation in
// management.provisioning the same audit says it could not.
func TestRealAuditVerifiesStepUpCompletionOverOpenAPIAndNative(t *testing.T) {
	t.Run("native: the ledger records an approval and the commit that follows applies it once", func(t *testing.T) {
		_, w := nativeProvisionLedgerWorld(t, nil)
		transportStepUpExpect(t, transportStepUpSafety(t, w.in), "invoices.void", true)
	})

	t.Run("native: without the approval method in management.provisioning the completion is unverified", func(t *testing.T) {
		_, w := nativeProvisionLedgerWorld(t, nativeProvisionCopyPackage(t, func(rel string, b []byte) []byte {
			if rel == "schemas/adapters/ledger-bridge.yaml" {
				return []byte(strings.Replace(string(b), "    - \"approve_step_up\"\n", "", 1))
			}
			return b
		}))
		transportStepUpExpect(t, transportStepUpSafety(t, w.in), "invoices.void", false)
	})

	t.Run("openapi: facility-maintenance applies the effect in its own UI and the commit stays refused", func(t *testing.T) {
		app := transportStepUpStartFacility(t)
		in := transportStepUpFacilityInput(t, app.baseURL, nil)
		transportStepUpExpect(t, transportStepUpSafety(t, in), "assets.decommission", true)
	})

	t.Run("openapi: without the approval operation in management.provisioning the completion is unverified", func(t *testing.T) {
		app := transportStepUpStartFacility(t)
		in := transportStepUpFacilityInput(t, app.baseURL, func(dir string) string {
			return nativeProvisionCopyPackage(t, func(rel string, b []byte) []byte {
				if rel == "schemas/adapters/facility-api.yaml" {
					return []byte(strings.Replace(string(b), "provisioning: [createTenant, seedTenant, cleanupTenant, approveStepUp]", "provisioning: [createTenant, seedTenant, cleanupTenant]", 1))
				}
				return b
			})(dir)
		}, func(pkg *apppkg.Package) {
			// the manifest pins the hash of the approved operations, which this package no longer approves all of
			for id, ep := range pkg.Manifest.Endpoints {
				if v, ok := ep.Value.(ap.ManifestOpenAPIEndpoint); ok {
					v.ContractSHA256 = nil
					pkg.Manifest.Endpoints[id] = ap.ManifestEndpoint{Value: v}
				}
			}
		})
		transportStepUpExpect(t, transportStepUpSafety(t, in), "assets.decommission", false)
	})
}

// ---- the completers, one transport at a time, against an application that only records ----------

// transportStepUpPrincipal is the principal of a tenant the audit created, by position.
func transportStepUpPrincipal(tenant probes.Tenant, i int) probes.Principal {
	return tenant.Principals[i]
}

func transportStepUpTextFact(field, value string) act.Fact {
	return act.Fact{Field: common.FieldName(field), Value: act.CanonicalValue{Value: act.TextValue{Kind: "text", Value: value}}}
}

func transportStepUpIdentifierFact(field, value string) act.Fact {
	return act.Fact{Field: common.FieldName(field), Value: act.CanonicalValue{Value: act.IdentifierValue{Kind: "identifier", Value: common.Identifier(value)}}}
}

func transportStepUpIntegerFact(field string, n int64) act.Fact {
	return act.Fact{Field: common.FieldName(field), Value: act.CanonicalValue{Value: act.IntegerValue{Kind: "integer", Value: n}}}
}

const transportStepUpKey = "idem_01HZZZZZZZZZZZZZZZZZZZZZZZ"

// transportStepUpOf is the step-up the audit asks a completer for: the preparation of a capability for a
// principal, whose confirmation mode is step_up, with the application's own Prepare reply.
func transportStepUpOf(tenantID string, p probes.Principal, capability string, facts ...act.Fact) probes.StepUp {
	prepared := contracts.PreparedAction{
		CapabilityID:   common.CapabilityID(capability),
		IdempotencyKey: act.IdempotencyKey(transportStepUpKey),
		Confirmation:   act.ConfirmationRequirement{Mode: act.ConfirmationRequirementModeStepUp},
	}
	return probes.StepUp{
		CapabilityID: capability,
		Scope: contracts.Scope{
			ApplicationID: "app", TenantID: tenantID, WorkspaceID: p.WorkspaceID, PrincipalID: p.ID, Roles: p.Roles,
		},
		Prepared: prepared,
		Result:   act.DeterministicResult{Status: act.DeterministicResultStatusOk, Values: facts},
	}
}

// transportStepUpWant is the arguments a completer sends, whatever the transport: the identifiers
// of the disposable tenant, the capability and key, and the text and identifier facts of the Prepare
// reply by field, and nothing else.
func transportStepUpWant(s probes.StepUp, values map[string]any) map[string]any {
	return map[string]any{
		"tenant_id": s.Scope.TenantID, "workspace_id": s.Scope.WorkspaceID, "principal_id": s.Scope.PrincipalID,
		"capability_id": s.CapabilityID, "idempotency_key": string(s.Prepared.IdempotencyKey), "values": values,
	}
}

// transportStepUpRefusals are the step-ups a completer refuses before it sends anything: what it was
// not made for. sent says how many requests have reached the application.
func transportStepUpRefusals(t *testing.T, tenant probes.Tenant, complete func(probes.StepUp) error, sent func() int) {
	t.Helper()
	p := transportStepUpPrincipal(tenant, 0)
	good := transportStepUpOf(tenant.ID, p, "appointments.cancel", transportStepUpIdentifierFact("step_up_reference", "SU-0001"))
	notStepUp := good
	notStepUp.Prepared.Confirmation.Mode = act.ConfirmationRequirementModeExplicit
	otherTenant := good
	otherTenant.Scope.TenantID = "tnt_nobody_created_this_0123"
	strangerPrincipal := good
	strangerPrincipal.Scope.PrincipalID = "usr_not_of_this_tenant_0123"
	otherWorkspace := good
	otherWorkspace.Scope.WorkspaceID = tenant.Workspaces[1]
	badKey := good
	badKey.Prepared.IdempotencyKey = "idem_with\nnewline"
	badCapability := good
	badCapability.CapabilityID = "not a capability"

	for name, c := range map[string]struct {
		step probes.StepUp
		code contracts.ErrCode
	}{
		"a tenant nobody created":                 {otherTenant, contracts.ErrPermissionDenied},
		"a preparation that needs no step-up":     {notStepUp, contracts.ErrValidationFailed},
		"a principal the tenant does not have":    {strangerPrincipal, contracts.ErrValidationFailed},
		"a workspace the principal is not in":     {otherWorkspace, contracts.ErrValidationFailed},
		"an idempotency key that is not usable":   {badKey, contracts.ErrValidationFailed},
		"a capability identifier that is no such": {badCapability, contracts.ErrValidationFailed},
	} {
		before := sent()
		if err := complete(c.step); contracts.CodeOf(err) != c.code {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
		if sent() != before {
			t.Errorf("%s: the application was asked", name)
		}
	}
}

// ---- MCP ---------------------------------------------------------------------------------------

// transportStepUpMCPApp is the application's approval tool: it records what it is asked and answers as
// a test says.
type transportStepUpMCPApp struct {
	mu    sync.Mutex
	calls []map[string]any
	reply func(args map[string]any) mcpfixture.Result
}

func (a *transportStepUpMCPApp) handle(_ context.Context, raw json.RawMessage) mcpfixture.Result {
	args := mcpProvisionArgs(raw)
	a.mu.Lock()
	a.calls = append(a.calls, args)
	reply := a.reply
	a.mu.Unlock()
	if reply != nil {
		return reply(args)
	}
	return mcpProvisionResult(map[string]any{"status": "ok", "tenant_id": args["tenant_id"], "approved": true})
}

func (a *transportStepUpMCPApp) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

func (a *transportStepUpMCPApp) answer(f func(args map[string]any) mcpfixture.Result) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reply = f
}

// transportStepUpMCPOpts vary what the package approves and what the server offers.
type transportStepUpMCPOpts struct {
	// approved puts the approval tool on the adapter configuration's tools.allow.
	approved bool
	// listed makes the server list it.
	listed bool
	// without removes these tools from tools.allow.
	without []string
}

// transportStepUpMCPWorld is the clinic package audited against a fixture MCP server that offers the
// provisioning tools and, as the options say, the approval tool.
func transportStepUpMCPWorld(t *testing.T, o transportStepUpMCPOpts, edit ...func(pkg *apppkg.Package)) (*mcpProvisionWorld, *transportStepUpMCPApp) {
	t.Helper()
	dirOf := func(dir string) string {
		return mcpProvisionCopyPackage(t, dir, func(cfg string) string {
			var keep []string
			for _, line := range strings.Split(cfg, "\n") {
				drop := false
				for _, tool := range o.without {
					drop = drop || strings.TrimSpace(line) == "- "+tool
				}
				if !drop {
					keep = append(keep, line)
				}
			}
			cfg = strings.Join(keep, "\n")
			if o.approved {
				cfg = strings.Replace(cfg, "    - test_cleanup\n", "    - test_cleanup\n    - "+mcpProvisionApproveTool+"\n", 1)
			}
			return cfg
		})
	}
	w := mcpProvisionFixture(t, true, dirOf, edit...)
	app := &transportStepUpMCPApp{}
	if o.listed {
		w.srv.Add(mcpfixture.Tool{
			Name:  mcpProvisionApproveTool,
			Input: map[string]any{"type": "object", "properties": map[string]any{"tenant_id": map[string]any{"type": "string"}}},
		}, app.handle)
	}
	return w, app
}

// The approval tool is a convention of the bridge, like the change-record and fault tools: the
// provisioner implements probes.StepUpCompleter exactly when the package approves the tool, no
// capability maps it and the server lists it, and it sends the application the identifiers of a
// disposable tenant of its own and the facts of the Prepare reply, and trusts nothing it says back.
func TestMCPStepUpCompleterWhenTheAppOffersAnApprovalTool(t *testing.T) {
	type abilities struct{ change, fault, stepUp bool }
	has := func(p probes.Provisioner) abilities {
		_, change := p.(probes.RecordChanger)
		_, fault := p.(probes.FaultInjector)
		_, stepUp := p.(probes.StepUpCompleter)
		return abilities{change, fault, stepUp}
	}

	t.Run("the ability is there only when the tool is approved, mapped by no capability, and listed", func(t *testing.T) {
		w, _ := transportStepUpMCPWorld(t, transportStepUpMCPOpts{approved: true, listed: true})
		if got := has(mcpProvisionMustSource(t, w.in)); got != (abilities{change: true, fault: true, stepUp: true}) {
			t.Errorf("abilities = %+v, want all three", got)
		}

		w, _ = transportStepUpMCPWorld(t, transportStepUpMCPOpts{approved: true, listed: true, without: []string{mcpProvisionChangeTool, mcpProvisionFaultTool}})
		if got := has(mcpProvisionMustSource(t, w.in)); got != (abilities{stepUp: true}) {
			t.Errorf("abilities = %+v, want only StepUpCompleter", got)
		}
		w, _ = transportStepUpMCPWorld(t, transportStepUpMCPOpts{approved: true, listed: true, without: []string{mcpProvisionFaultTool}})
		if got := has(mcpProvisionMustSource(t, w.in)); got != (abilities{change: true, stepUp: true}) {
			t.Errorf("abilities = %+v, want RecordChanger and StepUpCompleter", got)
		}
		w, _ = transportStepUpMCPWorld(t, transportStepUpMCPOpts{approved: true, listed: true, without: []string{mcpProvisionChangeTool}})
		if got := has(mcpProvisionMustSource(t, w.in)); got != (abilities{fault: true, stepUp: true}) {
			t.Errorf("abilities = %+v, want FaultInjector and StepUpCompleter", got)
		}

		// approved, and the server does not list it: no ability, and the other two are unaffected
		w, _ = transportStepUpMCPWorld(t, transportStepUpMCPOpts{approved: true})
		if got := has(mcpProvisionMustSource(t, w.in)); got != (abilities{change: true, fault: true}) {
			t.Errorf("abilities = %+v, want the other two only: the server does not list the approval tool", got)
		}

		// listed, and the package does not approve it: no ability, and the tool is not even pinned
		w, _ = transportStepUpMCPWorld(t, transportStepUpMCPOpts{listed: true})
		if got := has(mcpProvisionMustSource(t, w.in)); got != (abilities{change: true, fault: true}) {
			t.Errorf("abilities = %+v, want the other two only: the package does not approve the approval tool", got)
		}
		audited := w.in.Adapter.(transportsClosing).inner.(*mcptransport.Adapter)
		if pin := audited.Pins()["clinic-mcp/"+mcpProvisionApproveTool]; pin != "" {
			t.Error("the tool the package does not approve was pinned: the bridge looked at a tool it must ignore")
		}

		// a tool a capability maps is the adapter contract's: no ability
		w, _ = transportStepUpMCPWorld(t, transportStepUpMCPOpts{approved: true, listed: true}, func(pkg *apppkg.Package) {
			c := pkg.Capabilities["availability.list"]
			tool := ap.OperationName(mcpProvisionApproveTool)
			c.Operations.Validate = &ap.CapabilityOperationBinding{Endpoint: "clinic-mcp", MCPTool: &tool}
			pkg.Capabilities["availability.list"] = c
		})
		if got := has(mcpProvisionMustSource(t, w.in)); got.stepUp {
			t.Errorf("abilities = %+v: a tool a capability maps must not be offered as a step-up completer", got)
		}
	})

	t.Run("it sends the identifiers of its own tenant and the facts of the Prepare reply, and nothing else", func(t *testing.T) {
		w, app := transportStepUpMCPWorld(t, transportStepUpMCPOpts{approved: true, listed: true})
		ctx := mcpProvisionCtx(t)
		prov := mcpProvisionMustSource(t, w.in)
		completer, ok := prov.(probes.StepUpCompleter)
		if !ok {
			t.Fatal("the provisioner of an application that offers the approval tool is not a step-up completer")
		}
		tenant := mcpProvisionTenantOf(mcpProvisionSuffix)
		if err := prov.CreateTenant(ctx, tenant); err != nil {
			t.Fatal(err)
		}
		oversize := strings.Repeat("x", mcpProvisionMaxStepUpFactBytes+1)
		step := transportStepUpOf(tenant.ID, transportStepUpPrincipal(tenant, 1), "appointments.cancel",
			transportStepUpIdentifierFact("step_up_reference", "SU-0001"), transportStepUpTextFact("asset_name", "Backup generator"),
			transportStepUpIntegerFact("open_work_orders", 3), transportStepUpTextFact("too_long", oversize))
		if err := completer.CompleteStepUp(ctx, step); err != nil {
			t.Fatalf("CompleteStepUp: %v", err)
		}
		want := transportStepUpWant(step, map[string]any{"step_up_reference": "SU-0001", "asset_name": "Backup generator"})
		if app.count() != 1 || cmp.Diff(want, app.calls[0]) != "" {
			t.Fatalf("the application was asked %v, want %v", app.calls, want)
		}

		// at most as many facts as the bound
		var many []act.Fact
		for i := 0; i < mcpProvisionMaxStepUpFacts+10; i++ {
			many = append(many, transportStepUpTextFact(fmt.Sprintf("f%03d", i), "v"))
		}
		if err := completer.CompleteStepUp(ctx, transportStepUpOf(tenant.ID, transportStepUpPrincipal(tenant, 1), "appointments.cancel", many...)); err != nil {
			t.Fatal(err)
		}
		if got := len(app.calls[1]["values"].(map[string]any)); got != mcpProvisionMaxStepUpFacts {
			t.Errorf("%d facts were sent, want the bound of %d", got, mcpProvisionMaxStepUpFacts)
		}
	})

	t.Run("it refuses what it was not made for before anything reaches the application", func(t *testing.T) {
		w, app := transportStepUpMCPWorld(t, transportStepUpMCPOpts{approved: true, listed: true})
		ctx := mcpProvisionCtx(t)
		prov := mcpProvisionMustSource(t, w.in)
		tenant := mcpProvisionTenantOf(mcpProvisionSuffix)
		if err := prov.CreateTenant(ctx, tenant); err != nil {
			t.Fatal(err)
		}
		transportStepUpRefusals(t, tenant, func(s probes.StepUp) error { return prov.(probes.StepUpCompleter).CompleteStepUp(ctx, s) }, app.count)

		// a second provisioner of the same audit (the audit builds one for every check) has created no tenant
		other := mcpProvisionMustSource(t, w.in)
		step := transportStepUpOf(tenant.ID, transportStepUpPrincipal(tenant, 0), "appointments.cancel", transportStepUpIdentifierFact("step_up_reference", "SU-0001"))
		if err := other.(probes.StepUpCompleter).CompleteStepUp(ctx, step); contracts.CodeOf(err) != contracts.ErrPermissionDenied || mcpProvisionReasonOf(err) != mcpProvisionReasonNotOurs {
			t.Errorf("a tenant another provisioner created: %v, want permission_denied", err)
		}
		if app.count() != 0 {
			t.Errorf("%d requests reached the application", app.count())
		}
	})

	t.Run("it does not trust what the application says", func(t *testing.T) {
		w, app := transportStepUpMCPWorld(t, transportStepUpMCPOpts{approved: true, listed: true})
		ctx := mcpProvisionCtx(t)
		prov := mcpProvisionMustSource(t, w.in)
		tenant := mcpProvisionTenantOf(mcpProvisionSuffix)
		if err := prov.CreateTenant(ctx, tenant); err != nil {
			t.Fatal(err)
		}
		step := transportStepUpOf(tenant.ID, transportStepUpPrincipal(tenant, 0), "appointments.cancel", transportStepUpIdentifierFact("step_up_reference", "SU-0001"))
		with := func(extra map[string]any) mcpfixture.Result {
			body := map[string]any{"status": "ok", "tenant_id": tenant.ID, "approved": true}
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
			reply mcpfixture.Result
			code  contracts.ErrCode // "" is success; unavailable is not asserted for a tool error
		}{
			"approved":                    {with(nil), ""},
			"approved false":              {with(map[string]any{"approved": false}), contracts.ErrSchemaInvalid},
			"approved missing":            {with(map[string]any{"approved": nil}), contracts.ErrSchemaInvalid},
			"another tenant":              {with(map[string]any{"tenant_id": "tnt_other"}), contracts.ErrSchemaInvalid},
			"no status":                   {with(map[string]any{"status": nil}), contracts.ErrSchemaInvalid},
			"a refusal of its own":        {mcpProvisionRejected("expired"), contracts.ErrExpired},
			"a refusal of no meaning":     {mcpProvisionRejected("no-such-code"), contracts.ErrValidationFailed},
			"a refusal for the principal": {mcpProvisionRejected("permission_denied"), contracts.ErrPermissionDenied},
			"a tool error with its text":  {mcpfixture.Result{Text: mcpProvisionCanary, IsError: true}, "-"},
		} {
			app.answer(func(map[string]any) mcpfixture.Result { return c.reply })
			err := prov.(probes.StepUpCompleter).CompleteStepUp(ctx, step)
			switch {
			case c.code == "" && err != nil:
				t.Errorf("%s: %v", name, err)
			case c.code != "" && err == nil:
				t.Errorf("%s: accepted", name)
			case c.code != "" && c.code != "-" && contracts.CodeOf(err) != c.code:
				t.Errorf("%s: %v, want %s", name, err, c.code)
			}
			if err != nil && strings.Contains(err.Error(), mcpProvisionCanary) {
				t.Errorf("%s: text the server chose reached an error: %v", name, err)
			}
		}
	})
}

// ---- OpenAPI -----------------------------------------------------------------------------------

// transportStepUpApprovalPath is the operation the fixture's document gains when the adapter
// configuration approves the completion of a step-up: the conventional operationId, the tenant as a
// path parameter and the statuses an application can answer with. The anchors are the ones of
// openapiProvisionPaths.
const transportStepUpApprovalPath = `  /tenants/{tenant_id}/step-ups:
    post:
      operationId: approveStepUp
      parameters:
        - name: tenant_id
          in: path
          required: true
          schema: { type: string }
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: "#/components/schemas/Open" }
      responses:
        "200": *open_reply
        "400": *open_reply
        "403": *open_reply
        "404": *open_reply
        "409": *open_reply
        "422": *open_reply
        "500": *open_reply
`

// transportStepUpWithApproval adds the approval operation to the fixture's document.
func transportStepUpWithApproval(t testing.TB, spec string) string {
	t.Helper()
	at := strings.Index(spec, "\ncomponents:\n")
	if at < 0 {
		t.Fatal("the fixture's document no longer has the shape these tests edit")
	}
	return spec[:at+1] + transportStepUpApprovalPath + spec[at+1:]
}

// stepUp is the application's own approval route: it says ok, for the tenant of its path.
func (a *openapiProvisionApp) stepUp(w http.ResponseWriter, r *http.Request, _ []byte) {
	openapiProvisionWrite(w, http.StatusOK, map[string]any{"status": "ok", "tenant_id": r.PathValue("tenant_id"), "approved": true})
}

// The approval operation is a convention of the bridge: the provisioner implements
// probes.StepUpCompleter exactly when the adapter configuration approves approveStepUp in
// management.provisioning. It sends the tenant as the path parameter the document declares and the
// rest as the body, and trusts nothing the application says back.
func TestOpenAPIStepUpCompleterSendsWhatTheDocumentDescribes(t *testing.T) {
	approval := "[createTenant, seedTenant, cleanupTenant, approveStepUp]"
	completerOf := func(t *testing.T, w *openapiProvisionWorld) (probes.Provisioner, probes.StepUpCompleter) {
		t.Helper()
		p := w.provisioner(t)
		c, _ := p.(probes.StepUpCompleter)
		return p, c
	}

	t.Run("without the operation in management.provisioning there is no ability", func(t *testing.T) {
		w := openapiProvisionNewWorld(t, openapiProvisionOpts{})
		if _, c := completerOf(t, w); c != nil {
			t.Error("a provisioner whose adapter configuration does not approve approveStepUp is a step-up completer")
		}
	})

	t.Run("an operation of the suite is not the completion", func(t *testing.T) {
		w := openapiProvisionNewWorld(t, openapiProvisionOpts{provisioning: approval, calls: func(p *ap.PackageTestsProvisioning) {
			p.Cleanup = openapiProvisionCall(openapiProvisionApproveOp)
		}})
		if p, err := openapiProvisionSource(openapiProvisionCtx(t), w.in); err == nil {
			if _, ok := p.(probes.StepUpCompleter); ok {
				t.Error("the operation the suite names as its cleanup was taken for the step-up completion")
			}
		}
	})

	t.Run("it sends the tenant in the path and the rest in the body", func(t *testing.T) {
		w := openapiProvisionNewWorld(t, openapiProvisionOpts{provisioning: approval})
		ctx := openapiProvisionCtx(t)
		p, c := completerOf(t, w)
		if c == nil {
			t.Fatal("the provisioner of an adapter configuration that approves approveStepUp is not a step-up completer")
		}
		tenant := openapiProvisionTenantOf(openapiProvisionSuffix)
		if err := p.CreateTenant(ctx, tenant); err != nil {
			t.Fatal(err)
		}
		step := transportStepUpOf(tenant.ID, tenant.Principals[0], "items.archive",
			transportStepUpIdentifierFact("step_up_reference", "SU-0001"), transportStepUpIntegerFact("count", 2), transportStepUpTextFact("title", "Alpha"))
		if err := c.CompleteStepUp(ctx, step); err != nil {
			t.Fatalf("CompleteStepUp: %v", err)
		}
		seen := w.app.requests("stepup")
		if len(seen) != 1 {
			t.Fatalf("%d requests reached the approval route, want 1", len(seen))
		}
		if seen[0].Method != http.MethodPost || seen[0].Path != "/tenants/"+tenant.ID+"/step-ups" {
			t.Errorf("the request was %s %s", seen[0].Method, seen[0].Path)
		}
		var body map[string]any
		if err := json.Unmarshal(seen[0].Body, &body); err != nil {
			t.Fatal(err)
		}
		want := transportStepUpWant(step, map[string]any{"step_up_reference": "SU-0001", "title": "Alpha"})
		delete(want, "tenant_id") // a path parameter, not a member of the body
		if diff := cmp.Diff(want, body); diff != "" {
			t.Errorf("the body (-want +got):\n%s", diff)
		}
		if got := seen[0].Header.Get("X-AlixAI-Identity"); got != "" {
			t.Error("an identity travelled with a provisioning operation")
		}
	})

	t.Run("it refuses what it was not made for before anything reaches the application", func(t *testing.T) {
		w := openapiProvisionNewWorld(t, openapiProvisionOpts{provisioning: approval})
		ctx := openapiProvisionCtx(t)
		p, c := completerOf(t, w)
		tenant := openapiProvisionTenantOf(openapiProvisionSuffix)
		if err := p.CreateTenant(ctx, tenant); err != nil {
			t.Fatal(err)
		}
		transportStepUpRefusals(t, tenant, func(s probes.StepUp) error { return c.CompleteStepUp(ctx, s) }, func() int { return len(w.app.requests("stepup")) })
	})

	t.Run("it does not trust what the application says", func(t *testing.T) {
		w := openapiProvisionNewWorld(t, openapiProvisionOpts{provisioning: approval})
		ctx := openapiProvisionCtx(t)
		p, c := completerOf(t, w)
		tenant := openapiProvisionTenantOf(openapiProvisionSuffix)
		if err := p.CreateTenant(ctx, tenant); err != nil {
			t.Fatal(err)
		}
		step := transportStepUpOf(tenant.ID, tenant.Principals[0], "items.archive", transportStepUpIdentifierFact("step_up_reference", "SU-0001"))
		ok := map[string]any{"status": "ok", "tenant_id": tenant.ID, "approved": true}
		refused := func(code string) map[string]any {
			return map[string]any{"status": "rejected", "errors": []any{map[string]any{"code": code, "message": openapiProvisionCanary}}}
		}
		for name, c2 := range map[string]struct {
			status int
			body   map[string]any
			code   contracts.ErrCode // "" is success
		}{
			"approved":                    {200, ok, ""},
			"approved without a status":   {200, map[string]any{"approved": true}, ""},
			"approved false":              {200, map[string]any{"status": "ok", "approved": false}, contracts.ErrSchemaInvalid},
			"approved missing":            {200, map[string]any{"status": "ok"}, contracts.ErrSchemaInvalid},
			"another tenant":              {200, map[string]any{"status": "ok", "tenant_id": "tnt_other", "approved": true}, contracts.ErrSchemaInvalid},
			"a status that means no":      {200, map[string]any{"status": "maybe", "approved": true}, contracts.ErrSchemaInvalid},
			"a refusal behind a 200":      {200, refused("expired"), contracts.ErrExpired},
			"an expired request":          {409, refused("expired"), contracts.ErrExpired},
			"a refusal of no meaning":     {422, refused("no-such-code"), contracts.ErrValidationFailed},
			"a refusal for the person":    {403, refused("permission_denied"), contracts.ErrPermissionDenied},
			"a refusal by status alone":   {409, map[string]any{"status": "rejected"}, contracts.ErrConflict},
			"a failure of the host":       {500, map[string]any{"status": "rejected"}, contracts.ErrUnavailable},
			"an unknown request":          {404, refused("scope_violation"), contracts.ErrScopeViolation},
			"a bad request by its status": {400, map[string]any{"status": "rejected"}, contracts.ErrValidationFailed},
		} {
			w.app.answer("stepup", c2.status, c2.body)
			err := c.CompleteStepUp(ctx, step)
			switch {
			case c2.code == "" && err != nil:
				t.Errorf("%s: %v", name, err)
			case c2.code != "" && contracts.CodeOf(err) != c2.code:
				t.Errorf("%s: %v, want %s", name, err, c2.code)
			}
			if err != nil && strings.Contains(err.Error(), openapiProvisionCanary) {
				t.Errorf("%s: text the application chose reached an error: %v", name, err)
			}
		}
	})
}

// ---- native ------------------------------------------------------------------------------------

// The approval method is a convention of the bridge: the provisioner implements
// probes.StepUpCompleter exactly when the adapter configuration approves approve_step_up in
// management.provisioning. It sends the arguments of a native management call and trusts nothing the
// bridge says back.
func TestNativeStepUpCompleterSendsWhatTheBridgeNeeds(t *testing.T) {
	var mu sync.Mutex
	var calls []map[string]any
	var reply func(args map[string]any) (any, error)
	held := map[string]bool{}
	handlers := map[string]bridge.ManageHandler{
		"create_tenant": func(_ context.Context, args map[string]any) (any, error) {
			mu.Lock()
			defer mu.Unlock()
			held[fmt.Sprint(args["tenant_id"])] = true
			return map[string]any{"status": "ok", "tenant_id": args["tenant_id"]}, nil
		},
		"seed": func(context.Context, map[string]any) (any, error) { return nil, errors.New("must not be asked") },
		"cleanup": func(_ context.Context, args map[string]any) (any, error) {
			return map[string]any{"status": "ok", "tenant_id": args["tenant_id"], "existed": true, "remaining": map[string]any{}}, nil
		},
		"approve_step_up": func(_ context.Context, args map[string]any) (any, error) {
			mu.Lock()
			calls = append(calls, args)
			f := reply
			mu.Unlock()
			if f != nil {
				return f(args)
			}
			return map[string]any{"status": "ok", "tenant_id": args["tenant_id"], "approved": true}, nil
		},
	}
	sent := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(calls)
	}
	answer := func(f func(args map[string]any) (any, error)) {
		mu.Lock()
		defer mu.Unlock()
		reply = f
	}

	t.Run("without the method in management.provisioning there is no ability", func(t *testing.T) {
		w := nativeProvisionWorldFor(t, nativeProvisionFakeBridge(t, handlers), nativeProvisionCopyPackage(t, func(rel string, b []byte) []byte {
			if rel == "schemas/adapters/ledger-bridge.yaml" {
				return []byte(strings.Replace(string(b), "    - \"approve_step_up\"\n", "", 1))
			}
			return b
		}))
		if _, ok := nativeProvisionMustSource(t, w.in).(probes.StepUpCompleter); ok {
			t.Error("a provisioner whose adapter configuration does not approve approve_step_up is a step-up completer")
		}
	})

	w := nativeProvisionWorldFor(t, nativeProvisionFakeBridge(t, handlers), nil)
	ctx := nativeProvisionCtx(t)
	p := nativeProvisionMustSource(t, w.in)
	c, ok := p.(probes.StepUpCompleter)
	if !ok {
		t.Fatal("the provisioner of an adapter configuration that approves approve_step_up is not a step-up completer")
	}
	tenant := nativeProvisionTenantOf(nativeProvisionSuffix)
	if err := p.CreateTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}

	t.Run("it sends the arguments of a management call", func(t *testing.T) {
		step := transportStepUpOf(tenant.ID, tenant.Principals[1], "invoices.void",
			transportStepUpIdentifierFact("void_reference", "VOID-INV-0002-1"), transportStepUpIntegerFact("count", 2), transportStepUpTextFact("invoice_number", "INV-0002"))
		if err := c.CompleteStepUp(ctx, step); err != nil {
			t.Fatalf("CompleteStepUp: %v", err)
		}
		want := transportStepUpWant(step, map[string]any{"void_reference": "VOID-INV-0002-1", "invoice_number": "INV-0002"})
		if sent() != 1 || cmp.Diff(want, calls[0]) != "" {
			t.Fatalf("the bridge was asked %v, want %v", calls, want)
		}
	})

	t.Run("it refuses what it was not made for before anything reaches the application", func(t *testing.T) {
		transportStepUpRefusals(t, tenant, func(s probes.StepUp) error { return c.CompleteStepUp(ctx, s) }, sent)
	})

	t.Run("it does not trust what the application says", func(t *testing.T) {
		step := transportStepUpOf(tenant.ID, tenant.Principals[1], "invoices.void", transportStepUpIdentifierFact("void_reference", "VOID-INV-0002-1"))
		ok := map[string]any{"status": "ok", "tenant_id": tenant.ID, "approved": true}
		refused := func(code string) map[string]any {
			return map[string]any{"status": "rejected", "errors": []any{map[string]any{"code": code, "message": "TEXT-THE-BRIDGE-CHOSE"}}}
		}
		for name, c2 := range map[string]struct {
			body map[string]any
			err  error
			code contracts.ErrCode // "" is success
		}{
			"approved":                {ok, nil, ""},
			"approved false":          {map[string]any{"status": "ok", "tenant_id": tenant.ID, "approved": false}, nil, contracts.ErrSchemaInvalid},
			"approved missing":        {map[string]any{"status": "ok", "tenant_id": tenant.ID}, nil, contracts.ErrSchemaInvalid},
			"no tenant named":         {map[string]any{"status": "ok", "approved": true}, nil, contracts.ErrSchemaInvalid},
			"another tenant":          {map[string]any{"status": "ok", "tenant_id": "tnt_other", "approved": true}, nil, contracts.ErrSchemaInvalid},
			"no status":               {map[string]any{"tenant_id": tenant.ID, "approved": true}, nil, contracts.ErrSchemaInvalid},
			"a refusal of its own":    {refused("expired"), nil, contracts.ErrExpired},
			"a refusal of no meaning": {refused("no-such-code"), nil, contracts.ErrValidationFailed},
			"a refusal for the user":  {refused("permission_denied"), nil, contracts.ErrPermissionDenied},
		} {
			answer(func(map[string]any) (any, error) { return c2.body, c2.err })
			err := c.CompleteStepUp(ctx, step)
			switch {
			case c2.code == "" && err != nil:
				t.Errorf("%s: %v", name, err)
			case c2.code != "" && contracts.CodeOf(err) != c2.code:
				t.Errorf("%s: %v, want %s", name, err, c2.code)
			}
			if err != nil && strings.Contains(err.Error(), "TEXT-THE-BRIDGE-CHOSE") {
				t.Errorf("%s: text the bridge chose reached an error: %v", name, err)
			}
		}
		// a failure of the host is an error, and what it said is not repeated
		answer(func(map[string]any) (any, error) { return nil, errors.New("TEXT-THE-BRIDGE-CHOSE") })
		if err := c.CompleteStepUp(ctx, step); err == nil || strings.Contains(err.Error(), "TEXT-THE-BRIDGE-CHOSE") {
			t.Errorf("a failure of the host: %v", err)
		}
	})
}
