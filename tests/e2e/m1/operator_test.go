package m1

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/cli"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/tools/devkeys"
)

// The identifiers of the deployment under test.
const (
	clinicApplication = "clinic-booking"
	deploymentID      = "dep_m1_gate"
	// audience is what the clinic's context exchange (alixai.yaml) says its tokens carry.
	audience = "alixai"
	// auditSignerID is the platform key that signs integration-audit reports. It is generated
	// for the test: `alixai audit run` refuses the published TEST-ONLY key for that role.
	auditSignerID  = "m1-gate-audit-signer"
	gateOperator   = "m1-gate-operator"
	strangerKeyID  = "m1-gate-stranger"
	clinicPkgDir   = clinicDir + "/package"
	signedAtOfPkg  = "2026-10-01T00:00:00Z"
	signExpiresPkg = "2036-10-01T00:00:00Z"
)

// cliMu serialises the in-process command line: the configuration registry and the
// sections it fills are process-wide, as they are in the real binary, one process per command.
var cliMu sync.Mutex

// cliResult is what one `alixai` command did.
type cliResult struct {
	Code   int
	Stdout string
	Stderr string
}

// operator is the deployment's operator: a data directory, a configuration file and the real
// `alixai` command line run in-process against them.
type operator struct {
	t       tb
	root    string
	dataDir string
	cfgPath string
	// signerKeyPath is the private half of the platform key that signs audit reports.
	signerKeyPath string
}

// newOperator creates an empty deployment: a data directory, the platform audit-signing key
// (registered in the trust store under alixai.platform), the publisher key of the clinic's
// package (the published TEST-ONLY key, accepted only because the operator says so) and a
// configuration that names who may accept audit warnings.
func newOperator(t tb) *operator {
	t.Helper()
	root := t.TempDir()
	o := &operator{t: t, root: root, dataDir: filepath.Join(root, "data")}

	pub, priv, err := trust.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	privPath, pubPath, err := trust.WriteKeyFiles(root, auditSignerID, priv)
	if err != nil {
		t.Fatal(err)
	}
	_ = pub
	o.signerKeyPath = privPath

	o.cfgPath = filepath.Join(root, "alixai.yaml")
	cfg := fmt.Sprintf(`data_dir: %q
identity:
  deployment_id: %s
  audience: %s
integration_audit:
  signing_key_id: %s
  signing_key_file: %q
  accept_operators: [%s]
`, filepath.ToSlash(o.dataDir), deploymentID, audience, auditSignerID, filepath.ToSlash(privPath), gateOperator)
	if err := os.WriteFile(o.cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	o.must("trust", "add-key", "-app", trust.PlatformApplicationID, "-key-id", auditSignerID, "-public-key-file", pubPath, "-operator", "root")
	o.must("trust", "add-key", "-app", clinicApplication, "-key-id", devkeys.KeyID, "-public-key", devkeys.PublicKeyBase64(),
		"-allow-test-keys", "-operator", "root")
	return o
}

// run executes one command of the real binary in-process: its own arguments, a configuration
// registry that is the process's own (every section the binary registers), no environment.
// The configuration flag is added after the command's name (one or two words).
func (o *operator) run(args ...string) cliResult {
	o.t.Helper()
	cliMu.Lock()
	defer cliMu.Unlock()
	words := 1
	if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
		words = 2
	}
	full := append(append(append([]string{}, args[:words]...), "-config", o.cfgPath), args[words:]...)
	var out, errb bytes.Buffer
	code := cli.Run(context.Background(), full, &cli.Env{
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb,
		LookupEnv: func(string) (string, bool) { return "", false },
		Config:    config.Default,
	})
	return cliResult{Code: code, Stdout: out.String(), Stderr: errb.String()}
}

// must runs a command that has to succeed.
func (o *operator) must(args ...string) cliResult {
	o.t.Helper()
	r := o.run(args...)
	if r.Code != cli.ExitOK {
		o.t.Fatalf("alixai %s: exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), r.Code, r.Stdout, r.Stderr)
	}
	return r
}

// decode reads a command's JSON output.
func decodeOut(t tb, what, out string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(out), v); err != nil {
		t.Fatalf("%s: the output is not the JSON that was expected: %v\n%s", what, err, out)
	}
}

// ---- the signed package under test -----------------------------------------------------------

// packageMutation changes a copy of the clinic package before it is signed.
type packageMutation func(t tb, dir string)

// signedClinicPackage copies the clinic's own package, points its MCP endpoint (and the egress
// entry that allows exactly that endpoint) at the port the clinic subprocess got, applies the
// mutations, and signs the result with the TEST-ONLY publisher key. Nothing else of the package
// changes: its capabilities, risk levels, fields, policy and conversation contracts are the
// reference application's.
func signedClinicPackage(t tb, clinic *clinicApp, mutations ...packageMutation) string {
	t.Helper()
	dir := copyTree(t, clinicPkgDir)
	retarget(t, dir, clinic.port)
	for _, m := range mutations {
		m(t, dir)
	}
	signPackage(t, dir, devkeys.SigningKey())
	return dir
}

// retarget rewrites the one port the manifest pins, in both places that name it.
func retarget(t tb, dir string, port int) {
	t.Helper()
	editFile(t, filepath.Join(dir, "alixai.yaml"), func(s string) string {
		for _, r := range [][2]string{
			{"url: http://127.0.0.1:8787/mcp", fmt.Sprintf("url: http://127.0.0.1:%d/mcp", port)},
			{"      - 8787\n", fmt.Sprintf("      - %d\n", port)},
		} {
			if strings.Count(s, r[0]) != 1 {
				t.Fatalf("alixai.yaml has no single %q to retarget", r[0])
			}
			s = strings.Replace(s, r[0], r[1], 1)
		}
		return s
	})
}

func signPackage(t tb, dir string, key trust.SigningKey) {
	t.Helper()
	signedAt, err := time.Parse(time.RFC3339, signedAtOfPkg)
	if err != nil {
		t.Fatal(err)
	}
	expires, err := time.Parse(time.RFC3339, signExpiresPkg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trust.SignPackageDir(dir, trust.SignOptions{
		PackageIdentity: trust.PackageIdentity{ApplicationID: clinicApplication, PackageVersion: "1.0.0"},
		Keys:            []trust.SigningKey{key}, Now: signedAt, ExpiresAt: expires,
	}); err != nil {
		t.Fatalf("sign the package: %v", err)
	}
}

// editFile rewrites a file in place and requires that the edit changed it.
func editFile(t tb, path string, edit func(string) string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	next := edit(string(b))
	if next == string(b) {
		t.Fatalf("editing %s changed nothing", path)
	}
	if err := os.WriteFile(path, []byte(next), 0o600); err != nil {
		t.Fatal(err)
	}
}

// copyTree copies a directory so a test can change its copy.
func copyTree(t tb, from string) string {
	t.Helper()
	to := t.TempDir()
	err := filepath.Walk(from, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(to, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return to
}

// ---- install, audit, activate ----------------------------------------------------------------

type installResult struct {
	Created      bool   `json:"created"`
	Application  string `json:"application_id"`
	ManifestHash string `json:"manifest_hash"`
	State        string `json:"state"`
	SignedBy     string `json:"signed_by"`
	Path         string `json:"path"`
}

// install installs a package into quarantine and returns its record.
func (o *operator) install(pkgDir string) installResult {
	o.t.Helper()
	r := o.must("package", "install", "-operator", "root", pkgDir)
	var out installResult
	decodeOut(o.t, "package install", r.Stdout, &out)
	if out.State != "quarantined" {
		o.t.Fatalf("an installed package is quarantined until an audit activates it, got %q", out.State)
	}
	return out
}

type auditFinding struct {
	Key        string `json:"key"`
	Severity   string `json:"severity"`
	CheckID    string `json:"check_id"`
	Capability string `json:"capability"`
	Message    string `json:"message"`
	Accepted   bool   `json:"accepted"`
	AcceptedBy string `json:"accepted_by"`
}

// auditRun is what `alixai audit run` prints.
type auditRun struct {
	RunID             string `json:"run_id"`
	ManifestHash      string `json:"manifest_hash"`
	Outcome           string `json:"outcome"`
	Activated         bool   `json:"activated"`
	StaysInQuarantine string `json:"stays_in_quarantine"`
	Summary           struct {
		Critical int `json:"critical"`
		Warnings int `json:"warnings"`
	} `json:"summary"`
	Targets  []string       `json:"targets"`
	Findings []auditFinding `json:"findings"`
	Sections []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Status string `json:"status"`
	} `json:"sections"`
}

func (a auditRun) severities(sev string) []auditFinding {
	var out []auditFinding
	for _, f := range a.Findings {
		if f.Severity == sev {
			out = append(out, f)
		}
	}
	return out
}

func (a auditRun) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "outcome %s, activated %v, critical %d, warnings %d", a.Outcome, a.Activated, a.Summary.Critical, a.Summary.Warnings)
	for _, f := range a.Findings {
		fmt.Fprintf(&b, "\n  %s %s [%s] %s", f.Severity, f.CheckID, f.Capability, f.Message)
	}
	return b.String()
}

// audit runs the integration audit of the application's newest quarantined package, which
// activates it only when the audit is clear. The exit code is the command's: zero only when the
// package is active now.
func (o *operator) audit() (auditRun, cliResult) {
	o.t.Helper()
	return o.auditPackage("")
}

// auditPackage is audit for the installed package with the given manifest hash.
func (o *operator) auditPackage(hash string) (auditRun, cliResult) {
	o.t.Helper()
	args := []string{"audit", "run", "-operator", gateOperator}
	if hash != "" {
		args = append(args, "-package", hash)
	}
	r := o.run(append(args, clinicApplication)...)
	var out auditRun
	if strings.TrimSpace(r.Stdout) != "" {
		decodeOut(o.t, "audit run", r.Stdout, &out)
	}
	return out, r
}

// acceptWarning records that the authorised operator accepted a warning of the latest report.
func (o *operator) acceptWarning(key, reason string) {
	o.t.Helper()
	o.must("audit", "accept-warning", "-finding", key, "-operator", gateOperator, "-reason", reason, clinicApplication)
}

// activate is the whole operator path of a deployment that wants the application serving: the
// audit, then the acceptance by the authorised operator of every warning it raised (and of no
// critical finding, which cannot be accepted), then the audit again. It fails the test on a
// critical finding, and returns the run that activated the package.
func (o *operator) activate() auditRun {
	o.t.Helper()
	first, res := o.audit()
	if first.Summary.Critical != 0 {
		o.t.Fatalf("the clinic's audit has critical findings:\n%s\nstderr: %s", first, res.Stderr)
	}
	if first.Activated {
		return first
	}
	if first.Outcome != "needs_acceptance" {
		o.t.Fatalf("the audit neither activated the package nor asked for acceptance: %s\nstderr: %s", first, res.Stderr)
	}
	for _, f := range first.severities("warning") {
		o.acceptWarning(f.Key, "reviewed by the gate: "+f.CheckID)
	}
	second, res := o.audit()
	if !second.Activated {
		o.t.Fatalf("every warning was accepted and the package still did not activate: %s\nstderr: %s", second, res.Stderr)
	}
	return second
}

// quarantine takes the active package out of service at once.
func (o *operator) quarantine(reason string) cliResult {
	o.t.Helper()
	return o.must("audit", "quarantine", "-operator", gateOperator, "-reason", reason, clinicApplication)
}

// addIssuerKey provisions the key the clinic signs its context tokens with: the operator step
// that lets AlixAI verify tokens the host mints (SEC-037), checked against the package's own
// context_exchange block.
func (o *operator) addIssuerKey(pkgDir string) {
	o.t.Helper()
	o.must("identity", "add-issuer-key", "-package", pkgDir, "-key-id", devkeys.KeyID, "-public-key", devkeys.PublicKeyBase64(), "-allow-test-key")
}
