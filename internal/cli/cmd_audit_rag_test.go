package cli

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	ragaudit "github.com/buxee/alix-ai/internal/integrationaudit/rag"
	"github.com/buxee/alix-ai/internal/rag/source"
	ragsources "github.com/buxee/alix-ai/internal/rag/sources"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/tools/devkeys"
)

const (
	auditRAGApp     = "demo-library"
	auditRAGMountID = "lib-docs"
)

// auditRAGMountPackage returns a signed copy of the demo library whose one RAG source, loan-policies,
// is a mounted volume (root: mount lib-docs, path policies) instead of documentation shipped in the
// package: the fs provider serves it, so the audit can reach it live once the deployment says where
// the mount is.
func auditRAGMountPackage(t *testing.T) string {
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
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	decl := filepath.Join(dst, "rag-sources", "loan-policies.yaml")
	b, err := os.ReadFile(decl)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for old, repl := range map[string]string{
		"type: documentation_package":                            "type: mounted_volume",
		"root:\n  kind: package\n  path: documentation/policies": "root:\n  kind: mount\n  mount_id: " + auditRAGMountID + "\n  path: policies",
	} {
		if !strings.Contains(text, old) {
			t.Fatalf("the fixture no longer contains %q", old)
		}
		text = strings.Replace(text, old, repl, 1)
	}
	if err := os.WriteFile(decl, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ := run(t, Default, nil, "package", "sign", "-app", auditRAGApp, "-version", "1.0.0",
		"-key", devkeys.KeyID+"="+filepath.Join("..", "..", "tools", "devkeys", "test-only-publisher.key.pem"), dst)
	if res.code != ExitOK {
		t.Fatalf("sign: exit %d\n%s", res.code, res.stderr)
	}
	return dst
}

// auditRAGEnv is a deployment with a package installed in quarantine, ready to be audited.
type auditRAGEnv struct {
	*opsEnv
	signer string
}

// auditRAGNewEnv builds a deployment on a throwaway data directory whose "rag.mounts" are mounts,
// trusts the platform and publisher keys and installs the package at pkgDir.
func auditRAGNewEnv(t *testing.T, pkgDir string, mounts map[string]string) *auditRAGEnv {
	t.Helper()
	// the audit and rag sections are process-wide variables: leave them as found
	oldAudit, oldRAG := auditSection, ragSection
	t.Cleanup(func() { auditSection, ragSection = oldAudit, oldRAG })
	// the audit of these tests is the static checks and the RAG checks; the runtime probes are
	// fail closed and, with no adapter, would block every package on something else
	auditChecksRAGWithoutRuntimeProbes(t)
	transportsSwapDefault(t)

	extra := ""
	if len(mounts) > 0 {
		extra = "rag:\n  mounts:\n"
		for id, p := range mounts {
			extra += "    " + id + ": \"" + filepath.ToSlash(p) + "\"\n"
		}
	}
	e := &auditRAGEnv{opsEnv: newOpsEnv(t, extra)}
	signerPriv, signerPub, _ := e.genKey("audit-signer")
	e.signer = signerPriv
	e.mustCmd("trust", "add-key", "-app", trust.PlatformApplicationID, "-key-id", "audit-signer", "-public-key-file", signerPub, "-operator", "root")
	e.mustCmd("trust", "add-key", "-app", auditRAGApp, "-key-id", devkeys.KeyID, "-public-key", devkeys.PublicKeyBase64(), "-allow-test-keys", "-operator", "root")
	e.mustCmd("package", "install", "-operator", "root", pkgDir)
	return e
}

// cmd runs one command the way a process would, in its own configuration registry, which holds
// the audit and rag sections (opsEnv.run holds only the first, so it refuses a "rag:" block).
func (e *auditRAGEnv) cmd(args ...string) result {
	e.t.Helper()
	auditSection = auditConfig{}
	ragSection = ragConfig{DiskFloorBytes: ragDefaultDiskFloor}
	reg := config.NewRegistry()
	reg.Register(auditSectionName, &auditSection)
	reg.Register(ragSectionName, &ragSection)
	var out, errb bytes.Buffer
	full := append([]string{args[0], args[1], "-config", e.cfgPath}, args[2:]...)
	code := Default.Run(context.Background(), full, &Env{
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb,
		LookupEnv: func(string) (string, bool) { return "", false }, Config: reg,
	})
	return result{code, out.String(), errb.String()}
}

func (e *auditRAGEnv) mustCmd(args ...string) result {
	e.t.Helper()
	res := e.cmd(args...)
	if res.code != ExitOK {
		e.t.Fatalf("%s: exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), res.code, res.stdout, res.stderr)
	}
	return res
}

// auditRAGRunOut is what `audit run` prints, as far as these tests read it.
type auditRAGRunOut struct {
	Summary  struct{ Critical, Warnings int }
	Findings []struct {
		CheckID  string `json:"check_id"`
		Severity string `json:"severity"`
		Message  string `json:"message"`
	}
}

// audit runs `audit run -no-activate`. A blocked audit exits non-zero and still prints its report.
func (e *auditRAGEnv) audit() auditRAGRunOut {
	e.t.Helper()
	res := e.cmd("audit", "run", "-signing-key", "audit-signer="+e.signer, "-operator", "root", "-no-activate", auditRAGApp)
	var out auditRAGRunOut
	decode(e.t, res.stdout, &out)
	return out
}

// findings returns the messages of the findings a check raised, by severity.
func (o auditRAGRunOut) findings(checkID, severity string) []string {
	var msgs []string
	for _, f := range o.Findings {
		if f.CheckID == checkID && f.Severity == severity {
			msgs = append(msgs, f.Message)
		}
	}
	return msgs
}

// register records a registration in the deployment's durable registry, as `alixai serve` would.
func (e *auditRAGEnv) register(packageSourceID string) {
	e.t.Helper()
	db, err := store.Open(context.Background(), store.Options{Path: filepath.Join(e.dir, "data", store.DefaultFileName)})
	if err != nil {
		e.t.Fatal(err)
	}
	defer db.Close()
	reg, err := ragsources.New(db, clock.Real{})
	if err != nil {
		e.t.Fatal(err)
	}
	// the demo library's source is shared at both levels, so a registration of it binds neither
	if err := reg.Put(context.Background(), ragsources.Registration{
		Scope: storetest.Scope(auditRAGApp, "tnt-1", "ws-1"), SourceID: ids.New(source.IDPrefix),
		PackageSourceID: packageSourceID, PackageVersion: "1.0.0", ManifestHash: "sha256:" + strings.Repeat("a", 64),
	}); err != nil {
		e.t.Fatal(err)
	}
}

// sources returns the RAG sources of the newest stored report: what the audit verified, and how.
func (e *auditRAGEnv) sources() []integrationaudit.SourceResult {
	e.t.Helper()
	db, err := store.Open(context.Background(), store.Options{Path: filepath.Join(e.dir, "data", store.DefaultFileName)})
	if err != nil {
		e.t.Fatal(err)
	}
	defer db.Close()
	var doc string
	if err := db.QueryRowContext(context.Background(),
		`SELECT document FROM integrationaudit_runs WHERE application_id = ? ORDER BY seq DESC LIMIT 1`, auditRAGApp).Scan(&doc); err != nil {
		e.t.Fatal(err)
	}
	signed, err := integrationaudit.ParseSignedReport([]byte(doc))
	if err != nil {
		e.t.Fatal(err)
	}
	return signed.Report.RAGSources
}

func auditRAGLiveSource(srcs []integrationaudit.SourceResult, id string) bool {
	for _, s := range srcs {
		if s.ID == id && s.Verified == "live" {
			return true
		}
	}
	return false
}

// TestAuditRunSeesTheDeploymentProvidersAndMounts: a real `alixai audit run` of a package whose
// source is a mount root reaches the live checks of section 3 when the deployment's "rag.mounts"
// holds the mount, through the filesystem provider the deployment runs; with no such mount, or one
// whose host path is gone, the audit says the source cannot be reached (it does not record the
// mount root as unverified, which is all it could do before the deployment was supplied).
func TestAuditRunSeesTheDeploymentProvidersAndMounts(t *testing.T) {
	pkg := auditRAGMountPackage(t)

	t.Run("a registered mount whose host path exists", func(t *testing.T) {
		host := t.TempDir()
		if err := os.MkdirAll(filepath.Join(host, "policies"), 0o755); err != nil {
			t.Fatal(err)
		}
		e := auditRAGNewEnv(t, pkg, map[string]string{auditRAGMountID: host})
		out := e.audit()
		if got := out.findings("ragprobe.source_unreachable", "warning"); len(got) != 0 {
			t.Errorf("the source was reported unreachable: %v", got)
		}
		if srcs := e.sources(); !auditRAGLiveSource(srcs, "loan-policies") {
			t.Errorf("loan-policies was not verified live: %+v", srcs)
		}
	})

	t.Run("a mount the deployment has not registered", func(t *testing.T) {
		e := auditRAGNewEnv(t, pkg, nil)
		out := e.audit()
		got := out.findings("ragprobe.source_unreachable", "warning")
		if len(got) != 1 || !strings.Contains(got[0], auditRAGMountID) {
			t.Fatalf("want one unreachable finding naming the mount, got %v\n%+v", got, out.Findings)
		}
		if auditRAGLiveSource(e.sources(), "loan-policies") {
			t.Errorf("a source on an unregistered mount was verified live")
		}
	})

	t.Run("a registered mount whose host path is gone", func(t *testing.T) {
		e := auditRAGNewEnv(t, pkg, map[string]string{auditRAGMountID: filepath.Join(t.TempDir(), "gone")})
		out := e.audit()
		if got := out.findings("ragprobe.source_unreachable", "warning"); len(got) != 1 {
			t.Fatalf("want one unreachable finding, got %v\n%+v", got, out.Findings)
		}
		if auditRAGLiveSource(e.sources(), "loan-policies") {
			t.Errorf("a source whose root is gone was verified live")
		}
	})
}

// TestAuditReportsAnUndeclaredRegistrationCritical: the registrations of the deployment's durable
// source registry reach the audit, so a registration the package does not declare is reported
// critical (ragprobe.source_undeclared) and blocks the package, and a registration of a declared
// source is not.
func TestAuditReportsAnUndeclaredRegistrationCritical(t *testing.T) {
	e := auditRAGNewEnv(t, filepath.Join(fixtures, "valid-minimal"), nil)
	e.register("loan-policies") // declared: bound as declared (shared at both levels)
	e.register("shadow-docs")   // not declared by the package

	out := e.audit()
	got := out.findings("ragprobe.source_undeclared", "critical")
	if len(got) != 1 || !strings.Contains(got[0], `"shadow-docs"`) || strings.Contains(got[0], "loan-policies") {
		t.Fatalf("want one critical naming shadow-docs, got %v\n%+v", got, out.Findings)
	}
	if out.Summary.Critical < 1 {
		t.Errorf("an undeclared registration must block the package: %+v", out.Summary)
	}
	if got := out.findings("ragprobe.source_binding_mismatch", "critical"); len(got) != 0 {
		t.Errorf("the declared registration was reported as bound against its declaration: %v", got)
	}

	// and with nothing undeclared in the registry the finding is not there
	clean := auditRAGNewEnv(t, filepath.Join(fixtures, "valid-minimal"), nil)
	clean.register("loan-policies")
	if got := clean.audit().findings("ragprobe.source_undeclared", "critical"); len(got) != 0 {
		t.Errorf("a declared registration was reported undeclared: %v", got)
	}
}

// TestDeploymentSourceDoesNotPileUpAcrossAudits: an audit takes its deployment source out again
// when it ends. Two audits in one process (two deployments, two stores) each see their own
// registry: if the first audit's source stayed installed it would be consulted first by the second,
// with a store that is closed, and the second audit would lose its critical finding.
func TestDeploymentSourceDoesNotPileUpAcrossAudits(t *testing.T) {
	first := auditRAGNewEnv(t, filepath.Join(fixtures, "valid-minimal"), nil)
	first.register("shadow-first")
	if got := first.audit().findings("ragprobe.source_undeclared", "critical"); len(got) != 1 || !strings.Contains(got[0], "shadow-first") {
		t.Fatalf("first audit: %v", got)
	}

	second := auditRAGNewEnv(t, filepath.Join(fixtures, "valid-minimal"), nil)
	second.register("ghost-second")
	got := second.audit().findings("ragprobe.source_undeclared", "critical")
	if len(got) != 1 || !strings.Contains(got[0], "ghost-second") || strings.Contains(got[0], "shadow-first") {
		t.Fatalf("second audit saw the wrong registry: %v", got)
	}

	// nothing is left behind: a source installed now is the first one consulted, and answers
	var consulted bool
	remove := ragaudit.SetDeploymentSource(func(context.Context, *integrationaudit.Input) (*ragaudit.Deployment, error) {
		consulted = true
		return &ragaudit.Deployment{}, nil
	})
	defer remove()
	for _, c := range ragaudit.Checks(ragaudit.Options{}) {
		if c.ID() != ragaudit.IDSources {
			continue
		}
		res, err := apppkg.Load(context.Background(), apppkg.FromDir(filepath.Join(fixtures, "valid-minimal")), apppkg.Options{CoreVersion: "0.1.0"})
		if err != nil || res.Package == nil {
			t.Fatalf("load: %v %v", err, res.Findings)
		}
		if _, err := c.Run(context.Background(), &integrationaudit.Input{Package: res.Package, Clock: clock.Real{}}); err != nil {
			t.Fatal(err)
		}
	}
	if !consulted {
		t.Errorf("an earlier audit's deployment source is still installed and answers before a new one")
	}
}

// TestAuditRAGAddedProvidersJoinTheFilesystemOne: a provider registered with auditRAGAddProvider is
// among the providers of the next audit, after the filesystem provider, and a constructor that
// returns nothing adds nothing.
func TestAuditRAGAddedProvidersJoinTheFilesystemOne(t *testing.T) {
	old := auditRAGProviders
	t.Cleanup(func() { auditRAGProviders = old })
	base := auditRAGProvidersFor(clock.Real{})
	if len(base) < 1 || !providesKind(base[0], ap.RAGSourceTypeMountedVolume) || !providesKind(base[len(base)-1], ap.RAGSourceTypeApplicationDocumentAPI) {
		t.Fatalf("the audit's own provider is the filesystem one: %v", base)
	}
	auditRAGAddProvider(nil)
	auditRAGAddProvider(func(clock.Clock) source.Provider { return nil })
	auditRAGAddProvider(func(clock.Clock) source.Provider {
		return auditRAGKindsOnly{kinds: []ap.RAGSourceType{ap.RAGSourceTypeHTTPAPI}}
	})
	got := auditRAGProvidersFor(clock.Real{})
	if len(got) != len(base)+1 || !providesKind(got[len(base)], ap.RAGSourceTypeHTTPAPI) {
		t.Fatalf("providers = %v", got)
	}
}

func providesKind(p source.Provider, k ap.RAGSourceType) bool {
	for _, x := range p.Kinds() {
		if x == k {
			return true
		}
	}
	return false
}

// auditRAGKindsOnly is a provider that serves only the kinds it lists: every other method is the
// embedded nil provider's, which these tests never call.
type auditRAGKindsOnly struct {
	source.Provider
	kinds []ap.RAGSourceType
}

func (k auditRAGKindsOnly) Kinds() []ap.RAGSourceType { return k.kinds }
