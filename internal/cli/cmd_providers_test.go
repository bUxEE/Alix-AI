//go:build !noexternal

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/identity"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// extWireModuleRoot walks up from the test's directory to the directory with go.mod.
func extWireModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// The local-only build links nothing of milestone X: not the adapters, not the credential service, the
// OAuth engine, the data-egress policy, the provider registry, the budget, the mode router, the external
// multimodal gate, nor the dialogue wiring of external mode. The check is on the dependency graph of the
// binary built with -tags noexternal, so a new untagged import in this package fails here.
func TestNoexternalBuildStaysFree(t *testing.T) {
	const module = "github.com/buxee/alix-ai/"
	root := extWireModuleRoot(t)
	cmd := exec.Command("go", "list", "-deps", "-tags", "noexternal", "-f", "{{.ImportPath}}", "./cmd/alixai")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("go list: %v\n%s", err, errb.String())
	}
	forbidden := []string{
		"internal/dataegress", "internal/providerreg", "internal/credentials", "internal/oauth", "internal/providerbudget",
		"internal/modes", "internal/extmodal", "internal/dialogue/extmode", "adapters/external",
	}
	n := 0
	for _, pkg := range strings.Fields(out.String()) {
		if !strings.HasPrefix(pkg, module) {
			continue
		}
		n++
		rel := strings.TrimPrefix(pkg, module)
		for _, f := range forbidden {
			if rel == f || strings.HasPrefix(rel, f+"/") {
				t.Errorf("the noexternal build links %s", pkg)
			}
		}
	}
	if n < 20 {
		t.Fatalf("only %d module packages listed: the check read nothing", n)
	}
}

// extWireEnvRun runs one command of the default dispatcher over an environment that also knows the
// external_providers section, and returns the exit code and what it printed.
func extWireEnvRun(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut syncBuf
	env := serveEnv(t, &out, &errOut)
	env.Config.Register(externalSectionName, &externalSection)
	code = Default.Run(context.Background(), args, env)
	return code, out.String(), errOut.String()
}

// extWireSaveSection restores the process-wide section (and the identity section) after a test that loads one.
func extWireSaveSection(t *testing.T) {
	t.Helper()
	old, oldID := externalSection, *identity.ConfigSection()
	t.Cleanup(func() { externalSection, *identity.ConfigSection() = old, oldID })
}

// extWireTestReport is a passing conformance report for the exact provider, model id and version of p (a text-only
// profile: the three media checks are not applicable), shaped as tools/providerconf writes it.
func extWireTestReport(t *testing.T, p pv.ProviderProfile, done time.Time) []byte {
	t.Helper()
	checks := map[string][]string{
		"inference":  {"text_generation_and_streaming", "bounded_context", "structured_output", "cancellation_and_timeout", "usage_reporting", "malformed_and_error_mapping"},
		"control":    {"typed_task_proposal", "unknown_capability_refusal", "argument_schema_compliance", "no_direct_adapter_invocation", "directive_and_receipt_handling", "confirmation_and_cancellation"},
		"rag":        {"authorized_excerpt_handling", "source_reference_preservation", "no_unsupported_citations", "prompt_injection_resistance", "local_only_source_enforcement", "no_path_or_cred_leakage"},
		"multimodal": {"image_input_output", "file_input_output", "audio_input_output", "cancellation_and_partial_stream", "sanitized_rich_blocks"},
		"operations": {"creds_never_in_responses_or_logs", "quota_and_budget_enforcement", "provider_disablement", "fallback_and_outage_behavior", "tenant_isolation", "audit_completeness", "terms_and_capability_profile_validation"},
	}
	notApplicable := map[string]bool{"image_input_output": true, "file_input_output": true, "audio_input_output": true}
	groups := map[string]map[string]any{}
	for grp, names := range checks {
		groups[grp] = map[string]any{}
		for _, n := range names {
			res := "passed"
			if notApplicable[n] {
				res = "not_applicable"
			}
			groups[grp][n] = map[string]any{"result": res, "reason": nil}
		}
	}
	raw, err := json.Marshal(map[string]any{
		"report_id":    "conf_" + strings.Repeat("0", 25) + "R",
		"provider":     map[string]any{"profile_id": p.ProfileID, "provider_id": p.ProviderID, "model_id": p.ModelID, "model_version": p.ModelVersion},
		"suite":        map[string]any{"version": "1.0.0", "mode": "fixtures"},
		"declared":     map[string]any{"image": false, "file": false, "audio": false},
		"started_at":   done.Add(-4 * time.Minute).UTC().Format(time.RFC3339),
		"completed_at": done.UTC().Format(time.RFC3339),
		"outcome":      "passed", "groups": groups,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// An operator connects a provider end to end with the commands alone: register the profile, store the key (which
// answered no_profile until now), acknowledge the billing notice, record the conformance report, qualify, and then
// hold, review and retire. Nothing leaves the machine: the key is validated by a stub.
func TestProviderProfileCommandsConnectAProviderEndToEnd(t *testing.T) {
	extWireSaveSection(t)
	cfg, dataDir := minimalConfig(t, "identity:\n  deployment_id: "+externalTestDeployment+"\n  audience: alixai\n"+
		"external_providers:\n  enabled: true\n  deployment_operators: [app_demo/usr_ops]\n  tenant_admins: [\"app_demo/ten_acme/usr_admin\"]\n")
	ctx := context.Background()
	today := time.Now().UTC().Format("2006-01-02")
	hash := "sha256:" + strings.Repeat("ab", 32)
	base := []string{"-config", cfg, "-operator", "usr_ops", "-application", "app_demo", "-tenant", "ten_acme"}
	run := func(sub string, extra ...string) (profile pv.ProviderProfile) {
		t.Helper()
		args := append(append([]string{"providers", sub}, base...), extra...)
		code, out, errOut := extWireEnvRun(t, args...)
		if code != ExitOK {
			t.Fatalf("providers %s = %d\nstdout: %s\nstderr: %s", sub, code, out, errOut)
		}
		if sub != "list" {
			if err := json.Unmarshal([]byte(out), &profile); err != nil {
				t.Fatalf("providers %s printed %q: %v", sub, out, err)
			}
		}
		return profile
	}
	fails := func(sub string, extra ...string) (int, string) {
		t.Helper()
		code, _, errOut := extWireEnvRun(t, append(append([]string{"providers", sub}, base...), extra...)...)
		return code, errOut
	}

	// 1. Registration: the profile is a candidate with no credential, bound to nobody yet.
	p := run("register", "-template", "anthropic", "-reviewed-on", today, "-terms-hash", hash)
	if p.ProfileID == "" || p.Lifecycle != pv.ProfileLifecycleCandidate || p.CredRef != nil || p.ProviderID != "anthropic" || p.Terms.TermsHash != pv.SHA256(hash) {
		t.Fatalf("registered %+v", p)
	}
	id := string(p.ProfileID)

	// 2. The key can now be stored: the service of the route found a profile where it used to answer no_profile.
	db := openData(t, dataDir)
	audLog, err := audit.New(db, audit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := newExternalAdmin(externalAdminDeps{
		DB: db, Audit: audLog, DeploymentID: externalTestDeployment, Dir: filepath.Join(dataDir, "external"), Validator: externalAcceptKeys{},
		Section: externalConfig{Enabled: true, DeploymentOperators: []string{"app_demo/usr_ops"}, TenantAdmins: []string{"app_demo/ten_acme/usr_admin"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tenant := externalTestScope("ten_acme", "usr_admin")
	var in gateway.ProvidersCredentialInput
	if err := json.Unmarshal([]byte(`{"key":"sk-test-0123456789"}`), &in); err != nil {
		t.Fatal(err)
	}
	st, err := admin.SubmitCredential(ctx, tenant, "anthropic", in)
	if err != nil || st.State != pv.ConnectionStatusStateConnected {
		t.Fatalf("SubmitCredential = %+v, %v", st, err)
	}
	run("list")

	// 3. Not qualified yet: no acknowledgement, no report. The refusal says so.
	if code, errOut := fails("qualify", "-profile", id); code == ExitOK || !strings.Contains(errOut, "cannot be production-qualified") {
		t.Fatalf("qualify before the acknowledgement and the report = %d: %s", code, errOut)
	}
	if code, _ := fails("ack-billing", "-profile", id, "-notice-version", p.BillingNotice.Version); code == ExitOK {
		t.Fatal("the billing notice was acknowledged without the two confirmations")
	}
	acked := run("ack-billing", "-profile", id, "-notice-version", p.BillingNotice.Version, "-confirm-not-api-access", "-confirm-separate-billing")
	if acked.BillingNoticeAck == nil || acked.BillingNoticeAck.NoticeVersion != p.BillingNotice.Version {
		t.Fatalf("acknowledged %+v", acked.BillingNoticeAck)
	}
	live := run("review", "-profile", id, "-terms-hash", hash, "-reviewed-on", today)
	if live.CredRef == nil {
		t.Fatal("the credential was not bound to the profile that had none")
	}
	report := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(report, extWireTestReport(t, live, time.Now().Add(-time.Hour)), 0o600); err != nil {
		t.Fatal(err)
	}
	run("conformance", "-profile", id, "-report", report)

	// 4. Qualified; the host sees a connected, qualified provider.
	if q := run("qualify", "-profile", id); q.Lifecycle != pv.ProfileLifecycleQualified {
		t.Fatalf("qualify left it %s", q.Lifecycle)
	}
	conns, err := admin.Connections(ctx, tenant)
	if err != nil || len(conns) != 1 || conns[0].State != pv.ConnectionStatusStateConnected || conns[0].Lifecycle != pv.ProfileLifecycleQualified || !conns[0].BillingNoticeAcknowledged {
		t.Fatalf("connections = %+v, %v", conns, err)
	}

	// 5. Hold quarantines it; release refuses a reason it does not own; review ends the hold; retire is final.
	if h := run("hold", "-profile", id); h.Lifecycle != pv.ProfileLifecycleQuarantined {
		t.Fatalf("hold left it %s", h.Lifecycle)
	}
	if code, _ := fails("release", "-profile", id); code == ExitOK {
		t.Fatal("an operator hold was released without a review")
	}
	if r := run("review", "-profile", id, "-terms-hash", hash, "-reviewed-on", today); r.Lifecycle != pv.ProfileLifecycleCandidate {
		t.Fatalf("review left it %s", r.Lifecycle)
	}
	if r := run("retire", "-profile", id); r.Lifecycle != pv.ProfileLifecycleRetired {
		t.Fatalf("retire left it %s", r.Lifecycle)
	}

	// 6. The authority is the configuration: a stranger and a disabled section change nothing.
	stranger := []string{"providers", "list", "-config", cfg, "-operator", "usr_stranger", "-application", "app_demo"}
	if code, _, errOut := extWireEnvRun(t, stranger...); code == ExitOK || !strings.Contains(errOut, "deployment_operators") {
		t.Fatalf("a stranger = %d: %s", code, errOut)
	}
	off, _ := minimalConfig(t, "identity:\n  deployment_id: "+externalTestDeployment+"\n  audience: alixai\n")
	externalSection = externalConfig{}
	if code, _, errOut := extWireEnvRun(t, "providers", "list", "-config", off, "-operator", "usr_ops", "-application", "app_demo"); code == ExitOK || !strings.Contains(errOut, "external mode is off") {
		t.Fatalf("external mode off = %d: %s", code, errOut)
	}
}

// The pricing table and the billing notice of a registered profile are changed with their own commands, offline,
// under the same operator authority as every other providers command.
func TestExtFinalSetterCommandsChangePricingAndBillingNotice(t *testing.T) {
	extWireSaveSection(t)
	cfg, _ := minimalConfig(t, "identity:\n  deployment_id: "+externalTestDeployment+"\n  audience: alixai\n"+
		"external_providers:\n  enabled: true\n  deployment_operators: [app_demo/usr_ops]\n")
	today := time.Now().UTC().Format("2006-01-02")
	hash := "sha256:" + strings.Repeat("ab", 32)
	base := []string{"-config", cfg, "-operator", "usr_ops", "-application", "app_demo", "-tenant", "ten_acme"}
	run := func(sub string, extra ...string) pv.ProviderProfile {
		t.Helper()
		code, out, errOut := extWireEnvRun(t, append(append([]string{"providers", sub}, base...), extra...)...)
		if code != ExitOK {
			t.Fatalf("providers %s = %d\nstdout: %s\nstderr: %s", sub, code, out, errOut)
		}
		var p pv.ProviderProfile
		if err := json.Unmarshal([]byte(out), &p); err != nil {
			t.Fatalf("providers %s printed %q: %v", sub, out, err)
		}
		return p
	}
	fails := func(sub string, extra ...string) (int, string) {
		t.Helper()
		code, _, errOut := extWireEnvRun(t, append(append([]string{"providers", sub}, base...), extra...)...)
		return code, errOut
	}
	write := func(v any) string {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "in.json")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	p := run("register", "-template", "anthropic", "-reviewed-on", today, "-terms-hash", hash)
	id := string(p.ProfileID)

	// Pricing: the table is replaced and everything else is kept.
	table := p.Pricing
	if table.Prices.ToksIn == nil {
		t.Fatal("the template has no input price to change")
	}
	changed := *table.Prices.ToksIn
	changed.MicroUnits++
	table.Prices.ToksIn = &changed
	got := run("set-pricing", "-profile", id, "-pricing-file", write(table))
	if got.Pricing.Prices.ToksIn == nil || got.Pricing.Prices.ToksIn.MicroUnits != changed.MicroUnits {
		t.Fatalf("the price was not replaced: %+v", got.Pricing.Prices.ToksIn)
	}
	if got.ProfileID != p.ProfileID || got.Lifecycle != p.Lifecycle || got.Terms.TermsHash != p.Terms.TermsHash {
		t.Fatalf("a price change must keep the profile: %+v", got)
	}
	// A price that is not valid, a file that is not a table and a missing flag change nothing.
	bad := table
	bad.Currency = "eur"
	if code, _ := fails("set-pricing", "-profile", id, "-pricing-file", write(bad)); code == ExitOK {
		t.Fatal("a currency that is not three upper case letters was accepted")
	}
	if code, _ := fails("set-pricing", "-profile", id, "-pricing-file", write(map[string]any{"currency": 3})); code == ExitOK {
		t.Fatal("a file that is not a price table was accepted")
	}
	if code, _ := fails("set-pricing", "-profile", id); code != ExitUsage {
		t.Fatalf("no -pricing-file = %d, want usage", code)
	}
	if code, _ := fails("set-pricing", "-profile", "pprf_"+strings.Repeat("0", 26), "-pricing-file", write(table)); code == ExitOK {
		t.Fatal("the price of a profile that does not exist was set")
	}

	// Billing notice: acknowledge, then a changed notice drops the acknowledgement; the same notice is a no-op.
	run("ack-billing", "-profile", id, "-notice-version", p.BillingNotice.Version, "-confirm-not-api-access", "-confirm-separate-billing")
	same := run("set-billing-notice", "-profile", id, "-billing-notice-file", write(p.BillingNotice))
	if same.BillingNoticeAck == nil {
		t.Fatal("an unchanged notice dropped the acknowledgement")
	}
	notice := p.BillingNotice
	notice.Version += "-next"
	notice.Text += " Updated."
	next := run("set-billing-notice", "-profile", id, "-billing-notice-file", write(notice))
	if next.BillingNotice.Version != notice.Version || next.BillingNoticeAck != nil {
		t.Fatalf("a changed notice must replace the notice and drop the acknowledgement: %+v / %+v", next.BillingNotice, next.BillingNoticeAck)
	}
	claim := map[string]any{"version": notice.Version, "text": notice.Text, "subscription_grants_api_access": true}
	if code, _ := fails("set-billing-notice", "-profile", id, "-billing-notice-file", write(claim)); code == ExitOK {
		t.Fatal("a notice that claims a subscription is API access was accepted")
	}

	// A retired profile is not changed, by either command.
	run("retire", "-profile", id)
	if code, _ := fails("set-pricing", "-profile", id, "-pricing-file", write(table)); code == ExitOK {
		t.Fatal("the price of a retired profile was set")
	}
	if code, _ := fails("set-billing-notice", "-profile", id, "-billing-notice-file", write(notice)); code == ExitOK {
		t.Fatal("the notice of a retired profile was set")
	}

	// The authority is the configuration.
	other := []string{"providers", "set-pricing", "-config", cfg, "-operator", "usr_stranger", "-application", "app_demo", "-profile", id, "-pricing-file", write(table)}
	if code, _, errOut := extWireEnvRun(t, other...); code == ExitOK || !strings.Contains(errOut, "deployment_operators") {
		t.Fatalf("a stranger = %d: %s", code, errOut)
	}
}
