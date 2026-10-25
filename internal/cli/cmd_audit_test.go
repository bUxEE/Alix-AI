package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/fsperm"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/tools/devkeys"
)

// opsEnv is a deployment on a throwaway data directory that the operator
// commands run against, one process per call.
type opsEnv struct {
	t       *testing.T
	dir     string
	cfgPath string
}

func newOpsEnv(t *testing.T, extra string) *opsEnv {
	t.Helper()
	e := &opsEnv{t: t, dir: t.TempDir()}
	e.setConfig(extra)
	return e
}

func (e *opsEnv) setConfig(extra string) {
	e.t.Helper()
	e.cfgPath = filepath.Join(e.dir, "alixai.yaml")
	body := fmt.Sprintf("data_dir: %q\n%s", filepath.ToSlash(filepath.Join(e.dir, "data")), extra)
	if err := os.WriteFile(e.cfgPath, []byte(body), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// run executes one command in its own configuration registry, like a process.
func (e *opsEnv) run(args ...string) result {
	e.t.Helper()
	auditSection = auditConfig{}
	reg := config.NewRegistry()
	reg.Register(auditSectionName, &auditSection)
	var out, errb bytes.Buffer
	// every command of these tests is two words; flags follow, then the operand
	full := append([]string{args[0], args[1], "-config", e.cfgPath}, args[2:]...)
	code := Default.Run(context.Background(), full, &Env{
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb,
		LookupEnv: func(string) (string, bool) { return "", false }, Config: reg,
	})
	return result{code, out.String(), errb.String()}
}

func (e *opsEnv) mustRun(args ...string) result {
	e.t.Helper()
	res := e.run(args...)
	if res.code != ExitOK {
		e.t.Fatalf("%s: exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), res.code, res.stdout, res.stderr)
	}
	return res
}

func decode(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
}

// genKey writes a fresh key pair and returns the files.
func (e *opsEnv) genKey(name string) (privPath, pubPath string, pub ed25519.PublicKey) {
	e.t.Helper()
	pub, priv, err := trust.GenerateKey()
	if err != nil {
		e.t.Fatal(err)
	}
	privPath, pubPath, err = trust.WriteKeyFiles(e.dir, name, priv)
	if err != nil {
		e.t.Fatal(err)
	}
	return privPath, pubPath, pub
}

// eventTypes lists the audit event types of an application's chain.
func (e *opsEnv) eventTypes(app string) []string {
	e.t.Helper()
	db, err := store.Open(context.Background(), store.Options{Path: filepath.Join(e.dir, "data", store.DefaultFileName)})
	if err != nil {
		e.t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(context.Background(),
		`SELECT event_type FROM audit_events WHERE application_id = ? AND tenant_id = '' ORDER BY chain_seq`, app)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

type runOut struct {
	RunID             string `json:"run_id"`
	ManifestHash      string `json:"manifest_hash"`
	Outcome           string `json:"outcome"`
	Activated         bool   `json:"activated"`
	StaysInQuarantine string `json:"stays_in_quarantine"`
	Summary           struct {
		Critical int `json:"critical"`
		Warnings int `json:"warnings"`
	} `json:"summary"`
	Findings []struct {
		Key        string `json:"key"`
		Severity   string `json:"severity"`
		CheckID    string `json:"check_id"`
		Accepted   bool   `json:"accepted"`
		AcceptedBy string `json:"accepted_by"`
	} `json:"findings"`
	Sections []struct {
		Number int    `json:"number"`
		Status string `json:"status"`
	} `json:"sections"`
}

// TestAuditCommandsEndToEnd walks the operator's path: trust a publisher and the
// platform's report-signing key, install into quarantine, audit, accept what an
// authorised operator accepts, audit again, and the package is active.
func TestAuditCommandsEndToEnd(t *testing.T) {
	auditChecksProbesBuiltinChecksOnly(t) // this test is about what follows an audit, not the runtime probes (M1-18)
	transportsSwapDefault(t)              // no transport: the audit records the application as unverified, whatever the binary registers
	e := newOpsEnv(t, "")
	signerPriv, signerPub, _ := e.genKey("audit-signer")

	// the platform key and the publisher key
	e.mustRun("trust", "add-key", "-app", trust.PlatformApplicationID, "-key-id", "audit-signer", "-public-key-file", signerPub, "-operator", "root")
	e.mustRun("trust", "add-key", "-app", "demo-library", "-key-id", devkeys.KeyID, "-public-key", devkeys.PublicKeyBase64(), "-allow-test-keys", "-operator", "root")

	var inst installOut
	decode(t, e.mustRun("package", "install", "-operator", "root", filepath.Join(fixtures, "valid-minimal")).stdout, &inst)
	if !inst.Created || inst.State != "quarantined" || inst.ApplicationID != "demo-library" {
		t.Fatalf("install = %+v", inst)
	}
	if inst.Next != "alixai audit run demo-library" {
		t.Errorf("next = %q", inst.Next)
	}

	// 1. with no adapter in this build, the application and runtime are unverified
	// and the sections nothing implements are not run: warnings the operator must accept
	res := e.run("audit", "run", "-signing-key", "audit-signer="+signerPriv, "-operator", "root", "demo-library")
	if res.code != ExitError {
		t.Fatalf("a package with warnings that nobody accepted must stay in quarantine: exit %d\n%s\n%s", res.code, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stderr, "no adapter is available") {
		t.Errorf("the command says why the application is unverified: %q", res.stderr)
	}
	var first runOut
	decode(t, res.stdout, &first)
	if first.Activated || first.Outcome != "needs_acceptance" || first.Summary.Critical != 0 || first.Summary.Warnings < 2 {
		t.Fatalf("run = %+v", first)
	}
	if first.StaysInQuarantine == "" {
		t.Errorf("the output must say why the package stays in quarantine")
	}
	var ids []string
	for _, f := range first.Findings {
		if f.Severity == "warning" {
			ids = append(ids, f.CheckID)
		}
		if f.Accepted {
			t.Errorf("nothing is accepted yet: %+v", f)
		}
	}
	if !strings.Contains(strings.Join(ids, ","), "surface.not_inspected") || !strings.Contains(strings.Join(ids, ","), "audit.section_not_run") {
		t.Errorf("warnings = %v", ids)
	}
	if first.Sections[0].Status != "pass" || first.Sections[6].Status != "unverified" || first.Sections[7].Status != "not_run" {
		t.Errorf("sections 1/7/8 = %v", first.Sections)
	}

	// 2. nobody is authorised to accept until the configuration says who
	accept := func(operator, key string) result {
		return e.run("audit", "accept-warning", "-finding", key, "-operator", operator, "-reason", "reviewed by the operator", "demo-library")
	}
	if res := accept("alice", first.Findings[0].Key); res.code != ExitError || !strings.Contains(res.stderr, "not authorised") {
		t.Fatalf("with no accept_operators configured nobody may accept: exit %d\n%s", res.code, res.stderr)
	}
	e.setConfig("integration_audit:\n  accept_operators: [alice]\n")
	if res := accept("mallory", first.Findings[0].Key); res.code != ExitError {
		t.Errorf("an operator who is not listed cannot accept: exit %d", res.code)
	}
	if res := e.run("audit", "accept-warning", "-finding", first.Findings[0].Key, "-operator", "alice", "demo-library"); res.code != ExitUsage {
		t.Errorf("a reason is required: exit %d", res.code)
	}
	for _, f := range first.Findings {
		if f.Severity != "warning" {
			continue
		}
		var acc acceptOut
		decode(t, mustOK(t, accept("alice", f.Key)).stdout, &acc)
		if acc.OperatorID != "alice" || acc.FindingKey != f.Key || acc.AlreadyAccepted || acc.AcceptedAt.IsZero() {
			t.Errorf("acceptance = %+v", acc)
		}
	}

	// 3. audit again: the same warnings are accepted, and the package activates
	var second runOut
	decode(t, e.mustRun("audit", "run", "-signing-key", "audit-signer="+signerPriv, "-operator", "root", "demo-library").stdout, &second)
	if !second.Activated || second.StaysInQuarantine != "" {
		t.Fatalf("run = %+v", second)
	}
	for _, f := range second.Findings {
		if f.Severity == "warning" && (!f.Accepted || f.AcceptedBy != "alice") {
			t.Errorf("a warning in the activating report is shown as accepted by whom: %+v", f)
		}
	}
	events := e.eventTypes("demo-library")
	for _, want := range []string{"package.installed", "audit_run.started", "audit_run.completed", "audit_run.warning_accepted", "package.activated"} {
		if !contains(events, want) {
			t.Errorf("audit log lacks %s: %v", want, events)
		}
	}
	if n := count(events, "audit_run.warning_accepted"); n != first.Summary.Warnings {
		t.Errorf("%d acceptances recorded, want %d", n, first.Summary.Warnings)
	}

	// 4. a second activation is a conflict; -no-activate audits a package without touching it
	res = e.run("audit", "run", "-signing-key", "audit-signer="+signerPriv, "-no-activate", "demo-library")
	if res.code != ExitError {
		t.Errorf("no quarantined package is left to audit: exit %d", res.code)
	}
}

func mustOK(t *testing.T, r result) result {
	t.Helper()
	if r.code != ExitOK {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}
	return r
}

func contains(list []string, s string) bool { return count(list, s) > 0 }

func count(list []string, s string) int {
	n := 0
	for _, x := range list {
		if x == s {
			n++
		}
	}
	return n
}

// A critical finding stops the package whatever an operator says: here the
// publisher's key is revoked after the install.
func TestAuditRunBlockedByRevokedKey(t *testing.T) {
	e := newOpsEnv(t, "integration_audit:\n  accept_operators: [alice]\n")
	signerPriv, signerPub, _ := e.genKey("audit-signer")
	e.mustRun("trust", "add-key", "-app", trust.PlatformApplicationID, "-key-id", "audit-signer", "-public-key-file", signerPub)
	e.mustRun("trust", "add-key", "-app", "demo-library", "-key-id", devkeys.KeyID, "-public-key", devkeys.PublicKeyBase64(), "-allow-test-keys")
	e.mustRun("package", "install", filepath.Join(fixtures, "valid-minimal"))
	e.mustRun("trust", "revoke-key", "-app", "demo-library", "-key-id", devkeys.KeyID, "-reason", "compromised")

	res := e.run("audit", "run", "-signing-key", "audit-signer="+signerPriv, "demo-library")
	if res.code != ExitError || !strings.Contains(res.stderr, "critical") {
		t.Fatalf("exit %d\n%s\n%s", res.code, res.stdout, res.stderr)
	}
	var out runOut
	decode(t, res.stdout, &out)
	if out.Outcome != "blocked" || out.Activated || out.Summary.Critical == 0 {
		t.Fatalf("run = %+v", out)
	}
	var crit string
	for _, f := range out.Findings {
		if f.Severity == "critical" {
			crit = f.Key
		}
	}
	if r := e.run("audit", "accept-warning", "-finding", crit, "-operator", "alice", "-reason", "ship it", "demo-library"); r.code != ExitError {
		t.Errorf("a critical finding cannot be accepted: exit %d", r.code)
	}
}

func TestAuditRunNeedsASigningKey(t *testing.T) {
	e := newOpsEnv(t, "")
	if res := e.run("audit", "run", "demo-library"); res.code != ExitUsage || !strings.Contains(res.stderr, "no signing key") {
		t.Errorf("exit %d\n%s", res.code, res.stderr)
	}
	// the published TEST-ONLY key is refused as a signing key
	keyFile := filepath.Join(e.dir, "devkey.pem")
	b, err := os.ReadFile("../../tools/devkeys/test-only-publisher.key.pem")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	// Owner only, so the refusal is about the key and not about who can read the file.
	if err := fsperm.Restrict(keyFile, false); err != nil {
		t.Fatal(err)
	}
	if res := e.run("audit", "run", "-signing-key", "x="+keyFile, "demo-library"); res.code != ExitUsage || !strings.Contains(res.stderr, "TEST-ONLY") {
		t.Errorf("exit %d\n%s", res.code, res.stderr)
	}
	for _, args := range [][]string{
		{"audit", "run"},
		{"audit", "run", "a", "b"},
		{"audit", "accept-warning", "demo-library"},
		{"audit", "run", "-operator", "has space", "demo-library"},
	} {
		if res := e.run(args...); res.code != ExitUsage {
			t.Errorf("%v: exit %d, want usage", args, res.code)
		}
	}
}

func TestAuditConfigSectionValidation(t *testing.T) {
	for name, c := range map[string]auditConfig{
		"key id without file": {SigningKeyID: "k"},
		"file without id":     {SigningKeyFile: "/x"},
		"bad operator":        {AcceptOperators: []string{"has space"}},
		"bad profile":         {Profile: "huge"},
		"negative age":        {MaxReportAgeHours: -1},
		"absurd age":          {MaxReportAgeHours: 100000},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (auditConfig{SigningKeyID: "k", SigningKeyFile: "/x", AcceptOperators: []string{"alice", "bob@example.test"}, Profile: "lite", MaxReportAgeHours: 12}).Validate(); err != nil {
		t.Errorf("valid section refused: %v", err)
	}
}
