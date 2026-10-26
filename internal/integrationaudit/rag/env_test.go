package rag

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	fsprovider "github.com/buxee/alix-ai/internal/rag/providers/fs"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

const (
	clinicDir       = "../../../examples/reference-apps/clinic-booking/package"
	validMinimalDir = "../../../examples/packages/valid-minimal"
)

var testT0 = time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)

// loadRef loads one of the repository's two signed reference packages.
func loadRef(t testing.TB, dir string) *apppkg.Package {
	t.Helper()
	res, err := apppkg.Load(context.Background(), apppkg.FromDir(dir), apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
	if err != nil {
		t.Fatal(err)
	}
	if res.Package == nil {
		t.Fatalf("package %s did not load: %v", dir, res.Findings)
	}
	return res.Package
}

// folderSource is a declaration of a source the lab can host: a folder source whose scope
// bindings are the given ones. It is the library's loan-policies declaration with its
// root, type and bindings changed.
func folderSource(t testing.TB, root string, tenant, workspace ap.RAGSourceScopeBinding) ap.RAGSource {
	t.Helper()
	pkg := loadRef(t, validMinimalDir)
	decl := pkg.Sources["loan-policies"]
	decl.Type = ap.RAGSourceTypeLocalFolder
	decl.Root = ap.RAGSourceRoot{Value: ap.RAGSourceRootFolder{Kind: "folder", Path: root}}
	decl.Scope.Tenant, decl.Scope.Workspace = tenant, workspace
	return decl
}

var (
	bindRequest = ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeRequest}
	bindShared  = ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeShared}
)

func bindFixed(id string) ap.RAGSourceScopeBinding {
	var b ap.RAGSourceScopeBinding
	if err := json.Unmarshal([]byte(`{"mode":"fixed","id":"`+id+`"}`), &b); err != nil {
		panic(err)
	}
	return b
}

// labPackage is the library reference package with its one source made a request-bound
// folder source, so that the lab can host it. Its suite names the tenants (network-a with two
// branches, network-b with one), the principals (member-a1, member-a2, visitor-b1), the
// retrieve probe member-a1-allow and the two RAG smoke paths.
func labPackage(t testing.TB) *apppkg.Package {
	t.Helper()
	pkg := loadRef(t, validMinimalDir)
	root := filepath.Join(t.TempDir(), "docs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	pkg.Sources["loan-policies"] = folderSource(t, root, bindRequest, bindRequest)
	return pkg
}

// folderRoot returns the root of the lab package's folder source.
func folderRoot(pkg *apppkg.Package) string {
	return pkg.Sources["loan-policies"].Root.Value.(ap.RAGSourceRootFolder).Path
}

// auditInput is an audit input for a package: a valid audit scope, a fixed clock and the four
// RAG checks listed as registered, as they are in the binary.
func auditInput(pkg *apppkg.Package) *integrationaudit.Input {
	return &integrationaudit.Input{
		Package: pkg,
		Scope: contracts.Scope{
			DeploymentID: integrationaudit.DefaultDeploymentID, ApplicationID: string(pkg.Manifest.Application.ID),
			TenantID: "tnt_audit", WorkspaceID: "ws_audit", PrincipalID: "usr_audit", Roles: []string{"auditor"},
			CapabilityScopes: []string{"audit"}, SessionID: "ses_audit",
		},
		Clock: clock.NewFake(testT0),
		Registered: []integrationaudit.CheckInfo{
			{ID: IDSources, Section: integrationaudit.SectionRAGInventory},
			{ID: IDCitations, Section: integrationaudit.SectionRAGInventory},
			{ID: IDScope, Section: integrationaudit.SectionScope},
			{ID: IDSmoke, Section: integrationaudit.SectionRuntimeSmoke},
		},
	}
}

// folderDeployment is a deployment that serves folder sources, lists no registrations and
// knows no mounts.
func folderDeployment(*integrationaudit.Input) *Deployment {
	return &Deployment{Providers: []source.Provider{fsprovider.New(fsprovider.Config{})}}
}

// checkOf builds the check with the given ID from options.
func checkOf(t testing.TB, o Options, id string) integrationaudit.Check {
	t.Helper()
	for _, c := range Checks(o) {
		if c.ID() == id {
			return c
		}
	}
	t.Fatalf("no check %s", id)
	return nil
}

// runCheck runs one check and requires the outcome to be one the audit framework accepts.
func runCheck(t testing.TB, c integrationaudit.Check, in *integrationaudit.Input) *integrationaudit.Outcome {
	t.Helper()
	out, err := c.Run(context.Background(), in)
	if err != nil {
		t.Fatalf("%s: %v", c.ID(), err)
	}
	validOutcome(t, c.ID(), out)
	return out
}

// validOutcome holds an outcome to what the framework requires of one: complete findings, a
// kind on every probe, consistent coverage and extension data that can be signed.
func validOutcome(t testing.TB, id string, o *integrationaudit.Outcome) {
	t.Helper()
	for _, f := range o.Findings {
		if err := f.Validate(); err != nil {
			t.Errorf("%s: invalid finding %v: %v", id, f, err)
		}
	}
	for _, p := range o.Probes {
		if p.Kind == "" || p.Subject == "" {
			t.Errorf("%s: a probe without kind or subject: %+v", id, p)
		}
	}
	for _, c := range o.Coverage {
		if c.Name == "" || c.Covered < 0 || c.Total < c.Covered {
			t.Errorf("%s: inconsistent coverage %+v", id, c)
		}
	}
	for _, u := range o.Unverified {
		if u.Subject == "" || u.Reason == "" || u.Section == 0 || !u.Target.Valid() {
			t.Errorf("%s: incomplete unverified item %+v", id, u)
		}
	}
	if o.Extension != nil {
		if _, err := integrationaudit.Digest(o.Extension); err != nil {
			t.Errorf("%s: extension cannot be signed: %v", id, err)
		}
	}
	noGeneratedIDs(t, id, o)
	if _, err := integrationaudit.Digest(struct {
		F []contracts.Finding
		U []integrationaudit.Unverified
		S []integrationaudit.SourceResult
		P []integrationaudit.ProbeResult
		C []integrationaudit.CoverageItem
	}{o.Findings, o.Unverified, o.Sources, o.Probes, o.Coverage}); err != nil {
		t.Errorf("%s: the results cannot be signed: %v", id, err)
	}
}

func findingIDs(o *integrationaudit.Outcome, sev contracts.Severity) []string {
	var out []string
	for _, f := range o.Findings {
		if f.Severity == sev {
			out = append(out, f.CheckID)
		}
	}
	sort.Strings(out)
	return out
}

func hasFinding(o *integrationaudit.Outcome, id string) bool {
	for _, f := range o.Findings {
		if f.CheckID == id {
			return true
		}
	}
	return false
}

func hasCritical(o *integrationaudit.Outcome) bool {
	return len(findingIDs(o, contracts.SeverityCritical)) > 0
}

func unverifiedSubjects(o *integrationaudit.Outcome) []string {
	var out []string
	for _, u := range o.Unverified {
		out = append(out, u.Subject)
	}
	sort.Strings(out)
	return out
}

func hasUnverified(o *integrationaudit.Outcome, subject string) bool {
	for _, u := range o.Unverified {
		if u.Subject == subject {
			return true
		}
	}
	return false
}

func probeNamed(o *integrationaudit.Outcome, contains string) *integrationaudit.ProbeResult {
	for i := range o.Probes {
		if strings.Contains(o.Probes[i].Subject, contains) {
			return &o.Probes[i]
		}
	}
	return nil
}

// dump renders an outcome for a failure message.
func dump(o *integrationaudit.Outcome) string {
	b, _ := json.MarshalIndent(struct {
		Findings   []contracts.Finding
		Unverified []integrationaudit.Unverified
		Probes     []integrationaudit.ProbeResult
		Coverage   []integrationaudit.CoverageItem
		Sources    []integrationaudit.SourceResult
	}{o.Findings, o.Unverified, o.Probes, o.Coverage, o.Sources}, "", " ")
	return string(b)
}

// failedProbes lists the probes that did not pass.
func failedProbes(o *integrationaudit.Outcome) []integrationaudit.ProbeResult {
	var out []integrationaudit.ProbeResult
	for _, p := range o.Probes {
		if !p.Passed {
			out = append(out, p)
		}
	}
	return out
}

// readDirNames lists the names in a directory.
func readDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out, nil
}

// generatedID matches what the audit generates for a run, and must never reach a report: a
// source, file, tenant, workspace, principal, session or generation identifier, a marker of
// the lab's synthetic passages, or the lab's throwaway directory.
var generatedID = regexp.MustCompile(`\b(src|fil|tnt|ws|usr|ses|gen|chk)_[a-z0-9_]{6,}|\bmk[a-z]{10}\b|alixai-audit-rag`)

// noGeneratedIDs requires the whole outcome to be free of what the audit generates for a run.
func noGeneratedIDs(t testing.TB, id string, o *integrationaudit.Outcome) {
	t.Helper()
	raw, err := json.Marshal(struct {
		F []contracts.Finding
		U []integrationaudit.Unverified
		S []integrationaudit.SourceResult
		P []integrationaudit.ProbeResult
		C []integrationaudit.CoverageItem
		E map[string]any
	}{o.Findings, o.Unverified, o.Sources, o.Probes, o.Coverage, o.Extension})
	if err != nil {
		t.Fatal(err)
	}
	if m := generatedID.FindString(string(raw)); m != "" {
		t.Errorf("%s: the report carries %q, which the audit generated for the run", id, m)
	}
	if strings.Contains(string(raw), os.TempDir()) {
		t.Errorf("%s: the report carries the temporary directory", id)
	}
}
