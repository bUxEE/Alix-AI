package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/apps"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/internal/version"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/tools/devkeys"
)

const fixtures = "../../examples/packages"

type validateOut struct {
	OK             bool   `json:"ok"`
	ApplicationID  string `json:"application_id"`
	PackageVersion string `json:"package_version"`
	ManifestHash   string `json:"manifest_hash"`
	Authenticity   string `json:"authenticity"`
	SignedBy       string `json:"signed_by"`
	CoreVersion    string `json:"core_version"`
	Files          int    `json:"files"`
	Critical       int    `json:"critical"`
	Warnings       int    `json:"warnings"`
	Findings       []struct {
		CheckID     string `json:"check_id"`
		Severity    string `json:"severity"`
		Capability  string `json:"capability"`
		Path        string `json:"path"`
		Message     string `json:"message"`
		Remediation string `json:"remediation"`
	} `json:"findings"`
}

func validate(t *testing.T, args ...string) (result, validateOut) {
	t.Helper()
	res, _ := run(t, Default, nil, append([]string{"package", "validate"}, args...)...)
	var out validateOut
	if strings.HasPrefix(res.stdout, "{") {
		if err := json.Unmarshal([]byte(res.stdout), &out); err != nil {
			t.Fatalf("stdout is not the JSON report: %v\n%s", err, res.stdout)
		}
	}
	return res, out
}

func (o validateOut) checks(severity string) []string {
	seen := map[string]bool{}
	for _, f := range o.Findings {
		if f.Severity == severity {
			seen[f.CheckID] = true
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// TestPackageValidateValidMinimal is the acceptance command: the valid fixture
// exits 0, prints the report as JSON and says plainly that without a trust
// anchor nobody vouched for the signature.
func TestPackageValidateValidMinimal(t *testing.T) {
	if version.Version != "" {
		t.Skip("the binary is stamped with a version; this test needs a development build")
	}
	res, out := validate(t, filepath.Join(fixtures, "valid-minimal"))
	if res.code != ExitOK {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	if !out.OK || out.Critical != 0 || out.ApplicationID != "demo-library" || out.PackageVersion != "1.0.0" || len(out.ManifestHash) != 64 {
		t.Fatalf("report = %+v", out)
	}
	if out.CoreVersion != apppkg.DevCoreVersion {
		t.Errorf("core version = %q, want the development version %q", out.CoreVersion, apppkg.DevCoreVersion)
	}
	if out.Authenticity != "integrity_only" || out.SignedBy != "" {
		t.Errorf("authenticity = %q signed_by = %q; with no trust anchor nobody is established as the signer", out.Authenticity, out.SignedBy)
	}
	if got := out.checks("warning"); len(got) != 1 || got[0] != "signature.not_checked" {
		t.Errorf("warnings = %v, want only signature.not_checked", got)
	}
	if res.stderr != "" {
		t.Errorf("stderr = %q", res.stderr)
	}
}

func TestPackageValidateWithTrustKey(t *testing.T) {
	dir := filepath.Join(fixtures, "valid-minimal")
	key := "alixai-test-only-publisher=" + devkeys.PublicKeyBase64()

	t.Run("the published test key needs the explicit flag", func(t *testing.T) {
		res, _ := validate(t, "-trust-key", key, dir)
		if res.code != ExitUsage || !strings.Contains(res.stderr, "TEST-ONLY") {
			t.Fatalf("exit = %d stderr = %q", res.code, res.stderr)
		}
	})
	t.Run("verified when the key is trusted", func(t *testing.T) {
		res, out := validate(t, "-trust-key", key, "-allow-test-keys", dir)
		if res.code != ExitOK || out.Authenticity != "verified" || out.SignedBy != devkeys.KeyID || len(out.Findings) != 0 {
			t.Fatalf("exit = %d out = %+v\n%s", res.code, out, res.stderr)
		}
	})
	t.Run("a key that did not sign it", func(t *testing.T) {
		other := filepath.Join(t.TempDir(), "keys")
		_, pub := keygen(t, other, "someone-else")
		res, out := validate(t, "-trust-key", "someone-else="+pub, dir)
		if res.code != ExitError || out.OK {
			t.Fatalf("exit = %d out = %+v", res.code, out)
		}
		if got := out.checks("critical"); len(got) != 1 || got[0] != "signature.invalid" {
			t.Fatalf("critical checks = %v", got)
		}
	})
	t.Run("malformed key flags", func(t *testing.T) {
		for _, bad := range []string{"nokey", "=abc", "id=", "id=not-base64!", "id=" + strings.Repeat("A", 8)} {
			res, _ := validate(t, "-trust-key", bad, dir)
			if res.code != ExitUsage {
				t.Errorf("-trust-key %q: exit = %d, want usage error", bad, res.code)
			}
		}
	})
}

// TestPackageValidateBroken: each committed broken package fails with exit 1,
// prints its findings as JSON, and names the capability where there is one.
func TestPackageValidateBroken(t *testing.T) {
	cases := []struct {
		dir  string
		want []string
	}{
		{"broken-unsigned", []string{"signature.missing"}},
		{"broken-tampered", []string{"signature.tampered"}},
		{"broken-missing-doc-facet", []string{"docs.facet_missing"}},
		{"broken-missing-contract", []string{"contract.missing"}},
		{"broken-no-adapter-mapping", []string{"capability.adapter_mapping"}},
		{"broken-rag-no-scope", []string{"schema.rag_source"}},
		{"broken-missing-safety-probes", []string{"tests.safety_probe_missing"}},
		{"broken-incompatible-core", []string{"compat.core_out_of_range"}},
	}
	for _, c := range cases {
		t.Run(c.dir, func(t *testing.T) {
			res, out := validate(t, "-core-version", "0.1.0", filepath.Join(fixtures, c.dir))
			if res.code != ExitError {
				t.Fatalf("exit = %d, want 1\nstderr: %s", res.code, res.stderr)
			}
			if !strings.Contains(res.stderr, "critical finding") {
				t.Errorf("stderr = %q", res.stderr)
			}
			if out.OK || out.Critical == 0 {
				t.Fatalf("report says ok: %+v", out)
			}
			got := out.checks("critical")
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Fatalf("critical checks = %v, want %v", got, c.want)
			}
			for _, f := range out.Findings {
				if f.Message == "" || f.Remediation == "" {
					t.Errorf("incomplete finding: %+v", f)
				}
			}
		})
	}
}

func TestPackageValidateCoreVersionFlag(t *testing.T) {
	dir := filepath.Join(fixtures, "valid-minimal")
	res, out := validate(t, "-core-version", "1.0.0", dir)
	if res.code != ExitError || out.CoreVersion != "1.0.0" || len(out.checks("critical")) != 1 || out.checks("critical")[0] != "compat.core_out_of_range" {
		t.Fatalf("exit = %d out = %+v", res.code, out)
	}
	if res, _ := validate(t, "-core-version", "0.5.0", dir); res.code != ExitOK {
		t.Fatalf("0.5.0 is inside the range: exit = %d", res.code)
	}
}

// TestPackageValidateArchive: a .tar.gz of the package is validated exactly
// like the directory, and a hostile archive is a finding, not a crash.
func TestPackageValidateArchive(t *testing.T) {
	src := filepath.Join(fixtures, "valid-minimal")
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if err := tw.WriteHeader(&tar.Header{Name: "./" + filepath.ToSlash(rel), Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = tw.Write(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var gzb bytes.Buffer
	zw := gzip.NewWriter(&gzb)
	zw.Write(raw.Bytes())
	zw.Close()

	archive := filepath.Join(t.TempDir(), "demo-library-1.0.0.tar.gz")
	if err := os.WriteFile(archive, gzb.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	res, fromTar := validate(t, archive)
	if res.code != ExitOK || !fromTar.OK {
		t.Fatalf("exit = %d out = %+v\n%s", res.code, fromTar, res.stderr)
	}
	_, fromDir := validate(t, src)
	if fromTar.ManifestHash != fromDir.ManifestHash || fromTar.Files != fromDir.Files {
		t.Fatalf("archive and directory differ: %s/%d vs %s/%d", fromTar.ManifestHash, fromTar.Files, fromDir.ManifestHash, fromDir.Files)
	}

	bad := filepath.Join(t.TempDir(), "bad.tar.gz")
	if err := os.WriteFile(bad, []byte("not an archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, out := validate(t, bad)
	if res.code != ExitError || len(out.checks("critical")) != 1 || out.checks("critical")[0] != "archive.invalid" {
		t.Fatalf("exit = %d out = %+v", res.code, out)
	}
}

func TestPackageValidateUsage(t *testing.T) {
	for name, args := range map[string][]string{
		"no package":      {},
		"two packages":    {"a", "b"},
		"unknown flag":    {"-nope", "a"},
		"duplicate trust": {"-trust-key", "k=" + devkeys.PublicKeyBase64(), "-trust-key", "k=" + devkeys.PublicKeyBase64(), "-allow-test-keys", "a"},
	} {
		t.Run(name, func(t *testing.T) {
			res, _ := validate(t, args...)
			if res.code != ExitUsage || res.stdout != "" {
				t.Fatalf("exit = %d stdout = %q stderr = %q", res.code, res.stdout, res.stderr)
			}
		})
	}
	t.Run("missing path", func(t *testing.T) {
		res, _ := validate(t, filepath.Join(t.TempDir(), "nope"))
		if res.code != ExitError || res.stdout != "" {
			t.Fatalf("exit = %d stdout = %q", res.code, res.stdout)
		}
	})
	t.Run("help", func(t *testing.T) {
		res, _ := validate(t, "-h")
		if res.code != ExitOK || !strings.Contains(res.stdout, "alixai package validate") {
			t.Fatalf("exit = %d stdout = %q", res.code, res.stdout)
		}
	})
	t.Run("listed under package", func(t *testing.T) {
		if !strings.Contains(strings.Join(Default.Names(), ","), "package validate") {
			t.Fatalf("package validate is not registered: %v", Default.Names())
		}
	})
}

// ---- the security-domain rule (ADR 0016, item 5) ------------------------------------------------

// domainPackage returns a signed copy of the demo library as another application, declaring the
// given security domain ("" declares none).
func domainPackage(t *testing.T, app, domain string) string {
	t.Helper()
	src := filepath.Join(fixtures, "valid-minimal")
	dst := filepath.Join(t.TempDir(), "pkg")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		text := string(b)
		switch filepath.ToSlash(rel) {
		case "alixai.yaml":
			text = strings.Replace(text, "  id: demo-library\n", "  id: "+app+"\n", 1)
			if domain != "" {
				text += "security_domain: " + domain + "\n"
			}
		case "rag-sources/loan-policies.yaml":
			text = strings.Replace(text, "application_id: demo-library", "application_id: "+app, 1)
		}
		return os.WriteFile(filepath.Join(dst, rel), []byte(text), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	res, _ := run(t, Default, nil, "package", "sign", "-app", app, "-version", "1.0.0",
		"-key", devkeys.KeyID+"="+filepath.Join("..", "..", "tools", "devkeys", "test-only-publisher.key.pem"), dst)
	if res.code != ExitOK {
		t.Fatalf("sign %s: exit %d\n%s", app, res.code, res.stderr)
	}
	return dst
}

// domainDeployment is a deployment that trusts the audit-signing key and lets alice accept warnings.
type domainDeployment struct {
	*opsEnv
	signer string
}

func domainNewDeployment(t *testing.T) *domainDeployment {
	t.Helper()
	auditChecksProbesBuiltinChecksOnly(t) // this test is about activation, not the runtime probes
	transportsSwapDefault(t)              // no transport: every application is audited as unverified
	d := &domainDeployment{opsEnv: newOpsEnv(t, "integration_audit:\n  accept_operators: [alice]\n")}
	priv, pub, _ := d.genKey("audit-signer")
	d.signer = priv
	d.mustRun("trust", "add-key", "-app", trust.PlatformApplicationID, "-key-id", "audit-signer", "-public-key-file", pub, "-operator", "root")
	return d
}

// trustPublisher trusts the published test key for an application.
func (d *domainDeployment) trustPublisher(app string) {
	d.t.Helper()
	d.mustRun("trust", "add-key", "-app", app, "-key-id", devkeys.KeyID, "-public-key", devkeys.PublicKeyBase64(), "-allow-test-keys", "-operator", "root")
}

// do runs one command through the dispatcher's own command table and returns what it returned, so
// that the error is the typed error and not its text.
func (d *domainDeployment) do(args ...string) (string, error) {
	d.t.Helper()
	auditSection = auditConfig{}
	reg := config.NewRegistry()
	reg.Register(auditSectionName, &auditSection)
	full := append([]string{args[0], args[1], "-config", d.cfgPath}, args[2:]...)
	cmd, rest, ok := Default.match(full)
	if !ok {
		d.t.Fatalf("no command %v", args)
	}
	var out, errb bytes.Buffer
	err := cmd.Run(context.Background(), (&Env{
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb,
		LookupEnv: func(string) (string, bool) { return "", false }, Config: reg,
	}).withDefaults(), rest)
	return out.String(), err
}

// audit runs `audit run` for the application with the platform key.
func (d *domainDeployment) audit(app string) (runOut, error) {
	d.t.Helper()
	out, err := d.do("audit", "run", "-signing-key", "audit-signer="+d.signer, "-operator", "root", app)
	var run runOut
	if strings.HasPrefix(out, "{") {
		decode(d.t, out, &run)
	}
	return run, err
}

// acceptWarnings accepts every warning of the application's latest report, as alice.
func (d *domainDeployment) acceptWarnings(app string, run runOut) {
	d.t.Helper()
	for _, f := range run.Findings {
		if f.Severity == "warning" {
			d.mustRun("audit", "accept-warning", "-finding", f.Key, "-operator", "alice", "-reason", "reviewed by the operator", app)
		}
	}
}

// activate audits the application, accepts its warnings and audits again: the activating audit.
func (d *domainDeployment) activate(app string) (runOut, error) {
	d.t.Helper()
	first, err := d.audit(app)
	if _, refused := apps.AsRefusal(err); err == nil || refused {
		return first, err // activated, or refused by the rule, on the first audit: warnings accepted earlier still stand
	}
	d.acceptWarnings(app, first)
	return d.audit(app)
}

// openStore opens the deployment's store.
func (d *domainDeployment) openStore() *store.DB {
	d.t.Helper()
	db, err := store.Open(context.Background(), store.Options{Path: filepath.Join(d.dir, "data", store.DefaultFileName)})
	if err != nil {
		d.t.Fatal(err)
	}
	return db
}

// status reads the state of the application's newest package straight from the store, or "none".
func (d *domainDeployment) status(app string) string {
	d.t.Helper()
	db := d.openStore()
	defer db.Close()
	var st string
	err := db.QueryRowContext(context.Background(),
		`SELECT status FROM apppkg_packages WHERE application_id = ? ORDER BY installed_at DESC, status_changed_at DESC LIMIT 1`, app).Scan(&st)
	if errors.Is(err, sql.ErrNoRows) {
		return "none"
	}
	if err != nil {
		d.t.Fatal(err)
	}
	return st
}

// dropTheRule removes the store's triggers of the rule: the same deployment, without it.
func (d *domainDeployment) dropTheRule() {
	d.t.Helper()
	db := d.openStore()
	defer db.Close()
	for _, trg := range []string{"apps_security_domain_on_install", "apps_security_domain_on_activation"} {
		if _, err := db.ExecContext(context.Background(), `DROP TRIGGER `+trg); err != nil {
			d.t.Fatal(err)
		}
	}
}

func domainWantRefusal(t *testing.T, err error, phase apps.Phase, app, domain, active string) {
	t.Helper()
	ref, ok := apps.AsRefusal(err)
	if !ok {
		t.Fatalf("the command returned %v, want a refusal of the security-domain rule", err)
	}
	if contracts.CodeOf(err) != contracts.ErrConflict || ref.Details["reason"] != apps.ReasonSecurityDomain || ref.Details["phase"] != string(phase) {
		t.Errorf("refusal = %v, want code conflict, reason %s, phase %s", err, apps.ReasonSecurityDomain, phase)
	}
	if ref.Details["application_id"] != app || ref.Details["security_domain"] != domain || ref.Details["active_application_id"] != active {
		t.Errorf("the refusal names %v, want application %s (%q) refused for %s", ref.Details, app, domain, active)
	}
	if strings.Contains(err.Error(), "could not be recorded") {
		t.Errorf("the refusal's audit event is missing: %v", err)
	}
}

// A process refuses to install or activate an application whose security domain differs from the
// applications already active in it (ADR 0016 item 5): through the real `package install` and
// `audit run`, with a typed error and an audit event on the refused application's own chain, and
// two applications of one domain keep their own chains and share nothing.
func TestActivationRefusesADifferentSecurityDomain(t *testing.T) {
	d := domainNewDeployment(t)
	declared := []struct{ app, domain string }{{"ops-a", "ops"}, {"ops-d", "ops"}, {"legal-b", "legal"}, {"bare-c", ""}, {"legal-e", "legal"}}
	pkg := map[string]string{}
	for _, a := range declared {
		pkg[a.app] = domainPackage(t, a.app, a.domain)
		d.trustPublisher(a.app)
	}

	// Nothing is active, so every domain installs: b and c wait in quarantine for their turn.
	for _, app := range []string{"ops-a", "legal-b", "bare-c"} {
		if _, err := d.do("package", "install", "-operator", "root", pkg[app]); err != nil {
			t.Fatalf("install %s with nothing active: %v", app, err)
		}
	}

	// The first application activates, whatever its domain.
	if run, err := d.activate("ops-a"); err != nil || !run.Activated {
		t.Fatalf("ops-a (domain ops) with nothing else active: activated %v, %v", run.Activated, err)
	}

	t.Run("a different domain is refused at activation, typed and recorded, and stays quarantined", func(t *testing.T) {
		_, err := d.activate("legal-b")
		domainWantRefusal(t, err, apps.PhaseActivation, "legal-b", "legal", "ops-a")
		if st := d.status("legal-b"); st != "quarantined" {
			t.Errorf("the refused application is %s, want quarantined", st)
		}
		events := d.eventTypes("legal-b")
		if !contains(events, "package.domain_refused") || contains(events, "package.activated") {
			t.Errorf("legal-b's chain = %v: want package.domain_refused and no package.activated", events)
		}
		if contains(d.eventTypes("ops-a"), "package.domain_refused") {
			t.Error("the refusal was recorded on the chain of the application that was already active")
		}
	})

	t.Run("an application that declares no domain is not in the domain ops", func(t *testing.T) {
		_, err := d.activate("bare-c")
		domainWantRefusal(t, err, apps.PhaseActivation, "bare-c", "", "ops-a")
		if st := d.status("bare-c"); st != "quarantined" {
			t.Errorf("bare-c is %s, want quarantined", st)
		}
	})

	t.Run("a different domain is refused at install, typed and recorded", func(t *testing.T) {
		_, err := d.do("package", "install", "-operator", "root", pkg["legal-e"])
		domainWantRefusal(t, err, apps.PhaseInstall, "legal-e", "legal", "ops-a")
		if st := d.status("legal-e"); st != "none" {
			t.Errorf("a refused install stored a package: legal-e is %s", st)
		}
		events := d.eventTypes("legal-e")
		if !contains(events, "package.domain_refused") || contains(events, "package.installed") {
			t.Errorf("legal-e's chain = %v", events)
		}
		if res := d.run("package", "install", pkg["legal-e"]); res.code != ExitError || !strings.Contains(res.stderr, "conflict") || !strings.Contains(res.stderr, "legal-e") {
			t.Errorf("the command line says %q (exit %d): want the conflict, naming the application", res.stderr, res.code)
		}
	})

	t.Run("the same domain is admitted, and the two applications share nothing", func(t *testing.T) {
		if _, err := d.do("package", "install", "-operator", "root", pkg["ops-d"]); err != nil {
			t.Fatalf("install ops-d: %v", err)
		}
		if run, err := d.activate("ops-d"); err != nil || !run.Activated {
			t.Fatalf("ops-d (domain ops, like ops-a): activated %v, %v", run.Activated, err)
		}
		for _, app := range []string{"ops-a", "ops-d"} {
			events := d.eventTypes(app)
			if !contains(events, "package.activated") || contains(events, "package.domain_refused") {
				t.Errorf("%s's chain = %v", app, events)
			}
		}
		if d.status("ops-a") != "active" || d.status("ops-d") != "active" {
			t.Errorf("ops-a is %s and ops-d is %s, want both active", d.status("ops-a"), d.status("ops-d"))
		}
	})

	t.Run("when the applications of the other domain are out of service another domain may be activated", func(t *testing.T) {
		for _, app := range []string{"ops-a", "ops-d"} {
			d.mustRun("audit", "quarantine", "-reason", "moving the process to another domain", "-operator", "root", app)
		}
		if run, err := d.activate("legal-b"); err != nil || !run.Activated {
			t.Fatalf("legal-b with every application of the domain ops quarantined: activated %v, %v", run.Activated, err)
		}
		// and now ops-a is the one that does not fit
		_, err := d.activate("ops-a")
		domainWantRefusal(t, err, apps.PhaseActivation, "ops-a", "ops", "legal-b")
	})
}

// The test above shows the rule refuses; this one shows it is the rule that refuses and not the
// scenario: the same deployment with the store's triggers removed activates the other domain.
func TestActivationOfADifferentSecurityDomainNeedsTheRuleToBeRefused(t *testing.T) {
	d := domainNewDeployment(t)
	for _, a := range []struct{ app, domain string }{{"ops-a", "ops"}, {"legal-b", "legal"}} {
		d.trustPublisher(a.app)
		if _, err := d.do("package", "install", "-operator", "root", domainPackage(t, a.app, a.domain)); err != nil {
			t.Fatal(err)
		}
	}
	if run, err := d.activate("ops-a"); err != nil || !run.Activated {
		t.Fatalf("ops-a: activated %v, %v", run.Activated, err)
	}
	d.dropTheRule()
	run, err := d.activate("legal-b")
	if err != nil || !run.Activated || d.status("legal-b") != "active" {
		t.Fatalf("without the rule legal-b should activate beside ops-a: activated %v, %v, status %s", run.Activated, err, d.status("legal-b"))
	}
	if contains(d.eventTypes("legal-b"), "package.domain_refused") {
		t.Error("an event of a refusal that never happened")
	}
}

// The wrappers that type and record the refusal are in place for the two commands that can hit it.
func TestSecurityDomainRuleWrapsInstallAndActivation(t *testing.T) {
	for _, name := range []string{domainCommandInstall, domainCommandActivate} {
		if _, _, ok := Default.match(strings.Fields(name)); !ok {
			t.Errorf("%q is not a registered command", name)
		}
	}
	// a command that is not registered cannot be wrapped: found at start, not at the first refusal
	defer func() {
		if recover() == nil {
			t.Error("wrapping an unregistered command did not panic")
		}
	}()
	domainWrapCommand("package nonexistent", domainAfterInstall)
}
